// Package land drives a stack through GitHub's merge queue. It only needs
// the PR bodies and a clone, so it runs the same locally and in Actions.
//
// Two landing modes:
//
//   - atomic (default for new stacks): every part is reviewed on its own,
//     then the top PR is retargeted to trunk and queued once. The queue
//     tests and merges the whole stack as a single entry, so trunk never
//     holds half of it and one revert undoes all of it.
//   - incremental: the bottom PR is queued; each merge restacks the next
//     PR onto trunk and queues it.
//
// In both modes a held (activation) PR is restacked onto trunk after the
// rest lands but never queued automatically.
package land

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"stackq/internal/forge"
	"stackq/internal/git"
	"stackq/internal/prio"
	"stackq/internal/stack"
)

const (
	LabelConflict   = "stackq:conflict"
	LabelBlocked    = "stackq:blocked"
	LabelLanding    = "stackq:landing"
	LabelActivation = "stackq:activation"
	LabelIntegrity  = "stackq:integrity"
	LabelRevert     = "stackq:revert"
)

type Runner struct {
	Repo    *git.Repo
	Forge   forge.Forge
	Remote  string
	Enqueue bool // incremental mode: queue the next PR after restacking it
	// Atomic gate.
	MinApprovals    int  // per part; default 1
	AllowUnreviewed bool // skip the approval gate
	// KeepBranches leaves the parts' branches after an atomic landing.
	KeepBranches bool
	// Prio routes every enqueue through the priority pool (see schedule.go).
	Prio *prio.Config
	Now  func() time.Time
	Log  io.Writer
}

func (c *Runner) logf(format string, a ...any) {
	if c.Log != nil {
		fmt.Fprintf(c.Log, format+"\n", a...)
	}
}

func (c *Runner) remoteRef(branch string) string { return c.Remote + "/" + branch }

type stackPRs struct {
	prs   []forge.PR
	marks map[int]stack.Marker
}

func (c *Runner) loadStack(name string) (*stackPRs, error) {
	all, err := c.Forge.ListPRs("all")
	if err != nil {
		return nil, err
	}
	sp := &stackPRs{marks: map[int]stack.Marker{}}
	seen := map[int]bool{}
	for _, p := range all {
		if m, ok := stack.ParseMarker(p.Body); ok && m.Stack == name && !seen[p.Number] {
			seen[p.Number] = true
			sp.prs = append(sp.prs, p)
			sp.marks[p.Number] = m
		}
	}
	sort.Slice(sp.prs, func(i, j int) bool { return sp.marks[sp.prs[i].Number].Index < sp.marks[sp.prs[j].Number].Index })
	return sp, nil
}

// lander is the highest-index PR that is not held back.
func (sp *stackPRs) lander() (forge.PR, bool) {
	var best forge.PR
	found := false
	for _, p := range sp.prs {
		m := sp.marks[p.Number]
		if m.Hold || p.State == "CLOSED" && !m.Atomic() {
			continue
		}
		if !found || m.Index > sp.marks[best.Number].Index {
			best, found = p, true
		}
	}
	return best, found
}

func (sp *stackPRs) nav() []stack.NavEntry {
	l, ok := sp.lander()
	landed := ok && l.Merged()
	var nav []stack.NavEntry
	for _, p := range sp.prs {
		m := sp.marks[p.Number]
		e := stack.NavEntry{Index: m.Index, PR: p.Number, Branch: p.HeadRef, Merged: p.Merged(), Hold: m.Hold}
		if p.State == "CLOSED" {
			if !(m.Atomic() && landed && !m.Hold) {
				continue
			}
			e.Closed = true
		}
		nav = append(nav, e)
	}
	return nav
}

func (sp *stackPRs) set(p forge.PR) {
	for i := range sp.prs {
		if sp.prs[i].Number == p.Number {
			sp.prs[i] = p
		}
	}
}

