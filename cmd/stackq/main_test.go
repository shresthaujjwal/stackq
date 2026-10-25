package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"stackq/internal/git"
	"stackq/internal/split"
)

func event(t *testing.T, name, payload string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "event.json")
	if err := os.WriteFile(p, []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GITHUB_EVENT_NAME", name)
	t.Setenv("GITHUB_EVENT_PATH", p)
}

func TestFromEventTicksOnReviewsAndCIRunsWhenEnabled(t *testing.T) {
	for _, name := range []string{"pull_request_review", "workflow_run", "check_suite", "schedule"} {
		event(t, name, `{}`)
		if sub, _, err := fromEvent("", false); err != nil || sub != "" {
			t.Errorf("%s with nothing enabled: got %q, %v; want no-op", name, sub, err)
		}
		if sub, _, err := fromEvent("", true); err != nil || sub != "tick" {
			t.Errorf("%s with autoLand: got %q, %v; want tick", name, sub, err)
		}
		if sub, _, err := fromEvent("stackq:queue", false); err != nil || sub != "tick" {
			t.Errorf("%s with a pool: got %q, %v; want tick", name, sub, err)
		}
	}
}

func TestFromEventKeepsTheMergeAndDequeueMappings(t *testing.T) {
	event(t, "pull_request", `{"action":"closed","pull_request":{"number":7,"merged":true}}`)
	if sub, n, _ := fromEvent("", true); sub != "after-merge" || n != 7 {
		t.Errorf("merged PR: got %q #%d", sub, n)
	}
	event(t, "pull_request", `{"action":"dequeued","pull_request":{"number":8,"merged":false}}`)
	if sub, n, _ := fromEvent("", true); sub != "dequeued" || n != 8 {
		t.Errorf("dequeued PR: got %q #%d", sub, n)
	}
	event(t, "workflow_dispatch", `{"inputs":{"command":"tick"}}`)
	if sub, _, err := fromEvent("", true); err != nil || sub != "tick" {
		t.Errorf("dispatch tick: got %q, %v", sub, err)
	}
}

func TestVersionString(t *testing.T) {
	oldV, oldC := version, commit
	defer func() { version, commit = oldV, oldC }()
	version, commit = "dev", ""
	if got := versionString(); got != "stackq dev" {
		t.Errorf("dev build: %q", got)
	}
	version, commit = "v1.2.3", "abc1234"
	if got := versionString(); got != "stackq v1.2.3 (abc1234)" {
		t.Errorf("release build: %q", got)
	}
}

func TestPlanCommentShowsThePlanInAFenceLongerThanAnyInIt(t *testing.T) {
	p := &split.Plan{Base: "0123456789abcdef", Slices: []split.Slice{
		{Name: "api", Files: []git.Change{{Status: 'A', Path: "api/handler.go", Added: 40}}},
		{Name: "docs", Files: []git.Change{{Status: 'M', Path: "docs/```odd.md", Added: 3, Deleted: 1}}, Hold: true},
	}}
	got := planComment("feature/big", "origin/main", 300, p)
	if !strings.HasPrefix(got, PlanMarker+"\n") {
		t.Fatalf("comment does not start with the marker:\n%s", got)
	}
	for _, want := range []string{
		"would become 2 stacked PRs with a budget of 300 changed lines per slice",
		"\n````\nSplit feature/big into 2 slices on origin/main",
		"A api/handler.go",
		"[held: activation, merged by hand]",
		"Add the `stackq:split` label",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("comment lacks %q:\n%s", want, got)
		}
	}
	if !strings.HasSuffix(strings.TrimSpace(strings.SplitN(got, "\n\nAdd the", 2)[0]), "````") {
		t.Errorf("the code block is not closed with the longer fence:\n%s", got)
	}
}

func TestWithoutFlagDropsTheFlagAndItsValue(t *testing.T) {
	got := strings.Join(withoutFlag([]string{"--max-lines", "300", "--markdown", "p.md", "-markdown=q.md", "--from", "x"}, "markdown"), " ")
	if got != "--max-lines 300 --from x" {
		t.Fatalf("got %q", got)
	}
}
