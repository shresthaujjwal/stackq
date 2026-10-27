package land

import (
	"fmt"
	"os"
	"strings"
	"time"

	"stackq/internal/forge"
	"stackq/internal/prio"
)

func (c *Runner) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Runner) prioOn() bool { return c.Prio != nil && c.Prio.Enabled }

// submit asks GitHub to merge pr (and, in a GitHub stack, the open PRs below
// it: parts in all): straight away when prioritization is off, otherwise
// through the pool and a scheduling pass.
func (c *Runner) submit(pr forge.PR, trunk string, parts int) (queued bool, err error) {
	if !c.prioOn() {
		if err := c.merge(pr, parts); err != nil {
			return false, err
		}
		return true, nil
	}
	if err := c.Forge.AddLabels(pr.Number, c.Prio.Label); err != nil {
		return false, err
	}
	if _, ok := c.markerOf(pr); ok {
		c.setStatus(pr, statusPool(parts))
	}
	c.logf("⧗ #%d is waiting in the priority pool", pr.Number)
	decisions, err := c.Schedule(trunk, false)
	if err != nil {
		return false, err
	}
	// The pool is found by a label search that can lag behind the label, so
	// the pass may not have seen this PR yet. Only claim what happened.
	for _, d := range decisions {
		if d.PR.Number == pr.Number && (d.Action == prio.Admit || d.Action == prio.Jump) {
			return true, nil
		}
	}
	return false, nil
}

// Schedule runs one admission pass: rank the pool and top GitHub's queue up
// to MaxInFlight, letting urgent PRs jump within the jump budget.
func (c *Runner) Schedule(trunk string, dryRun bool) ([]prio.Decision, error) {
	if !c.prioOn() {
		return nil, fmt.Errorf("prioritization is off; set priority.enabled in .stackq.json")
	}
	cfg := c.Prio
	pool, err := c.Forge.ListPool(cfg.Label)
	if err != nil {
		return nil, err
	}
	if len(pool) == 0 {
		// Nothing to admit, so don't require a readable merge queue: this pass
		// runs on every merge and on a timer, including in repos where the
		// queue isn't enabled yet.
		c.logf("priority pool is empty; nothing to queue")
		return nil, nil
	}
	queue, err := c.Forge.Queue(trunk)
	if err != nil {
		return nil, err
	}
	queued := map[int]bool{}
	jumps := 0
	for _, e := range queue {
		queued[e.PR] = true
		if e.Jumped {
			jumps++
		}
	}

	byNum := map[int]forge.PR{}
	stacks := map[string]*stackPRs{}
	var cands []prio.PR
	for _, p := range pool {
		if queued[p.Number] {
			// Someone queued it by hand; it no longer needs the pool.
			if !dryRun {
				c.Forge.RemoveLabels(p.Number, cfg.Label)
			}
			continue
		}
		byNum[p.Number] = p
		cp := prio.PR{Number: p.Number, Title: p.Title, Body: p.Body, Branch: p.HeadRef, Author: p.Author,
			Labels: p.Labels, Lines: p.Additions + p.Deletions, Files: p.ChangedFiles, Eligible: true}
		if t, err := c.Forge.ReadyAt(p.Number, cfg.Label); err == nil && !t.IsZero() {
			cp.ReadyAt = t
		} else {
			cp.ReadyAt = p.CreatedAt
		}
		cp.Eligible, cp.Why = c.eligible(p, trunk, stacks)
		cands = append(cands, cp)
	}

	decisions := cfg.Decide(cfg.Rank(cands, c.now()), len(queue), jumps)
	for i := range decisions {
		d := &decisions[i]
		if dryRun || (d.Action != prio.Jump && d.Action != prio.Admit) {
			continue
		}
		pr := byNum[d.PR.Number]
		jump := d.Action == prio.Jump
		var err error
		if jump {
			// Only a PR outside a GitHub stack can jump; a stack is merged
			// with the asynchronous merge API, which joins the back.
			if err = c.Forge.Jump(pr); err != nil && !forge.IsPermanent(err) {
				c.logf("! #%d could not jump (%v); queueing it normally", pr.Number, err)
				jump = false
				d.Note = "jump refused by GitHub; first in line instead"
				err = c.merge(pr, c.mergeCount(pr, stacks))
			}
		} else {
			err = c.merge(pr, c.mergeCount(pr, stacks))
		}
		if err != nil {
			d.Action, d.Note = prio.Skip, "enqueue failed: "+err.Error()
			c.Forge.RemoveLabels(pr.Number, cfg.Label)
			c.Forge.AddLabels(pr.Number, LabelBlocked)
			c.Forge.Comment(pr.Number, fmt.Sprintf("⚠️ stackq could not add this PR to the merge queue:\n\n```\n%v\n```\n\n"+
				"Fix it, then add the `%s` label again.", err, cfg.Label))
			continue
		}
		c.Forge.RemoveLabels(pr.Number, cfg.Label, LabelBlocked)
		labels := []string{"stackq:prio:" + d.Class.Name}
		if jump {
			labels = append(labels, forge.LabelJumped)
		}
		c.Forge.AddLabels(pr.Number, labels...)
		where := "joined the back of the queue"
		if jump {
			where = "**jumped to the front of the queue** (in-flight entries rebuild)"
		}
		c.Forge.Comment(pr.Number, fmt.Sprintf("⇢ Queued by stackq: priority class **%s** (%s), waited %s, %s.",
			d.Class.Name, d.Reason, roundDur(d.Waited), where))
		c.logf("⇢ #%d queued: %s (%s), score %.1f%s", pr.Number, d.Class.Name, d.Reason, d.Score, map[bool]string{true: ", jumped"}[jump])
	}
	c.report(trunk, len(queue), decisions, dryRun)
	return decisions, nil
}