// Land queues a stack. Given any PR of an atomic stack it lands the whole
// stack; for an incremental stack it queues that PR, which must target trunk.
func (c *Runner) Land(n int) error {
	pr, err := c.Forge.GetPR(n)
	if err != nil {
		return err
	}
	m, isStack := stack.ParseMarker(pr.Body)
	if isStack && m.Atomic() {
		return c.landAtomic(m)
	}
	if !pr.Open() {
		return fmt.Errorf("#%d is %s", n, strings.ToLower(pr.State))
	}
	if isStack && m.Hold {
		c.logf("#%d is the activation PR; queueing it because you asked", n)
	}
	if isStack && pr.BaseRef != m.Trunk {
		return fmt.Errorf("#%d targets %s, not %s; land the PRs below it first", n, pr.BaseRef, m.Trunk)
	}
	c.Forge.RemoveLabels(n, LabelBlocked, LabelConflict)
	trunk := pr.BaseRef
	if isStack {
		trunk = m.Trunk
	}
	if err := c.submit(pr, trunk); err != nil {
		return err
	}
	if !c.prioOn() {
		c.logf("⇢ #%d is in the merge queue (or will join it when its checks pass)", n)
	}
	return nil
}

// landAtomic gates every part, retargets the lander to trunk and queues it
// as one entry carrying the whole stack.
func (c *Runner) landAtomic(m stack.Marker) error {
	sp, err := c.loadStack(m.Stack)
	if err != nil {
		return err
	}
	var parts []forge.PR
	for _, p := range sp.prs {
		pm := sp.marks[p.Number]
		switch {
		case pm.Hold:
		case p.Open():
			parts = append(parts, p)
		case p.State == "CLOSED":
			return fmt.Errorf("#%d (part %d) was closed without merging; reopen it or rebuild the stack", p.Number, pm.Index)
		}
	}
	if len(parts) == 0 {
		return fmt.Errorf("stack %s has nothing left to land", m.Stack)
	}
	lander := parts[len(parts)-1]

	fetch := []string{"fetch", "--quiet", c.Remote, "+refs/heads/" + m.Trunk + ":refs/remotes/" + c.remoteRef(m.Trunk)}
	for _, p := range parts {
		fetch = append(fetch, "+refs/heads/"+p.HeadRef+":refs/remotes/"+c.remoteRef(p.HeadRef))
	}
	if _, err := c.Repo.Run(fetch...); err != nil {
		return err
	}

	minApprovals := c.MinApprovals
	if minApprovals <= 0 {
		minApprovals = 1
	}
	var problems []string
	type row struct {
		pr        forge.PR
		approvers string
		checks    string
	}
	var rows []row
	for i, p := range parts {
		d, err := c.Forge.Details(p.Number)
		if err != nil {
			return err
		}
		idx := sp.marks[p.Number].Index
		if len(d.ChangesRequested) > 0 {
			problems = append(problems, fmt.Sprintf("part %d #%d: changes requested by %s", idx, p.Number, strings.Join(d.ChangesRequested, ", ")))
		}
		if !c.AllowUnreviewed && len(d.Approvals) < minApprovals {
			problems = append(problems, fmt.Sprintf("part %d #%d: %d of %d required approvals", idx, p.Number, len(d.Approvals), minApprovals))
		}
		switch d.Checks {
		case "failure":
			problems = append(problems, fmt.Sprintf("part %d #%d: checks are failing", idx, p.Number))
		case "pending":
			problems = append(problems, fmt.Sprintf("part %d #%d: checks are still running", idx, p.Number))
		}
		if i > 0 {
			below, above := c.remoteRef(parts[i-1].HeadRef), c.remoteRef(p.HeadRef)
			if !c.Repo.IsAncestor(below, above) {
				problems = append(problems, fmt.Sprintf("part %d #%d is not stacked on part %d; run stackq sync && stackq push",
					idx, p.Number, sp.marks[parts[i-1].Number].Index))
			}
		}
		approvers := strings.Join(d.Approvals, ", ")
		if approvers == "" {
			approvers = "—"
		}
		rows = append(rows, row{p, approvers, d.Checks})
	}
	if len(problems) > 0 {
		msg := "stack " + m.Stack + " is not ready to land:\n  - " + strings.Join(problems, "\n  - ")
		c.Forge.Comment(lander.Number, "⏸️ **Not landing yet.** Every part must be approved and green before the stack lands as one:\n\n- "+
			strings.Join(problems, "\n- ")+"\n\nFix these, then run `stackq land` again.")
		c.Forge.AddLabels(lander.Number, LabelBlocked)
		return errors.New(msg)
	}

	if lander.BaseRef != m.Trunk {
		base := m.Trunk
		if err := c.Forge.EditPR(lander.Number, &base, nil); err != nil {
			return err
		}
		lander.BaseRef = m.Trunk
		c.logf("~ #%d now targets %s and carries all %d parts", lander.Number, m.Trunk, len(parts))
	}
	head, err := c.Repo.RevParse(c.remoteRef(lander.HeadRef))
	if err != nil {
		return err
	}
	lander.HeadSHA = head

	var b strings.Builder
	fmt.Fprintf(&b, "🚀 **Landing stack `%s` atomically.** This PR now targets `%s` and carries all %d parts as a single merge-queue entry. "+
		"Trunk gets either the whole stack or none of it.\n\n| Part | PR | Approved by | Checks |\n|---|---|---|---|\n", m.Stack, m.Trunk, len(parts))
	for _, r := range rows {
		fmt.Fprintf(&b, "| %d | #%d | %s | %s |\n", sp.marks[r.pr.Number].Index, r.pr.Number, r.approvers, r.checks)
	}
	fmt.Fprintf(&b, "\nRoll back after merge with `stackq revert --pr %d`.", lander.Number)
	c.Forge.Comment(lander.Number, b.String())
	for _, p := range parts {
		c.Forge.AddLabels(p.Number, LabelLanding)
	}
	c.Forge.RemoveLabels(lander.Number, LabelBlocked, LabelConflict)
	if err := c.submit(lander, m.Trunk); err != nil {
		return err
	}
	c.logf("⇢ #%d carries the whole stack (%d parts) into the queue", lander.Number, len(parts))
	return nil
}

