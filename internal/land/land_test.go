package land_test

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"stackq/internal/forge"
	"stackq/internal/git"
	"stackq/internal/land"
	"stackq/internal/split"
	"stackq/internal/stack"
	"stackq/internal/testrepo"
)

type fixture struct {
	dev  *git.Repo // developer clone with the stack
	bare *git.Repo // "GitHub"
	f    *forge.Fake
	s    *stack.Stack
}

func setup(t *testing.T) *fixture {
	t.Helper()
	return setupWith(t, testrepo.GoApp(t), nil, "")
}

var shopFlags = &split.Flags{
	Paths:       []string{"internal/flags/"},
	Activation:  []string{"config/prod.yaml"},
	Guards:      []string{"flags.Enabled"},
	Entrypoints: []string{"HandleFunc"},
}

func setupWith(t *testing.T, dev *git.Repo, flags *split.Flags, landing string) *fixture {
	t.Helper()
	bare := testrepo.WithOrigin(t, dev)
	p, err := split.Make(dev, split.Options{Base: "origin/main", Head: "feature", MaxLines: 1, Flags: flags})
	if err != nil {
		t.Fatal(err)
	}
	s, err := stack.Create(dev, p, stack.CreateOptions{Name: "feat", Prefix: "stack/feat/", Trunk: "main", Remote: "origin",
		Source: "feature", Landing: landing})
	if err != nil {
		t.Fatal(err)
	}
	f := forge.NewFake()
	f.HeadSHA = func(b string) string {
		out, _ := bare.Run("rev-parse", "--verify", "--quiet", "refs/heads/"+b)
		return out
	}
	if err := stack.Push(dev, f, s, stack.PushOptions{}); err != nil {
		t.Fatal(err)
	}
	fx := &fixture{dev, bare, f, s}
	fx.publishPullRefs(t)
	return fx
}

// publishPullRefs mirrors GitHub's refs/pull/N/head, which survive branch
// deletion and are what stackq revert fetches.
func (fx *fixture) publishPullRefs(t *testing.T) {
	t.Helper()
	prs, _ := fx.f.ListPRs("all")
	for _, p := range prs {
		if sha, err := fx.bare.Run("rev-parse", "--verify", "--quiet", "refs/heads/"+p.HeadRef); err == nil && sha != "" {
			testrepo.Must(t, fx.bare, "update-ref", "refs/pull/"+strconv.Itoa(p.Number)+"/head", sha)
		}
	}
}

func (fx *fixture) mainTip(t *testing.T) string {
	return testrepo.Must(t, fx.bare, "rev-parse", "refs/heads/main")
}

// landQueue does what GitHub's merge queue does with the entries a merge
// request put there: it lands each queued PR on trunk, bottom first, with
// method (rebase, merge or squash) and records its merge commit. Each PR adds
// only its own commits, the ones after the PR below it, as when GitHub merges
// a stack. The fake then marks them merged and retargets the PRs above.
func (fx *fixture) landQueue(t *testing.T, method string) []int {
	t.Helper()
	fx.publishPullRefs(t)
	queued := append([]forge.QueueEntry(nil), fx.f.MQ...)
	heads := map[string]string{} // branch -> head it merged with, in this pass
	for _, e := range queued {
		pr, _ := fx.f.GetPR(e.PR)
		ci := testrepo.Clone(t, fx.bare)
		head := testrepo.Must(t, ci, "rev-parse", "origin/"+pr.HeadRef)
		// The PR's own commits start after the part below it, whether that
		// merged in this pass or an earlier one.
		from := ""
		for i, b := range fx.s.Branches {
			if b.PR == pr.Number && i > 0 {
				below := fx.s.Branches[i-1]
				if h, ok := heads[below.Name]; ok {
					from = h
				} else if bp, _ := fx.f.GetPR(below.PR); bp.Merged() {
					from = bp.HeadSHA
				}
			}
		}
		if from == "" || !ci.IsAncestor(from, head) {
			from = testrepo.Must(t, ci, "merge-base", "origin/main", head)
		}
		heads[pr.HeadRef] = head
		switch method {
		case "rebase":
			testrepo.RebaseMerge(t, fx.bare, from, pr.HeadRef, false)
		case "merge":
			testrepo.MergeCommitMerge(t, fx.bare, pr.HeadRef, false)
		case "squash":
			testrepo.SquashMerge(t, fx.bare, pr.HeadRef, false)
		}
		tip := fx.mainTip(t)
		fx.f.SetDetails(e.PR, func(d *forge.Details) { d.MergeCommit = tip })
	}
	return fx.f.LandQueue()
}

