package stack

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"stackq/internal/forge"
	"stackq/internal/git"
)

type SyncOptions struct {
	Fetch bool
	// RebaseTrunk also moves the bottom branch onto the latest trunk. Off by
	// default: the merge queue tests against the latest trunk anyway, and
	// restacking on every trunk move just churns CI.
	RebaseTrunk bool
	// Forge, when set, lets sync see parts that landed by squash, whose
	// commits git cannot recognise in trunk (G1). Nil means git alone.
	Forge forge.Forge
	Log   io.Writer
}

// Sync brings the local stack up to date: it picks up branches that CI
// restacked, notices branches that landed, and rebases every branch whose
// parent moved, replaying only the branch's own commits.
func Sync(r *git.Repo, s *Stack, o SyncOptions) error {
	if o.Log == nil {
		o.Log = io.Discard
	}
	hasRemote := r.HasRemote(s.Remote)
	if o.Fetch && hasRemote {
		if _, err := r.Run("fetch", "--quiet", "--prune", s.Remote); err != nil {
			return err
		}
	}
	trunk := s.TrunkRef(r)

	if hasRemote {
		for _, b := range s.Active() {
			rs, err := r.RevParse("refs/remotes/" + s.Remote + "/" + b.Name)
			if err != nil {
				continue
			}
			ls, err := r.RevParse(b.Name)
			if err != nil {
				if _, err := r.Run("branch", b.Name, rs); err != nil {
					return err
				}
				b.PushedSHA = rs
				continue
			}
			switch {
			case rs == ls || b.PushedSHA == "" || rs == b.PushedSHA:
				// in sync, never pushed by us, or only local moved
			case ls == b.PushedSHA:
				if err := r.ResetBranch(b.Name, rs); err != nil {
					return err
				}
				b.PushedSHA = rs
				fmt.Fprintf(o.Log, "↓ %s: took the version CI restacked on %s\n", b.Name, s.Remote)
			default:
				return fmt.Errorf("%s changed locally and on %s since the last push; reconcile it by hand "+
					"(e.g. git rebase %s/%s %s), then rerun sync", b.Name, s.Remote, s.Remote, b.Name, b.Name)
			}
		}
	}

	for _, b := range s.Active() {
		if r.RefExists(b.Name) && r.FullyApplied(trunk, b.Name) {
			b.Merged = true
			fmt.Fprintf(o.Log, "✓ %s has landed in %s\n", b.Name, trunk)
		}
	}
	if o.Forge != nil {
		markMergedByPR(r, s, o.Forge, o.Log)
	}

	for _, b := range s.Active() {
		p := s.Find(b.Parent)
		for p != nil && p.Merged {
			p = s.Find(p.Parent)
		}
		parentRef, newParent := trunk, s.Trunk
		if p != nil {
			parentRef, newParent = p.Name, p.Name
		}
		reparented := newParent != b.Parent
		b.Parent = newParent

		tip, err := r.RevParse(b.Name)
		if err != nil {
			return fmt.Errorf("branch %s is missing locally", b.Name)
		}
		newTip, err := r.RevParse(parentRef)
		if err != nil {
			return err
		}
		if r.IsAncestor(newTip, tip) {
			b.ParentTip = newTip
			continue
		}
		upstream := ""
		if b.ParentTip != "" && r.IsAncestor(b.ParentTip, tip) {
			upstream = b.ParentTip
		}
		if p == nil && !reparented && !o.RebaseTrunk && upstream != "" {
			continue // built on an older trunk; that's fine
		}
		fmt.Fprintf(o.Log, "↻ rebasing %s onto %s\n", b.Name, parentRef)
		if err := r.RebaseBranch(b.Name, newTip, upstream); err != nil {
			s.Save(r)
			var ce *git.ConflictError
			if errors.As(err, &ce) {
				return fmt.Errorf("%w\nresolve it with: git checkout %s && git rebase --onto %s %s\n"+
					"then run stackq sync again", err, b.Name, parentRef, orDefault(upstream, parentRef))
			}
			return err
		}
		b.ParentTip = newTip
	}
	return s.Save(r)
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

type PushOptions struct {
	Draft bool
	// SourcePR is the original big PR. Zero means: keep the one recorded at
	// split time, or look up the open PR whose head is the source branch.
	SourcePR   int
	NoSourcePR bool // don't look one up
	// CloseSource closes the original PR once the stack PRs exist.
	CloseSource bool
	Log         io.Writer
}

// Push publishes every unmerged branch with a lease, opens their PRs (or
// retargets ones outside a GitHub stack), links them as a GitHub stack,
// refreshes the stack overview in each PR body, and links the stack to the
// original PR.
func Push(r *git.Repo, f forge.Forge, s *Stack, o PushOptions) error {
	if o.Log == nil {
		o.Log = io.Discard
	}
	if !r.HasRemote(s.Remote) {
		return fmt.Errorf("stack %s has no remote; set one with --remote when splitting", s.Name)
	}
	src, err := resolveSourcePR(f, s, o)
	if err != nil {
		return err
	}
	if err := s.Save(r); err != nil {
		return err
	}

	for _, b := range s.Active() {
		sha, err := r.RevParse(b.Name)
		if err != nil {
			return err
		}
		if sha == b.PushedSHA {
			continue
		}
		lease := fmt.Sprintf("--force-with-lease=refs/heads/%s:%s", b.Name, b.PushedSHA)
		if _, err := r.Run("push", "--quiet", lease, s.Remote, sha+":refs/heads/"+b.Name); err != nil {
			return fmt.Errorf("%w\n%s moved on %s since stackq last pushed it; run stackq sync first", err, b.Name, s.Remote)
		}
		b.PushedSHA = sha
		if err := s.Save(r); err != nil {
			return err
		}
		fmt.Fprintf(o.Log, "↑ pushed %s\n", b.Name)
	}
	for i, b := range s.Branches {
		if b.Merged {
			continue
		}
		existing, err := f.FindOpenPRByHead(b.Name)
		if err != nil {
			return err
		}
		if existing == nil {
			body := RenderBlock(s.Marker(b), s.Nav())
			if i == 0 && src != nil {
				body = SourceIntro(*src) + body
			}
			pr, err := f.CreatePR(b.Name, b.Parent, b.Title, body, o.Draft || b.Hold)
			if err != nil {
				return err
			}
			b.PR = pr.Number
			fmt.Fprintf(o.Log, "+ opened #%d %s → %s\n", pr.Number, b.Name, b.Parent)
		} else {
			b.PR = existing.Number
			if existing.BaseRef != b.Parent {
				// In a GitHub stack the bases are GitHub's: it retargets
				// the PRs above a merge itself and refuses base changes.
				if n, err := f.StackOf(existing.Number); err == nil && n != 0 {
					fmt.Fprintf(o.Log, "- #%d targets %s, not %s; GitHub stack %d sets its base, so stackq leaves it\n", existing.Number, existing.BaseRef, b.Parent, n)
				} else {
					base := b.Parent
					if err := f.EditPR(existing.Number, &base, nil); err != nil {
						return err
					}
					fmt.Fprintf(o.Log, "~ retargeted #%d → %s\n", existing.Number, b.Parent)
				}
			}
		}
		if err := s.Save(r); err != nil {
			return err
		}
	}
	if err := LinkGitHubStack(f, s, o.Log); err != nil {
		return err
	}
	for _, b := range s.Active() {
		pr, err := f.GetPR(b.PR)
		if err != nil {
			return err
		}
		body := MergeBody(pr.Body, RenderBlock(s.Marker(b), s.Nav()))
		if body != pr.Body {
			if err := f.EditPR(b.PR, nil, &body); err != nil {
				return err
			}
		}
	}
	if src != nil {
		return linkSource(r, f, s, *src, o)
	}
	return nil
}

func resolveSourcePR(f forge.Forge, s *Stack, o PushOptions) (*forge.PR, error) {
	switch {
	case o.SourcePR > 0:
		s.SourcePR = o.SourcePR
	case s.SourcePR == 0 && !o.NoSourcePR && s.Source != "":
		pr, err := f.FindOpenPRByHead(s.Source)
		if err != nil {
			return nil, err
		}
		if pr != nil {
			s.SourcePR = pr.Number
			fmt.Fprintf(o.Log, "· #%d is the original PR for %s\n", pr.Number, s.Source)
		}
	}
	if s.SourcePR == 0 || o.NoSourcePR {
		return nil, nil
	}
	pr, err := f.GetPR(s.SourcePR)
	if err != nil {
		return nil, fmt.Errorf("original PR #%d: %w", s.SourcePR, err)
	}
	return &pr, nil
}

// SourceIntro is the author-owned text a bottom PR starts with: the
// original PR's description, credited. It is only written at creation.
func SourceIntro(src forge.PR) string {
	desc := strings.TrimSpace(stripBlock(src.Body))
	var b strings.Builder
	fmt.Fprintf(&b, "_Split from #%d: %s_\n\n", src.Number, src.Title)
	if desc != "" {
		b.WriteString(desc + "\n\n")
	}
	return b.String()
}

func stripBlock(body string) string {
	i := strings.Index(body, blockBegin)
	j := strings.Index(body, blockEnd)
	if i >= 0 && j > i {
		return body[:i] + body[j+len(blockEnd):]
	}
	return body
}

// linkSource comments on the original PR once with links to every slice,
// and closes it when asked.
func linkSource(r *git.Repo, f forge.Forge, s *Stack, src forge.PR, o PushOptions) error {
	if !s.SourceNotified {
		var b strings.Builder
		how := "They are a GitHub stack and land in order, bottom up"
		if s.Landing == Atomic {
			how = "They are a GitHub stack: each is reviewed on its own, and they land together, bottom up, as one merge"
		}
		fmt.Fprintf(&b, "📚 This PR was split into a stack of %d smaller PRs (stack `%s`). %s:\n\n", len(s.Branches), s.Name, how)
		for i, br := range s.Branches {
			hold := ""
			if br.Hold {
				hold = " 🔒 activation, merged by hand after the rest"
			}
			fmt.Fprintf(&b, "%d. #%d `%s`%s\n", i+1, br.PR, br.Name, hold)
		}
		b.WriteString("\nReview and merge those instead. The code here and the top of the stack are identical.")
		if o.CloseSource && src.Open() {
			b.WriteString(" Closing this PR in favor of the stack.")
		}
		if err := f.Comment(src.Number, b.String()); err != nil {
			return err
		}
		s.SourceNotified = true
		if err := s.Save(r); err != nil {
			return err
		}
		fmt.Fprintf(o.Log, "💬 linked the stack on #%d\n", src.Number)
	}
	if o.CloseSource && src.Open() {
		if err := f.ClosePR(src.Number); err != nil {
			return err
		}
		fmt.Fprintf(o.Log, "✕ closed #%d\n", src.Number)
	}
	return nil
}

// markMergedByPR asks the forge about each part git does not see as landed.
// A squash or merge commit puts the part's changes in trunk under new commit
// ids, so the patch comparison above finds nothing. A part counts as landed
// if its PR is merged and the PR's head is the branch's tip, so work added
// after the merge is never dropped. In an atomic stack the merged part carries
// everything below it into trunk, so those parts count as landed too. Errors
// from the forge are noted and ignored: sync still works from git alone.
func markMergedByPR(r *git.Repo, s *Stack, f forge.Forge, log io.Writer) {
	var landedAt = -1
	for i, b := range s.Branches {
		if b.Merged || b.Hold || b.PR == 0 || !r.RefExists(b.Name) {
			continue
		}
		pr, err := f.GetPR(b.PR)
		if err != nil {
			fmt.Fprintf(log, "! could not ask about #%d (%v); going by git alone\n", b.PR, err)
			return
		}
		if !pr.Merged() {
			continue
		}
		tip, err := r.RevParse(b.Name)
		if err != nil {
			continue
		}
		if tip != pr.HeadSHA && tip != b.PushedSHA {
			fmt.Fprintf(log, "! #%d merged, but %s has commits that were not in it; not treating it as landed\n", b.PR, b.Name)
			continue
		}
		b.Merged = true
		fmt.Fprintf(log, "✓ %s has landed (#%d merged)\n", b.Name, b.PR)
		landedAt = i
	}
	if s.Landing != Atomic || landedAt < 0 {
		return
	}
	for _, b := range s.Branches[:landedAt] {
		if !b.Merged && !b.Hold {
			b.Merged = true
			fmt.Fprintf(log, "✓ %s has landed inside #%d\n", b.Name, s.Branches[landedAt].PR)
		}
	}
}

// LinkGitHubStack makes the stack's open PRs, bottom first, one GitHub stack:
// it creates the stack, or adds PRs that were opened since on top of the one
// that exists. GitHub then shows the stack on every PR, runs trunk's rules and
// CI for each of them, merges them bottom up as one request, and rebases and
// retargets the PRs above a merge by itself. A stack of one open PR is not
// linked: GitHub stacks need two.
func LinkGitHubStack(f forge.Forge, s *Stack, log io.Writer) error {
	var open []int
	for _, b := range s.Branches {
		if !b.Merged && b.PR > 0 {
			open = append(open, b.PR)
		}
	}
	if len(open) < 2 {
		return nil
	}
	in := map[int]int{} // PR -> GitHub stack
	stacks := map[int]bool{}
	for _, n := range open {
		st, err := f.StackOf(n)
		if err != nil {
			return fmt.Errorf("could not read the GitHub stack of #%d: %w", n, err)
		}
		if st != 0 {
			in[n], stacks[st] = st, true
		}
	}
	switch len(stacks) {
	case 0:
		st, err := f.CreateStack(open)
		if err != nil {
			return fmt.Errorf("could not link %s as a GitHub stack: %w\nThe PRs exist; fix the cause and run stackq push again", prList(open), err)
		}
		fmt.Fprintf(log, "⧉ linked %s as GitHub stack %d\n", prList(open), st)
		return nil
	case 1:
	default:
		return fmt.Errorf("the PRs of stack %s are in more than one GitHub stack; unstack them on GitHub and run stackq push again", s.Name)
	}
	var st int
	for n := range stacks {
		st = n
	}
	// PRs already linked must be the bottom ones; new ones go on top.
	var missing []int
	for _, n := range open {
		if in[n] == 0 {
			missing = append(missing, n)
			continue
		}
		if len(missing) > 0 {
			return fmt.Errorf("#%d is in GitHub stack %d but %s below it is not; unstack on GitHub and run stackq push again", n, st, prList(missing))
		}
	}
	if len(missing) == 0 {
		return nil
	}
	if err := f.AddToStack(st, missing); err != nil {
		return fmt.Errorf("could not add %s to GitHub stack %d: %w", prList(missing), st, err)
	}
	fmt.Fprintf(log, "⧉ added %s to GitHub stack %d\n", prList(missing), st)
	return nil
}

func prList(prs []int) string {
	var out []string
	for _, n := range prs {
		out = append(out, fmt.Sprintf("#%d", n))
	}
	return strings.Join(out, ", ")
}
