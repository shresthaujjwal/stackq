// Package testrepo builds throwaway git repositories for tests.
package testrepo

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"stackq/internal/git"
)

// Delete as a file value removes the file.
const Delete = "\x00delete"

func env(t testing.TB) []string {
	home := t.TempDir()
	return []string{
		"HOME=" + home, "XDG_CONFIG_HOME=" + home, "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
	}
}

func configure(t testing.TB, r *git.Repo) {
	Must(t, r, "config", "user.name", "Test")
	Must(t, r, "config", "user.email", "test@example.com")
	Must(t, r, "config", "commit.gpgsign", "false")
}

// New creates a repository on branch main with no commits.
func New(t testing.TB) *git.Repo {
	t.Helper()
	r := &git.Repo{Dir: t.TempDir(), Env: env(t)}
	Must(t, r, "init", "-q", "-b", "main")
	configure(t, r)
	return r
}

// WithOrigin adds a bare "origin" remote and pushes main to it.
func WithOrigin(t testing.TB, r *git.Repo) *git.Repo {
	t.Helper()
	bare := &git.Repo{Dir: t.TempDir(), Env: r.Env}
	Must(t, bare, "init", "-q", "--bare", "-b", "main")
	Must(t, r, "remote", "add", "origin", bare.Dir)
	Must(t, r, "push", "-q", "origin", "main")
	Must(t, r, "fetch", "-q", "origin")
	return bare
}

// Clone makes a fresh clone, like a CI checkout.
func Clone(t testing.TB, bare *git.Repo) *git.Repo {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "clone")
	parent := &git.Repo{Dir: filepath.Dir(dir), Env: bare.Env}
	Must(t, parent, "clone", "-q", bare.Dir, dir)
	r := &git.Repo{Dir: dir, Env: bare.Env}
	configure(t, r)
	return r
}

func Must(t testing.TB, r *git.Repo, args ...string) string {
	t.Helper()
	out, err := r.Run(args...)
	if err != nil {
		t.Fatalf("%v", err)
	}
	return out
}

