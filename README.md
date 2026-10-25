# stackq

Split one large branch into a stack of small PRs, then land them through
GitHub's merge queue without conflicts or manual restacking.

```
feature/new-checkout ──split──▶  01-flags → 02-checkout → 03-cmd-shop → 🔒 04-activation
                                 (each PR reviewed on its own, showing only its own diff)
                                                     │
                     stackq land: every part approved + green → 03 retargeted to main,
                     queued once → main gets 01–03 together, or nothing
                                                     │
                     🔒 04 is rebased onto main and waits for a human to switch the feature on
```

## Why the merge queue needs special handling

GitHub's merge queue only accepts PRs that target the protected branch. A
stack's upper PRs target the branch below them, so they cannot be queued
together. Atomic landing (the default) avoids the problem: the top PR already
contains every part, so it is retargeted to `main` and queued as one entry.

In incremental mode, things are harder. After the queue rebase-merges the
bottom PR, its commits land on `main` with **new SHAs**. The next PR still
carries the old ones, so a naive retarget shows duplicated changes or
conflicts. stackq handles this too. When a stack PR merges, its Action:

1. rebases the next branch with `git rebase --onto main <parent's pre-merge head>`, which replays only that branch's own commits,
2. force-pushes it with a lease, so it never overwrites someone else's push,
3. retargets the PR to `main` and refreshes the stack overview in every PR body,
4. queues it, or enables auto-merge so it joins the queue once checks pass.

If the queue drops a PR, or a restack conflicts, the stack pauses at that PR.
stackq leaves a comment saying which files conflict and adds the
`stackq:conflict` or `stackq:blocked` label. Nothing above that PR moves until
you fix it.

## Install

```sh
go install ./cmd/stackq   # from a checkout of this repo; needs Go 1.22+, git 2.30+ and the gh CLI
```

## Quick start

```sh
git checkout feature/big-refactor
stackq plan                               # preview the slices, nothing is written
stackq split --check "go build ./..."     # create stack/big-refactor/01-… branches
stackq push --close-source                # push, open the stacked PRs, link + close the original PR
stackq land                               # once every part is approved: queue the whole stack as one entry
```

While the stack is in review:

```sh
git checkout stack/big-refactor/02-internal-types
# … address review comments, commit …
stackq sync     # rebases every branch above it, replaying only their own commits
stackq push     # lease-protected force push, keeps PR bases and bodies current
stackq status
```

`stackq sync` also picks up branches that CI restacked, and marks branches
that have already landed.

## Landing: all at once by default

**Atomic** (default, `"landing": "atomic"`): every part is reviewed on its own PR. `stackq land` then checks every part:
- approved (`minApprovals`, default 1),
- no requested changes,
- checks not red or running,
- still stacked on the part below.

If all pass, it retargets the top PR to `main` and queues it **once**, so the queue merges the whole stack as a single entry. Trunk never holds half a stack. After the merge, CI closes the lower parts, deletes their branches, and posts the rollback command.

**Incremental** (`--landing incremental`): one PR at a time. Each merge restacks and queues the next PR.

**Rollback:**

```sh
stackq revert --pr <top PR>              # one PR reverting the whole stack on today's main
stackq revert --pr <top PR> --enqueue    # …and queue it
```

This works for rebase, merge-commit and squash merges. Before opening the revert, stackq checks that it touches exactly the files the stack touched. If the feature is behind a flag, turning the flag off is faster.

## Feature flags first

```json
"flags": {
  "paths": ["internal/flags/"],
  "activation": ["config/env/prod/*.yaml"],
  "guards": ["flags.Enabled"],
  "entrypoints": ["HandleFunc", "RegisterWorkflow"]
}
```

- **Flag definitions** land in the first slice, before any code.
- **Activation files** (whatever turns the feature on) go into a final 🔒 draft PR. stackq never queues it; once the rest has landed, CI rebases it onto `main` and leaves it for a human to merge.
- **The guard lint** (Go) reports any new entrypoint call that isn't inside an `if flags.Enabled(...)` or after an early-return guard. Set `"enforce": true` to fail the split on it.

## Priority in the merge queue

GitHub's queue is first in, first out. Its only shortcut, "jump to the front", makes every in-flight entry rebuild. stackq adds priority without rebuilding the queue constantly:

