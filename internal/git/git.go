// Package git is a thin wrapper around the git CLI with the handful of
// operations stackq needs: diffs, worktrees and conflict-aware rebases.
package git

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// Repo runs git commands in Dir.
type Repo struct {
	Dir string
	Env []string

	authOnce  sync.Once
	persisted bool // a checkout already stored credentials in the git config
}

// networkCommands talk to the remote and so need credentials.
var networkCommands = map[string]bool{"fetch": true, "push": true, "pull": true, "clone": true, "ls-remote": true}

// authEnvFor returns the extra environment for one git process while the job
// has a token, or nil when it has none.
//
// For a command that talks to the remote (network) it lets git authenticate to
// the GitHub server with the token instead of storing it in .git/config, where
// any later command could read it, including the build check from
// .stackq.json. With `persist-credentials: false` nothing is stored, and the
// token reaches only the git process that talks to the remote, scoped to the
// server's URL. Credentials a checkout already stored are left alone: sending a
// second Authorization header would break the request.
//
// For every command it also turns off what the repository config can make git
// run: hooks, an fsmonitor program and credential helpers. The check command
// runs in a worktree of this repository and can write that config; nothing it
// set up should run while a process holding the token is its ancestor (a child
// can read its parents' environment from /proc).
//
// Config entries passed through GIT_CONFIG_COUNT are kept; ours are added after
// them.
func authEnvFor(getenv func(string) string, network, persisted bool) []string {
	token := getenv("GH_TOKEN")
	if token == "" {
		token = getenv("GITHUB_TOKEN")
	}
	if token == "" {
		return nil
	}
	// Add to config entries the environment already carries instead of
	// replacing them.
	n := 0
	if c := getenv("GIT_CONFIG_COUNT"); c != "" {
		var err error
		if n, err = strconv.Atoi(c); err != nil || n < 0 {
			return nil
		}
	}
	var entries [][2]string
	if network && !persisted {
		server := strings.TrimRight(getenv("GITHUB_SERVER_URL"), "/")
		if server == "" {
			server = "https://github.com"
		}
		cred := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
		entries = append(entries, [2]string{"http." + server + "/.extraheader", "AUTHORIZATION: basic " + cred})
	}
	entries = append(entries,
		[2]string{"core.hooksPath", os.DevNull},
		[2]string{"core.fsmonitor", "false"},
		[2]string{"credential.helper", ""},
	)
	env := []string{fmt.Sprintf("GIT_CONFIG_COUNT=%d", n+len(entries))}
	for i, e := range entries {
		env = append(env, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", n+i, e[0]), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", n+i, e[1]))
	}
	return env
}

// authEnv is authEnvFor for a git invocation in this repository.
func (r *Repo) authEnv(args []string) []string {
	if len(args) == 0 || (os.Getenv("GH_TOKEN") == "" && os.Getenv("GITHUB_TOKEN") == "") {
		return nil
	}
	network := networkCommands[args[0]]
	if network {
		r.authOnce.Do(func() {
			server := strings.TrimRight(os.Getenv("GITHUB_SERVER_URL"), "/")
			if server == "" {
				server = "https://github.com"
			}
			cmd := exec.Command("git", "config", "--get-all", "http."+server+"/.extraheader")
			cmd.Dir = r.Dir
			out, _ := cmd.Output()
			r.persisted = len(bytes.TrimSpace(out)) > 0
		})
	}
	return authEnvFor(os.Getenv, network, r.persisted)
}

// Error carries the failing command and its stderr.
type Error struct {
	Args   []string
	Stderr string
	Err    error
}

func (e *Error) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if msg == "" {
		msg = e.Err.Error()
	}
	return fmt.Sprintf("git %s: %s", strings.Join(e.Args, " "), msg)
}

