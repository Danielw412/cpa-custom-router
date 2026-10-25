#!/usr/bin/env python3
"""End-to-end test: real CLIProxyAPI binary + custom-quota-router .so + mock
Codex upstream (see staging.sh). Exits non-zero on the first failed check."""
import asyncio
import json
import os
import shutil
import subprocess
import sys
import time
import urllib.error
import urllib.request

import websockets

CPA = "http://127.0.0.1:18317"
MOCK = "http://127.0.0.1:18400"
MGMT_KEY = "staging-mgmt-key"
CLIENT_KEY = "staging-client-key"
API = "/v0/management/custom-quota-router"
STAGE = os.environ.get("STAGE", "/tmp/cqr-staging")
PASSED = []


def check(cond, what, detail=""):
    if not cond:
        print(f"FAIL: {what} {detail}")
        sys.exit(1)
    PASSED.append(what)
    print(f"  ok  {what}")


def http(method, url, body=None, headers=None):
    data = None if body is None else json.dumps(body).encode()
    req = urllib.request.Request(url, data=data, method=method, headers=headers or {})
    if body is not None:
        req.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(req, timeout=60) as r:
            raw = r.read()
            return r.status, raw
    except urllib.error.HTTPError as e:
        return e.code, e.read()


def mgmt(method, path, body=None):
    status, raw = http(method, CPA + path, body, {"Authorization": f"Bearer {MGMT_KEY}"})
    try:
        return status, json.loads(raw or b"null")
    except ValueError:
        return status, raw


def state():
    return mgmt("GET", API + "/state?history=200")[1]


def control(**keys):
    http("POST", MOCK + "/_control", keys)


def mock_log():
    return json.loads(http("GET", MOCK + "/_log")[1]) or []


def respond(session, extra=None):
    """One HTTP Responses request. Returns (status, [mock log entries it caused])."""
    before = len(mock_log())
    headers = {"Authorization": f"Bearer {CLIENT_KEY}", "session_id": session}
    headers.update(extra or {})
    status, _ = http("POST", CPA + "/v1/responses",
                     {"model": "gpt-5.5", "stream": True, "input": [{"role": "user", "content": "hi"}]}, headers)
    time.sleep(0.35)  # let CLIProxyAPI's async usage queue reach the plugin
    return status, mock_log()[before:]


def served(entries):
    ok = [e for e in entries if e["status"] == 200]
    return ok[-1]["key"] if ok else None


class WS:
    def __init__(self, session):
        self.session = session

    async def __aenter__(self):
        self.conn = await websockets.connect(
            CPA.replace("http", "ws") + "/v1/responses",
            extra_headers={"Authorization": f"Bearer {CLIENT_KEY}", "session_id": self.session},
            open_timeout=20)
        return self

    async def __aexit__(self, *exc):
        await self.conn.close()

    async def turn(self):
        before = len(mock_log())
        await self.conn.send(json.dumps({"type": "response.create", "model": "gpt-5.5",
                                         "input": [{"role": "user", "content": "hi"}]}))
        last = None
        while True:
            try:
                msg = json.loads(await asyncio.wait_for(self.conn.recv(), timeout=30))
            except websockets.ConnectionClosed as e:
                return ("closed", e.code, mock_log()[before:])
            last = msg.get("type")
            if last in ("response.completed", "error", "response.failed"):
                await asyncio.sleep(0.35)
                return (last, None, mock_log()[before:])