// ci is a Runner in a fresh clone, as a workflow run would have.
func (fx *fixture) ci(t *testing.T) *land.Runner {
	return &land.Runner{Repo: testrepo.Clone(t, fx.bare), Forge: fx.f, Remote: "origin", Enqueue: true}
}

func prBy(t *testing.T, fx *fixture, i int) forge.PR {
	t.Helper()
	pr, err := fx.f.GetPR(fx.s.Branches[i].PR)
	if err != nil {
		t.Fatal(err)
	}
	return pr
}

func comments(fx *fixture, n int) string { return strings.Join(fx.f.Comments[n], "\n---\n") }

func TestPushLinksTheStackOnGitHub(t *testing.T) {
	fx := setupWith(t, testrepo.FlagApp(t, "if"), shopFlags, stack.Atomic)
	st, _ := fx.f.StackOf(prBy(t, fx, 0).Number)
	prs, _ := fx.f.StackPRs(st)
	if st == 0 || len(prs) != 4 || prs[3] != prBy(t, fx, 3).Number {
		t.Fatalf("all four PRs, activation on top, should be one GitHub stack: %d %v", st, prs)
	}
}

func TestAtomicStackLandsWithOneMergeRequestAndActivationWaits(t *testing.T) {
	fx := setupWith(t, testrepo.FlagApp(t, "if"), shopFlags, stack.Atomic)
	orig, _ := fx.f.CreatePR("feature", "main", "New checkout", "desc", false)
	if err := stack.Push(fx.dev, fx.f, fx.s, stack.PushOptions{SourcePR: orig.Number}); err != nil {
		t.Fatal(err)
	}
	// flags, internal/checkout, cmd/shop, activation (held)
	if n := len(fx.s.Branches); n != 4 || !fx.s.Branches[3].Hold {
		t.Fatalf("unexpected stack: %+v", fx.s.Branches)
	}
	lander, hold := prBy(t, fx, 2), prBy(t, fx, 3)
	run := &land.Runner{Repo: fx.dev, Forge: fx.f, Remote: "origin"}

	// Gate: part 2 is not approved yet.
	fx.f.SetDetails(prBy(t, fx, 1).Number, func(d *forge.Details) { d.Approvals = nil })
	err := run.Land(prBy(t, fx, 0).Number)
	if err == nil || !strings.Contains(err.Error(), "required approvals") || len(fx.f.Merges) != 0 {
		t.Fatalf("unapproved part must block landing: %v %v", err, fx.f.Merges)
	}
	if !fx.f.Labels[lander.Number][land.LabelBlocked] {
		t.Fatal("blocked label missing on the lander")
	}
	fx.f.SetDetails(prBy(t, fx, 1).Number, func(d *forge.Details) { d.Approvals = []string{"ana"} })

	before := fx.mainTip(t)
	if err := run.Land(prBy(t, fx, 0).Number); err != nil {
		t.Fatal(err)
	}
	// One request, for the top part; GitHub queues the three parts in order.
	if len(fx.f.Merges) != 1 || fx.f.Merges[0] != lander.Number {
		t.Fatalf("want one merge request for the lander #%d, got %v", lander.Number, fx.f.Merges)
	}
	want := []int{prBy(t, fx, 0).Number, prBy(t, fx, 1).Number, lander.Number}
	if got := queueOrder(fx.f); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("queue should hold the parts bottom up, not the activation PR: %v", got)
	}
	for i := 0; i < 3; i++ {
		if p := prBy(t, fx, i); p.BaseRef != fx.s.Branches[i].Parent || !fx.f.Labels[p.Number][land.LabelLanding] {
			t.Fatalf("part %d: base=%s labels=%v (stackq must not retarget)", i+1, p.BaseRef, fx.f.Labels[p.Number])
		}
	}
	if !strings.Contains(prBy(t, fx, 2).Body, "stackq:merge-request") {
		t.Fatal("the merge request is not recorded on the lander")
	}

	// The queue lands them; the next tick finishes the stack.
	fx.landQueue(t, "rebase")
	if err := fx.ci(t).FinishLanded(); err != nil {
		t.Fatal(err)
	}
	ci := testrepo.Clone(t, fx.bare)
	if !ci.TreesEqual("origin/main", lander.HeadSHA) {
		t.Fatal("trunk is not exactly the stack minus activation")
	}
	if n := ci.CountCommits(before, "origin/main"); n != 3 {
		t.Fatalf("want the 3 parts on trunk, got %d commits", n)
	}
	if out := testrepo.Must(t, ci, "show", "origin/main:config/prod.yaml"); strings.Contains(out, "true") {
		t.Fatal("activation reached trunk")
	}
	for i := 0; i < 3; i++ {
		p := prBy(t, fx, i)
		if !p.Merged() || fx.f.Labels[p.Number][land.LabelLanding] {
			t.Fatalf("part %d: state=%s labels=%v", i+1, p.State, fx.f.Labels[p.Number])
		}
	}
	if b := prBy(t, fx, 2).Body; !strings.Contains(b, "Landed") || strings.Contains(b, "stackq:merge-request") {
		t.Fatalf("lander status not finished:\n%s", b)
	}
	h := prBy(t, fx, 3)
	if h.BaseRef != "main" || !fx.f.Labels[h.Number][land.LabelActivation] || !h.Open() {
		t.Fatalf("activation PR: base=%s labels=%v state=%s", h.BaseRef, fx.f.Labels[h.Number], h.State)
	}
	if !strings.Contains(comments(fx, hold.Number), "stackq will not merge it") {
		t.Fatalf("activation PR not told: %q", fx.f.Comments[hold.Number])
	}
	if c := comments(fx, orig.Number); !strings.Contains(c, "has landed on `main`") || !strings.Contains(c, fmt.Sprintf("#%d", hold.Number)) {
		t.Fatalf("original PR not told: %q", c)
	}
	if !strings.Contains(prBy(t, fx, 3).Body, "✅") {
		t.Fatal("nav not refreshed on the activation PR")
	}

	// Finishing again changes nothing.
	n := len(fx.f.Comments[orig.Number]) + len(fx.f.Comments[hold.Number])
	if err := fx.ci(t).FinishLanded(); err != nil || len(fx.f.Comments[orig.Number])+len(fx.f.Comments[hold.Number]) != n {
		t.Fatalf("a finished stack must be left alone: %v", err)
	}

	// One command rolls the whole stack back.
	rev, err := fx.ci(t).Revert(lander.Number, land.RevertOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ci = testrepo.Clone(t, fx.bare)
	if !ci.TreesEqual("origin/"+rev.HeadRef, before) || !strings.Contains(rev.Body, "| rebase |") ||
		!strings.Contains(rev.Body, "✅ The revert touches exactly") {
		t.Fatalf("revert PR wrong:\n%s", rev.Body)
	}
}

