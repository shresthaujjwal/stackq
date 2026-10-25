package land_test

import (
	"errors"
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

// queueLands simulates the merge queue merging PR n with method
// (rebase, merge or squash), deleting its branch, and recording the merge
// commit, then runs the CI handler in a fresh clone.
func (fx *fixture) queueLands(t *testing.T, n int, method string) error {
	t.Helper()
	fx.publishPullRefs(t)
	pr, _ := fx.f.GetPR(n)
	ci := testrepo.Clone(t, fx.bare)
	base := testrepo.Must(t, ci, "merge-base", "origin/main", "origin/"+pr.HeadRef)
	fx.f.MarkMerged(n)
	switch method {
	case "rebase":
		testrepo.RebaseMerge(t, fx.bare, base, pr.HeadRef, true)
	case "merge":
		testrepo.MergeCommitMerge(t, fx.bare, pr.HeadRef, true)
	case "squash":
		testrepo.SquashMerge(t, fx.bare, pr.HeadRef, true)
	}
	tip := fx.mainTip(t)
	fx.f.SetDetails(n, func(d *forge.Details) { d.MergeCommit = tip })
	run := &land.Runner{Repo: testrepo.Clone(t, fx.bare), Forge: fx.f, Remote: "origin", Enqueue: true}
	return run.AfterMerge(n)
}

// queueMerges simulates the merge queue landing PR i (0-based) with the
// rebase method and auto-deleting its branch, then runs the CI handler in a
// fresh clone, exactly as the workflow would.
func (fx *fixture) queueMerges(t *testing.T, i int) error {
	t.Helper()
	b := fx.s.Branches[i]
	pr, _ := fx.f.GetPR(b.PR)
	// the PR targets trunk by now, so its own commits are trunk-base..head
	ci := testrepo.Clone(t, fx.bare)
	base := testrepo.Must(t, ci, "merge-base", "origin/main", "origin/"+b.Name)
	fx.f.MarkMerged(pr.Number) // freezes HeadSHA before the branch is deleted
	testrepo.RebaseMerge(t, fx.bare, base, b.Name, true)
	run := &land.Runner{Repo: testrepo.Clone(t, fx.bare), Forge: fx.f, Remote: "origin", Enqueue: true}
	return run.AfterMerge(pr.Number)
}

func TestStackLandsOneByOneThroughQueue(t *testing.T) {
	fx := setup(t)
	n := len(fx.s.Branches)

	run := &land.Runner{Repo: fx.dev, Forge: fx.f, Remote: "origin"}
	if err := run.Land(fx.s.Branches[1].PR); err == nil {
		t.Fatal("landing a PR that targets another branch must fail")
	}
	if err := run.Land(fx.s.Branches[0].PR); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < n; i++ {
		if err := fx.queueMerges(t, i); err != nil {
			t.Fatalf("after merging %d: %v", i+1, err)
		}
		if i == n-1 {
			break
		}
		next := fx.s.Branches[i+1]
		pr, _ := fx.f.GetPR(next.PR)
		m, _ := stack.ParseMarker(pr.Body)
		if pr.BaseRef != "main" || m.Parent != "main" {
			t.Fatalf("#%d not retargeted: base=%s parent=%s", pr.Number, pr.BaseRef, m.Parent)
		}
		if got := fx.f.Enqueued[len(fx.f.Enqueued)-1]; got != pr.Number {
			t.Fatalf("expected #%d queued, last queued #%d", pr.Number, got)
		}
		tip := testrepo.Must(t, fx.bare, "rev-parse", "refs/heads/main")
		if mb := testrepo.Must(t, fx.bare, "merge-base", "refs/heads/main", "refs/heads/"+next.Name); mb != tip {
			t.Fatalf("%s is not on top of main", next.Name)
		}
		if c := testrepo.Must(t, fx.bare, "rev-list", "--count", "refs/heads/main..refs/heads/"+next.Name); c != "1" {
			t.Fatalf("%s carries %s commits over main, want only its own", next.Name, c)
		}
		if !strings.Contains(pr.Body, "✅") {
			t.Fatalf("nav not refreshed on #%d", pr.Number)
		}
	}

	// Everything landed; main now equals the original big branch.
	testrepo.Must(t, fx.dev, "fetch", "-q", "origin")
	if !fx.dev.TreesEqual("feature", "origin/main") {
		t.Fatal("main differs from the original branch after landing the whole stack")
	}

	// The developer's local stack catches up cleanly.
	if err := stack.Sync(fx.dev, fx.s, stack.SyncOptions{Fetch: true}); err != nil {
		t.Fatal(err)
	}
	if len(fx.s.Active()) != 0 {
		t.Fatalf("expected the whole stack to be marked landed, %d left", len(fx.s.Active()))
	}
}

func TestDevPicksUpCIRestackThenKeepsWorking(t *testing.T) {
	fx := setup(t)
	if err := fx.queueMerges(t, 0); err != nil {
		t.Fatal(err)
	}
	if err := stack.Sync(fx.dev, fx.s, stack.SyncOptions{Fetch: true}); err != nil {
		t.Fatal(err)
	}
	b := fx.s.Branches[1]
	remote := testrepo.Must(t, fx.bare, "rev-parse", "refs/heads/"+b.Name)
	if local, _ := fx.dev.RevParse(b.Name); local != remote {
		t.Fatal("local branch did not take CI's restack")
	}
	// A follow-up push after more local work must not be rejected.
	testrepo.Must(t, fx.dev, "checkout", "-q", b.Name)
	testrepo.Commit(t, fx.dev, "follow-up", map[string]string{"internal/types/extra.go": "package types\n"})
	testrepo.Must(t, fx.dev, "checkout", "-q", "feature")
	if err := stack.Sync(fx.dev, fx.s, stack.SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := stack.Push(fx.dev, fx.f, fx.s, stack.PushOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestConflictPausesStack(t *testing.T) {
	fx := setup(t)
	// Someone changes the same file slice 2 touches, on trunk.
	other := testrepo.Clone(t, fx.bare)
	testrepo.Commit(t, other, "conflicting", map[string]string{"internal/types/types.go": "package types\n\ntype ID int\n"})
	testrepo.Must(t, other, "push", "-q", "origin", "main")

	child := fx.s.Branches[1]
	before := testrepo.Must(t, fx.bare, "rev-parse", "refs/heads/"+child.Name)
	err := fx.queueMerges(t, 0)
	if err == nil || !strings.Contains(err.Error(), "paused") {
		t.Fatalf("expected the stack to pause, got %v", err)
	}
	if !fx.f.Labels[child.PR][land.LabelConflict] || len(fx.f.Comments[child.PR]) == 0 ||
		!strings.Contains(fx.f.Comments[child.PR][0], "internal/types/types.go") {
		t.Fatalf("conflict not reported on #%d: %v", child.PR, fx.f.Comments)
	}
	if after := testrepo.Must(t, fx.bare, "rev-parse", "refs/heads/"+child.Name); after != before {
		t.Fatal("a conflicted branch must not be pushed")
	}
	for _, q := range fx.f.Enqueued {
		if q == child.PR {
			t.Fatal("a conflicted PR must not be queued")
		}
	}
}

func TestEnqueueFailureIsReported(t *testing.T) {
	fx := setup(t)
	fx.f.EnqueueErr = errors.New("required checks are failing")
	if err := fx.queueMerges(t, 0); err == nil {
		t.Fatal("expected an error")
	}
	child := fx.s.Branches[1].PR
	if !fx.f.Labels[child][land.LabelBlocked] {
		t.Fatal("blocked label missing")
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
	fx.f.EnqueueErr = nil
	if err := run.Land(pr); err != nil || fx.f.Labels[pr][land.LabelBlocked] {
		t.Fatalf("retry should requeue and clear the label: %v", err)
	}
}

func TestIgnoresForeignPRs(t *testing.T) {
	fx := setup(t)
	pr, _ := fx.f.CreatePR("someone/else", "main", "unrelated", "no marker here", false)
	fx.f.MarkMerged(pr.Number)
	run := &land.Runner{Repo: fx.dev, Forge: fx.f, Remote: "origin", Enqueue: true}
	if err := run.AfterMerge(pr.Number); err != nil || len(fx.f.Enqueued) != 0 {
		t.Fatalf("foreign PR should be a no-op: %v %v", err, fx.f.Enqueued)
	}
}

func TestOriginalPRHearsWhenStackLands(t *testing.T) {
	fx := setup(t)
	orig, _ := fx.f.CreatePR("feature", "main", "Big refactor", "desc", false)
	if err := stack.Push(fx.dev, fx.f, fx.s, stack.PushOptions{SourcePR: orig.Number, CloseSource: true}); err != nil {
		t.Fatal(err)
	}
	for i := range fx.s.Branches {
		if err := fx.queueMerges(t, i); err != nil {
			t.Fatal(err)
		}
		if i < len(fx.s.Branches)-1 && len(fx.f.Comments[orig.Number]) != 1 {
			t.Fatalf("landed note posted early after #%d", i+1)
		}
	}
	cs := fx.f.Comments[orig.Number]
	if len(cs) != 2 || !strings.Contains(cs[1], "has landed on `main`") {
		t.Fatalf("want link comment + landed note, got %q", cs)
	}
}

func prBy(t *testing.T, fx *fixture, i int) forge.PR {
	t.Helper()
	pr, err := fx.f.GetPR(fx.s.Branches[i].PR)
	if err != nil {
		t.Fatal(err)
	}
	return pr
}

func TestAtomicStackLandsAsOneEntryAndActivationWaits(t *testing.T) {
	fx := setupWith(t, testrepo.FlagApp(t, "if"), shopFlags, stack.Atomic)
	// flags, internal/checkout, cmd/shop, activation (held)
	if n := len(fx.s.Branches); n != 4 || !fx.s.Branches[3].Hold || fx.s.Branches[0].Name != "stack/feat/01-flags" {
		t.Fatalf("unexpected stack: %+v", fx.s.Branches)
	}
	lander, hold := prBy(t, fx, 2), prBy(t, fx, 3)
	if !strings.Contains(lander.Body, "(lander)") || !strings.Contains(hold.Body, "Activation PR") {
		t.Fatalf("bodies don't explain the landing:\n%s\n---\n%s", lander.Body, hold.Body)
	}
	run := &land.Runner{Repo: fx.dev, Forge: fx.f, Remote: "origin"}

	// Gate: part 2 is not approved yet.
	fx.f.SetDetails(prBy(t, fx, 1).Number, func(d *forge.Details) { d.Approvals = nil })
	err := run.Land(prBy(t, fx, 0).Number)
	if err == nil || !strings.Contains(err.Error(), "required approvals") || len(fx.f.Enqueued) != 0 {
		t.Fatalf("unapproved part must block landing: %v %v", err, fx.f.Enqueued)
	}
	if !fx.f.Labels[lander.Number][land.LabelBlocked] {
		t.Fatal("blocked label missing on the lander")
	}
	fx.f.SetDetails(prBy(t, fx, 1).Number, func(d *forge.Details) { d.Approvals = []string{"ana"} })

	before := fx.mainTip(t)
	if err := run.Land(prBy(t, fx, 0).Number); err != nil {
		t.Fatal(err)
	}
	if lp := prBy(t, fx, 2); lp.BaseRef != "main" || len(fx.f.Enqueued) != 1 || fx.f.Enqueued[0] != lp.Number {
		t.Fatalf("lander not retargeted and queued alone: base=%s queued=%v", lp.BaseRef, fx.f.Enqueued)
	}

	// The queue merges the lander: trunk gets every part at once.
	if err := fx.queueLands(t, lander.Number, "rebase"); err != nil {
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
	for i := 0; i < 2; i++ {
		p := prBy(t, fx, i)
		if p.State != "CLOSED" || !strings.Contains(fx.f.Comments[p.Number][len(fx.f.Comments[p.Number])-1], "Landed on `main`") {
			t.Fatalf("part %d not closed as landed: %s", i+1, p.State)
		}
		if ci.RefExists("origin/" + p.HeadRef) {
			t.Fatalf("branch %s not cleaned up", p.HeadRef)
		}
	}
	h := prBy(t, fx, 3)
	if h.BaseRef != "main" || !fx.f.Labels[h.Number][land.LabelActivation] || len(fx.f.Enqueued) != 1 {
		t.Fatalf("activation PR: base=%s labels=%v queued=%v", h.BaseRef, fx.f.Labels[h.Number], fx.f.Enqueued)
	}
	if c := testrepo.Must(t, fx.bare, "rev-list", "--count", "refs/heads/main..refs/heads/"+h.HeadRef); c != "1" {
		t.Fatalf("activation branch carries %s commits over main", c)
	}
	if lc := fx.f.Comments[lander.Number]; !strings.Contains(lc[len(lc)-1], "landed atomically") ||
		!strings.Contains(lc[len(lc)-1], "stackq revert --pr") {
		t.Fatalf("landing record missing: %q", lc)
	}
	if !strings.Contains(prBy(t, fx, 3).Body, "✅") {
		t.Fatal("nav not refreshed on the activation PR")
	}

	// One command rolls the whole stack back.
	rev, err := (&land.Runner{Repo: testrepo.Clone(t, fx.bare), Forge: fx.f, Remote: "origin"}).Revert(lander.Number, land.RevertOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ci = testrepo.Clone(t, fx.bare)
	if !ci.TreesEqual("origin/"+rev.HeadRef, before) || rev.BaseRef != "main" || !strings.Contains(rev.Body, "| rebase |") ||
		!strings.Contains(rev.Body, "✅ The revert touches exactly") {
		t.Fatalf("revert PR wrong: base=%s\n%s", rev.BaseRef, rev.Body)
	}
}

func TestAtomicRevertMergeAndSquashMethods(t *testing.T) {
	for _, method := range []string{"merge", "squash"} {
		t.Run(method, func(t *testing.T) {
			fx := setupWith(t, testrepo.GoApp(t), nil, stack.Atomic)
			run := &land.Runner{Repo: fx.dev, Forge: fx.f, Remote: "origin"}
			if err := run.Land(prBy(t, fx, 0).Number); err != nil {
				t.Fatal(err)
			}
			before := fx.mainTip(t)
			top := prBy(t, fx, len(fx.s.Branches)-1)
			if err := fx.queueLands(t, top.Number, method); err != nil {
				t.Fatal(err)
			}
			// Someone else lands an unrelated change afterwards.
			other := testrepo.Clone(t, fx.bare)
			testrepo.Commit(t, other, "unrelated", map[string]string{"NOTES.txt": "later\n"})
			testrepo.Must(t, other, "push", "-q", "origin", "main")

			rev, err := (&land.Runner{Repo: testrepo.Clone(t, fx.bare), Forge: fx.f, Remote: "origin"}).Revert(top.Number, land.RevertOptions{Enqueue: true})
			if err != nil {
				t.Fatal(err)
			}
			ci := testrepo.Clone(t, fx.bare)
			// The revert undoes the stack and keeps the later change.
			if d := ci.ChangedFiles(before, "origin/"+rev.HeadRef); len(d) != 1 || d[0] != "NOTES.txt" {
				t.Fatalf("after revert the only difference from pre-stack main should be NOTES.txt, got %v", d)
			}
			if !strings.Contains(rev.Body, "| "+method+" |") || fx.f.Enqueued[len(fx.f.Enqueued)-1] != rev.Number {
				t.Fatalf("revert PR not described or not queued:\n%s", rev.Body)
			}
		})
	}
}

func TestAtomicPartMergedAloneIsNotQueuedOn(t *testing.T) {
	fx := setupWith(t, testrepo.GoApp(t), nil, stack.Atomic)
	first := prBy(t, fx, 0)
	if err := fx.queueLands(t, first.Number, "rebase"); err != nil {
		t.Fatal(err)
	}
	second := prBy(t, fx, 1)
	if second.BaseRef != "main" || len(fx.f.Enqueued) != 0 {
		t.Fatalf("restack expected without queueing: base=%s queued=%v", second.BaseRef, fx.f.Enqueued)
	}
	if cs := fx.f.Comments[second.Number]; len(cs) == 0 || !strings.Contains(cs[0], "merged on its own") {
		t.Fatalf("no warning on #%d: %q", second.Number, cs)
	}
	// The remainder still lands together.
	if err := (&land.Runner{Repo: fx.dev, Forge: fx.f, Remote: "origin"}).Land(second.Number); err != nil {
		t.Fatal(err)
	}
	if top := prBy(t, fx, len(fx.s.Branches)-1); len(fx.f.Enqueued) != 1 || fx.f.Enqueued[0] != top.Number {
		t.Fatalf("remainder not landed via the top PR: %v", fx.f.Enqueued)
	}
}

func TestIncrementalStackLeavesActivationForHumans(t *testing.T) {
	fx := setupWith(t, testrepo.FlagApp(t, "if"), shopFlags, "")
	orig, _ := fx.f.CreatePR("feature", "main", "New checkout", "desc", false)
	if err := stack.Push(fx.dev, fx.f, fx.s, stack.PushOptions{SourcePR: orig.Number}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := fx.queueLands(t, prBy(t, fx, i).Number, "rebase"); err != nil {
			t.Fatal(err)
		}
	}
	h := prBy(t, fx, 3)
	for _, q := range fx.f.Enqueued {
		if q == h.Number {
			t.Fatal("activation PR was queued automatically")
		}
	}
	if h.BaseRef != "main" || !fx.f.Labels[h.Number][land.LabelActivation] {
		t.Fatal("activation PR not restacked onto main")
	}
	cs := fx.f.Comments[orig.Number]
	if !strings.Contains(cs[len(cs)-1], "activation PR") {
		t.Fatalf("original PR not told about the waiting activation: %q", cs)
	}
}

func TestAutoLandWaitsQuietlyThenLandsOnce(t *testing.T) {
	fx := setupWith(t, testrepo.FlagApp(t, "if"), shopFlags, stack.Atomic)
	lander, part2 := prBy(t, fx, 2), prBy(t, fx, 1)
	run := &land.Runner{Repo: fx.dev, Forge: fx.f, Remote: "origin"}

	quiet := func(why string) {
		t.Helper()
		if err := run.AutoLand(); err != nil {
			t.Fatalf("%s: auto-land must not fail while waiting: %v", why, err)
		}
		if len(fx.f.Enqueued) != 0 || prBy(t, fx, 2).BaseRef == "main" {
			t.Fatalf("%s: stack was queued", why)
		}
		if len(fx.f.Comments[lander.Number]) != 0 || fx.f.Labels[lander.Number][land.LabelBlocked] {
			t.Fatalf("%s: waiting must not comment or label: %q %v", why, fx.f.Comments[lander.Number], fx.f.Labels[lander.Number])
		}
	}
	fx.f.SetDetails(part2.Number, func(d *forge.Details) { d.Approvals = nil })
	quiet("missing approval")
	fx.f.SetDetails(part2.Number, func(d *forge.Details) { d.Approvals = []string{"ana"}; d.Checks = "pending" })
	quiet("checks running")
	fx.f.SetDetails(part2.Number, func(d *forge.Details) { d.Checks = "failure" })
	quiet("checks failing")
	fx.f.SetDetails(part2.Number, func(d *forge.Details) { d.Checks = "none" })
	quiet("no checks reported yet")
	fx.f.SetDetails(part2.Number, func(d *forge.Details) { d.Checks = "success" })

	if err := run.AutoLand(); err != nil {
		t.Fatal(err)
	}
	if lp := prBy(t, fx, 2); lp.BaseRef != "main" || len(fx.f.Enqueued) != 1 || fx.f.Enqueued[0] != lp.Number {
		t.Fatalf("ready stack not queued as one entry: base=%s queued=%v", lp.BaseRef, fx.f.Enqueued)
	}
	comments := len(fx.f.Comments[lander.Number])

	// The next approval, CI run or timer tick must not queue it again.
	for i := 0; i < 3; i++ {
		if err := run.AutoLand(); err != nil {
			t.Fatal(err)
		}
	}
	if len(fx.f.Enqueued) != 1 || len(fx.f.Comments[lander.Number]) != comments {
		t.Fatalf("landing repeated: queued=%v comments=%d (was %d)", fx.f.Enqueued, len(fx.f.Comments[lander.Number]), comments)
	}
}

func TestAutoLandIgnoresIncrementalStacksAndForeignPRs(t *testing.T) {
	fx := setup(t) // incremental stack
	fx.f.CreatePR("someone/else", "main", "unrelated", "no marker here", false)
	run := &land.Runner{Repo: fx.dev, Forge: fx.f, Remote: "origin"}
	if err := run.AutoLand(); err != nil || len(fx.f.Enqueued) != 0 {
		t.Fatalf("incremental stacks and plain PRs are not auto-landed: %v %v", err, fx.f.Enqueued)
	}
}

func TestAutoLandWithoutApprovalsWhenAllowed(t *testing.T) {
	fx := setupWith(t, testrepo.FlagApp(t, "if"), shopFlags, stack.Atomic)
	for i := 0; i < 3; i++ {
		fx.f.SetDetails(prBy(t, fx, i).Number, func(d *forge.Details) { d.Approvals = nil })
	}
	strict := &land.Runner{Repo: fx.dev, Forge: fx.f, Remote: "origin"}
	if err := strict.AutoLand(); err != nil || len(fx.f.Enqueued) != 0 {
		t.Fatalf("unreviewed stack must wait by default: %v %v", err, fx.f.Enqueued)
	}
	lax := &land.Runner{Repo: fx.dev, Forge: fx.f, Remote: "origin", AllowUnreviewed: true}
	if err := lax.AutoLand(); err != nil || len(fx.f.Enqueued) != 1 {
		t.Fatalf("allowUnreviewed should land it: %v %v", err, fx.f.Enqueued)
	}
	// Still gated on checks.
	fx2 := setupWith(t, testrepo.FlagApp(t, "if"), shopFlags, stack.Atomic)
	fx2.f.SetDetails(prBy(t, fx2, 1).Number, func(d *forge.Details) { d.Approvals = nil; d.Checks = "failure" })
	lax2 := &land.Runner{Repo: fx2.dev, Forge: fx2.f, Remote: "origin", AllowUnreviewed: true}
	if err := lax2.AutoLand(); err != nil || len(fx2.f.Enqueued) != 0 {
		t.Fatalf("red checks must still block: %v %v", err, fx2.f.Enqueued)
	}
}

func TestManualLandStillAcceptsPartsWithoutChecks(t *testing.T) {
	fx := setupWith(t, testrepo.FlagApp(t, "if"), shopFlags, stack.Atomic)
	for i := 0; i < 3; i++ {
		fx.f.SetDetails(prBy(t, fx, i).Number, func(d *forge.Details) { d.Checks = "none" })
	}
	run := &land.Runner{Repo: fx.dev, Forge: fx.f, Remote: "origin"}
	if err := run.Land(prBy(t, fx, 0).Number); err != nil || len(fx.f.Enqueued) != 1 {
		t.Fatalf("a person running land can accept parts with no CI: %v %v", err, fx.f.Enqueued)
	}
}
