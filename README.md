# stackq

Split one large branch into a stack of small PRs, open them as a
[GitHub stack](https://docs.github.com/en/pull-requests/get-started/about-stacked-prs),
and land the stack when every part is approved and green.

```
feature/new-checkout ──split──▶  01-flags → 02-checkout → 03-cmd-shop → 🔒 04-activation
                                 (one GitHub stack: each PR reviewed on its own, showing only its own diff)
                                                     │
                     stackq land: every part approved + green → GitHub merges 01–03
                     bottom up as one request, through the merge queue → main gets all three, or none
                                                     │
                     🔒 04 is moved onto main by GitHub and waits for a human to switch the feature on
```

## What GitHub does, and what stackq adds

GitHub's own stacked pull requests do the hard parts of stacking: the stack
map on every PR, CI and branch rules (required reviews, CODEOWNERS, required
checks) for every PR as if it targeted `main`, a merge of a PR **together with
every open PR below it** as one all-or-nothing request (through the merge
queue, which keeps the stack together), and a server-side rebase and retarget
of the PRs above after a merge, with a **Rebase stack** button when a lower
branch changes.

stackq adds what GitHub leaves to you:

- **The split.** One big branch becomes ordered, reviewable slices (by
  directory, import graph and your rules), each of which builds on its own,
  with feature flags first and the switch that turns the feature on held back.
- **The stack on GitHub.** `stackq push` opens the PRs and links them as one
  GitHub stack; later pushes add new parts on top.
- **When to land.** `stackq land`, or `autoLand`, waits until every part is
  approved and green, then asks GitHub to merge the stack with the
  asynchronous merge API. It follows the request: pending, in the queue,
  merged, or failed with GitHub's reason, which pauses the stack.
- **What lands when.** A priority pool in front of the merge queue, the held
  activation PR (with reminders), a one-command revert, and cleanup.

## Install

```sh
go install ./cmd/stackq   # from a checkout of this repo; needs Go 1.22+, git 2.30+ and the gh CLI
```

## Quick start

```sh
git checkout feature/big-refactor
stackq plan                               # preview the slices, nothing is written
stackq split --check "go build ./..."     # create stack/big-refactor/01-… branches
stackq push --close-source                # push, open the PRs as a GitHub stack, link + close the original PR
stackq land                               # once every part is approved: GitHub merges the whole stack
```

While the stack is in review:

```sh
git checkout stack/big-refactor/02-internal-types
# … address review comments, commit …
stackq sync     # rebases every branch above it, replaying only their own commits
stackq push     # lease-protected force push, keeps the GitHub stack and PR bodies current
stackq status
```

`stackq sync` also picks up branches that GitHub restacked (after a merge, or
**Rebase stack**), and marks branches that have already landed. You can also
just use **Rebase stack** on GitHub and `git pull` the branches.

## Landing: all at once by default

**Atomic** (default, `"landing": "atomic"`): every part is reviewed on its own PR. `stackq land` then checks every part:
- approved (`minApprovals`, default 1),
- no requested changes, not a draft,
- checks not red or running,
- still stacked on the part below.

If all pass, it asks GitHub to merge the **top part**. In a GitHub stack that merges every part below it too, bottom up, as one request: through the merge queue when `main` has one, which keeps the stack together in one merge group. `main` gets the whole stack or none of it, with each part's own commits in order. stackq records the request in the top PR's status section and follows it on later runs; if GitHub turns it down (a rule a PR does not meet, a conflict), the stack is paused with GitHub's reason. Once it lands, stackq marks the parts landed, tells the original PR, and announces the activation PR.

**Incremental** (`--landing incremental`): one part at a time, bottom up. GitHub moves the next part onto `main` after each merge, and stackq lands it once it is ready. `stackq land --pr N` lands part N and every open part below it.

**Rollback:**

```sh
stackq revert --pr <top PR>              # one PR reverting the whole stack on today's main
stackq revert --pr <top PR> --enqueue    # …and merge it
```

This works for rebase, merge-commit and squash merges: each part merged on its own, and stackq finds each part's own commits (the ones after the part below). Before opening the revert, stackq checks that it touches exactly the files the stack touched. If the feature is behind a flag, turning the flag off is faster.

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
- **Activation files** (whatever turns the feature on) go into a final 🔒 draft PR on top of the GitHub stack. stackq never merges it; once the rest has landed, GitHub moves it onto `main` and stackq tells a human it is ready.
- **The guard lint** (Go) reports any new entrypoint call that isn't inside an `if flags.Enabled(...)` or after an early-return guard. Set `"enforce": true` to fail the split on it.

## Priority in the merge queue

GitHub's queue is first in, first out. Its only shortcut, "jump to the front", makes every in-flight entry rebuild. stackq adds priority without rebuilding the queue constantly:

- **The pool.** PRs ready to merge get the `stackq:queue` label (`stackq queue --pr N`, or `stackq land`).
- **Admission.** stackq keeps GitHub's queue short (`maxInFlight`, default 3). Whenever a slot frees up, it queues the **best** waiting PR, not the oldest.
- **Jumps.** Vulnerability fixes and reverts jump to the front, but only one jumped entry is allowed in flight at a time (`maxJumpsInFlight`). GitHub stacks cannot jump (they join through the asynchronous merge API), so a jump applies to PRs outside a stack, such as a revert.
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
- **When the stack lands:** CI posts "✅ has landed" on the original PR, naming the activation PR if one waits.
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
6. **Check.** `--check` runs a command, such as a build, in a throwaway worktree for each slice. If a slice fails, it is folded into the next one, so every branch in the stack builds. Build output from one slice's check stays in that worktree and can make the next slice's check pass; set `"cleanBetweenChecks": true` in `.stackq.json` to run `git clean -fdx` before each check.
7. **Verification.** stackq verifies that the top of the stack is byte-for-byte the original branch. Your working tree is never touched.

## GitHub setup

1. **GitHub stacks.** stackq needs [stacked pull requests](https://docs.github.com/en/pull-requests/get-started/about-stacked-prs) on github.com (public preview since July 2026; nothing to switch on). `stackq doctor` checks that the API answers.
2. **Merge queue.** Turn on the merge queue for `main` in a branch ruleset (recommended: GitHub stacks are merge-queue aware and keep a stack in one merge group). Rebase, merge and squash all work; rebase keeps one commit per part.
3. **CI triggers.** Add `merge_group:` and `workflow_dispatch:` to your CI workflow's triggers, and `push:` on your default branch so what lands is checked too. See `examples/ci-merge-group.yml`. CI on `pull_request` targeting `main` runs for every PR in a GitHub stack.
4. **stackq workflow.** Copy `examples/stackq.yml` to `.github/workflows/`, and point `uses:` at wherever you host this repo.
5. **Token.** The built-in `GITHUB_TOKEN` works (it can create stacks and merge them); see below for what an App token adds.

The files in `examples/` (`ci-merge-group.yml`, `stackq.yml`, `stackq-split.yml`) are meant to be copied unchanged apart from `YOUR_ORG`, so a repo's copies can be diffed against them to catch drift.

To split from GitHub instead of a laptop, copy `examples/stackq-split.yml`. Label a big PR `stackq:split` (create the label once) and the Action splits its branch, opens the stacked PRs as a GitHub stack, links them from the original PR, closes it, and removes the label. You can also run the workflow by hand with a branch name. To see the split first, label the PR `stackq:plan` (also created once): stackq comments the slices it would create, changes nothing else, and removes the label; add it again after a push to refresh the same comment. The workflow uses a `STACKQ_TOKEN` secret if you set one, and the built-in token otherwise (then enable "Allow GitHub Actions to create and approve pull requests"). PRs from forks are ignored (GitHub stacks don't support forks either).

**Landing by itself.** Set `"autoLand": true` in `.stackq.json` and a stack lands as soon as the last part is approved and green. The workflow checks on every review and every finished CI run (a scheduled run is only a safety net: GitHub may start it hours late); a stack that isn't ready gets no comments or labels, and one that is already landing is not merged twice. The top PR of the stack shows where the stack stands in a short status section of its description: what each part still waits for, waiting in the priority pool, landing (GitHub's merge request), paused (with GitHub's reason), or landed.

**Required checks need an App or PAT token.** With the built-in token, do not make your CI check a *required* status check on trunk. GitHub only counts a required check from CI that a pull request or the merge queue started; the CI stackq starts with `workflow_dispatch` (because pushes by the built-in token start no CI) does not count, and the merge queue starts no `merge_group` run for the built-in token. This was seen live before GitHub stacks and is GitHub's token rule, so it still applies. GitHub would then refuse the merge request, and stackq pauses the stack with GitHub's message. Without a required check the stack lands, but the merged result is tested only afterwards, by CI on the push to trunk. For a required check, give stackq a GitHub App token (or a PAT) as `STACKQ_TOKEN`: pushes and queue entries made with it start `pull_request` and `merge_group` runs by themselves. `stackq doctor` warns about this. Step-by-step setup: [docs/github-app.md](docs/github-app.md).

Manual controls, through workflow_dispatch or locally:

```sh
stackq land --pr 123                 # retry after fixing a paused stack
stackq ci after-merge --pr 122       # finish a merge whose run was lost
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

- **github.com with GitHub stacks.** stackq lands only through GitHub stacks (no fallback for GitHub Enterprise Server or repos without them). Stacks are a public preview, and GitHub says the API may change.
- **File-level splitting only.** A file never spans two slices. If one file must be split, do it by hand with `git restore -p --source=<big-branch>` on the relevant branch, then run `stackq sync`.
- **API-level coupling.** The import graph can't see that a package's exported types changed under its callers (for example a function that now returns a renamed type). `--check` catches it by folding the failing slice into the next one, which can pull unrelated slices along. Add a `slices` rule naming the coupled packages so only they ship together.
- **Import resolution.** TS path aliases (`@/…`) and bare package imports are not resolved. Use `slices` rules or `--check` to keep those orders right.
- **Squash merges.** Local `stackq sync` first compares patches with trunk (`git cherry`), which works for rebase and merge commits and for a squash of a one-commit part. A squash of a part that has several commits (review fix-ups) looks like new work to git, so sync also asks GitHub (with `gh`, when it is installed and signed in): a part whose PR merged, with the PR's head equal to the local branch's tip, counts as landed. In an atomic stack a merged part lands everything below it. A merged PR whose branch has commits it did not contain is reported and kept. If `gh` is missing or fails, sync goes by git alone and says so.
- **No jumping for stacks.** GitHub stacks are merged with the asynchronous merge API, which has no "jump to the front"; priority still decides when a stack is admitted.
- **One trunk per stack, one stack per PR.** GitHub stacks are linear and can't include forks; stackq's are too.
- **Guard lint** covers Go only, and is a lint, not a proof (SPEC §29.3).

## Activation PRs that wait

The held activation PR is never merged by stackq. If it waits longer than `activationReminderDays` (default 7) it gets a comment saying how long it has been ready and how to merge it (`stackq land --pr N`) or close it, and again every `activationReminderDays` after that. `stackq:reminded` is the label that times this. Set `"activationReminderDays": 0` in `.stackq.json` to turn it off. Reminders are sent by ticks, so they need `autoLand` or the priority pool to be on and can come late, never early.

## Which trunk CI trusts

A PR body can be written by anyone who can open a PR, and a stack's marker in it names a trunk. In CI stackq acts only on markers that name the repository's trunk: `trunk` in `.stackq.json`, else GitHub's default branch. Any other marker is ignored and noted in the run's log. If your stacks target a branch other than the default, set `trunk` in `.stackq.json`.

## Reading a run

Each stackq run writes a short summary on its run page in the Actions tab: the command, the event that started it, whether it worked, and what it did (the last 200 lines; the full log is in the job). Open the newest `stackq` run to see why a stack is or is not moving. The run also carries a one-line annotation (done, or the error), which shows on the PR's checks too.

## Checking the setup

`stackq doctor` checks, without changing anything, that the repository is set up the way stackq needs: the config, that GitHub stacks are available, a merge queue on trunk (and a note on required status checks, which need an App or PAT token), rebase merging, whether Actions may open PRs, the workflow files (the `checks: read` / `statuses: read` permissions, CI triggered on `push`, `merge_group` and `workflow_dispatch`, the CI name `workflow_run` waits for) and the `stackq:split` and `stackq:plan` labels. Each finding is ✓, ! (warning), ✗ (problem) or ? (could not check, with what to look at by hand), and any ✗ makes it exit non-zero. Run it locally, or from the Actions tab (the `stackq` workflow, command `doctor`) to check with the workflow's own token.

## Cleaning up branches

`stackq gc` lists remote branches left behind; `stackq gc --delete` deletes them, and so does the `stackq` workflow run by hand with command `gc` (it uses the workflow's token). A branch is deleted only if no open PR uses it, its tip is still exactly the head of its last PR, and that PR merged, or it is a stack or revert branch whose PR was closed, or it is the source of a stack that is completely finished. Branches that never had a PR are left alone. The same rules, limited to the branches stackq makes itself, run on every tick, so branches of landed stacks do not stay behind (unless `keepBranches` is set).

## Security

stackq runs with a token that can push branches and change `main` through the merge queue, so it treats everything a stranger can write as hostile:

- A stack marker in a PR body counts only for a PR whose head branch is in this repository and is named like a stack branch (`stack/<name>/…`). A PR from a fork with a copied marker is ignored.
- The build `check` from `.stackq.json` never runs under a process that holds the token: the Action gives `split` no token, and git commands that get the token ignore hooks and helpers the repository config sets up. With `persist-credentials: false` the token is never written to `.git/config`.
- Nothing puts `${{ }}` in a shell script, and the examples pin third-party actions to commit SHAs.

Some protection has to come from the repository settings: restrict who can create `stack/**` branches, require review for `.github/workflows/**`, keep default workflow permissions read-only, and use `allowUnreviewed` only in a sandbox. To limit supply-chain risk in a repository that matters, pin an exact stackq version instead of following `latest`. SPEC §22 lists the details.

## Releases

Every merged PR that changes more than Markdown is released automatically (a README or SPEC change waits for the next release): tests run, the version goes up (patch by default; put `[minor]` or `[major]` in the PR title for more), and binaries for Linux, macOS and Windows are attached to a GitHub release with a `checksums.txt`. The latest is always at `https://github.com/OWNER/stackq/releases/latest/download/stackq_linux_amd64`, and `stackq version` tells you which release you have.

The Action downloads and verifies that binary instead of compiling stackq on every run. By default (`uses: OWNER/stackq@v0`) it runs the newest release, so there is nothing to bump. For reproducible runs pin one with `version: v0.3.1` or `uses: OWNER/stackq@v0.3.1`. If the download or checksum fails it builds from source. Go is installed only for `split` (its build check usually needs it) and for a source build; set `setup-go: true` if another command's job needs it, or `setup-go: false` if your check needs no Go. See SPEC §15.5.

## Development

```sh
go test -race ./...
```

The tests build real repositories with a bare `origin` and a fake GitHub:

- **Split order.** Go, TS, deletions, renames, and cycles.
- **Fold on failure.** A slice that fails a real `go build` check is folded into the next one.
- **Sync.** Syncing after review edits and after rebase-merges.
- **Landing.** The fake GitHub behaves like GitHub stacks: it refuses base changes of stacked PRs, merges a stacked PR with everything below it, and retargets the PRs above after a merge. Incremental and atomic stacks land through a simulated queue; atomic landing ends with `main` equal to the stack minus the activation PR, which GitHub moved onto `main` and stackq left unmerged. The merge request is followed through pending, queued, failed and gone.
- **Flags.** Flags-first ordering, held activation, and the guard lint (`if` guard, early-return guard, unguarded).
- **Priority.** Classification (CVE/GHSA, labels, bots, size), aging, the jump budget, slot limits, eligibility (drafts, pending checks, other branches), and atomic stacks and reverts going through the pool.
- **Rollback.** `stackq revert` restores the pre-stack tree for rebase, merge-commit and squash merges, keeping later unrelated changes.
- **Failures.** Approval and check gates, drafts, incomplete stacks, merge requests GitHub refuses, merge errors, and dequeues.

The unit tests use a fake GitHub. The `gh` and GraphQL calls (`enqueuePullRequest`, falling back to `enablePullRequestAutoMerge`) were exercised on live GitHub in a sandbox repo, but do a first real run on a small throwaway stack in your own repo. Use the `permissions:` block from `examples/stackq.yml` as is: it includes `checks: read` and `statuses: read`, without which every PR looks as if it had no checks.
