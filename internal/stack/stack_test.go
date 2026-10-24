package stack_test

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"stackq/internal/forge"
	"stackq/internal/git"
	"stackq/internal/split"
	"stackq/internal/stack"
	"stackq/internal/testrepo"
)

func makeStack(t *testing.T, r *git.Repo, remote, check string) *stack.Stack {
	t.Helper()
	base := "main"
	if remote != "" {
		base = remote + "/main"
	}
	p, err := split.Make(r, split.Options{Base: base, Head: "feature", MaxLines: 1})
	if err != nil {
		t.Fatal(err)
	}
	s, err := stack.Create(r, p, stack.CreateOptions{
		Name: "feat", Prefix: "stack/feat/", Trunk: "main", Remote: remote, Source: "feature", Check: check, MergeOnFail: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestCreateEveryBranchBuildsAndTopMatches(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH")
	}
	r := testrepo.GoApp(t)
	var log bytes.Buffer
	p, err := split.Make(r, split.Options{Base: "main", Head: "feature", MaxLines: 1})
	if err != nil {
		t.Fatal(err)
	}
	s, err := stack.Create(r, p, stack.CreateOptions{
		Name: "feat", Prefix: "stack/feat/", Trunk: "main", Source: "feature",
		Check: "go build ./...", MergeOnFail: true, Log: &log,
	})
	if err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	if len(s.Branches) != 6 || strings.Contains(log.String(), "✗") {
		t.Fatalf("expected 6 clean slices, got %d:\n%s", len(s.Branches), log.String())
	}
	top := s.Branches[len(s.Branches)-1].Name
	if !r.TreesEqual(top, "feature") {
		t.Fatal("top of stack differs from feature")
	}
	for i, b := range s.Branches {
		want := "main"
		if i > 0 {
			want = s.Branches[i-1].Name
		}
		if b.Parent != want || r.CountCommits(want, b.Name) != 1 {
			t.Fatalf("%s: parent %s, %d commits", b.Name, b.Parent, r.CountCommits(want, b.Name))
		}
	}
	if r.CurrentBranch() != "feature" || !r.Clean() {
		t.Fatal("split touched the user's working tree")
	}
}

func TestCreateFoldsSliceThatFailsCheck(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH")
	}
	r := testrepo.GoApp(t)
	p, err := split.Make(r, split.Options{Base: "main", Head: "feature", MaxLines: 1})
	if err != nil {
		t.Fatal(err)
	}
	// Sabotage the order: put the entrypoint before the packages it needs.
	var cmd, rest []split.Slice
	for _, s := range p.Slices {
		if s.Name == "cmd/app" {
			cmd = append(cmd, s)
		} else {
			rest = append(rest, s)
		}
	}
	p.Slices = append(cmd, rest...)
	var log bytes.Buffer
	s, err := stack.Create(r, p, stack.CreateOptions{
		Name: "feat", Prefix: "stack/feat/", Trunk: "main", Source: "feature",
		Check: "go build ./...", MergeOnFail: true, Log: &log,
	})
	if err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	if !strings.Contains(log.String(), "folding") || len(s.Branches) >= 6 {
		t.Fatalf("expected the broken slice to be folded:\n%s", log.String())
	}
	if !r.TreesEqual(s.Branches[len(s.Branches)-1].Name, "feature") {
		t.Fatal("top of stack differs from feature")
	}
}

func TestCreateRefusesToClobber(t *testing.T) {
	r := testrepo.GoApp(t)
	makeStack(t, r, "", "")
	p, _ := split.Make(r, split.Options{Base: "main", Head: "feature"})
	_, err := stack.Create(r, p, stack.CreateOptions{Name: "feat", Prefix: "stack/feat/", Trunk: "main"})
	if err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("expected a clobber error, got %v", err)
	}
}

