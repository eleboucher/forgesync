# forgesync

You self-host Forgejo and push-mirror your repos out to GitHub for visibility. External users land on the GitHub mirror and file issues there. Those issues never make it back into your forge.

forgesync closes that loop. It reads the push-mirror config from each repo in your Forgejo, polls every mirror target, and writes new issues, comments, edits, and labels back into the source-of-truth Forgejo. Labels are matched by name, and any the destination doesn't have yet are created. forgesync only ever removes labels it set itself, so labels added on the other side stay put.

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
| `FORGESYNC_INITIAL_BACKFILL`     | `1h`    | no (look-back on the first tick, and the furthest a failing flow catches up) |
| `FORGESYNC_TICK_TIMEOUT`         | `0`     | no (`0` = no per-tick deadline)              |
| `FORGESYNC_HEALTH_LISTEN`        | `:8080` | no (serves `/healthz` and `/metrics`)    |
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
2. for each repo the token is an admin of, `GET /repos/{owner}/{repo}/push_mirrors` (listing push mirrors needs admin, so other repos are skipped),
3. classify each mirror's `remote_address` (github.com, or a host listed under `targets.forgejo`),
4. pull issues + comments from the target since `now - 2*pollInterval`, or since the last successful run of that repo/mirror/direction if failures have held it back (at most `initialBackfill`),
5. for each item, search the destination for the marker -- create if missing, PATCH if changed, skip if equal.

## Promoting a GitHub PR with `/sync`

Comment `/sync` on a `[PR #N]` shadow issue in your canonical Forgejo to promote it to a real Forgejo PR. forgesync will fetch the GitHub PR's head ref, push it to your Forgejo as `forgesync/pr-N`, and open a PR against the original base.

**Fork PRs work. Same-repo PRs opened directly on GitHub will be closed.**

Forgejo's push-mirror uses `git push --mirror`, which deletes refs on the remote that don't exist locally. If you opened a PR on the GitHub mirror from a branch that was never pushed through your canonical Forgejo, that branch only lives on GitHub. Creating `forgesync/pr-N` on Forgejo triggers push-mirror, which deletes the branch and GitHub closes the PR.

Fork PRs are not affected — the head branch lives in the contributor's fork, which push-mirror doesn't touch.

## Your pull requests to upstream

If a GitHub mirror target is itself a fork, forgesync also copies the pull requests the fork's owner opened on the parent repo into your canonical Forgejo, as `[upstream PR #N]` issues. They carry the PR's comments and follow its open/closed state, so you can track upstream reviews from Forgejo.

These are read-only. Comments you add to them stay on Forgejo; reply on GitHub to reach the upstream maintainers. Line-level review comments are not copied, only the PR's conversation.

## Keeping an issue on Forgejo

Label an issue `local-only` (any case) in your canonical Forgejo and forgesync won't copy it, or its comments, to any mirror. Add the label before the next tick: an issue that has already been copied keeps its mirror copy, which simply stops receiving updates. Imported issues are unaffected, so replies to them still flow back to the mirror.

## Metrics

Prometheus metrics are served on `/metrics`, on the same listener as `/healthz`.

| Metric | Use |
|---|---|
| `forgesync_last_success_timestamp_seconds` | when the last tick finished. A tick only fails if the canonical Forgejo can't be listed, so this says forgesync is running, not that every mirror syncs |
| `forgesync_ticks_total{result}`, `forgesync_tick_duration_seconds` | how often ticks fail, and how long they take |
| `forgesync_flow_runs_total{repo,mirror,direction,result}` | which repo, mirror or direction (`inbound`, `outbound`, `upstream`) is failing |
| `forgesync_flow_last_success_timestamp_seconds{repo,mirror,direction}` | how far behind a failing flow is |
| `forgesync_items_total{kind,from,to,result}` | issues and comments processed (written, or already up to date), and how many failed |
| `forgesync_github_rate_limit_remaining{resource}` | how close you are to GitHub's rate limit |
| `forgesync_build_info{version}` | the running version |

Example alerts:

```yaml
# forgesync is down, or not being scraped.
- alert: ForgesyncDown
  expr: absent(forgesync_build_info)
  for: 10m
# forgesync runs, but ticks keep failing (the gauge is 0 until the first tick finishes).
- alert: ForgesyncStale
  expr: forgesync_last_success_timestamp_seconds > 0 and time() - forgesync_last_success_timestamp_seconds > 1800
  for: 10m
# One repo/mirror/direction keeps failing.
- alert: ForgesyncFlowFailing
  expr: increase(forgesync_flow_runs_total{result="error"}[30m]) > 0 and ignoring(result) increase(forgesync_flow_runs_total{result="ok"}[30m]) == 0
  for: 15m
```
