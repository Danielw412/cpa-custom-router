# Operations

Running the plugin on a live CLIProxyAPI: the panel, the API, deploying,
rolling back, logs and troubleshooting. The paths below assume CLIProxyAPI
lives in `~/cliproxyapi` and runs as the systemd user service
`cliproxyapi`. Adjust them for your setup.

## Where things live

| What | Path |
| --- | --- |
| CLIProxyAPI config | `~/cliproxyapi/config.yaml` |
| Installed plugin | `~/cliproxyapi/plugins/custom-quota-router-v<version>.so` |
| Runtime state | `~/cliproxyapi/plugins/custom-quota-router/state.json` (`0600`) |
| Config and state backups | `~/cliproxyapi/backups/` |
| CLIProxyAPI log (file logging on) | `<log dir>/main.log`; see [Logs](#logs) |
| Credentials (never touched by the plugin except to read a token in-process for quota checks) | CLIProxyAPI's `auth-dir`, e.g. `~/.cli-proxy-api/` |

## Control panel

Open the CLIProxyAPI dashboard, then **Plugins → Quota Router**, or go
directly to `<cpa-url>/v0/resource/plugins/custom-quota-router/ui`.

Inside the dashboard the panel reuses the dashboard's login when *Remember
password* is ticked. Otherwise it asks for the management key once per
browser session. It shows:

* **Where new requests go and why.** If busy sessions are still running on
  another account, a line under the summary names them and says when they
  will move.
* **Each account's** 5-hour and weekly remaining quota, with your threshold
  marked on the bar. *Current* marks the account new sessions use.
* **Sessions per account.** "*N sessions working now*" means a request in
  the last 5 minutes. "*N idle*" are sessions with a warm cache that are not
  working. Idle accounts show when they were last used.
* **Pause and slow down:** *Pause* holds new requests (until you resume or
  for a set time, with optional *Auto-resume every N min*), *Resume now*
  lets them go, and *Slow down* adds a wait before each request. The status
  line shows how many requests are waiting and when the next auto-resume
  is ([routing.md](routing.md#pause-and-slow-down)).
* **Buttons:** *Use now* (make this account current, stay automatic), *Pin*
  (manual mode: send everything here) and *Automatic*.
* **Preferences:** ranking, threshold, account order and roles.
* **5-hour windows:** start or schedule window starts, and turn
  *Auto-start* on or off. Auto-start is saved to `config.yaml` like the
  preferences; each account's row shows when its next window starts
  automatically ([routing.md](routing.md#5-hour-windows)).
* **Recent switches** with their reasons.

Account management, usage statistics and detailed quota pages stay in
CLIProxyAPI's own dashboard.

## Management API

Every route is a CLIProxyAPI Management API route. It requires the
management key (`Authorization: Bearer <key>`) and obeys
`management.allow-remote`.

| Method and path | Body | Does |
| --- | --- | --- |
| `GET /v0/management/custom-quota-router/state?history=N` | | Full panel state: accounts, sessions, settings, history, pause, slow down and waiting requests. |
| `POST /v0/management/custom-quota-router/mode` | `{"mode":"auto"}` or `{"mode":"manual","account":"<auth id>"}` | Automatic mode, or pin one account. |
| `POST /v0/management/custom-quota-router/select` | `{"account":"<auth id>"}` | *Use now*: make an account current. |
| `POST /v0/management/custom-quota-router/refresh` | | Re-check every account's quota now. |
| `POST /v0/management/custom-quota-router/windows/start` | `{"delay_minutes":0,"stagger_minutes":0,"account":""}` | Start 5-hour windows (empty account means all). |
| `POST /v0/management/custom-quota-router/windows/schedule` | `{"account":"<auth id>","at":"<RFC 3339>","daily":true}` | Schedule a start. |
| `POST /v0/management/custom-quota-router/windows/cancel` | `{"id":"<plan id>"}`, `{"account":"<auth id>"}` or `{}` | Cancel one start, an account's starts, or everything. |
| `POST /v0/management/custom-quota-router/pause` | `{"minutes":0,"auto_resume_minutes":20}` | Pause new requests. `minutes` 0 means until resumed (max 1440). `auto_resume_minutes` 0 is off (max 24). While paused, this changes the timer and auto-resume. |
| `POST /v0/management/custom-quota-router/resume` | | End the pause. Waiting requests continue. |
| `POST /v0/management/custom-quota-router/slowdown` | `{"seconds":30}` | Wait this long before each request (0 is off, max 240). |

The panel files (`/v0/resource/plugins/custom-quota-router/ui`, `ui.js`,
`ui.css`) are static and contain no data, so they are served without
authentication.

Settings are saved through CLIProxyAPI's own config API:
`PATCH /v8/management/config/plugins/configs/custom-quota-router` with a
JSON object of the keys to change.

## Deploy and upgrade

```bash
make deploy VERSION=1.6.0      # or: scripts/deploy.py --version 1.6.0
```

Bump the version every time. CLIProxyAPI loads the highest
`custom-quota-router-v<x.y.z>.so` in its plugin directory and only hot-swaps
when it sees a new file. The script:

1. runs the unit tests (`--skip-tests` skips them);
2. builds `build/custom-quota-router-v<version>.so` with the version stamped
   in;
3. backs up `config.yaml` and the state file to `~/cliproxyapi/backups/`;
4. installs the library into `~/cliproxyapi/plugins/` (mode `0600`);
5. on first install, adds the plugin block to `config.yaml`; on upgrades,
   only rewrites `deployed_version` (in place, preserving the inode);
6. waits until CLIProxyAPI logs `plugin registered ... version=<version>`,
   reading both the journal and CLIProxyAPI's log files;
7. removes older builds of the plugin.

No restart happens. Open streams and WebSockets keep running on the old
code until they finish. The outgoing version persists its state when
CLIProxyAPI quiesces it, and the new one resumes from it.

Other options: `--dry-run` prints the new config without changing anything,
`--cpa-dir` points at another CLIProxyAPI install, `--force` reinstalls an
existing version, and `--seed-active <auth id>` picks the first current
account on a fresh install so warm sessions keep their cache.

### Before you deploy

* Run `make check` and, for anything touching the request path, `make e2e`.
* Deploying is safe while sessions are working: nothing restarts and no
  connection is closed. Avoid restarting CLIProxyAPI itself instead, which
  drops every open stream.
* Keep the printed backup path until the new version has routed a few
  requests and the panel looks right.

## Roll back

Either:

* restore the backup the deploy printed, in place:
  `cat ~/cliproxyapi/backups/config.yaml.<stamp>.bak > ~/cliproxyapi/config.yaml`;
* or turn the plugin off: set `plugins.configs.custom-quota-router.enabled:
  false`.

CLIProxyAPI hot-reloads either way, and its built-in routing (fill-first plus
session affinity) takes over. To go back to an older build, copy its `.so` in
with a version number higher than the current one and deploy again.

## Logs

With `observability.logs.logging-to-file: true`, CLIProxyAPI writes only to
`main.log` and its rotated copies, and the dashboard's *Logs* page reads
those files. The directory is chosen at startup:

1. `$WRITABLE_PATH/logs`, if that variable is set;
2. `logs/` in CLIProxyAPI's working directory, if it already exists;
3. otherwise `<auth-dir>/logs`, e.g. `~/.cli-proxy-api/logs/`.

The log viewer uses the directory chosen at startup. If you create
`~/cliproxyapi/logs` later, restart CLIProxyAPI so the writer and the viewer
agree.

With file logging off, logs go to stdout:
`journalctl --user -u cliproxyapi -f`.

Useful searches:

```bash
LOG=~/.cli-proxy-api/logs/main.log
grep 'quota-router' $LOG | tail                 # plugin load/upgrade and routing events
grep 'config changes detected' -A8 $LOG | tail  # what the last reload changed
grep -E 'websocket|codex_websockets' $LOG | tail
```

The plugin's own lines start with `quota-router:` (switches, window starts,
quota problems). It never logs tokens, prompts or replies.

## Diagnosing routing

The panel's *Recent switches* list answers most "why did it switch?"
questions. For more detail, query the state API or read the state file
(the examples use [jq](https://jqlang.org)):

```bash
KEY=...   # management key
curl -s -H "Authorization: Bearer $KEY" \
  'http://127.0.0.1:8317/v0/management/custom-quota-router/state?history=20' \
  | jq '{active, active_reason, accounts: [.accounts[] | {label, state,
         five_hour: .short.remaining_percent, weekly: .long.remaining_percent,
         busy_sessions, idle_sessions}],
         history: [.history[] | "\(.at) \(.reason)"]}'

# Which session is on which account, and how recently it was active:
jq -r '.families | to_entries | sort_by(.value.last_seen) | reverse | .[:10][]
       | "\(.value.last_seen)  \(.value.account)  last=\(.value.last // "-")  requests=\(.value.requests)"' \
  ~/cliproxyapi/plugins/custom-quota-router/state.json
```

Typical answers:

* **"Two accounts are draining at once."** A busy session stays on its
  account after the current account changes, to keep its prompt cache
  ([routing.md](routing.md#why-two-accounts-can-be-busy-at-once)). Check
  `busy_sessions` per account.
* **"It switched and I didn't ask it to."** Look for the history entry. A
  preferences save can switch the current account too, if the new ranking
  makes another account clearly better.
* **"A session went to an account I didn't choose."** Look for a CLIProxyAPI
  cooldown or a WebSocket close in the log around that time, and check
  `transient-error-cooldown-seconds` ([configuration.md](configuration.md#recommended-cliproxyapi-settings)).

## Troubleshooting

| Symptom | Check |
| --- | --- |
| Deploy times out waiting for `plugin registered` | CLIProxyAPI's log for `custom-quota-router` errors; that `config.yaml` actually changed (`config changes detected`); that the `.so` version is higher than the installed one. |
| *Quota Router* is missing from the dashboard | `plugins.enabled` and the plugin's `enabled` are `true`; `plugins.dir` points at the right directory; the log shows `plugin registered plugin_id=custom-quota-router`. |
| Settings edits have no effect | `config.yaml` was replaced rather than rewritten in place ([configuration.md](configuration.md#editing-configyaml-by-hand)); warnings in the panel. |
| Quota shows "Waiting for quota data" | `quota_poll_minutes` is not `0`; the account has sent at least one request; *Refresh* in the panel. |
| Dashboard *Logs* page says file logging is required | Turn on `observability.logs.logging-to-file` ([configuration.md](configuration.md#recommended-cliproxyapi-settings)). |
| Requests rejected with `quota_router_pinned_unavailable` | Manual mode with `manual_fallback: strict` and the pinned account unavailable. Switch to *Automatic* or `auto` fallback. |
| Requests rejected with `quota_router_no_account` | Every available Codex account is set to *Off*. |
| Requests rejected with `quota_router_paused` | Requests are paused and one waited past its limit (25 min over HTTP, 290 s over WebSocket), too many were waiting, or the plugin was upgraded mid-pause. The client retries. *Resume now*, or turn on auto-resume. |
| Agents seem stuck, nothing reaches the upstream | The panel's *Pause and slow down* status: a pause or a long slow down may be on. |
| Requests still held after turning the plugin off mid-pause | CLIProxyAPI waits for calls into the plugin to return before unloading it. Held requests end when the pause timer runs out or when they reach their hold limit (25 min at most). Resume before turning the plugin off. |
