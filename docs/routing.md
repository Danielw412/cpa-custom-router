# Routing

How the router chooses an account for each request, when it switches, and
where its quota numbers come from. Settings named here are described in
[configuration.md](configuration.md).

## Two decisions

The router makes two separate decisions:

1. **The current account.** One account receives every *new* session. It is
   re-evaluated on every request and changes only for a reason (below). The
   panel labels it *Current*.
2. **Each session's account.** Every conversation is bound to the account it
   started on and stays there while its prompt cache is warm, even if the
   current account changes in the meantime.

Keeping both stable is the point: Codex prompt caching is per account, so
moving a busy conversation costs a full uncached turn, and spreading new work
across accounts drains several 5-hour windows at once.

## Sessions and families

Session identity comes from request headers and metadata, extracted the same
way CLIProxyAPI does (`internal/engine/session.go`):

* Codex `session_id` / `thread_id` headers;
* Claude Code and generic session headers;
* CLIProxyAPI's per-WebSocket execution ID.

Subagents and forks carry their parent's ID, so they resolve to the parent's
*family*. A main agent and all of its subagents count as one session and
always share one account.

Requests with no session identity (for example clients that only send
`prompt_cache_key` in the body, which the scheduler hook cannot see) go to the
current account without stickiness.

## Per-request decision

For each request, `Engine.Pick` runs in this order:

| Situation | Result |
| --- | --- |
| CLIProxyAPI pinned the Responses WebSocket to an account (`pinned_auth_id`) | That account, always. Moving it would force a replay. |
| Manual mode and the pinned account is offered | The pinned account. |
| New session | The current account; the session is bound to it. |
| Bound account healthy | The bound account (*session affinity*). |
| Bound account healthy, but session idle longer than `cache_warm_minutes` and the current account differs | Rebind to the current account (*rejoin*); its cache is cold anyway. |
| Bound account below `switch_threshold_percent` | Move to the current account (or the best-ranked healthy one). |
| Bound account exhausted, set to *Off*, removed from CLIProxyAPI, or unavailable for 3+ minutes | Move now. |
| Bound account briefly missing from the candidates (cooldown, retry) | *Detour* this request only; the binding is kept for up to 3 minutes. |
| Every offered account looks exhausted to the router | The least-bad offered account that is not *Off* (*lenient pick*). The router never rejects a request on its own estimate. |
| Only non-Codex candidates, or nothing routable | Defer to CLIProxyAPI's built-in selector (`Handled: false`). |

Moves happen only at request boundaries. Streams already in flight are never
touched.

## The current account

`Engine.evaluateActive` changes the current account for these reasons only:

* **Emergency** (immediately, ignoring the dwell time): the account is below
  the threshold on its 5-hour or weekly window, hit a usage limit, was removed
  or disabled in CLIProxyAPI, has been unavailable for several minutes, or was
  set to *Off*. A low account is only traded for one that actually has more
  quota.
* **Switch back** (`switch_back: true`): a better-ranked account clearly wins
  (see below), has at least `switch_threshold_percent +
  switch_back_margin_percent` left, and `min_dwell_minutes` have passed since
  the last switch.
* **You**: *Use now* or *Pin* in the panel. A hand-picked account is never
  switched away from automatically except for an emergency.

When the plugin starts with no quota data yet, the first choice is
provisional for 15 minutes and is corrected once real numbers arrive.

## Ranking

`Engine.rank` orders accounts in two steps.

**1. Health tier.** Healthy normal accounts come first. Accounts below the
threshold, *Reserve* accounts and exhausted accounts follow. *Off* accounts
are never chosen automatically.

**2. Your criteria (`rank_by`)**, most important first. Each criterion only
decides between accounts that tie on the ones above it.

| Criterion | Prefers | Counts as a tie when |
| --- | --- | --- |
| `account_order` | Higher in `account_order` | Same position (unlisted accounts share the last one) |
| `weekly_reset` | Weekly window that resets sooner, so its quota is used before it is lost | Resets less than 24 h apart |
| `five_hour_reset` | Running 5-hour window that resets sooner. Windows that haven't started rank last, so they stay unstarted | Same reset time, or neither window is running |
| `most_remaining` | More quota left (the lower of the two windows) | Within 5 points |

The default is `account_order, weekly_reset, five_hour_reset,
most_remaining`. Missing criteria are appended in this order.

### Clear wins

Switching back uses the same criteria, but needs a *clear* win on the first
criterion where the two accounts differ:

