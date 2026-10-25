package land_test

import (
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"stackq/internal/forge"
	"stackq/internal/land"
	"stackq/internal/stack"
	"stackq/internal/testrepo"
)

// With the built-in token GitHub starts no CI for the merge queue's entry, so
// stackq starts it on the queue's branch: trunk as it is now plus the stack.
func TestQueueCIRunsOnTheMergeGroupBranch(t *testing.T) {
	fx := setupWith(t, testrepo.FlagApp(t, "if"), shopFlags, stack.Atomic)
	lander := prBy(t, fx, 2)
	sha := testrepo.Must(t, fx.bare, "rev-parse", "refs/heads/"+lander.HeadRef)
	group := "gh-readonly-queue/main/pr-" + strconv.Itoa(lander.Number) + "-" + sha[:12]
	var wg sync.WaitGroup
	t.Cleanup(wg.Wait)
	fx.f.OnEnqueue = func(forge.PR) {
		// GitHub builds the group a moment after the enqueue.
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(50 * time.Millisecond)
			if _, err := fx.bare.Run("update-ref", "refs/heads/"+group, sha); err != nil {
				t.Error(err)
			}
		}()
	}
	run := &land.Runner{Repo: fx.dev, Forge: fx.f, Remote: "origin", QueueCI: "ci.yml", QueueWait: 5 * time.Second}
	if err := run.AutoLand(); err != nil {
		t.Fatal(err)
	}
	if len(fx.f.Started) != 1 || fx.f.Started[0] != "ci.yml@"+group {
		t.Fatalf("CI not started on the merge group branch %q: %q", group, fx.f.Started)
	}

	// The next tick sees the lander queued; CI is already running there.
	fx.f.CI = map[string]string{sha: "pending"}
	if err := run.AutoLand(); err != nil {
		t.Fatal(err)
	}
	if len(fx.f.Started) != 1 {
		t.Fatalf("CI started twice on the same commit: %q", fx.f.Started)
	}
}

// If the group branch shows up only after the run gave up waiting, the next
// tick starts CI; with QueueCI unset nothing is started and nothing waits.
func TestQueueCIFallsBackToTheNextTickAndIsOffByDefault(t *testing.T) {
	fx := setupWith(t, testrepo.FlagApp(t, "if"), shopFlags, stack.Atomic)
	run := &land.Runner{Repo: fx.dev, Forge: fx.f, Remote: "origin", QueueCI: "ci.yml", QueueWait: 20 * time.Millisecond}
	if err := run.AutoLand(); err != nil {
		t.Fatal(err)
	}
	if len(fx.f.Started) != 0 {
		t.Fatalf("started CI without a merge group branch: %q", fx.f.Started)
	}
	lander := prBy(t, fx, 2)
	sha := testrepo.Must(t, fx.bare, "rev-parse", "refs/heads/"+lander.HeadRef)
	group := "gh-readonly-queue/main/pr-" + strconv.Itoa(lander.Number) + "-abc"
	testrepo.Must(t, fx.bare, "update-ref", "refs/heads/"+group, sha)
	if err := run.AutoLand(); err != nil {
		t.Fatal(err)
	}
	if len(fx.f.Started) != 1 || !strings.HasSuffix(fx.f.Started[0], group) {
		t.Fatalf("the next tick did not start CI on %s: %q", group, fx.f.Started)
	}

	off := setupWith(t, testrepo.FlagApp(t, "if"), shopFlags, stack.Atomic)
	start := time.Now()
	plain := &land.Runner{Repo: off.dev, Forge: off.f, Remote: "origin"}
	if err := plain.AutoLand(); err != nil {
		t.Fatal(err)
	}
	if len(off.f.Started) != 0 || time.Since(start) > 30*time.Second {
		t.Fatalf("queueCI unset must start nothing and not wait: %q after %s", off.f.Started, time.Since(start))
	}
}
