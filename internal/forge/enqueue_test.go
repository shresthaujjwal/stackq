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
// and its stdin, so it can tell the calls apart.
func fakeGH(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// The asynchronous merge API answers with a status in the body, also for a
// 409 (a request is already pending) and a 400 (the PR cannot merge).
func TestMergeReadsEveryAnswerOfTheAsynchronousMergeAPI(t *testing.T) {
	for _, c := range []struct {
		name, body string
		exit       int
		want       MergeRequest
	}{
		{"accepted", `{"status":"pending","details":{"message":"Merge request enqueued.","uuid":"u-1","merge_action":"merge_queue"}}`, 0,
			MergeRequest{UUID: "u-1", Status: MergePending, Message: "Merge request enqueued."}},
		{"already pending (409)", `{"status":"pending","details":{"message":"A merge request is already pending.","uuid":"u-0"}}`, 1,
			MergeRequest{UUID: "u-0", Status: MergePending, Message: "A merge request is already pending."}},
		{"in the queue already (200)", `{"status":"enqueued","details":{"message":"Pull request is already in the merge queue."}}`, 0,
			MergeRequest{Status: MergeEnqueued, Message: "Pull request is already in the merge queue."}},
		{"draft (400)", `{"status":"failed","details":{"message":"Pull request is in draft"}}`, 1,
			MergeRequest{Status: MergeFailed, Message: "Pull request is in draft"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			fakeGH(t, `cat > /dev/null; echo '`+c.body+`'; exit `+map[bool]string{true: "1", false: "0"}[c.exit == 1])
			got, err := (&GH{Dir: t.TempDir()}).Merge(7, "abc")
			if err != nil || got != c.want {
				t.Fatalf("got %+v, %v; want %+v", got, err, c.want)
			}
		})
	}
}

func TestMergeAsksForTheQueueAndTheHeadItChecked(t *testing.T) {
	dir := t.TempDir()
	fakeGH(t, `cat > `+dir+`/stdin; echo "$*" > `+dir+`/args; echo '{"status":"pending","details":{"uuid":"u"}}'`)
	if _, err := (&GH{Dir: t.TempDir()}).Merge(7, "abc123"); err != nil {
		t.Fatal(err)
	}
	args, _ := os.ReadFile(dir + "/args")
	stdin, _ := os.ReadFile(dir + "/stdin")
	for _, want := range []string{"-X PUT", "X-GitHub-Api-Version: 2026-03-10", "pulls/7/merge-async"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("args lack %q: %s", want, args)
		}
	}
	if !strings.Contains(string(stdin), `"merge_action":"default"`) || !strings.Contains(string(stdin), `"sha":"abc123"`) {
		t.Errorf("body: %s", stdin)
	}
}

func TestMergeReportsAnAnswerItCannotRead(t *testing.T) {
	fakeGH(t, `cat > /dev/null; echo 'Not Found' >&2; exit 1`)
	if _, err := (&GH{Dir: t.TempDir()}).Merge(7, ""); err == nil || !strings.Contains(err.Error(), "Not Found") {
		t.Fatalf("want GitHub's error, got %v", err)
	}
}

func TestStackCallsUseTheStacksAPI(t *testing.T) {
	dir := t.TempDir()
	fakeGH(t, `cat > `+dir+`/stdin; echo "$*" >> `+dir+`/args
case "$*" in
  *stacks/9*) echo '{"number":9,"pull_requests":[{"number":3},{"number":4}]}';;
  *stacks*) echo '{"number":9,"pull_requests":[{"number":3},{"number":4}]}';;
  *pulls/3*) echo '{"number":3,"stack":{"number":9,"position":1,"size":2}}';;
  *pulls/5*) echo '{"number":5,"stack":null}';;
esac`)
	g := &GH{Dir: t.TempDir()}
	if n, err := g.CreateStack([]int{3, 4}); err != nil || n != 9 {
		t.Fatalf("create: %d %v", n, err)
	}
	stdin, _ := os.ReadFile(dir + "/stdin")
	if strings.TrimSpace(string(stdin)) != `{"pull_requests":[3,4]}` {
		t.Fatalf("create body: %s", stdin)
	}
	if n, _ := g.StackOf(3); n != 9 {
		t.Fatalf("stack of #3: %d", n)
	}
	if n, _ := g.StackOf(5); n != 0 {
		t.Fatalf("stack of #5: %d", n)
	}
	if prs, _ := g.StackPRs(9); len(prs) != 2 || prs[0] != 3 {
		t.Fatalf("stack PRs: %v", prs)
	}
}

// GitHub's wording below was recorded from real runs in the sandbox repo.
func TestJumpClassifiesFinalRefusals(t *testing.T) {
	for _, c := range []struct{ name, msg, kind string }{
		{"merged", "Pull request is closed", "closed"},
		{"draft", "Pull request is in draft", "draft"},
		{"moved", "Failed to add PR #7: expected head oid does not match the current head oid", "stale"},
		{"gone", "Could not resolve to a node with the global id of 'PR_x'", "missing"},
		{"token", "Resource not accessible by integration", "forbidden"},
	} {
		t.Run(c.name, func(t *testing.T) {
			fakeGH(t, `echo "`+c.msg+`" >&2; exit 1`)
			err := (&GH{Dir: t.TempDir(), Log: &bytes.Buffer{}}).Jump(PR{Number: 7, NodeID: "PR_x"})
			var ee *EnqueueError
			if !errors.As(err, &ee) || ee.Kind != c.kind || !IsPermanent(err) || !strings.Contains(err.Error(), c.msg) {
				t.Fatalf("want a %s EnqueueError quoting GitHub, got %v", c.kind, err)
			}
		})
	}
	fakeGH(t, `echo "something else" >&2; exit 1`)
	if err := (&GH{Dir: t.TempDir()}).Jump(PR{Number: 7, NodeID: "PR_x"}); err == nil || IsPermanent(err) {
		t.Fatalf("an unknown refusal is an error but not a permanent one: %v", err)
	}
}