// Each part merges on its own in a GitHub stack, so revert has to find each
// part's own commits, for every merge method.
func TestRevertAfterAStackLandsWithEachMergeMethod(t *testing.T) {
	for _, method := range []string{"rebase", "merge", "squash"} {
		t.Run(method, func(t *testing.T) {
			fx := setupWith(t, testrepo.GoApp(t), nil, stack.Atomic)
			run := &land.Runner{Repo: fx.dev, Forge: fx.f, Remote: "origin"}
			if err := run.Land(prBy(t, fx, 0).Number); err != nil {
				t.Fatal(err)
			}
			before := fx.mainTip(t)
			fx.landQueue(t, method)
			// Someone else lands an unrelated change afterwards.
			other := testrepo.Clone(t, fx.bare)
			testrepo.Commit(t, other, "unrelated", map[string]string{"NOTES.txt": "later\n"})
			testrepo.Must(t, other, "push", "-q", "origin", "main")

			top := prBy(t, fx, len(fx.s.Branches)-1)
			rev, err := fx.ci(t).Revert(top.Number, land.RevertOptions{Enqueue: true})
			if err != nil {
				t.Fatal(err)
			}
			ci := testrepo.Clone(t, fx.bare)
			if d := ci.ChangedFiles(before, "origin/"+rev.HeadRef); len(d) != 1 || d[0] != "NOTES.txt" {
				t.Fatalf("after revert the only difference from pre-stack main should be NOTES.txt, got %v", d)
			}
			// Each part is one commit, so its squash is the same patch as a
			// rebase, and revert may call it either; both revert correctly.
			described := strings.Contains(rev.Body, "| "+method+" |") || method == "squash" && strings.Contains(rev.Body, "| rebase |")
			if !described || fx.f.Merges[len(fx.f.Merges)-1] != rev.Number {
				t.Fatalf("revert PR not described or not merged (merges %v, revert #%d):\n%s", fx.f.Merges, rev.Number, rev.Body)
			}
		})
	}
}

