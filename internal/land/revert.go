package land

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"stackq/internal/forge"
	"stackq/internal/git"
	"stackq/internal/stack"
)

type RevertOptions struct {
	// Only reverts just the given PR instead of every landed PR of its stack.
	Only    bool
	Enqueue bool
	// Force skips the file-set integrity check.
	Force bool
}

// landed describes what one merged PR put on trunk.
type landed struct {
	pr      forge.PR
	method  string   // merge, rebase or squash
	commits []string // trunk commits to revert, newest first
	files   []string // files the PR changed, sorted
}

// Revert opens one PR that reverts a landed stack (or one PR of it) on top
// of the current trunk. It works out which trunk commits each PR produced
// for every merge method, and refuses if the revert would touch anything
// the stack did not.
func (c *Runner) Revert(n int, o RevertOptions) (forge.PR, error) {
	r := c.Repo
	pr, err := c.Forge.GetPR(n)
	if err != nil {
		return forge.PR{}, err
	}
	m, isStack := c.markerOf(pr)
	trunk := pr.BaseRef
	if isStack {
		trunk = m.Trunk
	}
	targets := []forge.PR{pr}
	name := fmt.Sprintf("pr-%d", n)
	// below maps a stack PR to the head of the merged part under it: in a
	// GitHub stack each part merges on its own, and its branch also holds the
	// parts below, so its own commits start there.
	below := map[int]string{}
	if isStack {
		sp, err := c.loadStack(m.Stack)
		if err != nil {
			return forge.PR{}, err
		}
		prev := ""
		for _, p := range sp.prs {
			if !p.Merged() {
				continue
			}
			below[p.Number] = prev
			prev = p.HeadSHA
		}
		if !o.Only {
			name = m.Stack
			targets = nil
			for _, p := range sp.prs {
				if p.Merged() {
					targets = append(targets, p)
				}
			}
			// newest first: higher stack index landed later
			sort.Slice(targets, func(i, j int) bool { return sp.marks[targets[i].Number].Index > sp.marks[targets[j].Number].Index })
		}
	}
	if len(targets) == 0 {
		return forge.PR{}, fmt.Errorf("nothing from #%d has landed", n)
	}

	fetch := []string{"fetch", "--quiet", c.Remote, "+refs/heads/" + trunk + ":refs/remotes/" + c.remoteRef(trunk)}
	for _, t := range targets {
		if !t.Merged() {
			return forge.PR{}, fmt.Errorf("#%d is not merged", t.Number)
		}
		fetch = append(fetch, fmt.Sprintf("+refs/pull/%d/head:refs/stackq/pull/%d", t.Number, t.Number))
	}
	if _, err := r.Run(fetch...); err != nil {
		// refs/pull/* exists on GitHub; elsewhere fall back to trunk only.
		if _, err := r.Run(fetch[:5]...); err != nil {
			return forge.PR{}, err
		}
	}
	tip, err := r.RevParse(c.remoteRef(trunk))
	if err != nil {
		return forge.PR{}, err
	}

	var plan []landed
	for _, t := range targets {
		l, err := c.identify(t, below[t.Number])
		if err != nil {
			return forge.PR{}, err
		}
		plan = append(plan, l)
	}

	w, cleanup, err := r.AddWorktree(tip)
	if err != nil {
		return forge.PR{}, err
	}
	defer cleanup()
	for _, l := range plan {
		for _, sha := range l.commits {
			args := []string{"revert", "--no-edit", sha}
			if l.method == "merge" {
				args = []string{"revert", "--no-edit", "-m", "1", sha}
			}
			if _, err := w.Run(args...); err != nil {
				files, _ := w.Run("diff", "--name-only", "--diff-filter=U")
				w.Run("revert", "--abort")
				return forge.PR{}, fmt.Errorf("reverting #%d (%s) conflicts with later trunk changes in: %s\n"+
					"revert by hand, or revert the later changes first", l.pr.Number, git.Short(sha), strings.ReplaceAll(files, "\n", ", "))
			}
		}
	}
	head, err := w.RevParse("HEAD")
	if err != nil {
		return forge.PR{}, err
	}

	// Integrity: the revert must touch exactly the files the stack touched.
	want := map[string]bool{}
	for _, l := range plan {
		for _, f := range l.files {
			want[f] = true
		}
	}
	got := r.ChangedFiles(tip, head)
	var extra, missing []string
	gotSet := map[string]bool{}
	for _, f := range got {
		gotSet[f] = true
		if !want[f] {
			extra = append(extra, f)
		}
	}
	for f := range want {
		if !gotSet[f] {
			missing = append(missing, f)
		}
	}
	sort.Strings(missing)
	integrity := "✅ The revert touches exactly the files the stack changed."
	if len(extra)+len(missing) > 0 {
		detail := ""
		if len(extra) > 0 {
			detail += "\n  touched but not in the stack: " + strings.Join(extra, ", ")
		}
		if len(missing) > 0 {
			detail += "\n  in the stack but unchanged by the revert (already reverted or rewritten later?): " + strings.Join(missing, ", ")
		}
		if !o.Force {
			return forge.PR{}, fmt.Errorf("revert integrity check failed:%s\nrerun with --force after checking by hand", detail)
		}
		integrity = "⚠️ Integrity check overridden with --force:" + strings.ReplaceAll(detail, "\n  ", "\n- ")
	}

	branch := fmt.Sprintf("stackq/revert/%s-%s", stack.SafeName(name), git.Short(tip)[:7])
	if _, err := r.Run("push", "--quiet", "--force-with-lease=refs/heads/"+branch+":", c.Remote, head+":refs/heads/"+branch); err != nil {
		return forge.PR{}, fmt.Errorf("push %s: %w", branch, err)
	}

	var nums []string
	for _, l := range plan {
		nums = append(nums, "#"+strconv.Itoa(l.pr.Number))
	}
	title := fmt.Sprintf("Revert stack %s (%s)", name, strings.Join(nums, ", "))
	if !isStack || o.Only {
		title = fmt.Sprintf("Revert #%d: %s", pr.Number, pr.Title)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Reverts %s on top of `%s` (%s).\n\n| PR | Merge method | Trunk commits |\n|---|---|---|\n", strings.Join(nums, ", "), trunk, git.Short(tip))
	for _, l := range plan {
		var cs []string
		for _, s := range l.commits {
			cs = append(cs, "`"+git.Short(s)+"`")
		}
		fmt.Fprintf(&b, "| #%d | %s | %s |\n", l.pr.Number, l.method, strings.Join(cs, " "))
	}
	fmt.Fprintf(&b, "\n%s\n\nIf the feature is behind a flag, turning the flag off is faster than this revert and needs no deploy.", integrity)
	rpr, err := c.Forge.CreatePR(branch, trunk, title, b.String(), false)
	if err != nil {
		return forge.PR{}, err
	}
	c.logf("↩ opened #%d reverting %s", rpr.Number, strings.Join(nums, ", "))
	for _, l := range plan {
		c.Forge.Comment(l.pr.Number, fmt.Sprintf("↩ A revert of this change is open in #%d.", rpr.Number))
	}
	// Reverts are urgent by default (the stackq:revert label).
	c.Forge.AddLabels(rpr.Number, LabelRevert)
	if o.Enqueue {
		rpr.HeadSHA = head
		queued, err := c.submit(rpr, trunk, 1)
		if err != nil {
			return rpr, fmt.Errorf("opened #%d but could not queue it: %w", rpr.Number, err)
		}
		if queued {
			c.logf("⇢ #%d submitted to the queue", rpr.Number)
		} else {
			c.logf("⧗ #%d is in the priority pool until a pass queues it", rpr.Number)
		}
	}
	return rpr, nil
}

// identify finds the trunk commits PR t produced. below, if set, is the head of
// the stack part under t: t's own commits are the ones after it.
func (c *Runner) identify(t forge.PR, below string) (landed, error) {
	r := c.Repo
	d, err := c.Forge.Details(t.Number)
	if err != nil {
		return landed{}, err
	}
	x := d.MergeCommit
	if x == "" || !r.RefExists(x) {
		return landed{}, fmt.Errorf("#%d: merge commit %q is not on trunk", t.Number, x)
	}
	parents := r.Parents(x)
	head := t.HeadSHA
	if !r.RefExists(head) {
		if alt, err := r.RevParse(fmt.Sprintf("refs/stackq/pull/%d", t.Number)); err == nil {
			head = alt
		} else {
			return landed{}, errors.New(fmt.Sprintf("#%d: its head commit %s is not available; fetch refs/pull/%d/head", t.Number, git.Short(t.HeadSHA), t.Number))
		}
	}
	if len(parents) > 1 {
		files := r.ChangedFiles(parents[0], x)
		sort.Strings(files)
		return landed{pr: t, method: "merge", commits: []string{x}, files: files}, nil
	}
	if len(parents) == 0 {
		return landed{}, fmt.Errorf("#%d: merge commit %s has no parent", t.Number, git.Short(x))
	}

	fork, err := r.MergeBase(head, parents[0])
	if err != nil {
		return landed{}, err
	}
	if below != "" && r.RefExists(below) && r.IsAncestor(below, head) {
		fork = below
	}
	out, _ := r.Run("rev-list", "--reverse", "--no-merges", fork+".."+head)
	var own []string
	for _, l := range strings.Split(out, "\n") {
		if l != "" {
			own = append(own, l)
		}
	}
	files := r.ChangedFiles(fork, head)
	sort.Strings(files)
	if len(own) == 0 {
		return landed{}, fmt.Errorf("#%d: no commits of its own between %s and its head", t.Number, git.Short(fork))
	}

	// Rebase method: the last len(own) trunk commits up to x are the PR's
	// commits with new SHAs. Match them by patch-id, or by subject + files
	// when trunk changes nearby altered the diff context.
	key := func(sha string) string {
		subj, _ := r.Run("log", "-1", "--format=%s", sha)
		ps := r.Parents(sha)
		if len(ps) != 1 {
			return subj
		}
		f := r.ChangedFiles(ps[0], sha)
		sort.Strings(f)
		return subj + "\x00" + strings.Join(f, "\x00")
	}
	ids, keys := map[string]bool{}, map[string]bool{}
	for _, s := range own {
		if id := r.PatchID(s); id != "" {
			ids[id] = true
		}
		keys[key(s)] = true
	}
	var seq []string
	cur := x
	for len(seq) < len(own) {
		if !(ids[r.PatchID(cur)] || keys[key(cur)]) {
			break
		}
		seq = append(seq, cur)
		ps := r.Parents(cur)
		if len(ps) != 1 {
			break
		}
		cur = ps[0]
	}
	if len(seq) == len(own) {
		return landed{pr: t, method: "rebase", commits: seq, files: files}, nil
	}

	// Squash method: one trunk commit with the PR's whole change.
	sq := r.ChangedFiles(parents[0], x)
	sort.Strings(sq)
	if strings.Join(sq, "\x00") == strings.Join(files, "\x00") {
		return landed{pr: t, method: "squash", commits: []string{x}, files: files}, nil
	}
	return landed{}, fmt.Errorf("#%d: could not tell which trunk commits it produced (merge commit %s); revert it by hand",
		t.Number, git.Short(x))
}