// Raw runs git and returns stdout untouched.
func (r *Repo) Raw(stdin string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = r.Dir
	cmd.Env = append(os.Environ(), "GIT_EDITOR=true", "GIT_SEQUENCE_EDITOR=true", "GIT_TERMINAL_PROMPT=0")
	cmd.Env = append(cmd.Env, r.Env...)
	cmd.Env = append(cmd.Env, r.authEnv(args)...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.String(), &Error{Args: args, Stderr: errb.String(), Err: err}
	}
	return out.String(), nil
}

// Run runs git and returns stdout without the trailing newline.
func (r *Repo) Run(args ...string) (string, error) {
	out, err := r.Raw("", args...)
	return strings.TrimRight(out, "\n"), err
}

// In returns a Repo for another directory with the same environment.
func (r *Repo) In(dir string) *Repo { return &Repo{Dir: dir, Env: r.Env} }

func (r *Repo) RevParse(ref string) (string, error) {
	out, err := r.Run("rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err != nil || out == "" {
		return "", fmt.Errorf("unknown revision %q", ref)
	}
	return out, nil
}

func (r *Repo) RefExists(ref string) bool {
	_, err := r.RevParse(ref)
	return err == nil
}

func (r *Repo) MergeBase(a, b string) (string, error) {
	out, err := r.Run("merge-base", a, b)
	if err != nil {
		return "", fmt.Errorf("no common ancestor between %s and %s: %w", a, b, err)
	}
	return out, nil
}

// IsAncestor reports whether a is an ancestor of (or equal to) b.
func (r *Repo) IsAncestor(a, b string) bool {
	_, err := r.Run("merge-base", "--is-ancestor", a, b)
	return err == nil
}

func (r *Repo) CurrentBranch() string {
	out, _ := r.Run("symbolic-ref", "--quiet", "--short", "HEAD")
	return out
}

func (r *Repo) TopLevel() (string, error) { return r.Run("rev-parse", "--show-toplevel") }

// CommonDir is the .git directory shared by all worktrees.
func (r *Repo) CommonDir() (string, error) {
	out, err := r.Run("rev-parse", "--git-common-dir")
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(out) {
		out = filepath.Join(r.Dir, out)
	}
	return filepath.Clean(out), nil
}

func (r *Repo) TreesEqual(a, b string) bool {
	_, err := r.Run("diff", "--quiet", a, b)
	return err == nil
}

// Clean reports whether tracked files have no uncommitted changes.
func (r *Repo) Clean() bool {
	out, err := r.Run("status", "--porcelain", "--untracked-files=no")
	return err == nil && out == ""
}

func (r *Repo) Show(rev, path string) ([]byte, error) {
	out, err := r.Raw("", "show", rev+":"+path)
	return []byte(out), err
}

func (r *Repo) ListFiles(rev string) ([]string, error) {
	out, err := r.Raw("", "ls-tree", "-r", "--name-only", "-z", rev)
	if err != nil {
		return nil, err
	}
	return splitZ(out), nil
}

func (r *Repo) HasRemote(name string) bool {
	if name == "" {
		return false
	}
	_, err := r.Run("remote", "get-url", name)
	return err == nil
}

// CountCommits returns the number of commits in from..to.
func (r *Repo) CountCommits(from, to string) int {
	out, err := r.Run("rev-list", "--count", from+".."+to)
	if err != nil {
		return -1
	}
	n, _ := strconv.Atoi(out)
	return n
}

// FullyApplied reports whether every commit on branch that is not in
// upstream already has a patch-equivalent commit in upstream. That is how a
// rebase-merged PR looks: same patches, new SHAs.
func (r *Repo) FullyApplied(upstream, branch string) bool {
	out, err := r.Run("cherry", upstream, branch)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "+") {
			return false
		}
	}
	return true
}

// Change is one file in a diff.
type Change struct {
	Status  byte // A, M, D, R, C or T
	Path    string
	OldPath string // set for renames and copies
	Added   int
	Deleted int
	Binary  bool
}

// Lines is the size of the change used for slice budgeting.
func (c Change) Lines() int {
	n := c.Added + c.Deleted
	if c.Binary || n == 0 {
		return 1
	}
	return n
}