func TestIncrementalStackLandsPartByPart(t *testing.T) {
	fx := setup(t) // incremental
	n := len(fx.s.Branches)
	run := &land.Runner{Repo: fx.dev, Forge: fx.f, Remote: "origin"}
	if err := run.Land(prBy(t, fx, 0).Number); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if len(fx.f.Merges) != i+1 || fx.f.Merges[i] != prBy(t, fx, i).Number {
			t.Fatalf("step %d: want a merge request for part %d alone, got %v", i, i+1, fx.f.Merges)
		}
		if got := fx.landQueue(t, "rebase"); len(got) != 1 {
			t.Fatalf("step %d: one PR should land at a time, got %v", i, got)
		}
		if i+1 < n && prBy(t, fx, i+1).BaseRef != "main" {
			t.Fatalf("GitHub retargets the next part onto main")
		}
		if err := fx.ci(t).AfterMerge(prBy(t, fx, i).Number); err != nil {
			t.Fatal(err)
		}
	}
	// The top part's branch is byte for byte the feature branch.
	if top := prBy(t, fx, n-1); !testrepo.Clone(t, fx.bare).TreesEqual("origin/main", top.HeadSHA) {
		t.Fatal("trunk is not the feature after every part landed")
	}
}

// GitHub merges a stacked PR together with every open PR below it, so landing
// a higher PR of an incremental stack lands those too.
func TestLandingAHigherPartOfAnIncrementalStackTakesTheOnesBelow(t *testing.T) {
	fx := setup(t)
	run := &land.Runner{Repo: fx.dev, Forge: fx.f, Remote: "origin"}
	if err := run.Land(prBy(t, fx, 2).Number); err != nil {
		t.Fatal(err)
	}
	want := []int{prBy(t, fx, 0).Number, prBy(t, fx, 1).Number, prBy(t, fx, 2).Number}
	if fmt.Sprint(queueOrder(fx.f)) != fmt.Sprint(want) || len(fx.f.Merges) != 1 {
		t.Fatalf("want one request queueing parts 1-3, got merges=%v queue=%v", fx.f.Merges, queueOrder(fx.f))
	}
}

func TestAutoLandWaitsQuietlyThenLandsOnce(t *testing.T) {
	fx := setupWith(t, testrepo.FlagApp(t, "if"), shopFlags, stack.Atomic)
	run := &land.Runner{Repo: fx.dev, Forge: fx.f, Remote: "origin"}
	lander, part2 := prBy(t, fx, 2), prBy(t, fx, 1)

	fx.f.SetDetails(part2.Number, func(d *forge.Details) { d.Checks = "pending" })
	if err := run.AutoLand(); err != nil {
		t.Fatal(err)
	}
	if len(fx.f.Merges) != 0 || len(fx.f.Comments[lander.Number]) != 0 {
		t.Fatalf("a stack that isn't ready must wait without noise: %v %q", fx.f.Merges, fx.f.Comments[lander.Number])
	}
	if b := prBy(t, fx, 2).Body; !strings.Contains(b, "Not landing yet") || !strings.Contains(b, fmt.Sprintf("#%d: checks are still running", part2.Number)) {
		t.Fatalf("the waiting reason should be on the lander:\n%s", b)
	}

	fx.f.SetDetails(part2.Number, func(d *forge.Details) { d.Checks = "success" })
	for i := 0; i < 2; i++ {
		if err := run.AutoLand(); err != nil {
			t.Fatal(err)
		}
	}
	if len(fx.f.Merges) != 1 || fx.f.Merges[0] != lander.Number {
		t.Fatalf("want exactly one merge request, got %v", fx.f.Merges)
	}
	if b := prBy(t, fx, 2).Body; !strings.Contains(b, "Landing.") || strings.Contains(b, "Not landing yet") {
		t.Fatalf("status not updated:\n%s", b)
	}
}

