package stack

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"stackq/internal/git"
	"stackq/internal/split"
)

type CreateOptions struct {
	Name     string
	Prefix   string // branch prefix, e.g. stack/feat/
	Trunk    string
	Remote   string
	Source   string // the big branch, for reference
	SourcePR int    // the original PR, if known
	Landing  string // "atomic" or "incremental"
	// Check runs in each slice's worktree (e.g. "go build ./..."). A slice
	// that fails is folded into the next one when MergeOnFail is set.
	Check       string
	MergeOnFail bool
	// CleanBetweenChecks removes untracked and ignored files (git clean -fdx)
	// from the slice worktree before each check, so build output from one
	// slice's check cannot make the next slice's check pass (K4). The slice's
	// own files are staged, so they stay.
	CleanBetweenChecks bool
	Force              bool
	Log                io.Writer
}

// Create builds one commit per slice in a temporary worktree (your working
// tree is untouched), creates the stack branches and verifies that the top
// of the stack is byte-for-byte the original branch.
func Create(r *git.Repo, p *split.Plan, o CreateOptions) (*Stack, error) {
	if o.Log == nil {
		o.Log = io.Discard
	}
	if len(p.Slices) == 0 {
		return nil, fmt.Errorf("plan has no slices")
	}
	existing, _ := r.Run("for-each-ref", "--format=%(refname:short)", "refs/heads/"+strings.TrimSuffix(o.Prefix, "/"))
	if existing != "" {
		if !o.Force {
			return nil, fmt.Errorf("branches under %s already exist; use --force to replace them", o.Prefix)
		}
		for _, b := range strings.Split(existing, "\n") {
			if _, err := r.Run("branch", "--delete", "--force", b); err != nil {
				return nil, err
			}
		}
	}
	if _, err := Load(r, o.Name); err == nil && !o.Force {
		return nil, fmt.Errorf("a stack named %q already exists; use --force or another --name", o.Name)
	}

	w, cleanup, err := r.AddWorktree(p.Base)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	type built struct {
		name string
		sha  string
		hold bool
	}
	var commits []built
	var pending []string
	for i, sl := range p.Slices {
		if err := stage(w, p.Head, sl.Files); err != nil {
			return nil, fmt.Errorf("staging slice %s: %w", sl.Name, err)
		}
		pending = append(pending, sl.Name)
		last := i == len(p.Slices)-1
		if o.Check != "" {
			if o.CleanBetweenChecks {
				if _, err := w.Run("clean", "-fdx", "--quiet"); err != nil {
					return nil, fmt.Errorf("cleaning before the check of slice %s: %w", sl.Name, err)
				}
			}
			out, err := runCheck(w.Dir, o.Check)
			switch {
			case err == nil:
				fmt.Fprintf(o.Log, "  ✓ %s passes %q\n", strings.Join(pending, " + "), o.Check)
			case !last && o.MergeOnFail:
				fmt.Fprintf(o.Log, "  ✗ %s fails %q; folding it into the next slice\n", sl.Name, o.Check)
				continue
			case last:
				fmt.Fprintf(o.Log, "  ! the complete branch fails %q too, so this is not a split problem:\n%s\n", o.Check, tail(out, 15))
			default:
				return nil, fmt.Errorf("slice %d (%s) fails %q:\n%s", i+1, sl.Name, o.Check, tail(out, 15))
			}
		}
		name := pending[0]
		if len(pending) > 1 {
			name = fmt.Sprintf("%s +%d", pending[0], len(pending)-1)
		}
		msg := fmt.Sprintf("%s\n\nPart of stack %s, split from %s by stackq.\nSlices: %s", name, o.Name, o.Source, strings.Join(pending, ", "))
		if _, err := w.Run("commit", "--quiet", "--no-verify", "--allow-empty", "-m", msg); err != nil {
			return nil, err
		}
		sha, err := w.RevParse("HEAD")
		if err != nil {
			return nil, err
		}
		commits = append(commits, built{name, sha, sl.Hold})
		pending = nil
	}

	top := commits[len(commits)-1].sha
	if !r.TreesEqual(top, p.Head) {
		return nil, fmt.Errorf("internal error: top of stack differs from %s; no branches were created", git.Short(p.Head))
	}

	s := &Stack{Name: o.Name, Trunk: o.Trunk, Remote: o.Remote, Source: o.Source, SourcePR: o.SourcePR, Landing: o.Landing}
	parent, parentTip := o.Trunk, p.Base
	for i, c := range commits {
		bn := fmt.Sprintf("%s%02d-%s", o.Prefix, i+1, split.Slug(c.name))
		if _, err := r.Run("branch", "--force", bn, c.sha); err != nil {
			return nil, err
		}
		title := fmt.Sprintf("[%s %d/%d] %s", o.Name, i+1, len(commits), c.name)
		if c.hold {
			title = fmt.Sprintf("[%s activation] %s", o.Name, c.name)
		}
		s.Branches = append(s.Branches, &Branch{
			Name:      bn,
			Title:     title,
			Parent:    parent,
			ParentTip: parentTip,
			Hold:      c.hold,
		})
		parent, parentTip = bn, c.sha
	}
	return s, s.Save(r)
}

func stage(w *git.Repo, head string, files []git.Change) error {
	var rm, co []string
	for _, c := range files {
		switch c.Status {
		case 'D':
			rm = append(rm, c.Path)
		case 'R':
			rm = append(rm, c.OldPath)
			co = append(co, c.Path)
		default:
			co = append(co, c.Path)
		}
	}
	for _, chunk := range chunks(rm, 200) {
		args := append([]string{"--literal-pathspecs", "rm", "--quiet", "--ignore-unmatch", "--"}, chunk...)
		if _, err := w.Run(args...); err != nil {
			return err
		}
	}
	for _, chunk := range chunks(co, 200) {
		args := append([]string{"--literal-pathspecs", "checkout", head, "--"}, chunk...)
		if _, err := w.Run(args...); err != nil {
			return err
		}
	}
	return nil
}

func chunks(s []string, n int) [][]string {
	var out [][]string
	for len(s) > n {
		out = append(out, s[:n])
		s = s[n:]
	}
	if len(s) > 0 {
		out = append(out, s)
	}
	return out
}

// credentialVars hold tokens. The check command is whatever .stackq.json says,
// read from the branch being split, so it must not inherit them.
var credentialVars = map[string]bool{
	"GH_TOKEN": true, "GITHUB_TOKEN": true, "STACKQ_TOKEN": true,
	"GH_ENTERPRISE_TOKEN": true, "GITHUB_ENTERPRISE_TOKEN": true,
	"ACTIONS_ID_TOKEN_REQUEST_TOKEN": true, "ACTIONS_ID_TOKEN_REQUEST_URL": true,
	"ACTIONS_RUNTIME_TOKEN": true,
}

// envWithoutCredentials returns env minus the variables in credentialVars.
func envWithoutCredentials(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if name, _, _ := strings.Cut(kv, "="); credentialVars[name] {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func runCheck(dir, check string) (string, error) {
	cmd := exec.Command("sh", "-c", check)
	cmd.Dir = dir
	cmd.Env = envWithoutCredentials(os.Environ())
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return "    " + strings.Join(lines, "\n    ")
}