func splitZ(s string) []string {
	s = strings.TrimRight(s, "\x00")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\x00")
}

// Diff lists changed files between two revisions, with rename detection.
func (r *Repo) Diff(base, head string) ([]Change, error) {
	ns, err := r.Raw("", "diff", "--no-ext-diff", "--name-status", "-M", "-z", base, head)
	if err != nil {
		return nil, err
	}
	var changes []Change
	t := splitZ(ns)
	for i := 0; i < len(t); {
		st := t[i]
		i++
		c := Change{Status: st[0]}
		if c.Status == 'R' || c.Status == 'C' {
			if i+1 >= len(t) {
				return nil, fmt.Errorf("malformed diff output")
			}
			c.OldPath, c.Path = t[i], t[i+1]
			i += 2
		} else {
			if i >= len(t) {
				return nil, fmt.Errorf("malformed diff output")
			}
			c.Path = t[i]
			i++
		}
		changes = append(changes, c)
	}

	nm, err := r.Raw("", "diff", "--no-ext-diff", "--numstat", "-M", "-z", base, head)
	if err != nil {
		return nil, err
	}
	type stat struct {
		a, d int
		bin  bool
	}
	stats := map[string]stat{}
	t = splitZ(nm)
	for i := 0; i < len(t); {
		parts := strings.SplitN(t[i], "\t", 3)
		i++
		if len(parts) < 3 {
			continue
		}
		p := parts[2]
		if p == "" { // rename: old and new paths follow as separate fields
			if i+1 < len(t) {
				p = t[i+1]
			}
			i += 2
		}
		a, errA := strconv.Atoi(parts[0])
		d, errD := strconv.Atoi(parts[1])
		stats[p] = stat{a, d, errA != nil || errD != nil}
	}
	for i := range changes {
		s := stats[changes[i].Path]
		changes[i].Added, changes[i].Deleted, changes[i].Binary = s.a, s.d, s.bin
	}
	return changes, nil
}