func TestSyncAfterEditingLowerBranch(t *testing.T) {
	r := testrepo.GoApp(t)
	s := makeStack(t, r, "", "")
	b1, b2 := s.Branches[1], s.Branches[2]
	ownFiles := testrepo.Must(t, r, "diff", "--name-only", b1.Name, b2.Name)

	// Address review feedback on slice 2 (types).
	testrepo.Must(t, r, "checkout", "-q", b1.Name)
	testrepo.Commit(t, r, "review fix", map[string]string{"internal/types/types.go": "package types\n\n// ID identifies things.\ntype ID string\n"})
	testrepo.Must(t, r, "checkout", "-q", "feature")

	s, _ = stack.Load(r, "feat")
	if err := stack.Sync(r, s, stack.SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	for i := 2; i < len(s.Branches); i++ {
		if !r.IsAncestor(s.Branches[i-1].Name, s.Branches[i].Name) {
			t.Fatalf("%s is not stacked on %s after sync", s.Branches[i].Name, s.Branches[i-1].Name)
		}
	}
	if got := testrepo.Must(t, r, "diff", "--name-only", b1.Name, b2.Name); got != ownFiles {
		t.Fatalf("slice 3 picked up foreign changes: %q vs %q", got, ownFiles)
	}
	top := s.Branches[len(s.Branches)-1].Name
	if out := testrepo.Must(t, r, "show", top+":internal/types/types.go"); !strings.Contains(out, "identifies") {
		t.Fatal("fix did not propagate to the top")
	}
}

func TestPushThenSyncAfterRebaseMerge(t *testing.T) {
	r := testrepo.GoApp(t)
	bare := testrepo.WithOrigin(t, r)
	s := makeStack(t, r, "origin", "")
	f := forge.NewFake()
	f.HeadSHA = func(b string) string { out, _ := bare.Run("rev-parse", "refs/heads/"+b); return out }
	if err := stack.Push(r, f, s, stack.PushOptions{}); err != nil {
		t.Fatal(err)
	}
	for i, b := range s.Branches {
		pr, _ := f.GetPR(b.PR)
		m, ok := stack.ParseMarker(pr.Body)
		if !ok || m.Index != i+1 || pr.BaseRef != b.Parent || !strings.Contains(pr.Body, "👈 this PR") {
			t.Fatalf("PR for %s is wrong: base=%s marker=%+v", b.Name, pr.BaseRef, m)
		}
	}

	// Trunk moves, then the queue rebase-merges the bottom PR.
	other := testrepo.Clone(t, bare)
	testrepo.Commit(t, other, "unrelated", map[string]string{"NOTES.txt": "hi\n"})
	testrepo.Must(t, other, "push", "-q", "origin", "main")
	testrepo.RebaseMerge(t, bare, "origin/main~1", s.Branches[0].Name, true)

	if err := stack.Sync(r, s, stack.SyncOptions{Fetch: true}); err != nil {
		t.Fatal(err)
	}
	if !s.Branches[0].Merged || s.Branches[1].Parent != "main" {
		t.Fatalf("bottom not detected as merged: %+v", s.Branches[:2])
	}
	if n := len(testrepo.Lines(testrepo.Must(t, r, "cherry", "origin/main", s.Branches[1].Name))); n != 1 {
		t.Fatalf("slice 2 should carry exactly its own commit over trunk, has %d", n)
	}
	if err := stack.Push(r, f, s, stack.PushOptions{}); err != nil {
		t.Fatal(err)
	}
	pr, _ := f.GetPR(s.Branches[1].PR)
	if pr.BaseRef != "main" || !strings.Contains(pr.Body, "✅") {
		t.Fatalf("PR 2 not retargeted/refreshed: base=%s", pr.BaseRef)
	}
}

func TestMarkerRoundTripKeepsAuthorText(t *testing.T) {
	m := stack.Marker{Stack: "s", Index: 2, Total: 3, Parent: "p", Trunk: "main"}
	body := "My description\n\nmore words"
	b1 := stack.MergeBody(body, stack.RenderBlock(m, nil))
	m.Parent = "main"
	b2 := stack.MergeBody(b1, stack.RenderBlock(m, nil))
	got, ok := stack.ParseMarker(b2)
	if !ok || got != m || !strings.HasPrefix(b2, body) || strings.Count(b2, "stackq:begin") != 1 {
		t.Fatalf("bad body:\n%s", b2)
	}
}

func TestPushLinksAndClosesOriginalPR(t *testing.T) {
	r := testrepo.GoApp(t)
	bare := testrepo.WithOrigin(t, r)
	s := makeStack(t, r, "origin", "")
	f := forge.NewFake()
	f.HeadSHA = func(b string) string {
		out, _ := bare.Run("rev-parse", "--verify", "--quiet", "refs/heads/"+b)
		return out
	}
	orig, _ := f.CreatePR("feature", "main", "Big refactor", "Why: retries.\n\nHow: lots of files.", false)

	// No flag: the original PR is found from the source branch.
	if err := stack.Push(r, f, s, stack.PushOptions{CloseSource: true}); err != nil {
		t.Fatal(err)
	}
	if s.SourcePR != orig.Number {
		t.Fatalf("source PR not detected: %d", s.SourcePR)
	}
	bottom, _ := f.GetPR(s.Branches[0].PR)
	if !strings.HasPrefix(bottom.Body, "_Split from #1: Big refactor_") || !strings.Contains(bottom.Body, "Why: retries.") {
		t.Fatalf("bottom PR did not get the original description:\n%s", bottom.Body)
	}
	for _, b := range s.Branches[1:] {
		pr, _ := f.GetPR(b.PR)
		m, _ := stack.ParseMarker(pr.Body)
		if m.Source != orig.Number || !strings.Contains(pr.Body, "Split from #1.") || strings.Contains(pr.Body, "Why: retries.") {
			t.Fatalf("PR #%d body wrong:\n%s", b.PR, pr.Body)
		}
	}
	if len(f.Comments[orig.Number]) != 1 {
		t.Fatalf("want one link comment on the original, got %d", len(f.Comments[orig.Number]))
	}
	c := f.Comments[orig.Number][0]
	for _, b := range s.Branches {
		if !strings.Contains(c, fmt.Sprintf("#%d", b.PR)) {
			t.Fatalf("comment misses #%d:\n%s", b.PR, c)
		}
	}
	if o, _ := f.GetPR(orig.Number); o.State != "CLOSED" {
		t.Fatal("original PR not closed")
	}

	// Pushing again does not duplicate anything.
	if err := stack.Push(r, f, s, stack.PushOptions{CloseSource: true}); err != nil {
		t.Fatal(err)
	}
	again, _ := f.GetPR(s.Branches[0].PR)
	if len(f.Comments[orig.Number]) != 1 || again.Body != bottom.Body {
		t.Fatal("second push duplicated the comment or the description")
	}
}

func TestPushWithoutSourcePR(t *testing.T) {
	r := testrepo.GoApp(t)
	bare := testrepo.WithOrigin(t, r)
	s := makeStack(t, r, "origin", "")
	f := forge.NewFake()
	f.HeadSHA = func(b string) string {
		out, _ := bare.Run("rev-parse", "--verify", "--quiet", "refs/heads/"+b)
		return out
	}
	orig, _ := f.CreatePR("feature", "main", "Big refactor", "desc", false)
	if err := stack.Push(r, f, s, stack.PushOptions{NoSourcePR: true}); err != nil {
		t.Fatal(err)
	}
	bottom, _ := f.GetPR(s.Branches[0].PR)
	if s.SourcePR != 0 || len(f.Comments[orig.Number]) != 0 || strings.Contains(bottom.Body, "Split from") {
		t.Fatal("--no-source-pr still linked the original")
	}
	if o, _ := f.GetPR(orig.Number); o.State != "OPEN" {
		t.Fatal("original PR touched")
	}
}

func TestFlagStackEveryPartBuildsAndActivationIsDraft(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH")
	}
	r := testrepo.FlagApp(t, "return")
	bare := testrepo.WithOrigin(t, r)
	p, err := split.Make(r, split.Options{Base: "origin/main", Head: "feature", MaxLines: 1, Flags: &split.Flags{
		Paths: []string{"internal/flags/"}, Activation: []string{"config/prod.yaml"},
		Guards: []string{"flags.Enabled"}, Entrypoints: []string{"HandleFunc"}, Enforce: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	s, err := stack.Create(r, p, stack.CreateOptions{Name: "shop", Prefix: "stack/shop/", Trunk: "main", Remote: "origin",
		Source: "feature", Landing: stack.Atomic, Check: "go build ./...", Log: &log})
	if err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	if strings.Contains(log.String(), "✗") || !s.Branches[len(s.Branches)-1].Hold {
		t.Fatalf("every part should build and the last should be held:\n%s", log.String())
	}
	// With the flag slice alone on trunk, prod config is untouched.
	if out := testrepo.Must(t, r, "show", s.Branches[0].Name+":config/prod.yaml"); out != "features: {}" {
		t.Fatalf("flag slice changed prod config: %q", out)
	}
	f := forge.NewFake()
	f.HeadSHA = func(b string) string {
		out, _ := bare.Run("rev-parse", "--verify", "--quiet", "refs/heads/"+b)
		return out
	}
	if err := stack.Push(r, f, s, stack.PushOptions{}); err != nil {
		t.Fatal(err)
	}
	h, _ := f.GetPR(s.Branches[len(s.Branches)-1].PR)
	if !strings.HasPrefix(h.Title, "[shop activation]") {
		t.Fatalf("activation PR title: %q", h.Title)
	}
	if m, _ := stack.ParseMarker(h.Body); !m.Hold || !m.Atomic() {
		t.Fatalf("activation marker: %+v", m)
	}
}

// Moving a package to another directory used to glue the move and all its
// importers into one slice. Now each step builds on its own.
func TestCreateMovedPackageEveryBranchBuilds(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH")
	}
	r := testrepo.New(t)
	testrepo.Commit(t, r, "base", map[string]string{
		"go.mod":           "module example.com/m\n\ngo 1.22\n",
		"oldpkg/x.go":      "package oldpkg\n\n// Value is shared by many callers in the code base.\nfunc Value() int { return 42 }\n",
		"caller/caller.go": "package caller\n\nimport \"example.com/m/oldpkg\"\n\nfunc V() int { return oldpkg.Value() }\n",
		"other/other.go":   "package other\n\nimport \"example.com/m/oldpkg\"\n\nfunc W() int { return oldpkg.Value() }\n",
	})
	testrepo.Must(t, r, "checkout", "-q", "-b", "feature")
	testrepo.Must(t, r, "mv", "oldpkg", "newpkg")
	testrepo.Commit(t, r, "move", map[string]string{
		"newpkg/x.go":      "package newpkg\n\n// Value is shared by many callers in the code base.\nfunc Value() int { return 42 }\n",
		"caller/caller.go": "package caller\n\nimport \"example.com/m/newpkg\"\n\nfunc V() int { return newpkg.Value() }\n",
		"other/other.go":   "package other\n\nimport \"example.com/m/newpkg\"\n\nfunc W() int { return newpkg.Value() }\n",
	})
	var log bytes.Buffer
	p, err := split.Make(r, split.Options{Base: "main", Head: "feature", MaxLines: 1})
	if err != nil {
		t.Fatal(err)
	}
	s, err := stack.Create(r, p, stack.CreateOptions{
		Name: "mv", Prefix: "stack/mv/", Trunk: "main", Source: "feature",
		Check: "go build ./...", Log: &log,
	})
	if err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	if len(s.Branches) < 3 || strings.Contains(log.String(), "✗") {
		t.Fatalf("want the move spread over several clean slices, got %d:\n%s", len(s.Branches), log.String())
	}
	if !r.TreesEqual(s.Branches[len(s.Branches)-1].Name, "feature") {
		t.Fatal("top of stack differs from feature")
	}
}