// AfterMerge reacts to a merged stack PR: restack its children onto trunk,
// queue the next one (incremental), finish an atomic landing, and announce
// the activation PR once everything else is on trunk.
func (c *Runner) AfterMerge(n int) error {
	r := c.Repo
	pr, err := c.Forge.GetPR(n)
	if err != nil {
		return err
	}
	if !pr.Merged() {
		c.logf("#%d is not merged; nothing to do", n)
		return nil
	}
	m, ok := stack.ParseMarker(pr.Body)
	if !ok {
		c.logf("#%d is not part of a stackq stack; nothing to do", n)
		return nil
	}
	sp, err := c.loadStack(m.Stack)
	if err != nil {
		return err
	}
	pr.State = "MERGED" // in case the listing lags behind the event
	sp.set(pr)
	lander, _ := sp.lander()
	isLander := !m.Hold && lander.Number == pr.Number
	restOnTrunk := isLander // nothing but held PRs remain above

	var children []forge.PR
	for _, p := range sp.prs {
		if p.Open() && sp.marks[p.Number].Parent == pr.HeadRef {
			children = append(children, p)
		}
	}

	trunk := m.Trunk
	var failed []string
	done := map[int]bool{}
	if len(children) > 0 {
		fetch := []string{"fetch", "--quiet", c.Remote, "+refs/heads/" + trunk + ":refs/remotes/" + c.remoteRef(trunk)}
		for _, ch := range children {
			fetch = append(fetch, "+refs/heads/"+ch.HeadRef+":refs/remotes/"+c.remoteRef(ch.HeadRef))
		}
		if _, err := r.Run(fetch...); err != nil {
			return err
		}
		onto, err := r.RevParse(c.remoteRef(trunk))
		if err != nil {
			return err
		}
		for _, ch := range children {
			cm := sp.marks[ch.Number]
			old, err := r.RevParse(c.remoteRef(ch.HeadRef))
			if err != nil {
				return err
			}
			// The child was built on the parent's pre-merge head; replay only
			// the commits after it. Fall back to a plain rebase, which still
			// drops commits whose patches are already in trunk.
			upstream := ""
			if pr.HeadSHA != "" && r.IsAncestor(pr.HeadSHA, old) {
				upstream = pr.HeadSHA
			}
			newSHA, err := r.RebaseCommit(old, onto, upstream, ch.HeadRef)
			if err != nil {
				var ce *git.ConflictError
				if !errors.As(err, &ce) {
					return err
				}
				c.reportConflict(ch, pr, trunk, ce)
				failed = append(failed, fmt.Sprintf("#%d", ch.Number))
				continue
			}
			if newSHA != old {
				lease := fmt.Sprintf("--force-with-lease=refs/heads/%s:%s", ch.HeadRef, old)
				if _, err := r.Run("push", "--quiet", lease, c.Remote, newSHA+":refs/heads/"+ch.HeadRef); err != nil {
					return fmt.Errorf("push %s: %w", ch.HeadRef, err)
				}
			}
			cm.Parent = trunk
			sp.marks[ch.Number] = cm
			ch.BaseRef, ch.HeadSHA = trunk, newSHA
			ch.Body = stack.MergeBody(ch.Body, stack.RenderBlock(cm, sp.nav()))
			sp.set(ch)
			base, body := trunk, ch.Body
			if err := c.Forge.EditPR(ch.Number, &base, &body); err != nil {
				return err
			}
			done[ch.Number] = true
			c.logf("↻ #%d %s: rebased onto %s (%s) and retargeted", ch.Number, ch.HeadRef, trunk, git.Short(newSHA))

			switch {
			case cm.Hold:
				// A held PR always sits on the last part, so everything
				// else is on trunk now.
				c.Forge.AddLabels(ch.Number, LabelActivation)
				c.Forge.Comment(ch.Number, fmt.Sprintf("🔒 The rest of stack `%s` is on `%s`. This activation PR is rebased and ready, "+
					"but stackq will not queue it: merge it (or `stackq land --pr %d`) when you are ready to switch the feature on.",
					m.Stack, trunk, ch.Number))
				c.logf("🔒 #%d (activation) is ready and left for a human", ch.Number)
			case m.Atomic() && !isLander:
				// Keep the rest of the chain stacked so it can still land as one.
				if err := c.cascade(sp, ch.HeadRef, old, newSHA); err != nil {
					var ce *git.ConflictError
					if !errors.As(err, &ce) {
						return err
					}
					failed = append(failed, ce.Branch)
				}
				c.Forge.Comment(ch.Number, fmt.Sprintf("⚠️ #%d was merged on its own, although stack `%s` lands atomically. "+
					"The rest of the stack was rebased onto `%s` but nothing was queued. Run `stackq land` to land the remainder together.",
					n, m.Stack, trunk))
			case c.Enqueue && !m.Atomic():
				if err := c.submit(ch, trunk); err != nil {
					c.Forge.Comment(ch.Number, fmt.Sprintf("⚠️ stackq restacked this PR after #%d merged, but could not queue it:\n\n```\n%v\n```\n\nRun `stackq land --pr %d` once that is sorted out.", n, err, ch.Number))
					c.Forge.AddLabels(ch.Number, LabelBlocked)
					failed = append(failed, fmt.Sprintf("#%d (enqueue)", ch.Number))
					continue
				}
				c.logf("⇢ #%d queued", ch.Number)
			}
		}
	} else {
		c.logf("stack %s: #%d had no open PR above it", m.Stack, n)
	}
	if !m.Atomic() {
		restOnTrunk = true
		for _, p := range sp.prs {
			if pm := sp.marks[p.Number]; !pm.Hold && !p.Merged() {
				restOnTrunk = false
			}
		}
	}

	if isLander && m.Atomic() {
		if err := c.finishAtomic(sp, pr, m, len(failed) == 0); err != nil {
			return err
		}
	}
	if err := c.refreshNav(sp, done); err != nil {
		return err
	}
	if restOnTrunk {
		if err := c.reportLanded(sp, pr, m); err != nil {
			return err
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("stack %s paused at %s", m.Stack, strings.Join(failed, ", "))
	}
	return nil
}

// cascade rebases every open descendant of parent from oldTip onto newTip
// and pushes it, keeping the chain intact. Bases don't change.
func (c *Runner) cascade(sp *stackPRs, parent, oldTip, newTip string) error {
	r := c.Repo
	for _, p := range sp.prs {
		if !p.Open() || sp.marks[p.Number].Parent != parent {
			continue
		}
		if _, err := r.Run("fetch", "--quiet", c.Remote, "+refs/heads/"+p.HeadRef+":refs/remotes/"+c.remoteRef(p.HeadRef)); err != nil {
			return err
		}
		old, err := r.RevParse(c.remoteRef(p.HeadRef))
		if err != nil {
			return err
		}
		upstream := ""
		if r.IsAncestor(oldTip, old) {
			upstream = oldTip
		}
		newSHA, err := r.RebaseCommit(old, newTip, upstream, p.HeadRef)
		if err != nil {
			var ce *git.ConflictError
			if errors.As(err, &ce) {
				c.reportConflict(p, forge.PR{Number: 0}, parent, ce)
			}
			return err
		}
		if newSHA != old {
			lease := fmt.Sprintf("--force-with-lease=refs/heads/%s:%s", p.HeadRef, old)
			if _, err := r.Run("push", "--quiet", lease, c.Remote, newSHA+":refs/heads/"+p.HeadRef); err != nil {
				return fmt.Errorf("push %s: %w", p.HeadRef, err)
			}
			c.logf("↻ #%d %s: restacked onto %s", p.Number, p.HeadRef, parent)
		}
		if err := c.cascade(sp, p.HeadRef, old, newSHA); err != nil {
			return err
		}
	}
	return nil
}

// finishAtomic closes the parts that landed inside the lander and deletes
// their branches.
func (c *Runner) finishAtomic(sp *stackPRs, lander forge.PR, m stack.Marker, childrenOK bool) error {
	r := c.Repo
	var parts []forge.PR
	for _, p := range sp.prs {
		if pm := sp.marks[p.Number]; !pm.Hold && p.Number != lander.Number {
			parts = append(parts, p)
		}
	}
	for _, p := range parts {
		if !p.Open() {
			continue
		}
		c.Forge.Comment(p.Number, fmt.Sprintf("✅ Landed on `%s` as part of stack `%s`, inside #%d (one merge-queue entry). "+
			"Closing this part. Roll back the whole stack with `stackq revert --pr %d`.", m.Trunk, m.Stack, lander.Number, lander.Number))
		if err := c.Forge.ClosePR(p.Number); err != nil {
			return err
		}
		c.Forge.RemoveLabels(p.Number, LabelLanding)
		p.State = "CLOSED"
		sp.set(p)
		c.logf("✕ closed #%d (landed inside #%d)", p.Number, lander.Number)
	}
	c.Forge.RemoveLabels(lander.Number, LabelLanding)

	if !c.KeepBranches {
		// The lander's branch is the base of held PRs until they are
		// retargeted; keep it if that failed.
		toDelete := append([]forge.PR{}, parts...)
		if childrenOK {
			toDelete = append(toDelete, lander)
		}
		for _, p := range toDelete {
			ref := "refs/heads/" + p.HeadRef
			if _, err := r.Run("fetch", "--quiet", c.Remote, "+"+ref+":refs/remotes/"+c.remoteRef(p.HeadRef)); err != nil {
				continue // already gone (e.g. auto-deleted head branch)
			}
			sha, err := r.RevParse(c.remoteRef(p.HeadRef))
			if err != nil {
				continue
			}
			if _, err := r.Run("push", "--quiet", fmt.Sprintf("--force-with-lease=%s:%s", ref, sha), c.Remote, ":"+ref); err != nil {
				c.logf("! could not delete %s: %v", p.HeadRef, err)
				continue
			}
			c.logf("🗑 deleted %s", p.HeadRef)
		}
	}

	d, err := c.Forge.Details(lander.Number)
	if err != nil {
		return err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "✅ **Stack `%s` landed atomically** on `%s`: %d parts in one merge-queue entry.\n\n", m.Stack, m.Trunk, len(parts)+1)
	if d.MergeCommit != "" {
		fmt.Fprintf(&b, "Trunk commit: `%s`\n\n", git.Short(d.MergeCommit))
	}
	fmt.Fprintf(&b, "**Rollback:** if the change is behind a feature flag, turning the flag off is the fastest fix. "+
		"To remove the code, run `stackq revert --pr %d` (or the stackq workflow with command `revert`): it opens one PR that reverts the whole stack.", lander.Number)
	return c.Forge.Comment(lander.Number, b.String())
}

func (c *Runner) reportConflict(ch, parent forge.PR, trunk string, ce *git.ConflictError) {
	var b strings.Builder
	why := ""
	if parent.Number > 0 {
		why = fmt.Sprintf(" after #%d merged", parent.Number)
	}
	fmt.Fprintf(&b, "⚠️ **stackq could not restack this PR**%s: rebasing onto `%s` conflicts in\n\n", why, trunk)
	for _, f := range ce.Files {
		fmt.Fprintf(&b, "- `%s`\n", f)
	}
	fmt.Fprintf(&b, "\nThe stack is paused here. To fix it locally:\n\n```sh\n"+
		"stackq sync      # stops at the conflict and prints the exact rebase command to run\n"+
		"# run it, resolve, git rebase --continue, then\nstackq sync && stackq push && stackq land\n```\n")
	c.Forge.Comment(ch.Number, b.String())
	c.Forge.AddLabels(ch.Number, LabelConflict)
	c.logf("✗ #%d conflicts with %s: %s", ch.Number, trunk, strings.Join(ce.Files, ", "))
}

// reportLanded tells the original PR once everything except held PRs is on trunk.
func (c *Runner) reportLanded(sp *stackPRs, last forge.PR, m stack.Marker) error {
	if m.Source == 0 {
		return nil
	}
	var landed, held []string
	for _, p := range sp.prs {
		pm := sp.marks[p.Number]
		switch {
		case pm.Hold:
			held = append(held, fmt.Sprintf("#%d", p.Number))
		case p.Merged() || p.State == "CLOSED" && m.Atomic():
			landed = append(landed, fmt.Sprintf("#%d", p.Number))
		default:
			return nil
		}
	}
	if len(landed)+len(held) < m.Total {
		return nil // PRs missing from the listing
	}
	var b strings.Builder
	how := "one PR at a time"
	if m.Atomic() {
		how = fmt.Sprintf("atomically via #%d", last.Number)
	}
	fmt.Fprintf(&b, "✅ Stack `%s` has landed on `%s` %s: %s.", m.Stack, m.Trunk, how, strings.Join(landed, ", "))
	if len(held) > 0 {
		fmt.Fprintf(&b, "\n\n🔒 The activation PR %s is ready and waiting for a human to merge it.", strings.Join(held, ", "))
	}
	c.logf("✅ stack %s landed; noted on #%d", m.Stack, m.Source)
	return c.Forge.Comment(m.Source, b.String())
}

func (c *Runner) refreshNav(sp *stackPRs, skip map[int]bool) error {
	nav := sp.nav()
	for _, p := range sp.prs {
		if !p.Open() || skip[p.Number] {
			continue
		}
		body := stack.MergeBody(p.Body, stack.RenderBlock(sp.marks[p.Number], nav))
		if body != p.Body {
			if err := c.Forge.EditPR(p.Number, nil, &body); err != nil {
				return err
			}
		}
	}
	return nil
}

// Dequeued reports a PR the queue kicked out (failed checks or conflict).
func (c *Runner) Dequeued(n int) error {
	pr, err := c.Forge.GetPR(n)
	if err != nil {
		return err
	}
	if pr.Merged() {
		return nil
	}
	m, ok := stack.ParseMarker(pr.Body)
	if !ok {
		return nil
	}
	what := "the PRs above it will not be queued until it lands"
	if m.Atomic() {
		what = "nothing from the stack reached trunk"
	}
	c.Forge.Comment(n, fmt.Sprintf("⏸️ This PR left the merge queue without merging, so stack `%s` is paused here; %s.\n\n"+
		"Fix the failure, push, then run `stackq land --pr %d` (or the stackq workflow with command `land`).", m.Stack, what, n))
	c.Forge.AddLabels(n, LabelBlocked)
	c.logf("⏸ #%d left the queue; stack %s paused", n, m.Stack)
	return nil
}
