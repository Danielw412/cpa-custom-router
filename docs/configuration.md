# Configuration

## Plugin settings

Settings live in CLIProxyAPI's `config.yaml` under
`plugins.configs.custom-quota-router`. They are applied as soon as
CLIProxyAPI reloads the file, with no restart. You can change them in three
places:

* the panel's **Preferences** section (saved through CLIProxyAPI's config
  API);
* the dashboard's plugin settings page;
* `config.yaml` directly (see [Editing config.yaml by hand](#editing-configyaml-by-hand)).

| Key | Default | Range | Meaning |
| --- | --- | --- | --- |
| `enabled` | `true` | | Turn the plugin off to fall back to CLIProxyAPI's built-in routing. |
| `priority` | `1` | | CLIProxyAPI plugin priority. |
| `switch_threshold_percent` | `5` | 0–90 | Leave an account when either window's remaining quota drops below this. |
| `rank_by` | `[account_order, weekly_reset, five_hour_reset, most_remaining]` | | Ranking criteria, most important first ([routing.md](routing.md#ranking)). Unknown entries are dropped; missing ones are appended. |
| `strategy` | `balanced` | | Legacy preset, used only when `rank_by` is empty: `balanced` (the default ranking), `priority` (order, most quota, weekly, 5-hour), `soonest-reset` (weekly, 5-hour, order, most quota), `most-remaining` (most quota, order, weekly, 5-hour). |
| `switch_back` | `true` | | Return to a better-ranked account when it clearly wins. |
| `switch_back_margin_percent` | `20` | 0–90 | Headroom above the threshold an account needs before switching back to it. Also the margin a `most_remaining` win needs. |
| `min_dwell_minutes` | `20` | 0–1440 | Minimum time between non-emergency switches. |
| `cache_warm_minutes` | `30` | 0–1440 | How long an idle session keeps its account (estimated prompt-cache lifetime). |
| `account_order` | `[]` | | Auth IDs, most preferred first. Unlisted accounts share the lowest position. |
| `reserve_accounts` | `[]` | | Used automatically only when no normal account has quota. |
| `disabled_accounts` | `[]` | | *Off*: never chosen automatically. You can still pin them. |
| `manual_fallback` | `auto` | `auto`, `strict` | When the pinned account is unavailable: route automatically until it returns, or reject requests. |
| `quota_poll_minutes` | `10` | 0, or 2–1440 | Active quota checks for idle accounts. `0` uses live traffic only. |
| `model_level_limits` | `false` | | Scope usage-limit failures to the model that hit them. Enable only together with CLIProxyAPI's `upstream.codex.model-level-cooling`. |
| `stale_after_minutes` | `30` | 1–1440 | Flag older quota data as stale. |
| `history_limit` | `200` | 10–5000 | Routing history entries kept. |
| `window_start_model` | `gpt-6-luna` | | Model used to start 5-hour windows. |
| `window_start_effort` | `low` | | Reasoning effort for window starts. |
| `state_file` | `plugins/custom-quota-router/state.json` | | Runtime state. Relative paths resolve against CLIProxyAPI's working directory; `~` is expanded. |
| `deployed_version` | | | Written by the deploy script. Changing it is what makes CLIProxyAPI reload and pick up a new build. |

Account lists use CLIProxyAPI auth IDs, the auth file names such as
`codex-0123abcd-name@example.com-plus.json`. The panel fills them in for you.

Invalid values never take routing offline. Out-of-range numbers are clamped,
unknown names fall back to their defaults, an account listed as both reserve
and disabled is treated as disabled, and every adjustment is shown as a
warning in the panel.

### Example

```yaml
plugins:
    enabled: true
    dir: "/home/you/cliproxyapi/plugins"
    configs:
        custom-quota-router:
            enabled: true
            priority: 1
            deployed_version: "1.6.0"
            switch_threshold_percent: 5
            rank_by: [five_hour_reset, account_order, weekly_reset, most_remaining]
            switch_back: true
            switch_back_margin_percent: 10
            min_dwell_minutes: 20
            cache_warm_minutes: 30
            account_order:
                - codex-aaaa1111-first@example.com-plus.json
                - codex-bbbb2222-second@example.com-plus.json
            reserve_accounts: []
            disabled_accounts: []
            manual_fallback: auto
            quota_poll_minutes: 10
            state_file: "/home/you/cliproxyapi/plugins/custom-quota-router/state.json"
```

## Runtime state

The state file holds what must survive restarts and hot upgrades:

* the mode, pinned account and current account;
* session bindings and parent links;
* the last known quota per account;
* routing history;
* window-start plans and results.

It is written atomically with mode `0600` every 15 seconds when something
changed, and on shutdown or upgrade. It contains auth IDs, email labels, plan
names and percentages, and never tokens.

Deleting it while CLIProxyAPI is stopped resets the router to a fresh start:
warm sessions lose their bindings and the current account is chosen again.

## Recommended CLIProxyAPI settings

These are CLIProxyAPI settings, not plugin settings. The plugin works without
them, but behaves best with them.

```yaml
routing:
    strategy: "fill-first"            # built-in fallback when the plugin defers
    session-affinity: true
    session-affinity-ttl: "1h"
    session-affinity-rebind-on: "quota"
    cooldown:
        transient-error-cooldown-seconds: -1
observability:
    logs:
        logging-to-file: true
        logs-max-total-size-mb: 100
    usage:
        usage-statistics-enabled: true
```

* **`transient-error-cooldown-seconds: -1`** stops CLIProxyAPI from cooling an
  account down for 60 seconds after an unclassified error, such as a
  WebSocket closing with 1012. Without it, a reconnecting WebSocket can be
  pinned to a detour account ([routing.md](routing.md#transient-errors-and-cliproxyapi-cooldowns)).
  429 quota cooldowns are unaffected.
* **`logging-to-file: true`** is required for the dashboard's *Logs* page,
  which reads only log files. With it on, CLIProxyAPI stops logging to
  stdout, so the systemd journal no longer shows its logs
  ([operations.md](operations.md#logs)). Always set
  `logs-max-total-size-mb`: CLIProxyAPI keeps every rotated 10 MB file
  otherwise.
* **`usage-statistics-enabled: true`** keeps CLIProxyAPI's usage pages
  working. The plugin's passive quota observation does not depend on it.
* Leave **`request-log`** off. It writes every prompt and response to disk.
  Failed requests are still logged on their own (*Error Request Logs*).

## Editing config.yaml by hand

CLIProxyAPI watches `config.yaml` by inode. Editors and scripts that save by
writing a new file and renaming it over the old one break that watch, and
the change is silently ignored until CLIProxyAPI restarts. So:

* back up first: `cp -p config.yaml backups/config.yaml.$(date +%Y%m%d-%H%M%S).bak`;
* rewrite the file in place: open it read/write and truncate, use
  `cat new > config.yaml`, or use an editor that writes in place;
* check the reload in the log: `config changes detected:` followed by the
  keys you changed.

When CLIProxyAPI itself saves the file (the panel, the dashboard, the
management API), it re-serializes the whole YAML with 4-space indentation and
drops comments. Keep comments elsewhere if you need them.
