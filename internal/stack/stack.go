// Package stack creates, stores and maintains a stack of dependent branches.
package stack

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"stackq/internal/git"
)

type Branch struct {
	Name  string `json:"name"`
	Title string `json:"title,omitempty"`
	// Parent is the branch this one is stacked on (the trunk for the bottom).
	Parent string `json:"parent"`
	// ParentTip is the parent commit this branch was last built on, so a
	// rewritten or merged parent can be cut away with rebase --onto.
	ParentTip string `json:"parentTip,omitempty"`
	// PushedSHA is what stackq last pushed, used as the force-with-lease
	// guard and to tell local work apart from CI restacks.
	PushedSHA string `json:"pushedSha,omitempty"`
	PR        int    `json:"pr,omitempty"`
	Merged    bool   `json:"merged,omitempty"`
	// Hold marks the activation branch: it lands separately, by hand.
	Hold bool `json:"hold,omitempty"`
}

type Stack struct {
	Name   string `json:"name"`
	Trunk  string `json:"trunk"`
	Remote string `json:"remote,omitempty"`
	// Landing is "atomic" (the whole stack is one merge-queue entry) or
	// "incremental" (one PR at a time). Empty means incremental.
	Landing string `json:"landing,omitempty"`
	Source  string `json:"source,omitempty"`
	// SourcePR is the original big PR, if there is one.
	SourcePR int `json:"sourcePr,omitempty"`
	// SourceNotified records that the original PR got its link comment.
	SourceNotified bool      `json:"sourceNotified,omitempty"`
	Branches       []*Branch `json:"branches"`
}

// Active returns the branches that have not merged yet, bottom first.
func (s *Stack) Active() []*Branch {
	var out []*Branch
	for _, b := range s.Branches {
		if !b.Merged {
			out = append(out, b)
		}
	}
	return out
}

func (s *Stack) Find(name string) *Branch {
	for _, b := range s.Branches {
		if b.Name == name {
			return b
		}
	}
	return nil
}

func (s *Stack) Index(name string) int {
	for i, b := range s.Branches {
		if b.Name == name {
			return i + 1
		}
	}
	return 0
}

// TrunkRef is the remote-tracking trunk when there is one.
func (s *Stack) TrunkRef(r *git.Repo) string {
	if s.Remote != "" {
		ref := s.Remote + "/" + s.Trunk
		if r.RefExists("refs/remotes/" + ref) {
			return ref
		}
	}
	return s.Trunk
}

// Marker returns the PR metadata for branch b.
func (s *Stack) Marker(b *Branch) Marker {
	return Marker{Stack: s.Name, Index: s.Index(b.Name), Total: len(s.Branches), Parent: b.Parent, Trunk: s.Trunk,
		Source: s.SourcePR, Landing: s.Landing, Hold: b.Hold}
}

// Nav returns the stack overview rendered into every PR.
func (s *Stack) Nav() []NavEntry {
	var nav []NavEntry
	for i, b := range s.Branches {
		nav = append(nav, NavEntry{Index: i + 1, PR: b.PR, Branch: b.Name, Merged: b.Merged, Hold: b.Hold})
	}
	return nav
}

func stateDir(r *git.Repo) (string, error) {
	common, err := r.CommonDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(common, "stackq"), nil
}

func (s *Stack) Save(r *git.Repo) error {
	dir, err := stateDir(r)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, s.Name+".json.tmp")
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, s.Name+".json"))
}

func Load(r *git.Repo, name string) (*Stack, error) {
	dir, err := stateDir(r)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(dir, name+".json"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no stack named %q (run stackq list)", name)
		}
		return nil, err
	}
	var s Stack
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("stack %q: %w", name, err)
	}
	return &s, nil
}

func List(r *git.Repo) ([]string, error) {
	dir, err := stateDir(r)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if n, ok := strings.CutSuffix(e.Name(), ".json"); ok && !e.IsDir() {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names, nil
}

// Resolve picks a stack: the named one, the one containing the current
// branch, or the only one.
func Resolve(r *git.Repo, name string) (*Stack, error) {
	if name != "" {
		return Load(r, name)
	}
	names, err := List(r)
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("no stacks yet; create one with stackq split")
	}
	cur := r.CurrentBranch()
	for _, n := range names {
		s, err := Load(r, n)
		if err != nil {
			continue
		}
		if s.Find(cur) != nil || s.Source == cur {
			return s, nil
		}
	}
	if len(names) == 1 {
		return Load(r, names[0])
	}
	return nil, fmt.Errorf("several stacks exist (%s); pick one with --stack", strings.Join(names, ", "))
}

// SafeName makes a stack or PR name safe inside a branch name.
func SafeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-.")
}
