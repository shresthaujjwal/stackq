package main

import (
	"os"
	"path/filepath"
	"testing"
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