def main():
    print("auth and resources")
    check(http("GET", CPA + API + "/state")[0] == 401, "management API requires the key")
    status, raw = http("GET", CPA + "/v0/resource/plugins/custom-quota-router/ui")
    check(status == 200 and b"ui.js" in raw, "panel resource served")

    print("account discovery and mapping")
    st, _ = respond("warmup")
    check(st == 200, "request through plugin scheduler succeeds")
    accounts = [a["id"] for a in state()["accounts"]]
    check(len(accounts) == 3, "plugin sees the three codex credentials", accounts)
    key_of = {}
    for acc in accounts:
        check(mgmt("POST", API + "/mode", {"mode": "manual", "account": acc})[0] == 200, f"pin {acc[:24]}")
        _, entries = respond("mapping")
        key_of[acc] = served(entries)
    check(sorted(key_of.values()) == ["mock-key-1", "mock-key-2", "mock-key-3"], "manual pin routes each request to the pinned account", key_of)
    acc_of = {v: k for k, v in key_of.items()}
    a1, a2, a3 = acc_of["mock-key-1"], acc_of["mock-key-2"], acc_of["mock-key-3"]

    print("settings through CLIProxyAPI's config API")
    status, _ = mgmt("PATCH", "/v8/management/config/plugins/configs/custom-quota-router",
                     {"account_order": [a1, a2, a3], "switch_threshold_percent": 5, "min_dwell_minutes": 20})
    check(status == 200, "PATCH plugin config", status)
    time.sleep(0.8)
    check(state()["settings"]["account_order"] == [a1, a2, a3], "plugin hot-applied the new account order")
    with open(os.path.join(STAGE, "config.yaml")) as f:
        check(a1 in f.read(), "account order persisted in config.yaml")
    check(mgmt("POST", API + "/mode", {"mode": "auto"})[0] == 200, "resume automatic routing")
    check(mgmt("POST", API + "/select", {"account": a1})[0] == 200, "select account 1")

    print("session affinity")
    for s in ("s1", "s2"):
        check(served(respond(s)[1]) == "mock-key-1", f"{s} -> account 1")
    sub = {"Thread_id": "child-1", "X-Openai-Subagent": "collab_spawn"}
    check(served(respond("s1", sub)[1]) == "mock-key-1", "subagent of s1 shares account 1")

    print("threshold switch")
    control(**{"mock-key-1": {"short": 97}})
    check(served(respond("s1")[1]) == "mock-key-1", "request in flight finishes on account 1 (reports 97% used)")
    check(served(respond("s3")[1]) == "mock-key-2", "new session goes to account 2 once account 1 < 5%")
    check(served(respond("s1")[1]) == "mock-key-2", "s1 migrates at its next request boundary")
    check(served(respond("s1", sub)[1]) == "mock-key-2", "subagent follows its parent")
    v = state()
    check(v["active"] == a2, "account 2 is now current")
    check(any(h["kind"] == "switch" and "threshold" in h["reason"] for h in v["history"]), "history explains the switch")

    print("quota exhaustion (429 usage_limit_reached)")
    control(**{"mock-key-2": {"exhausted": True}})
    st, entries = respond("s1")
    statuses = [(e["key"], e["status"]) for e in entries]
    check(st == 200 and ("mock-key-2", 429) in statuses and served(entries) == "mock-key-3",
          "exhausted account fails over to account 3 within the same request", statuses)
    v = state()
    a2state = next(a for a in v["accounts"] if a["id"] == a2)
    check(a2state["state"] == "exhausted", "account 2 marked exhausted", a2state["state"])
    check(served(respond("s5")[1]) == "mock-key-3", "new sessions avoid the exhausted account")

    print("websocket pinning (upstream websocket transport)")
    control(**{"mock-key-2": {"exhausted": False}})

    async def ws_checks():
        async with WS("ws-a") as w1:
            kind, _, e1 = await w1.turn()
            check(kind == "response.completed" and served(e1) == "mock-key-3" and e1[-1]["transport"] == "ws",
                  "websocket turn served over upstream websocket by the current account", e1)
            conn1 = e1[-1]["conn"]
            check(mgmt("POST", API + "/mode", {"mode": "manual", "account": a1})[0] == 200, "pin account 1 mid-socket")
            kind, _, e2 = await w1.turn()
            check(kind == "response.completed" and served(e2) == "mock-key-3" and e2[-1]["conn"] == conn1,
                  "open socket keeps its account and upstream connection (not interrupted)", e2)
            async with WS("ws-b") as w2:
                kind, _, e3 = await w2.turn()
                check(kind == "response.completed" and served(e3) == "mock-key-1", "a new socket follows the manual pin", e3)
            control(**{"mock-key-3": {"exhausted": True}})
            kind, code, e4 = await w1.turn()
            print(f"      pinned socket on exhausted account -> {kind} {code} {[(x['key'], x['status']) for x in e4]}")
            check(any(x["key"] == "mock-key-3" and x["status"] == 429 for x in e4) or kind == "closed",
                  "exhaustion on a pinned socket is surfaced to CLIProxyAPI (pin released)")
        await asyncio.sleep(0.5)
        a3state = next(a for a in state()["accounts"] if a["id"] == a3)
        check(a3state["state"] == "exhausted", "websocket usage-limit error marks the account exhausted", a3state["state"])
        async with WS("ws-a") as w3:
            kind, _, e5 = await w3.turn()
            check(kind == "response.completed" and served(e5) == "mock-key-1", "reconnected session lands on a healthy account", e5)

    asyncio.run(ws_checks())
    stats = state()["pick_stats"]
    check(stats.get("websocket_pinned", 0) >= 1, "plugin saw CLIProxyAPI's websocket pin", stats)
    check(mgmt("POST", API + "/mode", {"mode": "auto"})[0] == 200, "resume automatic routing")

    print("hot upgrade keeps runtime state")
    before = state()
    so_new = os.environ.get("SO_NEXT")
    if so_new:
        shutil.copy(so_new, os.path.join(STAGE, "plugins", os.path.basename(so_new)))
        mgmt("PATCH", "/v8/management/config/plugins/configs/custom-quota-router", {"history_limit": 201})
        deadline = time.time() + 20
        after = None
        while time.time() < deadline:
            after = state()
            if after.get("version") != before.get("version"):
                break
            time.sleep(0.5)
        check(after and after.get("version") != before.get("version"), "new plugin version loaded without restart", after.get("version"))
        check(after["mode"] == before["mode"] and after["active"] == before["active"], "mode and current account preserved")
        check(len(after["history"]) >= len(before["history"]), "history preserved")
        check(served(respond("s5")[1]) is not None, "routing works after upgrade")
        time.sleep(1)
        check(not any("removed" in h["reason"] for h in state()["history"]), "no spurious 'removed' switch after upgrade")

    print(f"\nALL {len(PASSED)} CHECKS PASSED")
    print("pick stats:", json.dumps(state()["pick_stats"]))


if __name__ == "__main__":
    main()
