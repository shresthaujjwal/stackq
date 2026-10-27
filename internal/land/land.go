// Package land lands stacks with GitHub's own stacked pull requests. stackq
// links a stack's PRs as a GitHub stack when it pushes them; GitHub then shows
// the stack on every PR, runs trunk's rules and CI for each of them, merges a
// PR together with every open PR below it as one request, and rebases and
// retargets the PRs above a merge itself. What stackq adds is the decision of
// when to land and what:
//
//   - atomic (default): once every part is approved and green, stackq asks
//     GitHub to merge the top part. GitHub merges the whole stack bottom up as
//     one request, through the merge queue when trunk has one: trunk gets all
//     of it or none, with one commit history per part.
//   - incremental: stackq asks GitHub to merge the lowest open part, and the
//     next one after it lands.
//
// A held (activation) PR sits on top of the stack. stackq never merges it; it
// tells a human when everything below has landed.
//
// It needs only the PR bodies, the forge and a clone, so it runs the same
// locally and in Actions.
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
	"stackq/internal/plural"
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
	// LabelLanded marks the top part of a stack once stackq has reported the
	// landing, so the report is made once.
	LabelLanded = "stackq:landed"
	// LabelReminded marks an activation PR that was reminded about; the time
	// it was added is when to remind next.
	LabelReminded = "stackq:reminded"
)

type Runner struct {
	Repo    *git.Repo
	Forge   forge.Forge
	Remote  string
	Enqueue bool // incremental mode: land the next part after one merges
	// Atomic gate.
	MinApprovals    int  // per part; default 1
	AllowUnreviewed bool // skip the approval gate
	// KeepBranches leaves the stack's branches after it lands.
	KeepBranches bool
	// Prio routes every enqueue through the priority pool (see schedule.go).
	Prio *prio.Config
	// Prefix is the branch prefix of stack branches, "stack/{name}/" by default.
	Prefix string
	// ReminderDays is how long a ready activation PR waits before stackq
	// comments, and between comments. 0 turns the reminders off.
	ReminderDays int
	// Trunk, when set, is the only trunk a stack marker may name. A PR body is
	// written by whoever opens the PR, and the marker's trunk ends up in
	// refspecs and as the base of PRs stackq edits, so CI sets it to the
	// repository's trunk and ignores a marker that names any other branch (K5).
	Trunk   string
	Now     func() time.Time
	ignored map[int]bool
	Log     io.Writer
}

// markerOf returns the stack marker of p only if p can really be part of a
// stack. A PR body is written by whoever opens the PR, so the marker alone proves
// nothing: anyone can open a PR from a fork with a copied marker and a higher
// index, and without this check stackq would treat it as the stack's lander and
// queue it. A stack PR's head branch lives in this repository and is named like
// stackq names its stack branches.
func (c *Runner) markerOf(p forge.PR) (stack.Marker, bool) {
	m, ok := stack.ParseMarker(p.Body)
	if !ok || p.CrossRepo || !strings.HasPrefix(p.HeadRef, c.branchPrefix(m.Stack)) {
		return stack.Marker{}, false
	}
	if c.Trunk != "" && m.Trunk != c.Trunk {
		if !c.ignored[p.Number] {
			if c.ignored == nil {
				c.ignored = map[int]bool{}
			}
			c.ignored[p.Number] = true
			c.logf("! #%d names trunk %q in its stack marker, but this repository's trunk is %q; ignoring it", p.Number, m.Trunk, c.Trunk)
		}
		return stack.Marker{}, false
	}
	return m, true
}

func (c *Runner) branchPrefix(stackName string) string {
	prefix := c.Prefix
	if prefix == "" {
		prefix = "stack/{name}/"
	}
	return strings.ReplaceAll(prefix, "{name}", stackName)
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
		if m, ok := c.markerOf(p); ok && m.Stack == name && !seen[p.Number] {
			seen[p.Number] = true
			sp.prs = append(sp.prs, p)
			sp.marks[p.Number] = m
		}
	}
	sort.Slice(sp.prs, func(i, j int) bool { return sp.marks[sp.prs[i].Number].Index < sp.marks[sp.prs[j].Number].Index })
	return sp, nil
}

func (sp *stackPRs) anyMerged() bool {
	for _, p := range sp.prs {
		if p.Merged() {
			return true
		}
	}
	return false
}

// parts are the stack's PRs that are not held back, bottom first.
func (sp *stackPRs) parts() []forge.PR {
	var out []forge.PR
	for _, p := range sp.prs {
		if !sp.marks[p.Number].Hold {
			out = append(out, p)
		}
	}
	return out
}

