package forge

import (
	"bytes"
	"errors"
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

// GitHub's wording below was recorded from real runs in the sandbox repo.
func TestEnqueueDoesNotFallBackWhenAutoMergeWouldFailToo(t *testing.T) {
	for _, c := range []struct{ name, msg, kind, say string }{
		{"merged", "Pull request is closed", "closed", "already merged or closed"},
		{"draft", "Pull request is in draft", "draft", "mark it ready for review"},
		{"moved", "Failed to add PR #7: expected head oid does not match the current head oid", "stale", "changed since it was checked"},
		{"gone", "Could not resolve to a node with the global id of 'PR_x'", "missing", "was not found"},
		{"token", "Resource not accessible by integration", "forbidden", "contents: write"},
	} {
		t.Run(c.name, func(t *testing.T) {
			fakeGH(t, `case "$*" in
  *enablePullRequestAutoMerge*) echo AUTO-MERGE-WAS-TRIED >&2; exit 1;;
esac
echo "`+c.msg+`" >&2; exit 1`)
			var log bytes.Buffer
			g := &GH{Dir: t.TempDir(), Log: &log}
			err := g.Enqueue(PR{Number: 7, NodeID: "PR_x"}, EnqueueOptions{})
			var ee *EnqueueError
			if !errors.As(err, &ee) || ee.Kind != c.kind || !IsPermanent(err) {
				t.Fatalf("want a %s EnqueueError, got %v", c.kind, err)
			}
			if !strings.Contains(err.Error(), c.say) || !strings.Contains(err.Error(), c.msg) || strings.Contains(err.Error(), "AUTO-MERGE") {
				t.Fatalf("message should explain the cause, quote GitHub and not mention auto-merge: %v", err)
			}
			if log.Len() != 0 {
				t.Fatalf("nothing was retried, nothing to log: %q", log.String())
			}
		})
	}
}

func TestEnqueueStillFallsBackOnUnknownMessagesAndIsNotPermanent(t *testing.T) {
	fakeGH(t, `case "$*" in
  *enqueuePullRequest*) echo "Pull request is not in a mergeable state" >&2; exit 1;;
esac
echo '{}'`)
	g := &GH{Dir: t.TempDir(), Log: &bytes.Buffer{}}
	if err := g.Enqueue(PR{Number: 7, NodeID: "PR_x"}, EnqueueOptions{}); err != nil {
		t.Fatalf("unknown refusal should still fall back to auto-merge: %v", err)
	}
	if IsPermanent(errors.New("queue says no")) {
		t.Fatal("a plain error is not permanent")
	}
}

func TestJumpRefusalThatIsFinalReturnsTheClassifiedError(t *testing.T) {
	fakeGH(t, `echo "Pull request is closed" >&2; exit 1`)
	g := &GH{Dir: t.TempDir(), Log: &bytes.Buffer{}}
	err := g.Enqueue(PR{Number: 7, NodeID: "PR_x"}, EnqueueOptions{Jump: true})
	if !IsPermanent(err) {
		t.Fatalf("want a permanent error even for a jump, got %v", err)
	}
}