- **The pool.** PRs ready to merge get the `stackq:queue` label (`stackq queue --pr N`, or `stackq land`).
- **Admission.** stackq keeps GitHub's queue short (`maxInFlight`, default 3). Whenever a slot frees up, it queues the **best** waiting PR, not the oldest.
- **Jumps.** Vulnerability fixes and reverts jump to the front, but only one jumped entry is allowed in flight at a time (`maxJumpsInFlight`).
- **Order of the rest:** Dependabot/Renovate/`automerge` PRs, then small PRs (≤ 50 lines, ≤ 5 files), then everything else.
- **Aging.** Every 30 minutes of waiting adds a point, so big PRs never starve.

```json
"priority": { "enabled": true, "maxInFlight": 3 }
```

The default classes come built in; override them in `priority.classes` (see `.stackq.example.json`). A PR counts as urgent if it has a `security`/`hotfix` label, a `hotfix/*` branch, or a CVE/GHSA id in its title or body. To see what the next pass would do, run `stackq schedule --dry-run`.

PRs queued outside stackq ("Merge when ready", GitHub auto-merge) still enter GitHub's queue directly, first in, first out. Route merges through the label to get priority.

## The original PR

If the big branch already has an open PR, `stackq push` finds it (or pass `--from-pr N`).
- **Bottom PR:** starts with that PR's description, credited "Split from #N".
- **Every stack PR:** says "Split from #N" in its stack block.
- **The original PR:** gets one comment linking every slice. With `--close-source` (or `"closeSource": true` in config) it is also closed, so nobody merges it alongside the stack.
- **When the last slice merges:** CI posts "✅ fully landed" on the original PR.
- **To leave it alone entirely:** use `--no-source-pr`.

## How the split is decided

1. **Diff.** The diff runs from the merge base with trunk to the big branch, with rename detection.
2. **Groups.** Files are grouped by directory (a Go package or a TS folder). Dependency manifests (`go.mod`, `go.sum`, `package.json`, lockfiles) form their own group, which always lands first.
3. **Order.** Groups are ordered by import graph:
   - **Go imports** are parsed with `go/parser` and resolved through every `go.mod` in the repo.
   - **JS/TS relative imports** cover `import`, `export … from`, `require()` and dynamic `import()`.
   - **Deletions** land only after every file that used to import the deleted code has been updated, so deleting `internal/old` can't break a caller that changes in a later slice.
   - **Import cycles** are kept together in one slice, and stackq prints a warning.
   - **Ties** go to contracts (proto, schema, types), then libraries, entrypoints, infra, and docs.
4. **Packing.** Groups are packed into slices up to `--max-lines` (default 400 changed lines).
5. **Rules.** `slices` rules in `.stackq.json` pin files into named slices that keep their order.
6. **Check.** `--check` runs a command, such as a build, in a throwaway worktree for each slice. If a slice fails, it is folded into the next one, so every branch in the stack builds.
7. **Verification.** stackq verifies that the top of the stack is byte-for-byte the original branch. Your working tree is never touched.

## GitHub setup

1. **Merge queue.** Turn on the merge queue for `main` in the branch protection rules. Rebase, merge, and squash all work in CI. For local `stackq sync` landing detection, prefer rebase or merge (see Limits).
2. **CI triggers.** Add `merge_group:` and `workflow_dispatch:` to your CI workflow's triggers, and `push:` on your default branch so what lands is checked too. See `examples/ci-merge-group.yml`.
3. **stackq workflow.** Copy `examples/stackq.yml` to `.github/workflows/`, and point `uses:` at wherever you host this repo.
4. **Token.** Give the workflow a GitHub App token or a fine-grained PAT with contents and pull-requests write access. Pushes made with the default `GITHUB_TOKEN` don't trigger other workflows, so a restacked PR would never get its checks.

The files in `examples/` (`ci-merge-group.yml`, `stackq.yml`, `stackq-split.yml`) are meant to be copied unchanged apart from `YOUR_ORG`, so a repo's copies can be diffed against them to catch drift.

To split from GitHub instead of a laptop, copy `examples/stackq-split.yml`. Label a big PR `stackq:split` (create the label once) and the Action splits its branch, opens the stacked PRs, links them from the original PR, closes it, and removes the label. You can also run the workflow by hand with a branch name. It uses a `STACKQ_TOKEN` secret if you set one, and the built-in token otherwise (then enable "Allow GitHub Actions to create and approve pull requests"). PRs from forks are ignored.

