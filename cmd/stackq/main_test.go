package main

import (
	"errors"
	"fmt"
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

func TestWriteSummaryAppendsTheOutcomeAndTheLog(t *testing.T) {
	p := filepath.Join(t.TempDir(), "summary.md")
	if err := os.WriteFile(p, []byte("earlier step\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeSummary(p, "ci", []string{"tick"}, "workflow_dispatch", "- stack shop: landing\n", nil); err != nil {
		t.Fatal(err)
	}
	var long []string
	for i := 0; i < summaryLines+5; i++ {
		long = append(long, fmt.Sprintf("line %d", i))
	}
	if err := writeSummary(p, "ci", nil, "workflow_run", strings.Join(long, "\n"), errors.New("boom ``` here")); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	got := string(b)
	for _, want := range []string{
		"earlier step\n### stackq ci tick\n\nEvent: `workflow_dispatch`. Result: ✓ done.\n\n```\n- stack shop: landing\n```\n",
		"### stackq ci\n\nEvent: `workflow_run`. Result: ✗ failed.\n\n````\nboom ``` here\n````\n",
		fmt.Sprintf("Last %d of %d lines", summaryLines, summaryLines+5),
		"```\nline 5\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("summary lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "line 4\n") {
		t.Errorf("summary keeps lines beyond the cap:\n%s", got)
	}
}

func TestAnnotationReportsTheOutcomeOnOneEscapedLine(t *testing.T) {
	for _, c := range []struct {
		name       string
		log        string
		runErr     error
		summaryErr error
		args       []string
		want       string
	}{
		{"done", "a\nb\n", nil, nil, []string{"tick"}, "::notice title=stackq ci tick::done; 2 lines of output, written to the job summary"},
		{"quiet", "", nil, nil, nil, "::notice title=stackq ci::done; no output"},
		{"failed", "x", errors.New("bad: 100%\nnext"), nil, nil, "::error title=stackq ci::failed: bad: 100%25%0Anext"},
		{"summary", "x", nil, errors.New("disk"), nil, "::warning title=stackq ci::done; 1 line of output, written to the job summary (the job summary could not be written: disk)"},
	} {
		if got := annotation("ci", c.args, c.log, c.runErr, c.summaryErr); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
	if got := annotation("ci", []string{"--pr", "a,b"}, "x", nil, nil); !strings.Contains(got, "title=stackq ci --pr a%2Cb::") {
		t.Errorf("title is not escaped: %q", got)
	}
}

func TestPlanSaysOneFileAndOneStackedPR(t *testing.T) {
	p := &split.Plan{Base: "0123456789abcdef", Slices: []split.Slice{
		{Name: "api", Files: []git.Change{{Status: 'A', Path: "api/handler.go", Added: 5}}},
	}}
	got := planComment("feature/big", "origin/main", 0, p)
	for _, want := range []string{"into 1 slice on", "this PR would become 1 stacked PR.", "  1 file  "} {
		if !strings.Contains(got, want) {
			t.Errorf("plan lacks %q:\n%s", want, got)
		}
	}
	for _, bad := range []string{"1 slices", "1 files", "1 stacked PRs"} {
		if strings.Contains(got, bad) {
			t.Errorf("plan contains %q:\n%s", bad, got)
		}
	}
}

func TestCITrunkPrefersTheConfigThenGitHubsDefaultBranch(t *testing.T) {
	event(t, "workflow_dispatch", `{"repository":{"default_branch":"develop"}}`)
	if got := ciTrunk("release", "main"); got != "release" {
		t.Errorf("config first: %q", got)
	}
	if got := ciTrunk("", "main"); got != "develop" {
		t.Errorf("event default branch next: %q", got)
	}
	event(t, "workflow_dispatch", `{}`)
	if got := ciTrunk("", "main"); got != "main" {
		t.Errorf("resolved trunk last: %q", got)
	}
}