// Commit writes files into the working tree and commits everything.
func Commit(t testing.TB, r *git.Repo, msg string, files map[string]string) string {
	t.Helper()
	for p, content := range files {
		full := filepath.Join(r.Dir, p)
		if content == Delete {
			if err := os.Remove(full); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	Must(t, r, "add", "-A")
	Must(t, r, "commit", "-q", "-m", msg)
	return Must(t, r, "rev-parse", "HEAD")
}

// GoApp creates main with a small Go module plus a "feature" branch that
// adds packages, rewires the entrypoint and deletes an old package.
func GoApp(t testing.TB) *git.Repo {
	t.Helper()
	r := New(t)
	Commit(t, r, "base", map[string]string{
		"go.mod":              "module example.com/app\n\ngo 1.22\n",
		"internal/old/old.go": "package old\n\nfunc Old() int { return 1 }\n",
		"cmd/app/main.go":     "package main\n\nimport \"example.com/app/internal/old\"\n\nfunc main() { _ = old.Old() }\n",
		"README.md":           "# app\n",
	})
	Must(t, r, "checkout", "-q", "-b", "feature")
	Commit(t, r, "one big change", map[string]string{
		"go.mod":                  "module example.com/app\n\ngo 1.22\n\n// split me\n",
		"internal/types/types.go": "package types\n\ntype ID string\n",
		"internal/core/core.go":   "package core\n\nimport \"example.com/app/internal/types\"\n\nfunc New() types.ID { return \"x\" }\n",
		"cmd/app/main.go":         "package main\n\nimport \"example.com/app/internal/core\"\n\nfunc main() { _ = core.New() }\n",
		"internal/old/old.go":     Delete,
		"docs/guide.md":           "# guide\n",
	})
	return r
}

// Lines splits command output into non-empty lines.
func Lines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

// RebaseMerge mimics GitHub's "rebase and merge": it replays the commits of
// from..branch on top of the remote trunk (new SHAs) and pushes trunk.
// When deleteBranch is set, the remote branch is removed afterwards, like
// "automatically delete head branches".
func RebaseMerge(t testing.TB, bare *git.Repo, from, branch string, deleteBranch bool) {
	t.Helper()
	w := Clone(t, bare)
	// GitHub always writes new SHAs on rebase-merge; a different committer
	// date guarantees that here even when trunk hasn't moved.
	w.Env = append(append([]string{}, w.Env...), "GIT_COMMITTER_DATE=2030-01-01T00:00:00Z")
	Must(t, w, "checkout", "-q", "main")
	Must(t, w, "fetch", "-q", "origin")
	Must(t, w, "cherry-pick", "--allow-empty", from+".."+"origin/"+branch)
	Must(t, w, "push", "-q", "origin", "main")
	if deleteBranch {
		Must(t, w, "push", "-q", "origin", "--delete", branch)
	}
}

// FlagApp creates main with a small HTTP service and a flags package, plus
// a "feature" branch that adds a flag (default off), a new package, wires a
// route and enables the flag in prod config. guard picks how the route is
// wired: "if" (inside if flags.Enabled), "return" (early-return guard) or
// "none" (unguarded).
func FlagApp(t testing.TB, guard string) *git.Repo {
	t.Helper()
	r := New(t)
	Commit(t, r, "base", map[string]string{
		"go.mod":                  "module example.com/shop\n\ngo 1.22\n",
		"internal/flags/flags.go": "package flags\n\nvar defaults = map[string]bool{}\n\n// Enabled reports whether a feature is on.\nfunc Enabled(name string) bool { return defaults[name] }\n",
		"cmd/shop/main.go":        "package main\n\nimport \"net/http\"\n\nfunc main() {\n\tmux := http.NewServeMux()\n\tmux.HandleFunc(\"/\", func(http.ResponseWriter, *http.Request) {})\n\t_ = mux\n}\n",
		"config/prod.yaml":        "features: {}\n",
	})
	Must(t, r, "checkout", "-q", "-b", "feature")
	wire := map[string]string{
		"if":     "\tif flags.Enabled(flags.NewCheckout) {\n\t\tmux.HandleFunc(\"/checkout\", checkout.Handler)\n\t}\n",
		"return": "\tregister(mux)\n",
		"none":   "\tmux.HandleFunc(\"/checkout\", checkout.Handler)\n",
	}[guard]
	extra := ""
	if guard == "return" {
		extra = "\nfunc register(mux *http.ServeMux) {\n\tif !flags.Enabled(flags.NewCheckout) {\n\t\treturn\n\t}\n\tmux.HandleFunc(\"/checkout\", checkout.Handler)\n}\n"
	}
	Commit(t, r, "new checkout behind a flag", map[string]string{
		"internal/flags/defs.go":        "package flags\n\n// NewCheckout gates the new checkout flow. Off by default.\nconst NewCheckout = \"new-checkout\"\n\nfunc init() { defaults[NewCheckout] = false }\n",
		"internal/checkout/checkout.go": "package checkout\n\nimport \"net/http\"\n\n// Handler serves the new checkout.\nfunc Handler(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }\n",
		"cmd/shop/main.go": "package main\n\nimport (\n\t\"net/http\"\n\n\t\"example.com/shop/internal/checkout\"\n\t\"example.com/shop/internal/flags\"\n)\n\n" +
			"func main() {\n\tmux := http.NewServeMux()\n\tmux.HandleFunc(\"/\", func(http.ResponseWriter, *http.Request) {})\n" + wire + "\t_ = mux\n}\n" + extra,
		"config/prod.yaml": "features:\n  new-checkout: true\n",
	})
	return r
}

// MergeCommitMerge mimics GitHub's "create a merge commit".
func MergeCommitMerge(t testing.TB, bare *git.Repo, branch string, deleteBranch bool) {
	t.Helper()
	w := Clone(t, bare)
	Must(t, w, "checkout", "-q", "main")
	Must(t, w, "merge", "-q", "--no-ff", "-m", "Merge "+branch, "origin/"+branch)
	Must(t, w, "push", "-q", "origin", "main")
	if deleteBranch {
		Must(t, w, "push", "-q", "origin", "--delete", branch)
	}
}

// SquashMerge mimics GitHub's "squash and merge".
func SquashMerge(t testing.TB, bare *git.Repo, branch string, deleteBranch bool) {
	t.Helper()
	w := Clone(t, bare)
	Must(t, w, "checkout", "-q", "main")
	Must(t, w, "merge", "-q", "--squash", "origin/"+branch)
	Must(t, w, "commit", "-q", "-m", "Squashed "+branch)
	Must(t, w, "push", "-q", "origin", "main")
	if deleteBranch {
		Must(t, w, "push", "-q", "origin", "--delete", branch)
	}
}
