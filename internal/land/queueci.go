package land

import (
	"fmt"
	"strings"
	"time"

	"stackq/internal/forge"
)

// The merge queue tests each entry on a temporary branch: trunk as it is now,
// plus the PR. GitHub starts merge_group workflows there, except when the
// entry was added with the built-in GITHUB_TOKEN (events that token causes
// start no workflows). Then nothing tests the merged result, and if the CI
// check is required the queue waits for it until the entry times out.
//
// With QueueCI set, stackq starts that workflow on the queue's branch itself,
// through workflow_dispatch, which the built-in token may do. The check it
// reports is on the merge group's commit, so it is what the queue waits for.

// queueWait is how long startQueueCI waits for the queue to create its
// branch after an enqueue.
func (c *Runner) queueWait() time.Duration {
	if c.QueueWait > 0 {
		return c.QueueWait
	}
	return time.Minute
}

// startQueueCI starts c.QueueCI on the merge queue's branch for pr, waiting
// up to wait for the queue to create it. It does nothing if QueueCI is unset,
// or if the branch's commit already has checks (a merge_group run, or an
// earlier start). Failures are logged, never returned: the entry is queued
// either way, and the next run tries again.
func (c *Runner) startQueueCI(pr forge.PR, trunk string, wait time.Duration) {
	if c.QueueCI == "" {
		return
	}
	deadline := time.Now().Add(wait)
	for {
		if branch, sha := c.queueBranch(pr.Number, trunk); branch != "" {
			if v, err := c.Forge.CommitChecks(sha); err == nil && v != "none" {
				return
			}
			if err := c.Forge.StartCI(c.QueueCI, branch); err != nil {
				c.logf("! could not start %s on %s: %v", c.QueueCI, branch, err)
				return
			}
			c.logf("▶ started %s on %s: the queue's test of #%d on the latest %s", c.QueueCI, branch, pr.Number, trunk)
			return
		}
		left := time.Until(deadline)
		if left <= 0 {
			if wait > 0 {
				c.logf("… the merge queue has no branch for #%d yet; the next stackq run checks again", pr.Number)
			}
			return
		}
		time.Sleep(min(3*time.Second, left))
	}
}

// queueBranch finds the merge queue's branch for PR n on trunk
// (gh-readonly-queue/<trunk>/pr-<n>-<sha>) and its tip. A rebuilt group gets a
// new branch; the last one listed wins.
func (c *Runner) queueBranch(n int, trunk string) (branch, sha string) {
	out, err := c.Repo.Run("ls-remote", "--heads", c.Remote, "refs/heads/gh-readonly-queue/"+trunk+"/*")
	if err != nil {
		return "", ""
	}
	want := fmt.Sprintf("/pr-%d-", n)
	for _, line := range strings.Split(out, "\n") {
		s, ref, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if ok && strings.Contains(ref, want) {
			branch, sha = strings.TrimPrefix(ref, "refs/heads/"), s
		}
	}
	return branch, sha
}