**Landing by itself.** Set `"autoLand": true` in `.stackq.json` and an atomic stack lands as soon as the last part is approved and green. The workflow checks on every review and every finished CI run (a scheduled run is only a safety net: GitHub may start it hours late); a stack that isn't ready gets no comments or labels, and one that is already landing is not queued twice. The top PR of the stack shows where the stack stands in a short status section of its description: what each part still waits for (failing or running checks, missing approvals), waiting in the priority pool, in the merge queue, or paused. It is rewritten only when it changes and removed once the stack lands.

**Required checks need an App or PAT token.** With the built-in token, do not make your CI check a *required* status check on trunk. This was tried on live GitHub and it fails in two places. GitHub only counts a required check from CI that a pull request or the merge queue started, and the CI stackq starts with `workflow_dispatch` does not count: (1) the lander cannot join the queue until someone approves the PR's own CI run (GitHub holds it as "awaiting approval"), and stackq falls back to auto-merge; (2) the queue then waits on its temporary branch for a check that is never accepted, and the entry stays `AWAITING_CHECKS`. The name was right (`test`, from GitHub Actions); the event was the problem. Without a required check the stack lands, but the merged result is tested only afterwards, by CI on the push to trunk. For a required check, give stackq a GitHub App token (or a PAT) as `STACKQ_TOKEN`: pushes and queue entries made with it start `pull_request` and `merge_group` runs by themselves. `stackq doctor` warns about this.

**Built-in token vs. an App or PAT.** The Action works with just `GITHUB_TOKEN`, with two limits that come from GitHub, not from stackq: events that token causes do not start workflows, and the merge queue does not run `merge_group` workflows for entries it added. stackq works around the first (the stackq run that follows CI on the landed commit tidies the stack, and a step starts CI on stack branches), but the second means the queue merges without CI on the merged result; each part's CI has already passed, so this is safe unless `main` moved in between. Give stackq a GitHub App token or a PAT as `STACKQ_TOKEN` to get queue CI and immediate cleanup. The example CI's `push` trigger runs on every commit that lands, so a problem the queue could not catch still shows up right after the merge.

Manual controls, through workflow_dispatch or locally:

```sh
stackq land --pr 123                 # retry after fixing a blocked PR
stackq ci after-merge --pr 122       # re-run the restack for a merged PR
```

## Config: `.stackq.json`

```json
{
  "trunk": "main",
  "prefix": "stack/{name}/",
  "maxLines": 400,
  "check": "go build ./... && go vet ./...",
  "slices": [
    { "name": "proto", "paths": ["proto/", "*.proto"] },
    { "name": "migrations", "paths": ["/db/migrations/"] }
  ]
}
```

Patterns follow gitignore semantics:
- `*` stays inside one path segment.
- `**` crosses segments.
- A pattern without a `/` matches at any depth.
- A leading `/` anchors the pattern to the repo root.

## Where state lives

- **Local stack layout** is stored in `.git/stackq/<name>.json`. It is shared across worktrees and never committed.
- **CI needs no local state.** Each PR body carries a hidden `<!-- stackq:meta … -->` marker with the stack name, position, and parent. Text you write in a PR body outside the stackq block is preserved.

## Limits

- **File-level splitting only.** A file never spans two slices. If one file must be split, do it by hand with `git restore -p --source=<big-branch>` on the relevant branch, then run `stackq sync`.
- **API-level coupling.** The import graph can't see that a package's exported types changed under its callers (for example a function that now returns a renamed type). `--check` catches it by folding the failing slice into the next one, which can pull unrelated slices along. Add a `slices` rule naming the coupled packages so only they ship together.
- **Import resolution.** TS path aliases (`@/…`) and bare package imports are not resolved. Use `slices` rules or `--check` to keep those orders right.
- **Squash merges.** Local `stackq sync` detects landed branches by patch equivalence (`git cherry`). That works for rebase and merge commits, but not squash. The CI restack works with every merge method, because it cuts at the parent's recorded pre-merge head.
- **Atomic mode is verified end to end on live GitHub** (built-in token only), but not with required approvals or CODEOWNERS on the retargeted top PR: the sandbox used `allowUnreviewed`. Check how those rules treat the lander, and make sure CI runs on PRs targeting `stack/**` branches. See SPEC §28.6.
- **Incremental mode** takes one queue run per PR, since PRs that don't target `main` can't be queued.
- **Guard lint** covers Go only, and is a lint, not a proof (SPEC §29.3).

