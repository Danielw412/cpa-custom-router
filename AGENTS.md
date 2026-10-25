# AGENTS.md

Guidance for AI agents and humans working on this repository.

custom-quota-router is a CLIProxyAPI v8 plugin (a Go `c-shared` library) that
routes Codex requests across several ChatGPT accounts by remaining quota,
while keeping each conversation on one account to protect its prompt cache.
It runs inside a live proxy that real work depends on, so correctness and
safety come before features.

Read first: [README.md](README.md), then [docs/architecture.md](docs/architecture.md)
and [docs/routing.md](docs/routing.md).

## Hard rules

1. **Never expose credentials.** OAuth tokens, API keys and management keys
   must never appear in logs, the state file, API responses, error messages,
   test fixtures, docs or commits. Tokens are read only through
   `host.auth.get`, used in-process for the quota check, and dropped.
2. **This repository is public.** Never commit real account emails, auth
   IDs, hostnames (including Tailscale names), IP addresses, screenshots of
   the live panel, `config.yaml`, `state.json`, backups or logs. Use
   `alice@example.com`-style accounts and placeholders such as `<cpa-url>`.
3. **Don't modify CLIProxyAPI or its clients.** Use only the plugin ABI, the
   host callbacks and the Management API. Don't fork, patch or vendor
   CLIProxyAPI.
4. **Never fail a request because of the plugin.** On a panic, bad input or
   missing data, `scheduler.pick` returns `Handled: false` and CLIProxyAPI's
   own selector decides. Rejections are only for explicit user choices
   (strict pin, every account *Off*, a paused request past its hold limit).
5. **Respect provider limits.** Route only among credentials CLIProxyAPI
   offers. Never retry around a usage limit, disguise which account is
   used, or send requests beyond the documented 5-hour window starts.
