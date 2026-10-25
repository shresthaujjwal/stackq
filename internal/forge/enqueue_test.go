package forge

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeGH puts a `gh` script first on PATH. The script sees every argument,
// including the GraphQL query text, so it can tell the two mutations apart.
func fakeGH(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestEnqueueSaysWhyItFellBackToAutoMerge(t *testing.T) {
	fakeGH(t, `case "$*" in
  *enqueuePullRequest*) echo "Pull request is not mergeable: required checks are pending" >&2; exit 1;;
  *enablePullRequestAutoMerge*) echo '{}'; exit 0;;
esac
exit 1`)
	var log bytes.Buffer
	g := &GH{Dir: t.TempDir(), Log: &log}
	if err := g.Enqueue(PR{Number: 7, NodeID: "PR_x"}, EnqueueOptions{}); err != nil {
		t.Fatalf("auto-merge fallback should succeed: %v", err)
	}
	out := log.String()
	if !strings.Contains(out, "#7") || !strings.Contains(out, "required checks are pending") || !strings.Contains(out, "auto-merge") {
		t.Fatalf("the fallback must say why the direct enqueue failed, got %q", out)
	}
}

func TestEnqueueIsQuietWhenTheQueueAcceptsIt(t *testing.T) {
	fakeGH(t, `echo '{}'`)
	var log bytes.Buffer
	g := &GH{Dir: t.TempDir(), Log: &log}
	if err := g.Enqueue(PR{Number: 7, NodeID: "PR_x"}, EnqueueOptions{}); err != nil {
		t.Fatal(err)
	}
	if log.Len() != 0 {
		t.Fatalf("a normal enqueue should log nothing, got %q", log.String())
	}
}

func TestEnqueueReportsBothErrorsWhenBothFail(t *testing.T) {
	fakeGH(t, `case "$*" in
  *enqueuePullRequest*) echo "queue says no" >&2;;
  *) echo "auto-merge says no" >&2;;
esac
exit 1`)
	var log bytes.Buffer
	g := &GH{Dir: t.TempDir(), Log: &log}
	err := g.Enqueue(PR{Number: 7, NodeID: "PR_x"}, EnqueueOptions{})
	if err == nil || !strings.Contains(err.Error(), "queue says no") || !strings.Contains(err.Error(), "auto-merge says no") {
		t.Fatalf("want both errors, got %v", err)
	}
	if log.Len() != 0 {
		t.Fatalf("no fallback happened, nothing to log: %q", log.String())
	}
}