func TestAMergeRequestThatIsPendingThenFailsPausesTheStack(t *testing.T) {
	fx := setupWith(t, testrepo.FlagApp(t, "if"), shopFlags, stack.Atomic)
	run := &land.Runner{Repo: fx.dev, Forge: fx.f, Remote: "origin"}
	lander := prBy(t, fx, 2)
	fx.f.MergePending = true
	if err := run.AutoLand(); err != nil {
		t.Fatal(err)
	}
	fx.f.MQ = nil // GitHub has not put anything in the queue yet
	if err := run.AutoLand(); err != nil || len(fx.f.Merges) != 1 {
		t.Fatalf("a pending request must not be made again: %v %v", err, fx.f.Merges)
	}

	for _, r := range fx.f.Requests {
		r.Status, r.Message = forge.MergeFailed, "Required status check \"test\" is expected."
	}
	if err := run.AutoLand(); err != nil {
		t.Fatal(err)
	}
	if !fx.f.Labels[lander.Number][land.LabelBlocked] || !strings.Contains(comments(fx, lander.Number), "is expected") {
		t.Fatalf("the failure should pause the stack and quote GitHub: %v %q", fx.f.Labels[lander.Number], fx.f.Comments[lander.Number])
	}
	if b := prBy(t, fx, 2).Body; !strings.Contains(b, "Paused.") || strings.Contains(b, "stackq:merge-request") {
		t.Fatalf("status should say paused:\n%s", b)
	}
	if err := run.AutoLand(); err != nil || len(fx.f.Merges) != 1 {
		t.Fatalf("a paused stack is left alone: %v %v", err, fx.f.Merges)
	}

	// After a fix, landing by hand asks again and clears the pause.
	fx.f.MergePending = false
	if err := run.Land(lander.Number); err != nil {
		t.Fatal(err)
	}
	if fx.f.Labels[lander.Number][land.LabelBlocked] || len(fx.f.Merges) != 2 {
		t.Fatalf("manual land did not resume: labels=%v merges=%v", fx.f.Labels[lander.Number], fx.f.Merges)
	}
}

func TestAStackThatSilentlyLeftTheQueueIsPaused(t *testing.T) {
	fx := setupWith(t, testrepo.FlagApp(t, "if"), shopFlags, stack.Atomic)
	run := &land.Runner{Repo: fx.dev, Forge: fx.f, Remote: "origin"}
	if err := run.AutoLand(); err != nil {
		t.Fatal(err)
	}
	lander := prBy(t, fx, 2)
	fx.f.MQ = nil // ejected, and the dequeued run was lost
	if err := run.AutoLand(); err != nil {
		t.Fatal(err)
	}
	if !fx.f.Labels[lander.Number][land.LabelBlocked] || !strings.Contains(prBy(t, fx, 2).Body, "Paused.") {
		t.Fatalf("a lander that left the queue should be paused: %v", fx.f.Labels[lander.Number])
	}
}

func TestDequeuedPausesStack(t *testing.T) {
	fx := setup(t)
	pr := fx.s.Branches[0].PR
	run := &land.Runner{Repo: fx.dev, Forge: fx.f, Remote: "origin"}
	if err := run.Dequeued(pr); err != nil {
		t.Fatal(err)
	}
	if !fx.f.Labels[pr][land.LabelBlocked] || len(fx.f.Comments[pr]) != 1 {
		t.Fatal("dequeue not reported")
	}
	if err := run.Land(pr); err != nil || fx.f.Labels[pr][land.LabelBlocked] {
		t.Fatalf("retry should ask again and clear the label: %v", err)
	}
}

func TestMergeErrorIsReported(t *testing.T) {
	fx := setup(t)
	fx.f.MergeErr = errors.New("HTTP 403: Resource not accessible by integration")
	if err := (&land.Runner{Repo: fx.dev, Forge: fx.f, Remote: "origin"}).Land(fx.s.Branches[0].PR); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("expected GitHub's error, got %v", err)
	}
}

