package stack

import (
	"strings"
	"testing"
)

// The check command comes from .stackq.json on the branch being split, so it
// must not be able to read the tokens the workflow runs with.
func TestCheckCommandDoesNotSeeCredentials(t *testing.T) {
	for _, name := range []string{"GH_TOKEN", "GITHUB_TOKEN", "STACKQ_TOKEN", "ACTIONS_ID_TOKEN_REQUEST_TOKEN", "ACTIONS_RUNTIME_TOKEN"} {
		t.Setenv(name, "s3cret-"+name)
	}
	t.Setenv("KEEP_ME", "visible")
	out, err := runCheck(t.TempDir(), `env`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "s3cret") {
		t.Fatalf("the check command saw a credential:\n%s", out)
	}
	if !strings.Contains(out, "KEEP_ME=visible") || !strings.Contains(out, "PATH=") {
		t.Fatalf("the check command lost its normal environment:\n%s", out)
	}
}

func TestEnvWithoutCredentialsKeepsLookalikes(t *testing.T) {
	got := envWithoutCredentials([]string{"GH_TOKEN=a", "GH_TOKEN_FILE=b", "MY_GITHUB_TOKEN=c", "PATH=/bin", "GITHUB_TOKEN="})
	want := "GH_TOKEN_FILE=b MY_GITHUB_TOKEN=c PATH=/bin"
	if strings.Join(got, " ") != want {
		t.Fatalf("got %v, want %v", got, want)
	}
}
