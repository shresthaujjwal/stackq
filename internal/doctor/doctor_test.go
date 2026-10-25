package doctor

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"stackq/internal/forge"
)

type fakeAPI struct {
	paths    map[string]string
	queueErr error
}

func (f fakeAPI) API(path string) ([]byte, error) {
	if b, ok := f.paths[path]; ok {
		return []byte(b), nil
	}
	return nil, errors.New("HTTP 403: Resource not accessible by integration")
}

func (f fakeAPI) Queue(string) ([]forge.QueueEntry, error) { return nil, f.queueErr }

func healthy() fakeAPI {
	return fakeAPI{paths: map[string]string{
		"repos/{owner}/{repo}":                              `{"default_branch":"main","allow_auto_merge":true,"allow_rebase_merge":true,"delete_branch_on_merge":true}`,
		"repos/{owner}/{repo}/rules/branches/main":          `[{"type":"merge_queue","parameters":{"merge_method":"REBASE"}},{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"test"}]}}]`,
		"repos/{owner}/{repo}/actions/permissions/workflow": `{"default_workflow_permissions":"read","can_approve_pull_request_reviews":true}`,
		"repos/{owner}/{repo}/labels?per_page=100":          `[{"name":"bug"},{"name":"stackq:split"}]`,
	}}
}

const stackqYML = `name: stackq
on:
  workflow_run:
    workflows: [ci]
    types: [completed]
permissions:
  contents: write
  pull-requests: write
  actions: write
  checks: read
  statuses: read
jobs:
  stackq:
    steps:
      - uses: someone/stackq@v0
`

const splitYML = "name: stackq split\njobs:\n  split:\n    steps:\n      - uses: someone/stackq@v0\n        with:\n          command: split\n"

const ciYML = "name: ci\non:\n  push:\n    branches: [main]\n  pull_request:\n  merge_group:\n  workflow_dispatch:\n"

