// Package doctor checks that a repository is set up the way stackq needs:
// the config, the merge queue and rules on trunk, the repository and Actions
// settings, and the workflow files. Every check only reads. A check it cannot
// make (for example, a setting only an admin token can read) is reported as
// unknown with what to look at by hand, never as passing.
package doctor

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"stackq/internal/forge"
)

// Level is how a check came out.
type Level int

const (
	OK Level = iota
	Warn
	Fail
	Unknown
)

func (l Level) mark() string { return [...]string{"✓", "!", "✗", "?"}[l] }

// Result is one finding: what was found and, unless it is OK, what to do.
type Result struct {
	Level Level
	Text  string
	Fix   string
}

// API is what doctor reads from GitHub: REST paths (gh api style, with
// {owner}/{repo} placeholders) and the merge queue.
type API interface {
	API(path string) ([]byte, error)
	Queue(branch string) ([]forge.QueueEntry, error)
}

// Config is the part of .stackq.json the checks look at.
type Config struct {
	Trunk    string
	Prefix   string
	Landing  string
	AutoLand bool
	Priority bool
}

// Run makes every check. dir is the repository's top level.
func Run(api API, dir string, cfg Config) []Result {
	var rs []Result
	rs = append(rs, checkConfig(cfg)...)
	rs = append(rs, checkRepo(api, cfg)...)
	rs = append(rs, checkRules(api, cfg)...)
	rs = append(rs, checkActions(api)...)
	rs = append(rs, checkWorkflows(dir)...)
	rs = append(rs, checkLabels(api)...)
	return rs
}

// Print writes the results and a summary line, and reports whether any check
// failed.
func Print(w io.Writer, rs []Result) (failed bool) {
	count := map[Level]int{}
	for _, r := range rs {
		count[r.Level]++
		fmt.Fprintf(w, "  %s %s\n", r.Level.mark(), r.Text)
		if r.Level != OK && r.Fix != "" {
			fmt.Fprintf(w, "      → %s\n", r.Fix)
		}
	}
	fmt.Fprintf(w, "\n%d ok, %d warnings, %d problems, %d not checked\n", count[OK], count[Warn], count[Fail], count[Unknown])
	return count[Fail] > 0
}

func ok(text string) Result           { return Result{OK, text, ""} }
func warn(text, fix string) Result    { return Result{Warn, text, fix} }
func fail(text, fix string) Result    { return Result{Fail, text, fix} }
func unknown(text, fix string) Result { return Result{Unknown, text, fix} }
func readJSON(b []byte, v any) error  { return json.Unmarshal(b, v) }
func short(err error) string {
	s := strings.TrimSpace(strings.SplitN(err.Error(), "\n", 2)[0])
	if r := []rune(s); len(r) > 120 {
		s = string(r[:120]) + "…"
	}
	return s
}

func has(text string, words ...string) bool {
	for _, w := range words {
		if !strings.Contains(text, w) {
			return false
		}
	}
	return true
}

func checkConfig(cfg Config) []Result {
	var rs []Result
	switch {
	case cfg.Prefix == "":
		rs = append(rs, ok("branch prefix: stack/{name}/ (default)"))
	case !strings.Contains(cfg.Prefix, "{name}"):
		rs = append(rs, fail(fmt.Sprintf("branch prefix %q has no {name}", cfg.Prefix),
			`every stack would share one set of branches; use something like "stack/{name}/"`))
	default:
		rs = append(rs, ok("branch prefix: "+cfg.Prefix))
	}
	switch cfg.Landing {
	case "", "atomic":
		rs = append(rs, ok("landing: atomic"))
	case "incremental":
		rs = append(rs, ok("landing: incremental"))
	default:
		rs = append(rs, fail(fmt.Sprintf("landing %q is not atomic or incremental", cfg.Landing), `set "landing" to "atomic" or "incremental"`))
	}
	if cfg.AutoLand && !cfg.Priority {
		rs = append(rs, ok("autoLand on, priority pool off: ready stacks go straight to the merge queue"))
	}
	return rs
}