| Criterion | Clear win |
| --- | --- |
| `account_order` | Any difference |
| `weekly_reset` | Resets at least 24 h sooner, and both reset times are known |
| `five_hour_reset` | Resets at least 30 min sooner, or the current account has no running window |
| `most_remaining` | Only when it is the first criterion, and only by more than `switch_back_margin_percent` |

`most_remaining` is not use-it-or-lose-it, so it never triggers a switch on
its own unless it ranks first. This avoids pointless switches that would
leave two accounts draining at once.

## Why two accounts can be busy at once

Session affinity means a long-running conversation keeps its account after
the current account changes. The panel shows this as "*N sessions working
now*" on more than one account, plus a line under the routing summary naming
the busy sessions on other accounts. Each one moves to the current account
when:

* it goes idle for longer than `cache_warm_minutes` (default 30), or
* its account drops below the threshold, is exhausted, or is set to *Off*.

## Pinned WebSockets

CLIProxyAPI pins an open Codex Responses WebSocket to the account of its
first request and offers the plugin no other choice for it afterwards. If
that account falls below the threshold, the open socket keeps using it while
new sessions and reconnects move at once. If the account hits its hard
limit, CLIProxyAPI closes the socket, the client reconnects, and the
reconnect is routed to a healthy account. Quota is checked before generation
starts, so no output is lost.

This is the main reason the router moves new work proactively at the
threshold rather than waiting for a hard limit.

## Transient errors and CLIProxyAPI cooldowns

By default CLIProxyAPI puts an account on a 60-second cooldown after any
unclassified upstream error, such as a WebSocket closing with code 1012. The
router sees the account disappear from the candidates and detours the
affected requests. A WebSocket that reconnects during the detour is then
pinned to the detour account, which can leave work running on an account you
did not choose.

Set `routing.cooldown.transient-error-cooldown-seconds: -1` in CLIProxyAPI to
disable that cooldown. Real quota errors (429) still cool down as before. See
[configuration.md](configuration.md#recommended-cliproxyapi-settings).

## Quota data

* **Passive.** Every completed request reports Codex's rate-limit data: HTTP
  `x-codex-*` headers and WebSocket `codex.rate_limits` events, which
  CLIProxyAPI normalizes into the usage record the plugin receives. This adds
  nothing to the request path.
* **Active.** Idle accounts are refreshed every `quota_poll_minutes` (default
  10) through Codex's read-only usage endpoint, the same one CLIProxyAPI's
  dashboard uses. Calls go through CLIProxyAPI's HTTP transport, so its proxy
  settings apply. At most 3 calls are made per 15-second tick.
* **Exhaustion.** A `usage_limit_reached` failure marks the account exhausted
  until its reset time. Fresh data showing quota again lifts this early. With
  `model_level_limits: true` the block applies to that model only.
* **Staleness.** When a window's reset time passes, that window counts as
  full (shown as `~100%`) until new numbers arrive. Data older than
  `stale_after_minutes` is flagged in the panel.

## 5-hour windows

A Codex 5-hour window starts at an account's first request. Until then the
reported reset time keeps sliding. Once a request or a start has shown a
window running, the router counts it as running until its reset, even if
quota readings still show 0% used. If a later reading reports a different
reset time, for example because Codex reset the limits early, the reading
wins. The panel's **5-hour windows** section
starts windows on purpose with one tiny request, so they reset when you want:

* **Start all now:** every account whose window isn't already running.
* **Start all in N min:** the same, once, after a delay.
* **Stagger:** one account now, the next N minutes later, and so on. The
  order is the current account first, then your routing order.
* **Schedule** (per account): either **In minutes** (one start N minutes from
  now, replacing that account's other one-off starts) or **At a time** (a
  clock time, optionally every day, replacing all its scheduled starts).
* **Start now** (per account) and **Cancel** (per scheduled start).
* **Turn everything off:** cancels every scheduled start, daily ones
  included.

Each start:

1. reads the account's quota and sends nothing if its window is already
   running;
2. otherwise sends one request to `window_start_model` with
   `window_start_effort` (default `gpt-6-luna`, `low`) through CLIProxyAPI's
   normal pipeline, locked to that account;
3. reads quota again afterwards and records the cost.

Measured on 2026-10-24, one start cost 314 input and 5 output tokens. That is
below the 1% resolution Codex reports for both windows.

Starts are not saved anywhere as conversations. They use `store: false` and
no conversation or previous-response reference. Each one carries a fresh
random session and cache ID, so starts on different accounts share no
identifier. The plugin records only times, statuses, token counts and
percentages, never the prompt or reply.

Starts that fall due while CLIProxyAPI is down are reported as missed rather
than sent late. Starting a 5-hour window also starts the weekly window if it
isn't already running.