func TestAStackThatIsNotAGitHubStackIsLinkedWhenItLands(t *testing.T) {
	fx := setupWith(t, testrepo.FlagApp(t, "if"), shopFlags, stack.Atomic)
	fx.f.Stacks = map[int][]int{} // pushed before stackq used GitHub stacks
	var log bytes.Buffer
	if err := (&land.Runner{Repo: fx.dev, Forge: fx.f, Remote: "origin", Log: &log}).Land(prBy(t, fx, 0).Number); err != nil {
		t.Fatal(err)
	}
	if st, _ := fx.f.StackOf(prBy(t, fx, 0).Number); st == 0 || !strings.Contains(log.String(), "linked") {
		t.Fatalf("the PRs should be linked first:\n%s", log.String())
	}
	if len(fx.f.MQ) != 3 {
		t.Fatalf("then landed as one stack: %v", queueOrder(fx.f))
	}
}

func TestTheActivationPRLandsOnlyWhenNamed(t *testing.T) {
	fx := setupWith(t, testrepo.FlagApp(t, "if"), shopFlags, stack.Atomic)
	run := &land.Runner{Repo: fx.dev, Forge: fx.f, Remote: "origin"}
	if err := run.AutoLand(); err != nil {
		t.Fatal(err)
	}
	fx.landQueue(t, "rebase")
	if err := fx.ci(t).FinishLanded(); err != nil {
		t.Fatal(err)
	}
	if err := run.AutoLand(); err != nil || len(fx.f.Merges) != 1 {
		t.Fatalf("auto-land must never merge the activation PR: %v %v", err, fx.f.Merges)
	}
	hold := prBy(t, fx, 3)
	fx.f.PRs[hold.Number].Draft = false // a human marks it ready
	if err := run.Land(hold.Number); err != nil || fx.f.Merges[len(fx.f.Merges)-1] != hold.Number {
		t.Fatalf("landing it by name should work: %v %v", err, fx.f.Merges)
	}
}

func TestIgnoresForeignPRs(t *testing.T) {
	fx := setup(t)
	pr, _ := fx.f.CreatePR("someone/else", "main", "unrelated", "no marker here", false)
	fx.f.MarkMerged(pr.Number)
	if err := fx.ci(t).AfterMerge(pr.Number); err != nil || len(fx.f.Merges) != 0 {
		t.Fatalf("foreign PR should be a no-op: %v %v", err, fx.f.Merges)
	}
}

// A split opens its PRs one by one, and with a token that starts pull_request
// CI the first parts can be green before the last one exists. Landing the
// highest PR that exists would land half the feature.
func TestAnIncompleteStackIsNotLanded(t *testing.T) {
	fx := setupWith(t, testrepo.FlagApp(t, "if"), shopFlags, stack.Atomic)
	run := &land.Runner{Repo: fx.dev, Forge: fx.f, Remote: "origin"}
	top := prBy(t, fx, 2)
	hidden := fx.f.PRs[top.Number]
	delete(fx.f.PRs, top.Number) // the last part has not been created yet

	if err := run.AutoLand(); err != nil || len(fx.f.Merges) != 0 {
		t.Fatalf("auto-land must wait quietly: %v %v", err, fx.f.Merges)
	}
	if err := run.Land(prBy(t, fx, 0).Number); err == nil || !strings.Contains(err.Error(), "3 of 4 PRs") {
		t.Fatalf("a manual land must say the stack is incomplete, got %v", err)
	}
	fx.f.PRs[top.Number] = hidden // the split finishes
	if err := run.AutoLand(); err != nil || len(fx.f.Merges) != 1 || fx.f.Merges[0] != top.Number {
		t.Fatalf("the complete stack was not landed: %v %v", err, fx.f.Merges)
	}
}

