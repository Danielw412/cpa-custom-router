# Development

## Prerequisites

* **Go 1.26.10 with cgo.** The plugin must be built with exactly the Go
  version and `github.com/router-for-me/CLIProxyAPI/v8` version of the
  CLIProxyAPI binary that loads it (8.0.42 here). The Makefile sets
  `GOTOOLCHAIN=go1.26.10`, which downloads that toolchain if needed.
* **A C compiler** for cgo (`gcc` or `clang`).
* **Linux.** CLIProxyAPI loads `.so` plugins. Other platforms are untested.
* **Python 3** with `PyYAML` (deploy script) and `websockets` (end-to-end
  tests).
* **A CLIProxyAPI 8.0.42 binary** for the end-to-end tests, by default
  `~/cliproxyapi/cli-proxy-api` (override with `CPA_BIN`).

## Layout

```
main.go                     cgo bridge (C ABI exports)
internal/engine/            routing policy, quota parsing, sessions, settings, state (pure Go)
internal/plugin/            RPC dispatch and registration manifest
internal/service/           background loop, quota polling, window starts, persistence
internal/hostapi/           typed client for host.* callbacks
internal/web/               management routes; assets/ holds the panel (embedded)
scripts/deploy.py           build, back up, install and hot-load into a live CLIProxyAPI
test/e2e/                   staging launcher, mock Codex upstream, end-to-end suites
docs/                       documentation
```

See [architecture.md](architecture.md) for how the pieces fit together.

## Commands

| Command | Does |
| --- | --- |
| `make check` | `go vet`, a `gofmt` check and `go test -race` on every package. Run before every commit. |
| `make test` | Unit and RPC-level tests only. |
| `make fmt` | `gofmt -w .` |
| `make build VERSION=1.6.0` | Build `build/custom-quota-router-v1.6.0.so` with the version stamped in. |
| `make e2e` | Build the mock upstream and two staging versions, then run both end-to-end suites against a real CLIProxyAPI. |
| `make deploy VERSION=1.6.0` | Deploy to the local production CLIProxyAPI ([operations.md](operations.md#deploy-and-upgrade)). |
| `make clean` | Remove `build/`. |

## Tests

### Unit and RPC tests

```bash
make check
```

* `internal/engine` covers quota parsing, session-ID parity with
  CLIProxyAPI's extractor, routing policy, ranking, switch-back rules, manual
  modes, window starts, persistence and concurrency.
* `internal/plugin` drives the real RPC methods with a fake host: register,
  pick, usage, management routes, window starts, quiesce (state flushed,
  no secrets in it), bad input, and reconfigure keeping runtime state.

Engine tests use a fake clock and three example accounts
(`alice@example.com`, `bob@example.com`, `carol@example.com`). Helpers in
`engine_test.go`:

| Helper | Use |
| --- | --- |
| `newTestEngine(t, mutate)` | Engine with default settings plus your changes. Fails on settings warnings. |
| `clk.add(d)` | Advance time. |
| `quota(e, id, shortUsed, longUsed, shortReset, longReset)` | Quota seen on live traffic. This also marks the 5-hour window as running. |
| `polled(...)` | The same numbers from an idle poll, with no traffic. Use it for accounts that should look idle. |
| `pickFor(t, e, session("s1"), ids...)` | Route one request for a session. |
| `subagent(parent, child)` | Headers for a subagent of `parent`. |
| `mustRoute(t, res, id)` | Assert where a request went. |
| `historyHas(e, kind, text)` | Assert a history entry. |

Write a test for every routing rule you add or change, named after the
behaviour (for example `TestSwitchBackFollowsTheRanking`). When a bug is
found in production, add a regression test to `regression_test.go` first.

### End-to-end tests

```bash
make e2e
```

`test/e2e/staging.sh` starts an isolated CLIProxyAPI on `127.0.0.1:18317`
with its own config, auth directory and state in `/tmp/cqr-staging`
(override with `STAGE`). A mock Codex upstream on `127.0.0.1:18400` serves
HTTP and WebSocket responses with real rate-limit headers and events. Three
fake API keys stand in for accounts. Nothing touches real credentials or the
production instance.

* `e2e.py` checks authentication, manual pins, settings persistence through
  the config API, affinity and subagents, threshold switching, 429 failover,
  WebSocket pinning and hot upgrade (`SO_NEXT`).
* `e2e_windows.py` checks 5-hour window starts, schedules and cancellation.

To poke at staging by hand:

```bash
make mock build VERSION=0.0.1
test/e2e/staging.sh                 # prints "staging ready"
# browse to http://127.0.0.1:18317/v0/resource/plugins/custom-quota-router/ui
# (management key: staging-mgmt-key)
test/e2e/staging.sh stop
```

## Working on the panel

The panel is plain HTML, CSS and JavaScript in `internal/web/assets/`,
embedded into the library with `go:embed`. There is no build step and there
are no dependencies.

* All data comes from `GET .../state`. The page itself holds no data, so
  keep it that way: it is served without authentication.
* Keep the CSP intact: no inline scripts or styles, no external resources.
* Write UI text in plain language that says what happens ("New sessions go
  here"), and keep labels consistent with [operations.md](operations.md#control-panel).
* Check changes on staging at desktop and phone widths before deploying.

## Releasing

1. `make check` and `make e2e` pass.
2. Docs are updated for any behaviour, setting or UI change: `README.md`,
   the relevant file in `docs/`, and the setting descriptions in
   `internal/plugin/plugin.go` (shown in the dashboard).
3. Pick the next version. Patch for fixes, minor for new behaviour or
   settings. It must be higher than the installed one.
4. Commit, then `make deploy VERSION=x.y.z`.
5. Watch the panel and the log for a few minutes. Keep the backup path the
   script printed.
6. Tag the release: `git tag vx.y.z && git push --tags`.

## Upgrading CLIProxyAPI

The plugin ABI and the SDK types it uses are versioned with CLIProxyAPI.
When CLIProxyAPI is upgraded:

1. Update `github.com/router-for-me/CLIProxyAPI/v8` in `go.mod` to the same
   version, and the Go toolchain to the one CLIProxyAPI was built with.
2. Re-check the parts of CLIProxyAPI the plugin mirrors or relies on:
   session-ID extraction (`session.go` and its parity test), the
   `pinned_auth_id` metadata, the usage record fields, the config API path,
   and the config watcher.
3. Run `make check` and `make e2e` against the new binary
   (`CPA_BIN=/path/to/cli-proxy-api make e2e`).