var hunkRe = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@`)

// AddedLines returns the line numbers (in head) that base..head adds to path.
func (r *Repo) AddedLines(base, head, path string) (map[int]bool, error) {
	out, err := r.Raw("", "diff", "--no-ext-diff", "-U0", "-M", base, head, "--", path)
	if err != nil {
		return nil, err
	}
	lines := map[int]bool{}
	for _, l := range strings.Split(out, "\n") {
		m := hunkRe.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		start, _ := strconv.Atoi(m[1])
		n := 1
		if m[2] != "" {
			n, _ = strconv.Atoi(m[2])
		}
		for i := 0; i < n; i++ {
			lines[start+i] = true
		}
	}
	return lines, nil
}

// PatchID is git's stable patch-id of one commit's diff, or "" for an
// empty or merge commit.
func (r *Repo) PatchID(commit string) string {
	diff, err := r.Raw("", "show", "--no-ext-diff", "--format=", commit)
	if err != nil || strings.TrimSpace(diff) == "" {
		return ""
	}
	out, err := r.Raw(diff, "patch-id", "--stable")
	if err != nil {
		return ""
	}
	f := strings.Fields(out)
	if len(f) == 0 {
		return ""
	}
	return f[0]
}

// Parents returns a commit's parent SHAs.
func (r *Repo) Parents(commit string) []string {
	out, err := r.Run("rev-list", "--parents", "-n", "1", commit)
	if err != nil {
		return nil
	}
	f := strings.Fields(out)
	if len(f) == 0 {
		return nil
	}
	return f[1:]
}

// ChangedFiles lists paths that differ between two revisions (both sides of renames).
func (r *Repo) ChangedFiles(a, b string) []string {
	out, err := r.Raw("", "diff", "--no-ext-diff", "--name-only", "--no-renames", "-z", a, b)
	if err != nil {
		return nil
	}
	return splitZ(out)
}

// AddWorktree creates a temporary detached worktree at rev. Call cleanup
// when done; it removes the worktree and its directory.
func (r *Repo) AddWorktree(rev string) (*Repo, func(), error) {
	parent, err := os.MkdirTemp("", "stackq-wt-")
	if err != nil {
		return nil, nil, err
	}
	dir := filepath.Join(parent, "wt")
	if _, err := r.Run("worktree", "add", "--detach", "--quiet", dir, rev); err != nil {
		os.RemoveAll(parent)
		return nil, nil, err
	}
	cleanup := func() {
		r.Run("worktree", "remove", "--force", dir)
		os.RemoveAll(parent)
		r.Run("worktree", "prune")
	}
	return r.In(dir), cleanup, nil
}

// WorktreeOf returns the path of the worktree that has branch checked out.
func (r *Repo) WorktreeOf(branch string) string {
	out, err := r.Run("worktree", "list", "--porcelain")
	if err != nil {
		return ""
	}
	var cur string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "worktree ") {
			cur = strings.TrimPrefix(line, "worktree ")
		}
		if line == "branch refs/heads/"+branch {
			return cur
		}
	}
	return ""
}

// ConflictError is returned when a rebase stops on conflicts. The rebase
// has already been aborted, so nothing is left half-done.
type ConflictError struct {
	Branch string
	Onto   string
	Files  []string
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("rebasing %s onto %s hit conflicts in: %s", e.Branch, Short(e.Onto), strings.Join(e.Files, ", "))
}

func Short(sha string) string {
	if len(sha) > 10 {
		return sha[:10]
	}
	return sha
}

func rebaseIn(w *Repo, onto, upstream, label string) error {
	args := []string{"rebase", "--quiet"}
	if upstream != "" {
		args = append(args, "--onto", onto, upstream)
	} else {
		args = append(args, onto)
	}
	if _, err := w.Run(args...); err != nil {
		files, _ := w.Run("diff", "--name-only", "--diff-filter=U")
		w.Run("rebase", "--abort")
		var fl []string
		for _, f := range strings.Split(files, "\n") {
			if f != "" {
				fl = append(fl, f)
			}
		}
		if len(fl) == 0 {
			return err
		}
		return &ConflictError{Branch: label, Onto: onto, Files: fl}
	}
	return nil
}

// RebaseBranch rebases a local branch onto `onto`. With upstream set it
// replays only upstream..branch (git rebase --onto). It runs in the worktree
// that has the branch checked out, or in a temporary one, so the caller's
// working tree is never disturbed.
func (r *Repo) RebaseBranch(branch, onto, upstream string) error {
	if wt := r.WorktreeOf(branch); wt != "" {
		w := r.In(wt)
		if !w.Clean() {
			return fmt.Errorf("%s is checked out in %s with uncommitted changes; commit or stash them first", branch, wt)
		}
		return rebaseIn(w, onto, upstream, branch)
	}
	w, cleanup, err := r.AddWorktree(branch)
	if err != nil {
		return err
	}
	defer cleanup()
	if _, err := w.Run("checkout", "--quiet", branch); err != nil {
		return err
	}
	return rebaseIn(w, onto, upstream, branch)
}

// RebaseCommit rebases a detached commit and returns the new tip.
func (r *Repo) RebaseCommit(commit, onto, upstream, label string) (string, error) {
	w, cleanup, err := r.AddWorktree(commit)
	if err != nil {
		return "", err
	}
	defer cleanup()
	if err := rebaseIn(w, onto, upstream, label); err != nil {
		return "", err
	}
	return w.RevParse("HEAD")
}

// ResetBranch points branch at sha, updating its worktree if checked out.
func (r *Repo) ResetBranch(branch, sha string) error {
	if wt := r.WorktreeOf(branch); wt != "" {
		w := r.In(wt)
		if !w.Clean() {
			return fmt.Errorf("%s is checked out in %s with uncommitted changes", branch, wt)
		}
		_, err := w.Run("reset", "--hard", "--quiet", sha)
		return err
	}
	_, err := r.Run("branch", "--force", branch, sha)
	return err
}
