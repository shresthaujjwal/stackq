package git

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestAuthEnvFor(t *testing.T) {
	cred := base64.StdEncoding.EncodeToString([]byte("x-access-token:tok"))
	cases := []struct {
		name      string
		env       map[string]string
		persisted bool
		want      []string
	}{
		{"GH_TOKEN on github.com", map[string]string{"GH_TOKEN": "tok"}, false, []string{
			"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.https://github.com/.extraheader", "GIT_CONFIG_VALUE_0=AUTHORIZATION: basic " + cred}},
		{"GITHUB_TOKEN is the fallback", map[string]string{"GITHUB_TOKEN": "tok"}, false, []string{
			"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.https://github.com/.extraheader", "GIT_CONFIG_VALUE_0=AUTHORIZATION: basic " + cred}},
		{"a GitHub Enterprise server, trailing slash", map[string]string{"GH_TOKEN": "tok", "GITHUB_SERVER_URL": "https://ghe.example.com/"}, false, []string{
			"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.https://ghe.example.com/.extraheader", "GIT_CONFIG_VALUE_0=AUTHORIZATION: basic " + cred}},
		{"no token", map[string]string{}, false, nil},
		{"a checkout already stored credentials", map[string]string{"GH_TOKEN": "tok"}, true, nil},
		{"entries from GIT_CONFIG_COUNT are kept, ours goes after them", map[string]string{"GH_TOKEN": "tok", "GIT_CONFIG_COUNT": "2"}, false, []string{
			"GIT_CONFIG_COUNT=3", "GIT_CONFIG_KEY_2=http.https://github.com/.extraheader", "GIT_CONFIG_VALUE_2=AUTHORIZATION: basic " + cred}},
		{"a GIT_CONFIG_COUNT we cannot read means we add nothing", map[string]string{"GH_TOKEN": "tok", "GIT_CONFIG_COUNT": "many"}, false, nil},
	}
	for _, c := range cases {
		got := authEnvFor(env(c.env), c.persisted)
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
		if got := r.authEnv([]string{sub}); got != nil {
			t.Errorf("git %s must not receive the token, got %q", sub, got)
		}
	}
}
