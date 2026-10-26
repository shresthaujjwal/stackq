package land

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"stackq/internal/forge"
)

// Stale is a remote branch GC would delete, and why.
type Stale struct {
	Branch string
	SHA    string
	Why    string
}

// GC finds remote branches that stackq or a merge left behind and, with
// del set, deletes them. A branch qualifies only if no open PR uses it as
// head or base, its tip is still exactly the head of its last PR (nobody
// pushed since), and one of these holds:
//
//   - that PR merged (what GitHub's "delete head branch" would have done);
//   - it is a stack or revert branch and that PR was closed;
//   - it is the source branch a stack was split from, the source PR is
//     closed, and nothing of that stack is still open.
//
// Branches that never had a PR are left alone, as are trunk and PRs from
// forks. Each deletion is a push with a lease on the expected tip, so a branch
// that moved in the meantime is kept. Only the most recent closed and merged
// PRs are looked at (the forge's list limit), so very old leftovers may stay.
func (c *Runner) GC(trunk string, del bool) ([]Stale, error) {
	return c.gc(trunk, del, false)
}

// TidyStackBranches deletes the branches stackq itself made (stack and revert
// branches) once their PRs are merged or closed, by the same rules as GC. A
// stack of one part has no open PR left after the queue merges it, so the
// cleanup that follows a landing (which looks at stacks with open PRs) never
// sees it, and the merge queue's merge starts no "PR merged" run for the
// built-in token. It is quiet when there is nothing to do, so a tick can run it.
func (c *Runner) TidyStackBranches(trunk string) error {
	if c.KeepBranches {
		return nil
	}
	_, err := c.gc(trunk, true, true)
	return err
}

func (c *Runner) gc(trunk string, del, onlyStack bool) ([]Stale, error) {
	out, err := c.Repo.Run("ls-remote", "--heads", c.Remote)
	if err != nil {
		return nil, err
	}
	tips := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		sha, ref, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if ok && strings.HasPrefix(ref, "refs/heads/") {
			tips[strings.TrimPrefix(ref, "refs/heads/")] = sha
		}
	}

	open, err := c.Forge.ListPRs("open")
	if err != nil {
		return nil, err
	}
	all, err := c.Forge.ListPRs("all")
	if err != nil {
		return nil, err
	}
	closed, err := c.Forge.ListPRs("closed")
	if err != nil {
		return nil, err
	}

	inUse := map[string]bool{trunk: true}
	openStacks := map[string]bool{}
	for _, p := range open {
		inUse[p.HeadRef], inUse[p.BaseRef] = true, true
		if m, ok := c.markerOf(p); ok {
			openStacks[m.Stack] = true
		}
	}
	last := map[string]forge.PR{} // newest finished PR per head branch
	sources := map[int][]string{} // source PR → stacks split from it
	seen := map[int]bool{}
	for _, p := range append(all, closed...) {
		if p.Open() || seen[p.Number] {
			continue
		}
		seen[p.Number] = true
		if p.Number > last[p.HeadRef].Number {
			last[p.HeadRef] = p
		}
		if m, ok := c.markerOf(p); ok && m.Source > 0 {
			sources[m.Source] = append(sources[m.Source], m.Stack)
		}
	}

	var stale []Stale
	for branch, sha := range tips {
		p, ok := last[branch]
		if inUse[branch] || !ok || p.CrossRepo || p.HeadSHA != sha || (onlyStack && !c.isStackBranch(branch)) {
			continue
		}
		why := ""
		switch {
		case p.Merged():
			why = fmt.Sprintf("#%d merged", p.Number)
		case c.isStackBranch(branch):
			why = fmt.Sprintf("stack branch; #%d was closed", p.Number)
		case len(sources[p.Number]) > 0 && !anyOpen(sources[p.Number], openStacks):
			why = fmt.Sprintf("split into stack %s (#%d closed), and nothing of it is open", strings.Join(sources[p.Number], ", "), p.Number)
		default:
			continue
		}
		stale = append(stale, Stale{branch, sha, why})
	}
	sort.Slice(stale, func(i, j int) bool { return stale[i].Branch < stale[j].Branch })

	verb := "would delete"
	if del {
		verb = "deleted"
	}
	var errs []error
	for _, s := range stale {
		if del {
			ref := "refs/heads/" + s.Branch
			if _, err := c.Repo.Run("push", "--quiet", fmt.Sprintf("--force-with-lease=%s:%s", ref, s.SHA), c.Remote, ":"+ref); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", s.Branch, err))
				c.logf("! could not delete %s: %v", s.Branch, err)
				continue
			}
		}
		c.logf("🗑 %s %s (%s)", verb, s.Branch, s.Why)
	}
	switch {
	case len(stale) == 0:
		if !onlyStack {
			c.logf("no stale branches")
		}
	case !del:
		c.logf("\n%d stale branches; run again with --delete to delete them", len(stale))
	}
	return stale, errors.Join(errs...)
}

// isStackBranch reports whether branch is one stackq creates: a stack branch
// under the configured prefix, or a revert branch.
func (c *Runner) isStackBranch(branch string) bool {
	if strings.HasPrefix(branch, "stackq/revert/") {
		return true
	}
	before, _, _ := strings.Cut(c.branchPrefix("{name}"), "{name}")
	return before != "" && strings.HasPrefix(branch, before)
}

func anyOpen(stacks []string, open map[string]bool) bool {
	for _, s := range stacks {
		if open[s] {
			return true
		}
	}
	return false
}