// lander is the top part: merging it lands the whole stack.
func (sp *stackPRs) lander() (forge.PR, bool) {
	ps := sp.parts()
	for i := len(ps) - 1; i >= 0; i-- {
		if ps[i].State != "CLOSED" {
			return ps[i], true
		}
	}
	return forge.PR{}, false
}

// lowestOpen is the lowest open part: the next one an incremental stack lands.
func (sp *stackPRs) lowestOpen() (forge.PR, bool) {
	for _, p := range sp.parts() {
		if p.Open() {
			return p, true
		}
	}
	return forge.PR{}, false
}

// landed reports whether every part has merged (held PRs aside).
func (sp *stackPRs) landed() bool {
	ps := sp.parts()
	for _, p := range ps {
		if !p.Merged() {
			return false
		}
	}
	return len(ps) > 0
}

func (sp *stackPRs) nav() []stack.NavEntry {
	var nav []stack.NavEntry
	for _, p := range sp.prs {
		m := sp.marks[p.Number]
		if p.State == "CLOSED" {
			continue // closed without merging: no longer part of the stack
		}
		nav = append(nav, stack.NavEntry{Index: m.Index, PR: p.Number, Branch: p.HeadRef, Merged: p.Merged(), Hold: m.Hold})
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

// ensureGitHubStack links the stack's open PRs as a GitHub stack if they are
// not one yet (stacks pushed before stackq used GitHub stacks, or unstacked on
// GitHub since). It returns the stack number, or 0 when there is one open PR,
// which needs no stack.
func (c *Runner) ensureGitHubStack(sp *stackPRs) (int, error) {
	var open []int
	for _, p := range sp.prs {
		if p.Open() {
			open = append(open, p.Number)
		}
	}
	if len(open) == 0 {
		return 0, nil
	}
	st, err := c.Forge.StackOf(open[0])
	if err != nil || st != 0 || len(open) < 2 {
		return st, err
	}
	if st, err = c.Forge.CreateStack(open); err != nil {
		return 0, fmt.Errorf("its PRs are not a GitHub stack, and linking them failed: %w", err)
	}
	c.logf("⧉ linked %s as GitHub stack %d", prNums(open), st)
	return st, nil
}

func prNums(prs []int) string {
	var out []string
	for _, n := range prs {
		out = append(out, fmt.Sprintf("#%d", n))
	}
	return strings.Join(out, ", ")
}

// Land lands what PR n stands for. For a stack PR that is the whole stack
// (atomic), or PR n and every open part below it (incremental: GitHub merges
// a stacked PR together with what is below it). A held PR is merged only when
// named. Any other PR is merged on its own.
func (c *Runner) Land(n int) error {
	pr, err := c.Forge.GetPR(n)
	if err != nil {
		return err
	}
	if m, ok := c.markerOf(pr); ok {
		upTo := 0
		if !m.Atomic() || m.Hold {
			upTo = n
		}
		if m.Hold {
			c.logf("#%d is the activation PR; landing it because you asked", n)
		}
		return c.attempt(m, false, upTo)
	}
	if !pr.Open() {
		return fmt.Errorf("#%d is %s", n, strings.ToLower(pr.State))
	}
	c.Forge.RemoveLabels(n, LabelBlocked)
	queued, err := c.submit(pr, pr.BaseRef, 1)
	if err != nil {
		return err
	}
	if !queued {
		c.logf("⧗ #%d stays in the priority pool until a pass queues it (the next review, finished CI run or timer tick)", n)
	}
	return nil
}

// AutoLand lands every open stack that is ready and not landing already:
// atomic stacks as a whole, incremental ones part by part. It is meant to run
// on every approval, finished CI run and timer tick, so a stack that isn't
// ready is left alone without a comment, a label or an error.
func (c *Runner) AutoLand() error {
	names, byName, err := c.openStacks()
	if err != nil {
		return err
	}
	var errs []error
	for _, name := range names {
		if err := c.attempt(byName[name], true, 0); err != nil {
			errs = append(errs, fmt.Errorf("stack %s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

func (c *Runner) openStacks() ([]string, map[string]stack.Marker, error) {
	open, err := c.Forge.ListPRs("open")
	if err != nil {
		return nil, nil, err
	}
	byName := map[string]stack.Marker{}
	var names []string
	for _, p := range open {
		m, ok := c.markerOf(p)
		if !ok {
			continue
		}
		if _, dup := byName[m.Stack]; !dup {
			byName[m.Stack] = m
			names = append(names, m.Stack)
		}
	}
	sort.Strings(names)
	return names, byName, nil
}

// queued reports whether GitHub's queue for trunk already holds PR n.
func (c *Runner) queued(n int, trunk string) bool {
	q, err := c.Forge.Queue(trunk)
	if err != nil {
		return false
	}
	for _, e := range q {
		if e.PR == n {
			return true
		}
	}
	return false
}

// labeled reports whether open PR n carries label.
func (c *Runner) labeled(n int, label string) bool {
	prs, err := c.Forge.ListPool(label)
	if err != nil {
		return false
	}
	for _, p := range prs {
		if p.Number == n {
			return true
		}
	}
	return false
}

// Where a stack that stackq asked to land stands, as landingState reports it.
const (
	stateIdle    = "idle"    // stackq has not asked GitHub to land it
	statePending = "pending" // GitHub accepted the merge request and is working on it
	stateQueued  = "queued"  // in the merge queue
	statePool    = "pool"    // waiting in the priority pool
	stateFailed  = "failed"  // GitHub could not do the merge it was asked for
	stateGone    = "gone"    // it was queued, and left without merging
	stateUnknown = "unknown" // could not tell, or it merged meanwhile
)

// requestTag records the merge request stackq made in the target PR's status
// section, so later runs can ask GitHub how it went.
const requestTag = "<!-- stackq:merge-request "

func withRequest(status, uuid string) string {
	if uuid == "" {
		uuid = "-"
	}
	return status + "\n" + requestTag + uuid + " -->"
}

// requestOf returns the merge request recorded in a PR body: ok is false when
// none is, uuid is empty when GitHub gave none (it answered at once).
func requestOf(body string) (uuid string, ok bool) {
	i := strings.Index(body, requestTag)
	if i < 0 {
		return "", false
	}
	rest := body[i+len(requestTag):]
	if j := strings.Index(rest, " -->"); j >= 0 {
		rest = rest[:j]
	}
	if rest == "-" {
		rest = ""
	}
	return strings.TrimSpace(rest), true
}

// landingState reports where target stands. GitHub's merge request is the
// first source: pending while GitHub prepares the merge, failed with GitHub's
// reason (a rule the PR does not meet, a conflict), enqueued once it joined
// the queue. After that the queue says whether it is still there. GitHub sends
// a dequeued event when the queue drops a PR, but the run for it can be lost,
// so stateGone lets the next tick notice. Any doubt is stateUnknown.
func (c *Runner) landingState(target forge.PR, trunk string) (string, string) {
	if c.prioOn() && c.labeled(target.Number, c.Prio.Label) {
		return statePool, ""
	}
	uuid, asked := requestOf(target.Body)
	if !asked {
		return stateIdle, ""
	}
	if c.queued(target.Number, trunk) {
		return stateQueued, ""
	}
	if uuid != "" {
		r, err := c.Forge.MergeResult(target.Number, uuid)
		switch {
		case err != nil:
			return stateUnknown, ""
		case r.Status == forge.MergePending:
			return statePending, ""
		case r.Status == forge.MergeFailed:
			return stateFailed, r.Message
		case r.Status == forge.MergeMerged:
			return stateUnknown, ""
		}
	}
	// Last, so a merge that happened while we looked is seen.
	if pr, err := c.Forge.GetPR(target.Number); err != nil || !pr.Open() {
		return stateUnknown, ""
	}
	return stateGone, ""
}

// setStatus writes the landing status into the PR's body (stack.MergeStatus),
// and only when it changed, so a tick that finds nothing new makes no call. An
// empty status removes it.
func (c *Runner) setStatus(pr forge.PR, status string) {
	body := stack.MergeStatus(pr.Body, status)
	if body == pr.Body {
		return
	}
	if err := c.Forge.EditPR(pr.Number, nil, &body); err != nil {
		c.logf("! could not update the landing status on #%d: %v", pr.Number, err)
	}
}

func statusWaiting(problems []string) string {
	return "⏳ **Not landing yet.** stackq asks GitHub to land the stack once nothing below is left:\n\n- " + strings.Join(problems, "\n- ")
}

func statusLanding(parts int) string {
	if parts == 1 {
		return "🚀 **Landing.** GitHub is merging this PR, through the merge queue if trunk has one."
	}
	return fmt.Sprintf("🚀 **Landing.** GitHub is merging %s bottom up as one request, through the merge queue if trunk has one.", plural.N(parts, "PR"))
}

func statusPool(parts int) string {
	return fmt.Sprintf("⧗ **Waiting in the priority pool** for a merge-queue slot. Landing this PR merges %s.", plural.N(parts, "PR"))
}

// attempt gates and lands a stack: the whole of it (atomic), or up to upTo,
// or (incremental) its lowest open part. With auto set it is quiet: anything
// that stops the landing is logged and returns nil, because the next event will
// try again.
func (c *Runner) attempt(m stack.Marker, auto bool, upTo int) error {
	sp, err := c.loadStack(m.Stack)
	if err != nil {
		return err
	}
	// A split opens its PRs one after the other. Until every part exists the
	// highest one is not the stack's top, and landing it would land half a feature.
	if m.Total > len(sp.prs) && !sp.anyMerged() {
		err := fmt.Errorf("stack %s has %d of %d PRs so far; it is still being created, or a part was deleted", m.Stack, len(sp.prs), m.Total)
		if auto {
			c.logf("- %v", err)
			return nil
		}
		return err
	}
	var target forge.PR
	var ok bool
	switch {
	case upTo != 0:
		for _, p := range sp.prs {
			if p.Number == upTo {
				target, ok = p, p.Open()
			}
		}
	case m.Atomic():
		target, ok = sp.lander()
		ok = ok && target.Open()
	default:
		target, ok = sp.lowestOpen()
	}
	if !ok {
		if !auto {
			return fmt.Errorf("stack %s has nothing open to land", m.Stack)
		}
		return nil
	}

	// What GitHub merges with target: it and every open PR below it.
	var parts []forge.PR
	for _, p := range sp.prs {
		if p.Open() && sp.marks[p.Number].Index <= sp.marks[target.Number].Index {
			parts = append(parts, p)
		}
	}

	if c.labeled(target.Number, LabelBlocked) && auto {
		c.logf("- stack %s is paused: #%d is labeled %s; fix it, then run stackq land --pr %d", m.Stack, target.Number, LabelBlocked, target.Number)
		return nil
	}
	switch state, why := c.landingState(target, m.Trunk); state {
	case statePool:
		c.setStatus(target, withRequestKept(target.Body, statusPool(len(parts))))
		c.logf("- stack %s is waiting in the priority pool (#%d)", m.Stack, target.Number)
		return nil
	case stateQueued, statePending, stateUnknown:
		c.logf("- stack %s is already landing (#%d)", m.Stack, target.Number)
		return nil
	case stateFailed:
		if auto {
			return c.pause(target, m, "GitHub could not merge it: "+why)
		}
	case stateGone:
		if auto {
			c.logf("! #%d left the merge queue without merging, and no dequeued run handled it", target.Number)
			return c.Dequeued(target.Number)
		}
	}

	st, err := c.ensureGitHubStack(sp)
	if err != nil {
		if auto {
			c.logf("- stack %s: %v", m.Stack, err)
			return nil
		}
		return fmt.Errorf("stack %s: %w", m.Stack, err)
	}

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
		if p.Draft && !sp.marks[p.Number].Hold {
			problems = append(problems, fmt.Sprintf("part %d #%d is a draft", idx, p.Number))
		}
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
		case "none":
			// Nobody is watching an automatic landing, so "no checks yet"
			// (CI not started, or waiting for approval) is not green.
			if auto {
				problems = append(problems, fmt.Sprintf("part %d #%d: no checks have reported yet", idx, p.Number))
			}
		}
		if i > 0 {
			below, above := c.remoteRef(parts[i-1].HeadRef), c.remoteRef(p.HeadRef)
			if !c.Repo.IsAncestor(below, above) {
				problems = append(problems, fmt.Sprintf("part %d #%d is not on top of part %d; use **Rebase stack** on GitHub, or stackq sync && stackq push",
					idx, p.Number, sp.marks[parts[i-1].Number].Index))
			}
		}
		approvers := strings.Join(d.Approvals, ", ")
		if approvers == "" {
			approvers = "—"
		}
		rows = append(rows, row{p, approvers, d.Checks})
	}
	if len(problems) > 0 && auto {
		c.logf("- stack %s is not ready to land: %s", m.Stack, strings.Join(problems, "; "))
		c.setStatus(target, statusWaiting(problems))
		return nil
	}
	if len(problems) > 0 {
		msg := "stack " + m.Stack + " is not ready to land:\n  - " + strings.Join(problems, "\n  - ")
		c.Forge.Comment(target.Number, "⏸️ **Not landing yet.** Every PR GitHub would merge must be approved and green first:\n\n- "+
			strings.Join(problems, "\n- ")+"\n\nFix these, then run `stackq land` again.")
		c.Forge.AddLabels(target.Number, LabelBlocked)
		return errors.New(msg)
	}

	head, err := c.Repo.RevParse(c.remoteRef(target.HeadRef))
	if err != nil {
		return err
	}
	target.HeadSHA = head

	var b strings.Builder
	if len(parts) > 1 {
		fmt.Fprintf(&b, "🚀 **Landing stack `%s`.** stackq asked GitHub to merge this PR. As GitHub stack %d, that merges these %s into `%s` bottom up, as one request: all of them or none.\n\n",
			m.Stack, st, plural.N(len(parts), "PR"), m.Trunk)
	} else {
		fmt.Fprintf(&b, "🚀 **Landing** part %d of stack `%s` into `%s`.\n\n", sp.marks[target.Number].Index, m.Stack, m.Trunk)
	}
	b.WriteString("| Part | PR | Approved by | Checks |\n|---|---|---|---|\n")
	for _, r := range rows {
		fmt.Fprintf(&b, "| %d | #%d | %s | %s |\n", sp.marks[r.pr.Number].Index, r.pr.Number, r.approvers, r.checks)
	}
	fmt.Fprintf(&b, "\nRoll back after merge with `stackq revert --pr %d`.", target.Number)
	c.Forge.Comment(target.Number, b.String())
	for _, p := range parts {
		c.Forge.AddLabels(p.Number, LabelLanding)
	}
	c.Forge.RemoveLabels(target.Number, LabelBlocked, LabelConflict)
	queued, err := c.submit(target, m.Trunk, len(parts))
	if err != nil {
		return err
	}
	if queued {
		c.logf("⇢ asked GitHub to merge #%d (%s)", target.Number, plural.N(len(parts), "PR"))
	} else {
		c.logf("⧗ #%d (%s) stays in the priority pool until a pass queues it", target.Number, plural.N(len(parts), "PR"))
	}
	return nil
}

// withRequestKept keeps the merge request tag of body under a new status.
func withRequestKept(body, status string) string {
	if uuid, ok := requestOf(body); ok {
		return withRequest(status, uuid)
	}
	return status
}

// merge asks GitHub to merge target and records the request in its status
// section. A request GitHub turns down at once pauses the stack.
func (c *Runner) merge(target forge.PR, parts int) error {
	r, err := c.Forge.Merge(target.Number, target.HeadSHA)
	if err != nil {
		return err
	}
	if r.Status == forge.MergeFailed {
		if m, ok := c.markerOf(target); ok {
			return c.pause(target, m, "GitHub could not merge it: "+r.Message)
		}
		return fmt.Errorf("GitHub could not merge #%d: %s", target.Number, r.Message)
	}
	if _, ok := c.markerOf(target); ok {
		target.Body = stack.MergeStatus(target.Body, withRequest(statusLanding(parts), r.UUID))
		if err := c.Forge.EditPR(target.Number, nil, &target.Body); err != nil {
			c.logf("! could not record the merge request on #%d: %v", target.Number, err)
		}
	}
	return nil
}

// pause stops a stack at target with a reason, until someone lands it again.
func (c *Runner) pause(target forge.PR, m stack.Marker, why string) error {
	c.Forge.Comment(target.Number, fmt.Sprintf("⏸️ Stack `%s` is paused here; nothing from it reached `%s`. %s\n\n"+
		"Fix it, then run `stackq land --pr %d` (or the stackq workflow with command `land`).", m.Stack, m.Trunk, why, target.Number))
	c.Forge.AddLabels(target.Number, LabelBlocked)
	c.setStatus(target, fmt.Sprintf("⏸ **Paused.** %s Fix it, then run `stackq land --pr %d`.", why, target.Number))
	c.logf("⏸ #%d: %s; stack %s paused", target.Number, why, m.Stack)
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
	m, ok := c.markerOf(pr)
	if !ok {
		return nil
	}
	return c.pause(pr, m, "It left the merge queue without merging (a failed check, or a conflict with trunk).")
}

// FinishLanded finishes every stack stackq landed whose merge has happened:
// it clears the landing labels and status, reports the landing on the top
// part and the original PR, and announces the activation PR, which GitHub has
// retargeted onto trunk. A merge made by the merge queue for the built-in
// GITHUB_TOKEN starts no "PR merged" workflow, so this runs on every tick;
// once a stack is finished it does nothing.
func (c *Runner) FinishLanded() error {
	all, err := c.Forge.ListPRs("all")
	if err != nil {
		return err
	}
	var names []string
	seen := map[string]bool{}
	for _, p := range all {
		m, ok := c.markerOf(p)
		if !ok || seen[m.Stack] {
			continue
		}
		// A stack still to finish has a merged PR whose status says landing.
		if _, asked := requestOf(p.Body); asked && p.Merged() {
			seen[m.Stack] = true
			names = append(names, m.Stack)
		}
	}
	sort.Strings(names)
	var errs []error
	for _, name := range names {
		if err := c.finish(name); err != nil {
			errs = append(errs, fmt.Errorf("stack %s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

// AfterMerge reacts to a merged stack PR: it finishes what landed (and, for
// an incremental stack, lands the next part).
func (c *Runner) AfterMerge(n int) error {
	pr, err := c.Forge.GetPR(n)
	if err != nil {
		return err
	}
	if !pr.Merged() {
		c.logf("#%d is not merged; nothing to do", n)
		return nil
	}
	m, ok := c.markerOf(pr)
	if !ok {
		c.logf("#%d is not part of a stackq stack; nothing to do", n)
		return nil
	}
	return c.finish(m.Stack)
}

// finish tidies one stack after GitHub merged some or all of it. For an
// incremental stack it then lands the next part when Enqueue is set: a merge
// the queue made for the built-in token starts no "PR merged" run, so this has
// to happen wherever a merge is first noticed, on a tick as well.
func (c *Runner) finish(name string) error {
	sp, err := c.loadStack(name)
	if err != nil {
		return err
	}
	var landedNow []forge.PR
	for _, p := range sp.prs {
		if !p.Merged() {
			continue
		}
		if _, asked := requestOf(p.Body); asked {
			landedNow = append(landedNow, p)
		}
		c.Forge.RemoveLabels(p.Number, LabelLanding, LabelBlocked)
	}
	for _, p := range landedNow {
		m := sp.marks[p.Number]
		msg := fmt.Sprintf("✅ **Landed** on `%s`. Roll back with `stackq revert --pr %d`.", m.Trunk, p.Number)
		if d, err := c.Forge.Details(p.Number); err == nil && d.MergeCommit != "" {
			msg += fmt.Sprintf(" Trunk commit: `%s`.", git.Short(d.MergeCommit))
		}
		c.setStatus(p, msg)
		c.logf("✅ #%d landed (stack %s)", p.Number, name)
	}
	if err := c.refreshNav(sp); err != nil {
		return err
	}
	if len(landedNow) == 0 {
		return nil
	}
	if !sp.landed() {
		if m := sp.marks[landedNow[0].Number]; c.Enqueue && !m.Atomic() {
			return c.attempt(m, true, 0)
		}
		return nil
	}
	top, _ := sp.lander()
	m := sp.marks[top.Number]
	var held []string
	for _, p := range sp.prs {
		if pm := sp.marks[p.Number]; pm.Hold && p.Open() {
			held = append(held, fmt.Sprintf("#%d", p.Number))
			c.Forge.AddLabels(p.Number, LabelActivation)
			c.Forge.Comment(p.Number, fmt.Sprintf("🔒 The rest of stack `%s` is on `%s`, and GitHub moves this activation PR onto `%s`; "+
				"stackq will not merge it. Merge it (or run `stackq land --pr %d`) when you are ready to switch the feature on.",
				m.Stack, m.Trunk, m.Trunk, p.Number))
			c.logf("🔒 #%d (activation) is ready and left for a human", p.Number)
		}
	}
	var landed []string
	for _, p := range sp.parts() {
		landed = append(landed, fmt.Sprintf("#%d", p.Number))
	}
	if m.Source != 0 {
		msg := fmt.Sprintf("✅ Stack `%s` has landed on `%s`: %s.", m.Stack, m.Trunk, strings.Join(landed, ", "))
		if len(held) > 0 {
			msg += fmt.Sprintf("\n\n🔒 The activation PR %s is ready and waiting for a human to merge it.", strings.Join(held, ", "))
		}
		c.Forge.Comment(m.Source, msg)
		c.logf("✅ stack %s landed; noted on #%d", m.Stack, m.Source)
	}
	return nil
}

func (c *Runner) refreshNav(sp *stackPRs) error {
	nav := sp.nav()
	for _, p := range sp.prs {
		if !p.Open() {
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
