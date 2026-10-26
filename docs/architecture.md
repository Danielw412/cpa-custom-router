# Architecture

custom-quota-router is a CLIProxyAPI v8 *dynamic-library plugin*: a Go
`c-shared` library that CLIProxyAPI loads at runtime through its C ABI. It
uses only CLIProxyAPI's public plugin interfaces, so neither CLIProxyAPI nor
its clients are modified.

## Capabilities

The plugin registers three CLIProxyAPI capabilities:

| Capability | RPC methods | Used for |
| --- | --- | --- |
| Scheduler | `scheduler.pick` | Choosing the Codex credential for each request. |
| Usage plugin | `usage.handle` | Reading quota headers and failures from every completed request. |
| Management API | `management.register`, `management.handle` | The JSON API and the static control panel. |

Plus the lifecycle methods `plugin.register`, `plugin.reconfigure`,
`plugin.quiesce` and `plugin.shutdown`.

The host callbacks it uses:

| Callback | Used for |
| --- | --- |
| `host.auth.list` | The account roster (IDs, emails, plans, status). |
| `host.auth.get` | Reading a Codex token in-process for the read-only usage endpoint. |
| `host.http.operation_open`, `host.http.do`, `host.http.cancel` | Quota checks through CLIProxyAPI's HTTP transport and proxy settings. |
| `host.model.execute` | The single tiny request that starts a 5-hour window. |
| `host.log` | Writing `quota-router:` lines to CLIProxyAPI's log. |

## Layers

```
CLIProxyAPI (host)
   │  C ABI: cliproxy_plugin_init, cliproxyPluginCall, cliproxyPluginFree, cliproxyPluginShutdown
   ▼
main.go                  cgo bridge only: copies buffers, recovers panics, calls into Go
   │
internal/plugin          RPC dispatch, registration manifest, JSON <-> engine types
   │
internal/service         background loop: roster, quota polling, window starts, persistence, logging
   │        │
   │   internal/hostapi  typed client for host.* callbacks (transport injected, so testable)
   ▼
internal/engine          all routing policy; pure Go, no I/O, no cgo
   │
internal/web             management routes + embedded panel assets (ui.html, ui.js, ui.css)
```

Rules that keep this layering useful:

* **`engine` is pure.** The `Engine` takes a clock function and plain data,
  does no I/O and never calls the host. `LoadState`/`SaveState` in
  `store.go` are called only by the service. Every routing rule can be unit-tested with a
  fake clock.
* **`plugin` has no cgo.** The full RPC path, from JSON in to JSON out, is
  tested in `internal/plugin/plugin_test.go` without loading a shared
  library.
* **`main.go` stays thin.** It only translates between C buffers and
  `plugin.Handle`.

## Request path

1. A client request reaches CLIProxyAPI, which builds the candidate list of
   credentials and calls `scheduler.pick` with the candidates, the request
   headers and metadata (including `pinned_auth_id` for a pinned WebSocket).
2. `plugin.pick` converts the request. While requests are paused or
   slowed down, `service.Hold` waits first (`internal/service/hold.go`). It
   reads the pause state through `engine.Gate` and waits on timers and
   wake-ups, never under the engine lock. Window starts and non-Codex
   requests are never held.
3. `plugin.pick` then calls `engine.Pick`, which holds the engine mutex for
   the decision. The decision is in-memory only, with no I/O and no host
   calls on the request path.
4. The engine returns an auth ID (`Handled: true`), a rejection, or
   `Handled: false` to let CLIProxyAPI's built-in selector decide.
5. After the response, CLIProxyAPI calls `usage.handle` with the usage
   record. The plugin reads the `x-codex-*` rate-limit data or the failure
   body, and the engine updates that account's quota.

Any panic or malformed input in `pick` returns `Handled: false`, so a plugin
bug degrades to CLIProxyAPI's default routing rather than failing requests.
A panic in `Hold` lets the request go unheld.

A held request occupies one CLIProxyAPI goroutine and its thread inside the
plugin call. CLIProxyAPI gives the call no deadline and doesn't tell the
plugin when the client goes away, so `Hold` bounds itself: per-transport hold
limits, at most 200 waiting requests, and release on quiesce and shutdown
([routing.md](routing.md#pause-and-slow-down)).

## Background work

`service.Service` runs two goroutines per plugin instance:

* **Maintenance loop** (every 15 s): refresh the account roster (every 30 s),
  let the engine re-evaluate time-based state, poll up to 3 idle accounts'
  quota, write queued events to the log, and persist state if it changed.
* **Window starter:** sleeps until the next scheduled 5-hour window start
  or, with auto-start on, the next window reset (waking at least every
  30 s), then checks quota, sends the start request and records the result.

Each pass recovers from panics, so one bad pass never stops refreshing or
persisting.

## State and hot upgrade

`engine.State` is everything that must survive a restart: the mode, the
current account, session bindings and links, the last known quotas, history,
window plans, and a pause or slow down in progress. Pause and slow-down
changes are saved right away rather than on the next tick. `store.go` writes it atomically (temp file, fsync, rename)
with mode `0600`.

A hot upgrade works like this:

1. A new `custom-quota-router-v<higher>.so` appears in the plugin directory
   and `config.yaml` changes, usually by `deployed_version` being bumped.
2. CLIProxyAPI loads the new library and calls `plugin.register` on it.
3. CLIProxyAPI calls `plugin.quiesce` on the old instance. The old service
   ends any requests it is holding for a pause (retryable rejection), stops
   its loops, persists state one last time and *retires*, so it never writes
   the state file again.
4. The new instance restores the state file and carries on. In-flight
   requests finish on whichever instance started them.

If CLIProxyAPI rolls the upgrade back, `Start()` un-retires the old
instance.

## Concurrency

* `engine.Engine` guards all state with one mutex. Exported methods take
  the lock; unexported helpers (`pickLocked`, `decide`, `rank`, ...) assume
  it is held.
* Requests held for a pause or slow down wait in `service.Hold` on timers and
  a wake-up channel, outside the engine lock. Each wait re-reads
  `engine.Gate` at least every 5 seconds.
* `Export` copies state under the lock, so persistence and the panel never
  hold it during I/O.
* The engine never calls the host. Host callbacks (HTTP, model execute)
  are made only from the service goroutines, so they never run under the
  engine lock.
* Tests run with `-race`.

## Security model

* **Credentials.** Tokens are fetched with `host.auth.get` only for a quota
  check and used in-process. They are never logged, persisted or returned by
  the API. The state file and API expose only auth IDs, email labels, plan
  names and percentages.
* **API.** Every data route is a Management API route, so CLIProxyAPI checks
  the management key and `allow-remote` before the plugin sees the request.
* **Panel.** The static files contain no data. They are served with a strict
  CSP, `nosniff`, `no-referrer` and same-origin framing.
* **Failure containment.** Panics never cross the C boundary. Invalid
  settings are clamped and reported instead of rejected.
* **Provider limits.** The router only chooses among credentials CLIProxyAPI
  already offers and honours their limits. It never retries around a usage
  limit or hides which account is used.

## Known limitations

* **Pinned WebSockets finish on their account**
  ([routing.md](routing.md#pinned-websockets)).
* **Header-only session identity.** The scheduler hook receives headers and
  metadata but not request bodies.
* **Cache lifetime is estimated.** The provider's prompt-cache lifetime isn't
  observable. `cache_warm_minutes` is the estimate.
* **One upstream provider.** Only `codex` candidates are routed. Requests
  with other providers are left to CLIProxyAPI.