func checkRepo(api API, cfg Config) []Result {
	b, err := api.API("repos/{owner}/{repo}")
	if err != nil {
		return []Result{unknown("repository settings: "+short(err), "check that gh is logged in or GH_TOKEN is set")}
	}
	var repo struct {
		DefaultBranch    string `json:"default_branch"`
		AllowAutoMerge   bool   `json:"allow_auto_merge"`
		AllowRebaseMerge bool   `json:"allow_rebase_merge"`
		DeleteOnMerge    bool   `json:"delete_branch_on_merge"`
	}
	if err := readJSON(b, &repo); err != nil {
		return []Result{unknown("repository settings: "+short(err), "")}
	}
	var rs []Result
	if repo.DefaultBranch != cfg.Trunk {
		rs = append(rs, warn(fmt.Sprintf("trunk is %s, but the default branch is %s", cfg.Trunk, repo.DefaultBranch),
			`stackq.yml checks out the default branch; set "trunk" in .stackq.json to match`))
	}
	if repo.AllowAutoMerge {
		rs = append(rs, ok("auto-merge allowed (the fallback when a PR cannot join the queue directly)"))
	} else {
		rs = append(rs, warn("auto-merge is not allowed", "Settings > General > Allow auto-merge; without it the enqueue fallback fails"))
	}
	if !repo.AllowRebaseMerge {
		rs = append(rs, warn("rebase merging is off", "atomic stacks land one commit per part only with the rebase method"))
	}
	if !repo.DeleteOnMerge {
		rs = append(rs, warn("head branches are not deleted after merge", "Settings > General > Automatically delete head branches, or run stackq gc"))
	}
	return rs
}

func checkRules(api API, cfg Config) []Result {
	var rs []Result
	if _, err := api.Queue(cfg.Trunk); err != nil {
		rs = append(rs, fail(fmt.Sprintf("no merge queue on %s (%s)", cfg.Trunk, short(err)),
			"add a branch ruleset for "+cfg.Trunk+" that requires a merge queue"))
	} else {
		rs = append(rs, ok("merge queue on "+cfg.Trunk))
	}
	b, err := api.API("repos/{owner}/{repo}/rules/branches/" + cfg.Trunk)
	if err != nil {
		return append(rs, unknown("rules on "+cfg.Trunk+": "+short(err), "check the branch rulesets by hand"))
	}
	var rules []struct {
		Type       string `json:"type"`
		Parameters struct {
			MergeMethod string `json:"merge_method"`
			Checks      []struct {
				Context string `json:"context"`
			} `json:"required_status_checks"`
		} `json:"parameters"`
	}
	if err := readJSON(b, &rules); err != nil {
		return append(rs, unknown("rules on "+cfg.Trunk+": "+short(err), ""))
	}
	var checks []string
	method := ""
	for _, r := range rules {
		switch r.Type {
		case "merge_queue":
			method = strings.ToLower(r.Parameters.MergeMethod)
		case "required_status_checks":
			for _, c := range r.Parameters.Checks {
				checks = append(checks, c.Context)
			}
		}
	}
	switch method {
	case "", "rebase":
		if method == "rebase" {
			rs = append(rs, ok("merge queue merges with rebase: one commit per part on "+cfg.Trunk))
		}
	default:
		rs = append(rs, warn("merge queue merges with "+method, "rebase keeps one commit per part, so git bisect and revert work per part"))
	}
	if len(checks) == 0 {
		// Classic branch protection can require checks too.
		checks, err = protectionChecks(api, cfg.Trunk)
		if err != nil {
			return append(rs, unknown("required status checks on "+cfg.Trunk+": none in the rulesets, and branch protection could not be read ("+short(err)+")",
				"make sure your CI check is required on "+cfg.Trunk))
		}
	}
	if len(checks) == 0 {
		rs = append(rs, fail("no required status checks on "+cfg.Trunk,
			"require your CI check in the ruleset; otherwise the queue merges without testing the result against the latest "+cfg.Trunk))
	} else {
		sort.Strings(checks)
		rs = append(rs, ok("required checks on "+cfg.Trunk+": "+strings.Join(checks, ", ")))
	}
	return rs
}

// protectionChecks lists the checks classic branch protection requires.
func protectionChecks(api API, branch string) ([]string, error) {
	b, err := api.API("repos/{owner}/{repo}/branches/" + branch)
	if err != nil {
		return nil, err
	}
	var br struct {
		Protection struct {
			RequiredStatusChecks struct {
				Contexts []string `json:"contexts"`
			} `json:"required_status_checks"`
		} `json:"protection"`
	}
	if err := readJSON(b, &br); err != nil {
		return nil, err
	}
	return br.Protection.RequiredStatusChecks.Contexts, nil
}

func checkActions(api API) []Result {
	b, err := api.API("repos/{owner}/{repo}/actions/permissions/workflow")
	if err != nil {
		return []Result{unknown("Actions workflow permissions: "+short(err),
			`reading them needs admin rights; check Settings > Actions > General > "Allow GitHub Actions to create and approve pull requests" by hand (needed with the built-in token)`)}
	}
	var p struct {
		Default    string `json:"default_workflow_permissions"`
		CanApprove bool   `json:"can_approve_pull_request_reviews"`
	}
	if err := readJSON(b, &p); err != nil {
		return []Result{unknown("Actions workflow permissions: "+short(err), "")}
	}
	var rs []Result
	if p.CanApprove {
		rs = append(rs, ok("GitHub Actions may create pull requests"))
	} else {
		rs = append(rs, warn("GitHub Actions may not create pull requests",
			`with the built-in token the split cannot open PRs: Settings > Actions > General > "Allow GitHub Actions to create and approve pull requests" (not needed with STACKQ_TOKEN)`))
	}
	if p.Default == "write" {
		rs = append(rs, warn("the default workflow token can write", "set it to read; stackq's workflows ask for what they need"))
	}
	return rs
}

