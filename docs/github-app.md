# Using a GitHub App token with stackq

stackq works with the built-in `GITHUB_TOKEN`, but GitHub treats what that token does differently from what a person or an App does. With an App token:

- Branches stackq pushes start your `pull_request` CI by themselves. There is no "awaiting approval" hold on the PRs it opens and no `workflow_dispatch` workaround.
- A required status check on trunk works. Today it blocks every lander (README, "Required checks need an App or PAT token").
- The merge queue starts `merge_group` CI for the stack, so the merged result is tested before it lands, not after.
- Events stackq causes (a merge, a label) start workflows, so the "wait and tick" safety net is rarely needed.

A fine-grained personal access token (PAT) gives the same behaviour, but it belongs to one person and stops working if they leave. An App belongs to the organization.

> **Status:** these effects follow from how GitHub treats App tokens and are what the README and SPEC say. The stackq author has not yet run the setup below against a live repo. Do the first run on a throwaway stack, as in step 6.

## 1. Create the App

Organization: **Settings → Developer settings → GitHub Apps → New GitHub App** (or the same under your user account for a personal repo).

- **Name:** anything, for example `stackq-yourorg`. **Homepage URL:** your repo's URL.
- **Webhook:** untick *Active*. stackq needs no webhook.
- **Repository permissions:**

| Permission | Level | Why |
|---|---|---|
| Contents | Read and write | push stack branches, delete them, enqueue PRs |
| Pull requests | Read and write | open, edit, label and comment on PRs; enqueue |
| Actions | Read and write | start CI on branches (`workflow_dispatch`) |
| Checks | Read | read CI results |
| Commit statuses | Read | read CI results |
| Issues | Read and write | create the `stackq:*` labels (label creation is an Issues permission) |
| Workflows | Read and write | **only if your stacks change files under `.github/workflows/`**; GitHub refuses pushes that touch them without it |
| Metadata | Read | added automatically |

- **Where can this App be installed?** *Only on this account*.

Create it, then **Generate a private key** (a `.pem` file downloads) and note the **Client ID** shown on the App's page.

## 2. Install it on the repository

On the App's page choose **Install App**, pick your account or organization, and select **Only select repositories** with just the repo that runs stackq.

## 3. Store the credentials

In the repository (or organization) **Settings → Secrets and variables → Actions**:

- Variable `STACKQ_APP_CLIENT_ID`: the Client ID.
- Secret `STACKQ_APP_PRIVATE_KEY`: the whole contents of the `.pem` file.

Delete the `.pem` from your machine afterwards. Secrets are not given to workflows triggered by PRs from forks, which stackq ignores anyway.

## 4. Change the workflows

In `.github/workflows/stackq.yml` and `.github/workflows/stackq-split.yml`, add this step as the **first step of every job** (`stackq` in the first file, `plan` and `split` in the second), before `actions/checkout`:

```yaml
      - id: app
        uses: actions/create-github-app-token@bcd2ba49218906704ab6c1aa796996da409d3eb1 # v3.2.0
        with:
          client-id: ${{ vars.STACKQ_APP_CLIENT_ID }}
          private-key: ${{ secrets.STACKQ_APP_PRIVATE_KEY }}
```

Then replace every `secrets.STACKQ_TOKEN || github.token` in those files with `steps.app.outputs.token`. From the repo root:

```sh
sed -i 's/secrets\.STACKQ_TOKEN || github\.token/steps.app.outputs.token/g' \
  .github/workflows/stackq.yml .github/workflows/stackq-split.yml
```

Check the result with `git diff`: the token expression should now appear in `actions/checkout`, in the stackq `with: token:` line and in the `GH_TOKEN:` of each `run` step that uses `gh`. The token lasts about an hour and is revoked when the job ends. Leave the `permissions:` blocks as they are; they apply to `GITHUB_TOKEN`, which stackq no longer uses for these calls.

One `github.token` stays in `stackq-split.yml`: the `token:` input of the `plan` step, which makes no GitHub calls and gets no token from the action anyway.

## 5. Make the CI check required (optional)

With the App token you can require your CI check in the branch ruleset for trunk, alongside the merge queue. CI must run on `pull_request`, `merge_group`, `push` to trunk and `workflow_dispatch` (see `examples/ci-merge-group.yml`). Run the `stackq` workflow by hand with command `doctor` afterwards; it checks the settings stackq can read.

## 6. Try it on a throwaway stack

1. Open a small PR touching two or three folders and add the `stackq:split` label.
2. The stack's PRs should appear with their CI already running as `pull_request` runs (no "awaiting approval").
3. With `autoLand` on and the parts green, the lander should join the queue and a `merge_group` run should start for it.
4. If the required check still says "expected", look at which workflow run it comes from. It must be a `pull_request` or `merge_group` run, not a `workflow_dispatch` one.

## Keep the key safe

- The private key can push to the installed repositories. Keep it as a secret, install the App on one repository, and do not echo it.
- Rotate it by generating a new key on the App's page, updating the secret, and deleting the old key.
- Never expose the token to code from a PR. stackq already keeps it out of the `split` and `plan` steps, which run your build check; keep any step you add that runs repository code free of it too.