func TestAutoLandWithoutApprovalsWhenAllowed(t *testing.T) {
	fx := setupWith(t, testrepo.FlagApp(t, "if"), shopFlags, stack.Atomic)
	for i := 0; i < 3; i++ {
		fx.f.SetDetails(prBy(t, fx, i).Number, func(d *forge.Details) { d.Approvals = nil })
	}
	strict := &land.Runner{Repo: fx.dev, Forge: fx.f, Remote: "origin"}
	if err := strict.AutoLand(); err != nil || len(fx.f.Merges) != 0 {
		t.Fatalf("unreviewed stack must wait by default: %v %v", err, fx.f.Merges)
	}
	lax := &land.Runner{Repo: fx.dev, Forge: fx.f, Remote: "origin", AllowUnreviewed: true}
	if err := lax.AutoLand(); err != nil || len(fx.f.Merges) != 1 {
		t.Fatalf("allowUnreviewed should land it: %v %v", err, fx.f.Merges)
	}
	fx2 := setupWith(t, testrepo.FlagApp(t, "if"), shopFlags, stack.Atomic)
	fx2.f.SetDetails(prBy(t, fx2, 1).Number, func(d *forge.Details) { d.Approvals = nil; d.Checks = "failure" })
	lax2 := &land.Runner{Repo: fx2.dev, Forge: fx2.f, Remote: "origin", AllowUnreviewed: true}
	if err := lax2.AutoLand(); err != nil || len(fx2.f.Merges) != 0 {
		t.Fatalf("red checks must still block: %v %v", err, fx2.f.Merges)
	}
}

func TestManualLandStillAcceptsPartsWithoutChecks(t *testing.T) {
	fx := setupWith(t, testrepo.FlagApp(t, "if"), shopFlags, stack.Atomic)
	for i := 0; i < 3; i++ {
		fx.f.SetDetails(prBy(t, fx, i).Number, func(d *forge.Details) { d.Checks = "none" })
	}
	if err := (&land.Runner{Repo: fx.dev, Forge: fx.f, Remote: "origin"}).Land(prBy(t, fx, 0).Number); err != nil || len(fx.f.Merges) != 1 {
		t.Fatalf("a person landing by hand may land a repo without CI: %v %v", err, fx.f.Merges)
	}
}

func TestFinishLandedLeavesUnlandedStacksAlone(t *testing.T) {
	fx := setupWith(t, testrepo.FlagApp(t, "if"), shopFlags, stack.Atomic)
	if err := fx.ci(t).FinishLanded(); err != nil || len(fx.f.Comments) != 0 {
		t.Fatalf("nothing landed, nothing to do: %v %v", err, fx.f.Comments)
	}
}

// Right after the label is added the pool search can lag and not find the PR.
// The log must not claim the stack went to GitHub when it did not.
func TestLandOnlyClaimsAMergeWhenThePoolAdmittedIt(t *testing.T) {
	fx := setupWith(t, testrepo.FlagApp(t, "if"), shopFlags, stack.Atomic)
	var log bytes.Buffer
	run := &land.Runner{Repo: fx.dev, Forge: fx.f, Remote: "origin", Prio: prioCfg(), Log: &log}
	fx.f.PoolLag = true
	if err := run.Land(prBy(t, fx, 0).Number); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(log.String(), "asked GitHub to merge") || !strings.Contains(log.String(), "stays in the priority pool") || len(fx.f.Merges) != 0 {
		t.Fatalf("the pool missed the PR, the log must say so:\n%s\nmerges: %v", log.String(), fx.f.Merges)
	}
	fx.f.PoolLag = false
	if _, err := run.Schedule("main", false); err != nil || len(fx.f.Merges) != 1 {
		t.Fatalf("the next pass should merge it: %v %v", err, fx.f.Merges)
	}
	if !strings.Contains(prBy(t, fx, 2).Body, "stackq:merge-request") {
		t.Fatal("a merge made by the pool must be recorded too")
	}
}

// forgedPart opens a PR that copies the lander's marker with a higher index, as
// anyone could from a fork of a public repo. head must exist on origin so a
// fetch of it succeeds.
func forgedPart(t *testing.T, fx *fixture, head string, cross bool) int {
	t.Helper()
	body := prBy(t, fx, 2).Body
	forged := strings.Replace(body, `"index":3`, `"index":99`, 1)
	if forged == body {
		t.Fatalf("test setup: could not find the marker index in\n%s", body)
	}
	p, err := fx.f.CreatePR(head, fx.s.Branches[1].Name, "Fix a typo", forged, false)
	if err != nil {
		t.Fatal(err)
	}
	fx.f.PRs[p.Number].CrossRepo = cross
	return p.Number
}