func repoWith(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	wf := filepath.Join(dir, ".github", "workflows")
	if err := os.MkdirAll(wf, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(wf, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func healthyFiles() map[string]string {
	return map[string]string{"stackq.yml": stackqYML, "stackq-split.yml": splitYML, "ci.yml": ciYML}
}

var cfg = Config{Trunk: "main", Prefix: "stack/{name}/", Landing: "atomic", AutoLand: true, Priority: true, QueueCI: "ci.yml"}

func levels(rs []Result, l Level) []string {
	var out []string
	for _, r := range rs {
		if r.Level == l {
			out = append(out, r.Text)
		}
	}
	return out
}

func TestAHealthyRepoPasses(t *testing.T) {
	rs := Run(healthy(), repoWith(t, healthyFiles()), cfg)
	for _, l := range []Level{Warn, Fail, Unknown} {
		if got := levels(rs, l); len(got) > 0 {
			t.Fatalf("level %d findings on a healthy repo: %q", l, got)
		}
	}
	var out bytes.Buffer
	if Print(&out, rs) {
		t.Fatalf("a healthy repo must not fail:\n%s", out.String())
	}
}

// The setup gaps the live runs hit, each reported as a problem.
func TestReportsTheGapsFromTheLiveRuns(t *testing.T) {
	api := healthy()
	api.queueErr = errors.New("branch main has no merge queue")
	api.paths["repos/{owner}/{repo}/rules/branches/main"] = `[]`
	api.paths["repos/{owner}/{repo}/branches/main"] = `{"protection":{"required_status_checks":{"contexts":[]}}}`
	files := healthyFiles()
	files["stackq.yml"] = strings.Replace(stackqYML, "  checks: read\n  statuses: read\n", "", 1)
	files["ci.yml"] = "name: ci\non:\n  pull_request:\n"

	rs := Run(api, repoWith(t, files), cfg)
	fails := strings.Join(levels(rs, Fail), "\n")
	for _, want := range []string{"no merge queue on main", "no required status checks on main", "stackq.yml cannot read CI results", "has no merge_group trigger"} {
		if !strings.Contains(fails, want) {
			t.Errorf("missing problem %q in:\n%s", want, fails)
		}
	}
	warns := strings.Join(levels(rs, Warn), "\n")
	for _, want := range []string{"has no workflow_dispatch trigger", "has no push trigger"} {
		if !strings.Contains(warns, want) {
			t.Errorf("missing warning %q in:\n%s", want, warns)
		}
	}
	var out bytes.Buffer
	if !Print(&out, rs) || !strings.Contains(out.String(), "→ add checks: read and statuses: read") {
		t.Fatalf("problems must fail the run and say what to do:\n%s", out.String())
	}
}

func TestClassicProtectionChecksCount(t *testing.T) {
	api := healthy()
	api.paths["repos/{owner}/{repo}/rules/branches/main"] = `[]`
	api.paths["repos/{owner}/{repo}/branches/main"] = `{"protection":{"required_status_checks":{"contexts":["test"]}}}`
	rs := Run(api, repoWith(t, healthyFiles()), cfg)
	if got := levels(rs, Fail); len(got) > 0 {
		t.Fatalf("checks required by branch protection must count: %q", got)
	}
}

// A setting the token cannot read is unknown, never a pass.
func TestUnreadableSettingsAreUnknown(t *testing.T) {
	api := healthy()
	delete(api.paths, "repos/{owner}/{repo}/actions/permissions/workflow")
	rs := Run(api, repoWith(t, healthyFiles()), cfg)
	got := strings.Join(levels(rs, Unknown), "\n")
	if !strings.Contains(got, "Actions workflow permissions") {
		t.Fatalf("unreadable Actions settings not reported as unknown: %q", got)
	}
	if strings.Contains(strings.Join(levels(rs, OK), "\n"), "may create pull requests") {
		t.Fatal("an unreadable setting was reported as passing")
	}
}

func TestWorkflowNamingMismatch(t *testing.T) {
	files := healthyFiles()
	files["ci.yml"] = strings.Replace(ciYML, "name: ci", "name: build", 1)
	rs := Run(healthy(), repoWith(t, files), cfg)
	if got := strings.Join(levels(rs, Fail), "\n"); !strings.Contains(got, `waits for a workflow named "ci"`) {
		t.Fatalf("a workflow_run name nobody has must be a problem: %q", got)
	}
	if got := Run(healthy(), repoWith(t, map[string]string{}), cfg); !strings.Contains(strings.Join(levels(got, Fail), "\n"), "no workflow runs stackq") {
		t.Fatal("a repo without the stackq workflow must fail")
	}
}

func TestConfigProblems(t *testing.T) {
	bad := Config{Trunk: "main", Prefix: "stack/", Landing: "sometimes"}
	got := strings.Join(levels(checkConfig(bad), Fail), "\n")
	if !strings.Contains(got, "has no {name}") || !strings.Contains(got, "is not atomic or incremental") {
		t.Fatalf("config problems not reported: %q", got)
	}
}

func TestQueueCI(t *testing.T) {
	noQueueCI := cfg
	noQueueCI.QueueCI = ""
	rs := Run(healthy(), repoWith(t, healthyFiles()), noQueueCI)
	if got := strings.Join(levels(rs, Warn), "\n"); !strings.Contains(got, "queueCI is not set") {
		t.Fatalf("required checks without queueCI must warn: %q", got)
	}
	missing := cfg
	missing.QueueCI = "build.yml"
	rs = Run(healthy(), repoWith(t, healthyFiles()), missing)
	if got := strings.Join(levels(rs, Fail), "\n"); !strings.Contains(got, "build.yml, which is not in .github/workflows") {
		t.Fatalf("a queueCI file that does not exist must fail: %q", got)
	}
	files := healthyFiles()
	files["ci.yml"] = strings.Replace(ciYML, "  workflow_dispatch:\n", "", 1)
	rs = Run(healthy(), repoWith(t, files), cfg)
	if got := strings.Join(levels(rs, Fail), "\n"); !strings.Contains(got, "(queueCI) has no workflow_dispatch trigger") {
		t.Fatalf("queueCI without workflow_dispatch must fail: %q", got)
	}
}
