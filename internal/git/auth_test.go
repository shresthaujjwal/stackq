package git

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

// guarded is what authEnvFor adds for every command, starting at entry n.
func guarded(n int) []string {
	k := func(i int) string { return strconv.Itoa(n + i) }
	return []string{
		"GIT_CONFIG_KEY_" + k(0) + "=core.hooksPath", "GIT_CONFIG_VALUE_" + k(0) + "=" + os.DevNull,
		"GIT_CONFIG_KEY_" + k(1) + "=core.fsmonitor", "GIT_CONFIG_VALUE_" + k(1) + "=false",
		"GIT_CONFIG_KEY_" + k(2) + "=credential.helper", "GIT_CONFIG_VALUE_" + k(2) + "=",
	}
}

func TestAuthEnvFor(t *testing.T) {
	cred := base64.StdEncoding.EncodeToString([]byte("x-access-token:tok"))
	header := func(server string, n int) []string {
		k := strconv.Itoa(n)
		return []string{"GIT_CONFIG_KEY_" + k + "=http." + server + "/.extraheader", "GIT_CONFIG_VALUE_" + k + "=AUTHORIZATION: basic " + cred}
	}
	cat := func(parts ...[]string) []string {
		var out []string
		for _, p := range parts {
			out = append(out, p...)
		}
		return out
	}
	cases := []struct {
		name      string
		env       map[string]string
		network   bool
		persisted bool
		want      []string
	}{
		{"GH_TOKEN on github.com", map[string]string{"GH_TOKEN": "tok"}, true, false,
			cat([]string{"GIT_CONFIG_COUNT=4"}, header("https://github.com", 0), guarded(1))},
		{"GITHUB_TOKEN is the fallback", map[string]string{"GITHUB_TOKEN": "tok"}, true, false,
			cat([]string{"GIT_CONFIG_COUNT=4"}, header("https://github.com", 0), guarded(1))},
		{"a GitHub Enterprise server, trailing slash", map[string]string{"GH_TOKEN": "tok", "GITHUB_SERVER_URL": "https://ghe.example.com/"}, true, false,
			cat([]string{"GIT_CONFIG_COUNT=4"}, header("https://ghe.example.com", 0), guarded(1))},
		{"no token", map[string]string{}, true, false, nil},
		{"a checkout already stored credentials: guards only", map[string]string{"GH_TOKEN": "tok"}, true, true,
			cat([]string{"GIT_CONFIG_COUNT=3"}, guarded(0))},
		{"a local command gets the guards but never the token", map[string]string{"GH_TOKEN": "tok"}, false, false,
			cat([]string{"GIT_CONFIG_COUNT=3"}, guarded(0))},
		{"entries from GIT_CONFIG_COUNT are kept, ours go after them", map[string]string{"GH_TOKEN": "tok", "GIT_CONFIG_COUNT": "2"}, true, false,
			cat([]string{"GIT_CONFIG_COUNT=6"}, header("https://github.com", 2), guarded(3))},
		{"a GIT_CONFIG_COUNT we cannot read means we add nothing", map[string]string{"GH_TOKEN": "tok", "GIT_CONFIG_COUNT": "many"}, true, false, nil},
	}
	for _, c := range cases {
		got := authEnvFor(env(c.env), c.network, c.persisted)
		if strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
}

// headerServer records the Authorization headers git sends and refuses the
// request, which is enough: we only look at what was offered.
func headerServer(t *testing.T) (url string, headers func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Values("Authorization")...)
		mu.Unlock()
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string{}, seen...) }
}

func newRepo(t *testing.T, remote string) *Repo {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", remote}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return &Repo{Dir: dir}
}

func TestFetchSendsTheTokenWithoutStoringIt(t *testing.T) {
	url, headers := headerServer(t)
	t.Setenv("GH_TOKEN", "s3cret-token")
	t.Setenv("GITHUB_SERVER_URL", url)
	t.Setenv("HOME", t.TempDir()) // no user-level git config
	r := newRepo(t, url+"/o/r.git")

	_, _ = r.Run("fetch", "--quiet", "origin") // the server answers 404; only the header matters
	want := "basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:s3cret-token"))
	got := headers()
	if len(got) == 0 || got[0] != want {
		t.Fatalf("git did not send the token to the GitHub server: %q", got)
	}
	cfg, _ := os.ReadFile(filepath.Join(r.Dir, ".git", "config"))
	if strings.Contains(string(cfg), "s3cret") || strings.Contains(string(cfg), "extraheader") || strings.Contains(string(cfg), want) {
		t.Fatalf("the token was written to .git/config:\n%s", cfg)
	}
}

func TestFetchKeepsStoredCredentialsAndSendsOneHeader(t *testing.T) {
	url, headers := headerServer(t)
	t.Setenv("GH_TOKEN", "new-token")
	t.Setenv("GITHUB_SERVER_URL", url)
	t.Setenv("HOME", t.TempDir())
	r := newRepo(t, url+"/o/r.git")
	cmd := exec.Command("git", "config", "http."+url+"/.extraheader", "AUTHORIZATION: basic STORED")
	cmd.Dir = r.Dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	_, _ = r.Run("fetch", "--quiet", "origin")
	got := headers()
	if len(got) == 0 || got[0] != "basic STORED" {
		t.Fatalf("a stored credential must win: %q", got)
	}
	for _, h := range got {
		if h != "basic STORED" {
			t.Fatalf("a second, different Authorization header was sent: %q", got)
		}
	}
}

func TestOtherCommandsGetNoCredentials(t *testing.T) {
	t.Setenv("GH_TOKEN", "tok")
	r := &Repo{Dir: t.TempDir()}
	for _, sub := range []string{"status", "rebase", "log", "config", "rev-parse", "worktree"} {
		got := strings.Join(r.authEnv([]string{sub}), "|")
		if strings.Contains(got, "extraheader") || strings.Contains(got, "AUTHORIZATION") {
			t.Errorf("git %s must not receive the token, got %q", sub, got)
		}
		if !strings.Contains(got, "core.hooksPath") {
			t.Errorf("git %s must run with hooks off while a token is present, got %q", sub, got)
		}
	}
}

// The check command can write the repository's config (it runs in a worktree
// of it). A hook it registers must not run while git holds the token.
func TestPushRunsNoHookFromTheRepoConfig(t *testing.T) {
	t.Setenv("GH_TOKEN", "s3cret-token")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("HOME", t.TempDir())
	bare := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", "--bare", bare).CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	r := newRepo(t, bare)
	hooks := t.TempDir()
	leak := filepath.Join(t.TempDir(), "leak")
	script := "#!/bin/sh\nenv > " + leak + "\n"
	if err := os.WriteFile(filepath.Join(hooks, "pre-push"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "x"},
		{"config", "core.hooksPath", hooks},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = r.Dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if _, err := r.Run("push", "--quiet", "origin", "HEAD:refs/heads/x"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(leak); err == nil {
		t.Fatal("a pre-push hook from the repo config ran during an authenticated push")
	}
	// Local commands run under the same process, so the same goes for them.
	if err := os.WriteFile(filepath.Join(hooks, "post-checkout"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Run("checkout", "-q", "-b", "other"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(leak); err == nil {
		t.Fatal("a post-checkout hook from the repo config ran while a token was present")
	}
	// Without a token nothing is overridden, so the same hook runs.
	t.Setenv("GH_TOKEN", "")
	if _, err := r.Run("push", "--quiet", "origin", "HEAD:refs/heads/y"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(leak); err != nil {
		t.Fatal("the control push did not run the hook; the test proves nothing")
	}
}
