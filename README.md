# forgesync

You self-host Forgejo and push-mirror your repos out to GitHub for visibility. External users land on the GitHub mirror and file issues there. Those issues never make it back into your forge.

forgesync closes that loop. It reads the push-mirror config from each repo in your Forgejo, polls every mirror target, and writes new issues, comments, and edits back into the source-of-truth Forgejo.

forgesync is **stateless**: every issue and comment it creates carries an HTML-comment marker in the body, and that marker is the only state.

## Quick start

### Install

```bash
go install git.erwanleboucher.dev/eleboucher/forgesync/cmd/forgesync@latest
```

Or build from source:

```bash
make build        # binary at ./bin/forgesync
```

### Configure

forgesync reads env vars first; the YAML file is optional and only needed for things env vars can't easily express (the per-host map of Forgejo targets).

```bash
export FORGESYNC_SOURCE_URL=https://forgejo.example.com
export FORGESYNC_SOURCE_TOKEN=...        # PAT with repo + write:issue access
export FORGESYNC_BOT_USERNAME=forgesync-bot
export FORGESYNC_GITHUB_TOKEN=...        # PAT with repo:read for mirrored repos
```

Or a YAML file:

```bash
cp configs/forgesync.example.yaml configs/forgesync.yaml
```

| Var                              | Default | Required                                     |
| -------------------------------- | ------- | -------------------------------------------- |
| `FORGESYNC_SOURCE_URL`           | --      | yes                                          |
| `FORGESYNC_SOURCE_TOKEN`         | --      | yes                                          |
| `FORGESYNC_BOT_USERNAME`         | --      | yes                                          |
| `FORGESYNC_GITHUB_TOKEN`         | --      | only if any push mirror points at github.com |
| `FORGESYNC_FORGEJO_TOKEN_<HOST>` | --      | one per non-github mirror host (see below)   |
| `FORGESYNC_POLL_INTERVAL`        | `5m`    | no                                           |
| `FORGESYNC_HEALTH_LISTEN`        | `:8080` | no                                           |
| `FORGESYNC_LOG_FORMAT`           | `text`  | no (`text` or `json`)                        |
| `FORGESYNC_LOG_LEVEL`            | `info`  | no                                           |

### Run

```bash
forgesync run
```

Looks for `configs/forgesync.yaml` by default. Override with `-c` or `FORGESYNC_CONFIG`.

### Multiple Forgejo mirror targets

forgesync auto-derives each Forgejo target from the mirror's URL. The only thing it needs from you is the per-host token, set via an env var named after the host:

```bash
# codeberg.org    → FORGESYNC_FORGEJO_TOKEN_CODEBERG_ORG
# git.example.com → FORGESYNC_FORGEJO_TOKEN_GIT_EXAMPLE_COM
export FORGESYNC_FORGEJO_TOKEN_CODEBERG_ORG=...
```

Rule: uppercase, with non-alphanumerics replaced by `_`. Add as many as you have mirror hosts.

### Docker

```bash
docker compose up -d
```

The image is distroless, runs as non-root, and exposes `:8080` for `/healthz`. The container's own `HEALTHCHECK` uses `forgesync healthcheck` against localhost.

## How it discovers what to sync

No setup beyond the source token. Each tick:

1. enumerate repos via `GET /repos/search` on your Forgejo,
2. for each repo, `GET /repos/{owner}/{repo}/push_mirrors`,
3. classify each mirror's `remote_address` (github.com, or a host listed under `targets.forgejo`),
4. pull issues + comments from the target since `now - 2*pollInterval`,
5. for each item, search the destination for the marker -- create if missing, PATCH if changed, skip if equal.

## Promoting a GitHub PR with `/sync`

Comment `/sync` on a `[PR #N]` shadow issue in your canonical Forgejo to promote it to a real Forgejo PR. forgesync will fetch the GitHub PR's head ref, push it to your Forgejo as `forgesync/pr-N`, and open a PR against the original base.

**Fork PRs work. Same-repo PRs opened directly on GitHub will be closed.**

Forgejo's push-mirror uses `git push --mirror`, which deletes refs on the remote that don't exist locally. If you opened a PR on the GitHub mirror from a branch that was never pushed through your canonical Forgejo, that branch only lives on GitHub. Creating `forgesync/pr-N` on Forgejo triggers push-mirror, which deletes the branch and GitHub closes the PR.

Fork PRs are not affected — the head branch lives in the contributor's fork, which push-mirror doesn't touch.
