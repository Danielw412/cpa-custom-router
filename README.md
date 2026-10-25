# custom-quota-router

A [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) v8 plugin that
routes Codex requests across several ChatGPT/Codex accounts by remaining
quota. It keeps each agent session and its subagents on one account to
protect prompt caching, and adds a small control panel to the CLIProxyAPI
dashboard.

It uses only CLIProxyAPI's supported plugin interfaces (a C ABI dynamic
library). Nothing in CLIProxyAPI or its clients is forked or modified, and
the plugin can be upgraded or removed independently, without a restart.

## Features

* **One current account.** New sessions go to a single account until its
  5-hour or weekly quota falls below a threshold (default 5%), it hits a
  usage limit, or it becomes unavailable. Traffic stays concentrated instead
  of draining every account at once.
* **Session affinity.** Each conversation, together with its subagents and
  forks, stays on the account it started on while its prompt cache is warm.
  Idle sessions rejoin the current account later, so a switch costs no live
  cache.
* **Your ranking.** Choose what decides the next account: your account
  order, the soonest weekly reset, the soonest 5-hour reset, or the most
  quota left. The router switches back only for a clear win, so it never
  flaps.
* **Manual control.** *Use now*, *Pin*, and per-account *Normal*,
  *Reserve* and *Off* roles.
* **5-hour window starts.** Start or schedule an account's 5-hour window
  with one tiny request, so windows reset when you want them to.
* **Live quota** from every response's rate-limit data, plus light polling
  of idle accounts.
* **Safe by construction.** Tokens never leave the process. Every data
  route needs the management key. A plugin failure falls back to
  CLIProxyAPI's own routing instead of failing requests.

## Requirements

* CLIProxyAPI **8.0.42** on Linux. The plugin must be built against the same
  CLIProxyAPI module version and Go toolchain as the binary that loads it.
* Go **1.26.10** with cgo (the Makefile sets `GOTOOLCHAIN`), and a C
  compiler.
* Python 3 with `PyYAML` for the deploy script, and `websockets` for the
  end-to-end tests.

## Quick start

```bash
git clone git@github.com:Danielw412/cpa-custom-router.git
cd cpa-custom-router
make check                     # vet, format check, unit tests
make e2e                       # optional: full test against an isolated CLIProxyAPI
make deploy VERSION=1.0.0      # build, back up, install and hot-load into ~/cliproxyapi
```

The deploy script assumes CLIProxyAPI lives in `~/cliproxyapi` and runs as
the systemd user service `cliproxyapi` (`scripts/deploy.py --cpa-dir` to
change it). On first install it adds a `plugins` block to `config.yaml`. On
upgrades it only bumps `deployed_version`. It always backs up `config.yaml`
first.

Then open the CLIProxyAPI dashboard → **Plugins → Quota Router**, or
`<cpa-url>/v0/resource/plugins/custom-quota-router/ui`.

To roll back, restore the backup the script printed, or set
`plugins.configs.custom-quota-router.enabled: false`. CLIProxyAPI reloads
and its built-in routing takes over.

## Documentation

| Document | Covers |
| --- | --- |
| [docs/routing.md](docs/routing.md) | How accounts are chosen and switched, sessions, ranking, WebSocket pinning, quota data, 5-hour windows. |
| [docs/configuration.md](docs/configuration.md) | Every plugin setting, runtime state, recommended CLIProxyAPI settings, editing `config.yaml` safely. |
| [docs/operations.md](docs/operations.md) | Control panel, management API, deploy/upgrade/rollback, logs, diagnosing routing, troubleshooting. |
| [docs/architecture.md](docs/architecture.md) | Plugin capabilities, code layers, request path, background work, hot upgrade, security model. |
| [docs/development.md](docs/development.md) | Building, unit and end-to-end tests, the panel, releasing, upgrading CLIProxyAPI. |
| [AGENTS.md](AGENTS.md) | Rules and practices for anyone (human or AI) changing this repo. |

## Known limitations

* **Pinned WebSockets finish on their account.** CLIProxyAPI pins an open
  Codex Responses WebSocket to one account. New sessions and reconnects move
  immediately, but an open socket keeps its account until it closes.
* **Header-only session identity.** Clients that identify sessions only in
  the request body are routed without per-session stickiness.
* **Cache lifetime is estimated** (`cache_warm_minutes`, default 30).

See [docs/routing.md](docs/routing.md) and
[docs/architecture.md](docs/architecture.md#known-limitations) for details.