// A stranger's PR must never be queued, retargeted or landed on behalf of a stack
// just because its body carries a copied marker.
func TestForgedMarkerCannotTakeOverALanding(t *testing.T) {
	cases := map[string]struct {
		head  string
		cross bool
	}{
		"from a fork, named like the real lander": {head: "LANDER", cross: true},
		"same repo, off the stack branch prefix":  {head: "feature/evil", cross: false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			fx := setupWith(t, testrepo.FlagApp(t, "if"), shopFlags, stack.Atomic)
			testrepo.Must(t, fx.bare, "branch", "feature/evil", "main")
			real := prBy(t, fx, 2)
			head := c.head
			if head == "LANDER" {
				head = fx.s.Branches[2].Name
			}
			forged := forgedPart(t, fx, head, c.cross)
			// No reviewer in this repo, like the shop sandbox.
			run := &land.Runner{Repo: fx.dev, Forge: fx.f, Remote: "origin", AllowUnreviewed: true}
			if err := run.AutoLand(); err != nil {
				t.Fatalf("a forged PR must be ignored, not break the real stack: %v", err)
			}
			for _, n := range append(fx.f.Merges, fx.f.Enqueued...) {
				if n == forged {
					t.Fatalf("the forged PR #%d was merged or queued", forged)
				}
			}
			if len(fx.f.Merges) != 1 || fx.f.Merges[0] != real.Number {
				t.Fatalf("the real lander #%d should be merged, got %v", real.Number, fx.f.Merges)
			}
			if len(fx.f.Comments[forged]) != 0 || len(fx.f.Labels[forged]) != 0 {
				t.Fatalf("the forged PR must not be commented on or labeled: %q %v", fx.f.Comments[forged], fx.f.Labels[forged])
			}
		})
	}
}

// After a part lands, GitHub rebases the next one onto trunk on the server.
// A developer's sync must take that version, and later local work must push.
func TestDevPicksUpGitHubsRestackThenKeepsWorking(t *testing.T) {
	fx := setup(t)
	if err := (&land.Runner{Repo: fx.dev, Forge: fx.f, Remote: "origin"}).Land(prBy(t, fx, 0).Number); err != nil {
		t.Fatal(err)
	}
	below := prBy(t, fx, 0).HeadSHA
	fx.landQueue(t, "rebase")
	b := fx.s.Branches[1]
	gh := testrepo.Clone(t, fx.bare) // GitHub's server-side rebase of the stack
	testrepo.Must(t, gh, "checkout", "-q", "-B", b.Name, "origin/"+b.Name)
	testrepo.Must(t, gh, "rebase", "-q", "--onto", "origin/main", below)
	testrepo.Must(t, gh, "push", "-q", "-f", "origin", b.Name)

	if err := stack.Sync(fx.dev, fx.s, stack.SyncOptions{Fetch: true}); err != nil {
		t.Fatal(err)
	}
	remote := testrepo.Must(t, fx.bare, "rev-parse", "refs/heads/"+b.Name)
	if local, _ := fx.dev.RevParse(b.Name); local != remote {
		t.Fatal("local branch did not take GitHub's restack")
	}
	testrepo.Must(t, fx.dev, "checkout", "-q", b.Name)
	testrepo.Commit(t, fx.dev, "follow-up", map[string]string{"internal/types/extra.go": "package types\n"})
	testrepo.Must(t, fx.dev, "checkout", "-q", "feature")
	if err := stack.Sync(fx.dev, fx.s, stack.SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := stack.Push(fx.dev, fx.f, fx.s, stack.PushOptions{}); err != nil {
		t.Fatalf("a follow-up push after GitHub's restack must work: %v", err)
	}
}

// With the built-in token no "PR merged" run starts, so the next tick's
// FinishLanded must land the next part of an incremental stack itself.
func TestATickLandsTheNextPartOfAnIncrementalStack(t *testing.T) {
	fx := setup(t)
	if err := (&land.Runner{Repo: fx.dev, Forge: fx.f, Remote: "origin"}).Land(prBy(t, fx, 0).Number); err != nil {
		t.Fatal(err)
	}
	fx.landQueue(t, "rebase")
	if err := fx.ci(t).FinishLanded(); err != nil {
		t.Fatal(err)
	}
	if len(fx.f.Merges) != 2 || fx.f.Merges[1] != prBy(t, fx, 1).Number {
		t.Fatalf("the tick should land part 2: %v", fx.f.Merges)
	}
	if err := fx.ci(t).FinishLanded(); err != nil || len(fx.f.Merges) != 2 {
		t.Fatalf("and only once: %v %v", err, fx.f.Merges)
	}
}