## Checking the setup

`stackq doctor` checks, without changing anything, that the repository is set up the way stackq needs: the config, a merge queue on trunk (and a note on required status checks, which need an App or PAT token), auto-merge and rebase merging, whether Actions may open PRs, the workflow files (the `checks: read` / `statuses: read` permissions, CI triggered on `push`, `merge_group` and `workflow_dispatch`, the CI name `workflow_run` waits for) and the `stackq:split` label. Each finding is ✓, ! (warning), ✗ (problem) or ? (could not check, with what to look at by hand), and any ✗ makes it exit non-zero. Run it locally, or from the Actions tab (the `stackq` workflow, command `doctor`) to check with the workflow's own token.

## Cleaning up branches

`stackq gc` lists remote branches left behind; `stackq gc --delete` deletes them, and so does the `stackq` workflow run by hand with command `gc` (it uses the workflow's token). A branch is deleted only if no open PR uses it, its tip is still exactly the head of its last PR, and that PR merged, or it is a stack or revert branch whose PR was closed, or it is the source of a stack that is completely finished. Branches that never had a PR are left alone.

## Security

stackq runs with a token that can push branches and change `main` through the merge queue, so it treats everything a stranger can write as hostile:

- A stack marker in a PR body counts only for a PR whose head branch is in this repository and is named like a stack branch (`stack/<name>/…`). A PR from a fork with a copied marker is ignored.
- The build `check` from `.stackq.json` never runs under a process that holds the token: the Action gives `split` no token, and git commands that get the token ignore hooks and helpers the repository config sets up. With `persist-credentials: false` the token is never written to `.git/config`.
- Nothing puts `${{ }}` in a shell script, and the examples pin third-party actions to commit SHAs.

Some protection has to come from the repository settings: restrict who can create `stack/**` branches, require review for `.github/workflows/**`, keep default workflow permissions read-only, and use `allowUnreviewed` only in a sandbox. To limit supply-chain risk in a repository that matters, pin an exact stackq version instead of following `latest`. SPEC §22 lists the details.

## Releases

Every merged PR is released automatically: tests run, the version goes up (patch by default; put `[minor]` or `[major]` in the PR title for more), and binaries for Linux, macOS and Windows are attached to a GitHub release with a `checksums.txt`. The latest is always at `https://github.com/OWNER/stackq/releases/latest/download/stackq_linux_amd64`, and `stackq version` tells you which release you have.

The Action downloads and verifies that binary instead of compiling stackq on every run. By default (`uses: OWNER/stackq@v0`) it runs the newest release, so there is nothing to bump. For reproducible runs pin one with `version: v0.3.1` or `uses: OWNER/stackq@v0.3.1`. If the download or checksum fails it builds from source. Go is installed only for `split` and `plan` (their build check usually needs it) and for a source build; set `setup-go: true` if another command's job needs it, or `setup-go: false` if your check needs no Go. See SPEC §15.5.

## Development

```sh
go test -race ./...
```

The tests build real repositories with a bare `origin` and a fake GitHub:

- **Split order.** Go, TS, deletions, renames, and cycles.
- **Fold on failure.** A slice that fails a real `go build` check is folded into the next one.
- **Sync.** Syncing after review edits and after rebase-merges.
- **Landing.** Incremental and atomic stacks land through a simulated queue. Atomic landing ends with `main` equal to the stack minus the activation PR, with the activation PR left unqueued.
- **Flags.** Flags-first ordering, held activation, and the guard lint (`if` guard, early-return guard, unguarded).
- **Priority.** Classification (CVE/GHSA, labels, bots, size), aging, the jump budget, slot limits, eligibility (drafts, pending checks, other branches), and atomic stacks and reverts going through the pool.
- **Rollback.** `stackq revert` restores the pre-stack tree for rebase, merge-commit and squash merges, keeping later unrelated changes.
- **Failures.** Approval and check gates, stray merges of one part, conflicts, enqueue failures, and dequeues.

The unit tests use a fake GitHub. The `gh` and GraphQL calls (`enqueuePullRequest`, falling back to `enablePullRequestAutoMerge`) were exercised on live GitHub in a sandbox repo, but do a first real run on a small throwaway stack in your own repo. Use the `permissions:` block from `examples/stackq.yml` as is: it includes `checks: read` and `statuses: read`, without which every PR looks as if it had no checks.