var (
	ciNamesRe = regexp.MustCompile(`workflows:\s*\[([^\]]+)\]`)
	nameRe    = regexp.MustCompile(`(?m)^name:\s*["']?([^"'\n#]+?)["']?\s*(#.*)?$`)
)

// checkWorkflows reads .github/workflows as text. It looks for what stackq
// needs rather than validating YAML.
func checkWorkflows(dir string) []Result {
	files, _ := filepath.Glob(filepath.Join(dir, ".github", "workflows", "*.y*ml"))
	text := map[string]string{}
	for _, f := range files {
		if b, err := os.ReadFile(f); err == nil {
			text[filepath.Base(f)] = string(b)
		}
	}
	var landing, splitter string
	for _, name := range sortedKeys(text) {
		t := text[name]
		if !strings.Contains(t, "/stackq@") {
			continue
		}
		switch {
		case strings.Contains(t, "command: split"):
			splitter = name
		case strings.Contains(t, "workflow_run:"):
			landing = name
		}
	}
	var rs []Result
	if landing == "" {
		return append(rs, fail("no workflow runs stackq on finished CI runs", "copy examples/stackq.yml into .github/workflows/"))
	}
	t := text[landing]
	if has(t, "checks: read", "statuses: read") {
		rs = append(rs, ok(landing+" can read CI results (checks: read, statuses: read)"))
	} else {
		rs = append(rs, fail(landing+" cannot read CI results",
			"add checks: read and statuses: read to its permissions; without them every PR looks as if it had no checks and auto-land waits forever"))
	}
	if !strings.Contains(t, "actions: write") {
		rs = append(rs, warn(landing+" lacks actions: write", "it cannot start CI on branches pushed with the built-in token"))
	}
	if splitter == "" {
		rs = append(rs, warn("no workflow splits a PR on the stackq:split label", "copy examples/stackq-split.yml if you want label-triggered splits"))
	} else {
		rs = append(rs, ok(splitter+" splits PRs labeled stackq:split"))
	}

	m := ciNamesRe.FindStringSubmatch(t)
	if m == nil {
		return append(rs, warn(landing+" names no CI workflow under workflow_run", "list your CI workflow there, or stacks wait for the next review or timer"))
	}
	for _, ci := range strings.Split(m[1], ",") {
		ci = strings.Trim(strings.TrimSpace(ci), `"'`)
		file := ""
		for _, name := range sortedKeys(text) {
			if n := nameRe.FindStringSubmatch(text[name]); n != nil && strings.TrimSpace(n[1]) == ci {
				file = name
			}
		}
		if file == "" {
			rs = append(rs, fail(fmt.Sprintf("%s waits for a workflow named %q, and none has that name", landing, ci), "fix the name under workflow_run: workflows"))
			continue
		}
		c := text[file]
		for _, trig := range []struct{ on, why string }{
			{"merge_group", "the merge queue runs it on its temporary branches; without it queued PRs are never checked"},
			{"workflow_dispatch", "stackq starts CI this way on branches it pushed with the built-in token"},
			{"push:", "CI on the landed commit is what starts the cleanup after a merge"},
		} {
			if !strings.Contains(c, trig.on) {
				lvl := Warn
				if trig.on == "merge_group" {
					lvl = Fail
				}
				rs = append(rs, Result{lvl, fmt.Sprintf("%s (%s) has no %s trigger", file, ci, strings.TrimSuffix(trig.on, ":")), trig.why})
			}
		}
		if has(c, "merge_group", "workflow_dispatch", "push:") {
			rs = append(rs, ok(fmt.Sprintf("%s (%s) runs on push, merge_group and workflow_dispatch", file, ci)))
		}
	}
	return rs
}

func checkLabels(api API) []Result {
	b, err := api.API("repos/{owner}/{repo}/labels?per_page=100")
	if err != nil {
		return []Result{unknown("labels: "+short(err), "")}
	}
	var labels []struct {
		Name string `json:"name"`
	}
	if err := readJSON(b, &labels); err != nil {
		return []Result{unknown("labels: "+short(err), "")}
	}
	for _, l := range labels {
		if l.Name == "stackq:split" {
			return []Result{ok("label stackq:split exists")}
		}
	}
	return []Result{warn("label stackq:split does not exist", "create it once (Issues > Labels) so people can apply it; stackq creates its other labels itself")}
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
