package land_test

import (
	"bytes"
	"strings"
	"testing"

	"stackq/internal/forge"
	"stackq/internal/git"
	"stackq/internal/land"
	"stackq/internal/stack"
	"stackq/internal/testrepo"
)

func TestGCDeletesOnlyWhatIsSafe(t *testing.T) {
	dev := testrepo.GoApp(t)
	bare := testrepo.WithOrigin(t, dev)
	f := forge.NewFake()
	f.HeadSHA = func(b string) string {
		out, _ := bare.Run("rev-parse", "--verify", "--quiet", "refs/heads/"+b)
		return out
	}
	// branch pushes a commit of its own, so every branch has a distinct tip.
	branch := func(name string) {
		t.Helper()
		testrepo.Must(t, dev, "checkout", "-q", "-B", "work", "main")
		testrepo.Commit(t, dev, name, map[string]string{"notes/" + strings.ReplaceAll(name, "/", "_") + ".txt": name})
		testrepo.Must(t, dev, "push", "-q", "origin", "HEAD:refs/heads/"+name)
	}
	pr := func(head, body string) forge.PR {
		t.Helper()
		p, err := f.CreatePR(head, "main", head, body, false)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	marker := func(name string, source int) string {
		return stack.RenderBlock(stack.Marker{Stack: name, Index: 1, Total: 2, Parent: "main", Trunk: "main", Source: source, Landing: stack.Atomic}, nil)
	}

	branch("fix/merged")
	f.MarkMerged(pr("fix/merged", "").Number)

	branch("fix/merged-then-pushed")
	f.MarkMerged(pr("fix/merged-then-pushed", "").Number)
	testrepo.Must(t, dev, "checkout", "-q", "-B", "more", "origin/fix/merged-then-pushed")
	testrepo.Commit(t, dev, "later", map[string]string{"notes/later.txt": "x"})
	testrepo.Must(t, dev, "push", "-q", "origin", "HEAD:refs/heads/fix/merged-then-pushed")

	branch("stack/old/01-flags")
	f.ClosePR(pr("stack/old/01-flags", marker("old", 0)).Number)

	branch("stack/live/01-flags")
	pr("stack/live/01-flags", marker("live", 0))

	// A source whose stack is done, and one whose activation PR is still open.
	branch("feature/done")
	done := pr("feature/done", "")
	f.ClosePR(done.Number)
	branch("stack/done/01-flags")
	f.ClosePR(pr("stack/done/01-flags", marker("done", done.Number)).Number)

	branch("feature/held")
	held := pr("feature/held", "")
	f.ClosePR(held.Number)
	branch("stack/held/02-activation")
	pr("stack/held/02-activation", marker("held", held.Number)) // still open

	branch("wip") // never had a PR
	branch("closed/not-ours")
	f.ClosePR(pr("closed/not-ours", "").Number)

	var log bytes.Buffer
	run := &land.Runner{Repo: testrepo.Clone(t, bare), Forge: f, Remote: "origin", Log: &log}
	want := []string{"feature/done", "fix/merged", "stack/done/01-flags", "stack/old/01-flags"}

	// A dry run changes nothing.
	stale, err := run.GC("main", false)
	if err != nil {
		t.Fatal(err)
	}
	if got := names(stale); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("stale branches:\n got %q\nwant %q\n%s", got, want, log.String())
	}
	if !exists(t, bare, "fix/merged") || !strings.Contains(log.String(), "would delete") {
		t.Fatalf("dry run deleted something:\n%s", log.String())
	}

	if _, err := run.GC("main", true); err != nil {
		t.Fatal(err)
	}
	for _, b := range want {
		if exists(t, bare, b) {
			t.Errorf("%s should be deleted", b)
		}
	}
	for _, b := range []string{"main", "fix/merged-then-pushed", "stack/live/01-flags", "feature/held", "stack/held/02-activation", "wip", "closed/not-ours"} {
		if !exists(t, bare, b) {
			t.Errorf("%s must be kept", b)
		}
	}
	// Running again finds nothing.
	if again, err := run.GC("main", false); err != nil || len(again) != 0 {
		t.Fatalf("second run: %v %v", names(again), err)
	}
}

func names(s []land.Stale) []string {
	var out []string
	for _, x := range s {
		out = append(out, x.Branch)
	}
	return out
}

func exists(t *testing.T, bare *git.Repo, branch string) bool {
	t.Helper()
	out, _ := bare.Run("rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	return out != ""
}