func (c *Runner) stackFor(name string, stacks map[string]*stackPRs) (*stackPRs, error) {
	if sp := stacks[name]; sp != nil {
		return sp, nil
	}
	sp, err := c.loadStack(name)
	if err != nil {
		return nil, err
	}
	stacks[name] = sp
	return sp, nil
}

// mergeCount is how many PRs merging pr lands: it and, in a stack, the open
// parts below it.
func (c *Runner) mergeCount(pr forge.PR, stacks map[string]*stackPRs) int {
	m, ok := c.markerOf(pr)
	if !ok {
		return 1
	}
	sp, err := c.stackFor(m.Stack, stacks)
	if err != nil {
		return 1
	}
	n := 0
	for _, p := range sp.prs {
		if p.Open() && sp.marks[p.Number].Index <= m.Index {
			n++
		}
	}
	return max(n, 1)
}

// eligible decides whether a pool PR may be queued now.
func (c *Runner) eligible(p forge.PR, trunk string, stacks map[string]*stackPRs) (bool, string) {
	m, isStack := c.markerOf(p)
	// In a GitHub stack a part targets the part below it; what lands on
	// trunk is the whole stack, so the stack's trunk is what counts.
	if base := p.BaseRef; base != trunk && !(isStack && m.Trunk == trunk) {
		return false, "targets " + base + ", not " + trunk
	}
	if p.Draft {
		return false, "draft"
	}
	if isStack && !m.Hold {
		sp, err := c.stackFor(m.Stack, stacks)
		if err != nil {
			return false, err.Error()
		}
		if l, ok := sp.lander(); ok && m.Atomic() && l.Number != p.Number {
			return false, fmt.Sprintf("part of atomic stack %s; it lands with #%d", m.Stack, l.Number)
		}
	}
	d, err := c.Forge.Details(p.Number)
	if err != nil {
		return false, err.Error()
	}
	switch {
	case len(d.ChangesRequested) > 0:
		return false, "changes requested by " + strings.Join(d.ChangesRequested, ", ")
	case d.Checks == "failure":
		return false, "checks failing"
	case d.Checks == "pending":
		return false, "checks running"
	case c.Prio.RequireApproval() && !c.AllowUnreviewed && len(d.Approvals) == 0:
		return false, "waiting for approval"
	}
	return true, ""
}

func roundDur(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "under a minute"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

// report prints the pass and, in Actions, writes it to the job summary.
func (c *Runner) report(trunk string, inFlight int, ds []prio.Decision, dry bool) {
	var b strings.Builder
	title := "stackq priority pass"
	if dry {
		title += " (dry run)"
	}
	fmt.Fprintf(&b, "### %s: `%s` queue has %d/%d in flight\n\n", title, trunk, inFlight, c.Prio.MaxInFlight)
	if len(ds) == 0 {
		b.WriteString("The pool is empty.\n")
	} else {
		b.WriteString("| PR | Class | Why | Waited | Score | Decision |\n|---|---|---|---|---|---|\n")
		for _, d := range ds {
			dec := string(d.Action)
			if d.Note != "" {
				dec += ": " + d.Note
			}
			fmt.Fprintf(&b, "| #%d | %s | %s | %s | %.1f | %s |\n", d.PR.Number, d.Class.Name, d.Reason, roundDur(d.Waited), d.Score, dec)
		}
	}
	c.logf("%s", strings.TrimRight(b.String(), "\n"))
	if path := os.Getenv("GITHUB_STEP_SUMMARY"); path != "" {
		if f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644); err == nil {
			f.WriteString(b.String() + "\n")
			f.Close()
		}
	}
}