6. **Production is live.** Don't deploy, restart CLIProxyAPI, edit its
   `config.yaml` or touch its auth directory unless the user asked for that
   specific change. See [Production](#production).

## Commands

```bash
make check                    # go vet + gofmt check + go test -race ./...  (run before every commit)
make e2e                      # isolated CLIProxyAPI + mock upstream; never touches real credentials
make build VERSION=x.y.z      # build/custom-quota-router-vx.y.z.so
make deploy VERSION=x.y.z     # production hot deploy (only when asked)
```

Builds need `GOTOOLCHAIN=go1.26.10` (set by the Makefile) and cgo. The Go
version and the CLIProxyAPI module version must match the running binary.

## Code map

| Path | Rules |
| --- | --- |
| `main.go` | cgo bridge only. Keep it thin; recover every panic before it crosses into C. |
| `internal/engine/` | All routing policy. The `Engine` does no I/O, host calls or goroutines, and takes time only from `e.now()`. `store.go` (state file load/save) is called by the service, never by the engine. |
| `internal/plugin/` | RPC dispatch and the registration manifest (setting descriptions shown in the dashboard). No cgo. |
| `internal/service/` | Background loop, polling, window starts, persistence, and holding requests for a pause or slow down (`hold.go`). The only place host callbacks run. |
| `internal/hostapi/` | Typed host callbacks; the transport is injected for tests. |
| `internal/web/` | Management routes and the embedded panel (`assets/`). |
| `scripts/deploy.py` | Production deploy. Edits `config.yaml` in place and minimally. |
| `test/e2e/` | Staging launcher, mock Codex upstream, end-to-end suites. |

## Making changes

* **Keep the request path cheap.** `Engine.Pick` runs under one mutex for
  every proxied request: in-memory work only, with no I/O, logging or host
  calls. The only deliberate wait is `service.Hold` for a pause or slow down
  the user turned on. It waits outside the engine lock and must stay
  bounded (hold limits, `MaxHeld`, release on quiesce and shutdown).
* **Concurrency.** Exported engine methods take the lock and unexported
  helpers assume it. Never call the host or do I/O under the lock. Run tests
  with `-race`.
* **Stability over cleverness.** Every switch costs a prompt cache. New
  behaviour must not add switches, flapping, or two accounts draining at
  once, unless the user asked for that. Prefer tie tolerances, clear-win
  margins and dwell times over exact comparisons.
* **Adding a setting** touches all of these:
  1. a field with `yaml`/`json` tags in `engine.Settings`;
  2. its default in `DefaultSettings`;
  3. validation in `Normalize`, which clamps or falls back and adds a
     warning, never an error;
  4. a `ConfigField` in `internal/plugin/plugin.go`;
  5. the panel, if it is user-facing;
  6. the table in `docs/configuration.md`;
  7. tests.
* **State compatibility.** `engine.State` is read by the next version after a
  hot upgrade, and by the previous version after a rollback. Add fields
  only. Make them zero-value safe, use `omitempty` where it fits, and never
  rename or repurpose existing JSON keys.
* **ABI.** Don't change the exported C symbols or the RPC method set without
  checking CLIProxyAPI's `pluginabi` for the matching version.
* **User-facing text** (history reasons, panel labels, warnings) is plain,
  specific language that says what happened and why, e.g. "Switched to X:
  its 5-hour window resets sooner". No jargon, no internal identifiers.
* **Style.** `gofmt`, standard library first, no new dependencies without a
  strong reason. Comments explain *why*, at the density of the surrounding
  code. Name tests after the behaviour they pin down.
* **Panel.** Plain HTML, CSS and JS with no build step. No inline scripts or
  styles (strict CSP), no external resources, and no data in the static
  files: everything comes from the authenticated state API.

## Testing

* Every routing change needs an engine test with the fake clock. The helpers
  are listed in [docs/development.md](docs/development.md#unit-and-rpc-tests).
  Use `polled()` rather than `quota()` for accounts that should look idle:
  `quota()` simulates live traffic and starts the 5-hour window.
* Production bugs get a regression test in
  `internal/engine/regression_test.go` before the fix.
* Changes to the request path, WebSocket handling, settings saving or the
  deploy flow also need `make e2e`.
* Don't weaken or delete a failing test to get green. Find out which side is
  wrong.

## Production

The live instance is CLIProxyAPI running as the systemd user service
`cliproxyapi` from `~/cliproxyapi`. Treat it as shared infrastructure that
other tools (T3 Code, Codex sessions, remote access) depend on.

* **Deploy only through `make deploy`.** It runs the tests, backs up
  `config.yaml` and the state file, installs the new `.so`, bumps
  `deployed_version` in place and waits for the hot reload. Nothing
  restarts and open streams keep running.
* **Never restart CLIProxyAPI** to apply a change. It drops every open
  stream and WebSocket. Everything in this plugin reloads hot.
* **Back up before any config edit**, into `~/cliproxyapi/backups/`.
* **Write `config.yaml` in place.** CLIProxyAPI watches it by inode. A
  rename-over write is silently ignored until the next restart. Confirm the
  reload in the log (`config changes detected:`).
* **Versions only go up.** CLIProxyAPI loads the highest
  `custom-quota-router-v<x.y.z>.so` and only hot-swaps on a new file.
* **Logs** go to `main.log` under CLIProxyAPI's log directory when
  `logging-to-file` is on, and to the journal otherwise
  ([docs/operations.md](docs/operations.md#logs)).
* **Leave the rest of the setup alone.** Don't change credentials, the auth
  directory, Tailscale or network settings, or other CLIProxyAPI settings
  as a side effect of plugin work.
* After deploying, check the panel and the log, and report the backup path
  and the result.

## Git and docs

* Small, focused commits with imperative subjects ("Rank accounts by 5-hour
  reset"). Explain the why in the body when it isn't obvious.
* Before every commit, run `make check` and review `git diff --cached` for
  secrets, real emails, auth IDs and hostnames.
* Never commit `build/`, `*.so`, state, config, logs or screenshots.
  `.gitignore` covers the usual paths, but check anyway.
* Update the docs in the same change as the behaviour:
  * `README.md` for the overview;
  * `docs/routing.md` for policy;
  * `docs/configuration.md` for settings;
  * `docs/operations.md` for the panel, API and deploying;
  * `docs/architecture.md` for structure;
  * `docs/development.md` for tooling.
* Don't push, tag or deploy unless asked.
