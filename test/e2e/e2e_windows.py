#!/usr/bin/env python3
"""End-to-end test for 5-hour window starts against the real CLIProxyAPI
binary (run on a fresh staging instance: test/e2e/staging.sh)."""
import json
import os
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from e2e import API, check, mgmt, mock_log, respond, state, PASSED  # noqa: E402


def wait(what, cond, timeout=30):
    end = time.time() + timeout
    while time.time() < end:
        v = cond()
        if v:
            return v
        time.sleep(0.5)
    check(False, what, "(timed out)")


def main():
    print("window starts")
    st, entries = respond("warmup")
    check(st == 200, "warm-up request routed")
    warmed = entries[-1]["key"]
    accounts = state()["accounts"]
    check(len(accounts) == 3, "three accounts known")
    before = len(mock_log())

    # Window starts you scheduled go out even while agents' requests are paused.
    check(mgmt("POST", API + "/pause", {"minutes": 30})[0] == 200, "pause requests")
    status, _ = mgmt("POST", API + "/windows/start", {})
    check(status == 200, "start all now accepted")
    log = wait("start results for every account", lambda: (lambda l: l if len(l) >= 3 else None)(state()["starter"]["log"]))
    check(state().get("pause") is not None, "window starts completed while paused")
    check(mgmt("POST", API + "/resume")[0] == 200, "resume")
    results = {r["account"]: r for r in log}
    sent = [e for e in mock_log()[before:]]
    keys_sent = sorted(e["key"] for e in sent)
    check(len(sent) == 2 and warmed not in keys_sent, "requests sent only to the two idle accounts", keys_sent)
    check(all(e["model"] == "gpt-6-luna" and e["effort"] == "low" for e in sent), "each start is gpt-6-luna with low reasoning", sent)
    check(all(e.get("store") is False for e in sent), "each start is sent with store=false", sent)
    sessions = [e["session"] for e in sent]
    check(all(sessions) and len(set(sessions)) == len(sessions) and all(e.get("cache_key") == e["session"] for e in sent),
          "every start uses its own one-off session ID (nothing shared between accounts)", sent)
    statuses = sorted(r["status"] for r in results.values())
    check(statuses == ["already_running", "started", "started"], "warm account reported as already running", statuses)
    v = state()
    check(all(a["window"] == "running" for a in v["accounts"]), "every window now running", [a["window"] for a in v["accounts"]])
    cost = v["starter"].get("last_cost")
    check(cost and cost["input_tokens"] > 0, "cost measured from the response", cost)
    print(f"      measured on mock: {cost['input_tokens']} input / {cost['output_tokens']} output tokens")

    before = len(mock_log())
    mgmt("POST", API + "/windows/start", {})
    wait("second round results", lambda: len(state()["starter"]["log"]) >= 6)
    check(len(mock_log()) == before, "starting again sends nothing (all windows running)")

    acc = accounts[0]["id"]
    at = time.strftime("%Y-%m-%dT%H:%M:%S%z", time.localtime(time.time() + 3 * 3600))
    at = at[:-2] + ":" + at[-2:]
    status, _ = mgmt("POST", API + "/windows/schedule", {"account": acc, "at": at, "daily": True})
    check(status == 200, "manual daily start time accepted")
    status, _ = mgmt("POST", API + "/windows/start", {"delay_minutes": 30, "stagger_minutes": 20})
    check(status == 200, "delayed + staggered starts accepted")
    plans = state()["starter"]["plans"]
    check(len(plans) == 4 and sum(1 for p in plans if p.get("daily")) == 1, "daily plan kept alongside one-off plans", plans)
    gaps = sorted(time.mktime(time.strptime(p["at"][:19], "%Y-%m-%dT%H:%M:%S")) for p in plans if not p.get("daily"))
    check(round((gaps[1] - gaps[0]) / 60) == 20 and round((gaps[2] - gaps[1]) / 60) == 20, "stagger spacing is 20 min")
    status, _ = mgmt("POST", API + "/windows/cancel", {})
    check(status == 200 and not state()["starter"]["plans"], "turn everything off clears every plan")
    one = accounts[1]["id"]
    status, _ = mgmt("POST", API + "/windows/start", {"account": one, "delay_minutes": 25})
    plans = state()["starter"]["plans"]
    check(status == 200 and len(plans) == 1 and plans[0]["account"] == one, "one specific account scheduled in 25 min", plans)
    from datetime import datetime, timezone
    mins = (datetime.fromisoformat(plans[0]["at"]) - datetime.now(timezone.utc)).total_seconds() / 60
    check(24 <= mins <= 26, "it starts 25 minutes from now", round(mins, 1))
    mgmt("POST", API + "/windows/cancel", {})

    print("auto-start")
    cfg = "/v8/management/config/plugins/configs/custom-quota-router"
    before = len(mock_log())
    status, _ = mgmt("PATCH", cfg, {"auto_start_windows": True})
    check(status == 200, "auto-start turned on through CLIProxyAPI's config API", status)
    wait("plugin hot-applied auto-start", lambda: state()["settings"].get("auto_start_windows") is True)
    with open(os.path.join(os.environ.get("STAGE", "/tmp/cqr-staging"), "config.yaml")) as f:
        check("auto_start_windows: true" in f.read(), "auto-start saved in config.yaml")
    check(any("Auto-start on" in h["reason"] for h in state()["history"]), "turning it on is in the history")
    time.sleep(3)
    # The staging accounts are API keys: their quota can't be read before
    # sending, so auto-start leaves them alone.
    check(len(mock_log()) == before, "nothing sent for accounts whose quota can't be checked first")
    check(all(not a.get("auto_start_at", "0001").startswith("2") for a in state()["accounts"]), "no automatic start shown for them")
    status, _ = mgmt("PATCH", cfg, {"auto_start_windows": False})
    check(status == 200, "auto-start turned off")
    wait("plugin hot-applied auto-start off", lambda: state()["settings"].get("auto_start_windows") is False)
    print(f"\nALL {len(PASSED)} WINDOW CHECKS PASSED")


if __name__ == "__main__":
    main()
