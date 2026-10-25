#!/usr/bin/env python3
"""Build, back up, install and hot-load custom-quota-router into a running
CLIProxyAPI without restarting it.

  scripts/deploy.py --version 1.0.0 [--seed-active AUTH_ID] [--skip-tests]

Steps: run tests, build the versioned .so, back up config.yaml (and the state
file), install the .so atomically, make minimal targeted edits to config.yaml
(enable plugins, add/refresh this plugin's block), wait for CLIProxyAPI to
hot-load the new version, then remove older builds of this plugin.
CLIProxyAPI reloads when config.yaml's content changes; bumping
deployed_version is what triggers the hot reload on upgrades.
"""
import argparse
import datetime as dt
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
import time

import yaml

PLUGIN = "custom-quota-router"
MODULE = "github.com/Danielw412/cpa-custom-router"
ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))


def run(cmd, **kw):
    print("+", " ".join(cmd))
    subprocess.run(cmd, check=True, **kw)


def atomic_write(path, data, mode=0o600):
    d = os.path.dirname(path)
    fd, tmp = tempfile.mkstemp(dir=d, prefix=".deploy-")
    try:
        with os.fdopen(fd, "wb") as f:
            f.write(data)
            f.flush()
            os.fsync(f.fileno())
        os.chmod(tmp, mode)
        os.replace(tmp, path)
    finally:
        if os.path.exists(tmp):
            os.remove(tmp)


def plugin_block(version, plugins_dir, state_file):
    return f"""plugins:
  enabled: true
  dir: "{plugins_dir}"
  configs:
    {PLUGIN}:
      enabled: true
      priority: 1
      deployed_version: "{version}"
      switch_threshold_percent: 5
      strategy: "balanced"
      switch_back: true
      switch_back_margin_percent: 20
      min_dwell_minutes: 20
      cache_warm_minutes: 30
      manual_fallback: "auto"
      quota_poll_minutes: 10
      model_level_limits: false
      stale_after_minutes: 30
      history_limit: 200
      account_order: []
      reserve_accounts: []
      disabled_accounts: []
      state_file: "{state_file}"
"""


def update_config(text, version, plugins_dir, state_file):
    """Return new config text. Edits are minimal and line-based so the rest
    of the file (and any comments) is untouched."""
    data = yaml.safe_load(text) or {}
    plugins = data.get("plugins")
    if plugins is None:
        sep = "" if text.endswith("\n") else "\n"
        return text + sep + plugin_block(version, plugins_dir, state_file)

    lines = text.splitlines(keepends=True)
    start = next(i for i, l in enumerate(lines) if re.match(r"^plugins:\s*(#.*)?$", l))
    end = next((i for i in range(start + 1, len(lines)) if re.match(r"^\S", lines[i])), len(lines))
    block = lines[start:end]

    # plugins.enabled -> true
    child_indent = None
    for l in block[1:]:
        m = re.match(r"^(\s+)\S", l)
        if m:
            child_indent = m.group(1)
            break
    child_indent = child_indent or "  "
    en = [i for i, l in enumerate(block) if re.match(rf"^{child_indent}enabled:", l)]
    if en:
        block[en[0]] = f"{child_indent}enabled: true\n"
    else:
        block.insert(1, f"{child_indent}enabled: true\n")

    configs = (plugins or {}).get("configs") or {}
    if PLUGIN in configs:
        # Refresh deployed_version (and make sure the plugin is enabled).
        pi = next(i for i, l in enumerate(block) if re.match(rf"^\s+{PLUGIN}:\s*$", l))
        ind = re.match(r"^(\s+)", block[pi]).group(1)
        inner = None
        j = pi + 1
        while j < len(block) and (block[j].strip() == "" or len(re.match(r"^(\s*)", block[j]).group(1)) > len(ind)):
            if inner is None and block[j].strip():
                inner = re.match(r"^(\s*)", block[j]).group(1)
            j += 1
        inner = inner or ind + "  "
        seg = block[pi + 1:j]
        dv = [k for k, l in enumerate(seg) if re.match(rf"^{inner}deployed_version:", l)]
        if dv:
            seg[dv[0]] = f'{inner}deployed_version: "{version}"\n'
        else:
            seg.insert(0, f'{inner}deployed_version: "{version}"\n')
        ena = [k for k, l in enumerate(seg) if re.match(rf"^{inner}enabled:", l)]
        if ena:
            seg[ena[0]] = f"{inner}enabled: true\n"
        else:
            seg.insert(0, f"{inner}enabled: true\n")
        block[pi + 1:j] = seg
    else:
        ci = [i for i, l in enumerate(block) if re.match(rf"^{child_indent}configs:\s*(\{{\}})?\s*$", l)]
        body = plugin_block(version, plugins_dir, state_file).splitlines()[5:]  # lines under "<plugin>:"
        entry = f"{child_indent * 2}{PLUGIN}:\n" + "".join(child_indent * 3 + l[6:] + "\n" for l in body)
        if ci:
            block[ci[0]] = f"{child_indent}configs:\n"
            block.insert(ci[0] + 1, entry)
        else:
            block.append(f"{child_indent}configs:\n" + entry)
    lines[start:end] = block
    return "".join(lines)


def seed_state(path, active_id):
    now = dt.datetime.now(dt.timezone.utc).isoformat()
    st = {
        "version": 1, "mode": "auto", "active": active_id,
        "active_reason": "Continuing with the account CLIProxyAPI was already using, so warm sessions keep their cache",
        "active_since": now, "last_switch": now,
        "history": [{"at": now, "kind": "switch", "to": active_id,
                     "reason": "Quota router installed; continuing with the account CLIProxyAPI was already using"}],
        "families": {}, "links": {}, "accounts": {},
    }
    os.makedirs(os.path.dirname(path), mode=0o700, exist_ok=True)
    atomic_write(path, json.dumps(st, indent=2).encode())


def log_offsets(cpa_dir, cfg_text):
    """Current sizes of the main.log files CLIProxyAPI may write to.

    With logging-to-file enabled CLIProxyAPI logs only to
    $WRITABLE_PATH/logs, else <cpa-dir>/logs if that exists at startup, else
    <auth-dir>/logs; otherwise it logs to stdout (the journal). Every
    candidate is watched so the check works in all cases."""
    data = yaml.safe_load(cfg_text) or {}
    auth_dir = (data.get("oauth") or {}).get("auth-dir") or data.get("auth-dir") or "~/.cli-proxy-api"
    dirs = [os.path.join(cpa_dir, "logs"), os.path.join(os.path.expanduser(auth_dir), "logs")]
    if os.environ.get("WRITABLE_PATH"):
        dirs.insert(0, os.path.join(os.environ["WRITABLE_PATH"], "logs"))
    out = {}
    for d in dirs:
        path = os.path.join(d, "main.log")
        out[path] = os.path.getsize(path) if os.path.exists(path) else 0
    return out


def new_log_lines(offsets):
    """Lines appended to each log file since its recorded offset."""
    lines = []
    for path, off in offsets.items():
        if not os.path.exists(path):
            continue
        if os.path.getsize(path) < off:  # rotated: the new file starts fresh
            off = 0
        with open(path, "rb") as f:
            f.seek(off)
            lines += f.read().decode("utf-8", "replace").splitlines()
    return lines


def wait_for_load(version, since, offsets, timeout=45):
    deadline = time.time() + timeout
    pat = re.compile(rf"plugin registered plugin_id={PLUGIN} .*version={re.escape(version)}")
    while time.time() < deadline:
        out = subprocess.run(["journalctl", "--user", "-u", "cliproxyapi", "--since", since, "--no-pager", "-o", "cat"],
                             capture_output=True, text=True).stdout
        for line in out.splitlines() + new_log_lines(offsets):
            if pat.search(line):
                return line
            if PLUGIN in line and re.search(r"failed|invalid|fused", line, re.I):
                raise SystemExit("CLIProxyAPI reported a problem: " + line)
        time.sleep(1)
    return None


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--version", required=True)
    ap.add_argument("--cpa-dir", default=os.path.expanduser("~/cliproxyapi"))
    ap.add_argument("--seed-active", help="auth ID to start with on first install (keeps warm sessions' cache)")
    ap.add_argument("--skip-tests", action="store_true")
    ap.add_argument("--dry-run", action="store_true")
    ap.add_argument("--force", action="store_true", help="redeploy a version that is already installed")
    a = ap.parse_args()

    cfg_path = os.path.join(a.cpa_dir, "config.yaml")
    plugins_dir = os.path.join(a.cpa_dir, "plugins")
    state_file = os.path.join(plugins_dir, PLUGIN, "state.json")
    so_name = f"{PLUGIN}-v{a.version}.so"
    env = dict(os.environ, GOTOOLCHAIN="go1.26.10", CGO_ENABLED="1")
    if os.path.exists(os.path.join(plugins_dir, so_name)) and not a.force:
        raise SystemExit(f"{so_name} is already installed; bump --version (CLIProxyAPI only hot-swaps a new file)")

    if not a.skip_tests:
        run(["go", "test", "-race", "-count=1", "./..."], cwd=ROOT, env=env)
    os.makedirs(os.path.join(ROOT, "build"), exist_ok=True)
    built = os.path.join(ROOT, "build", so_name)
    run(["go", "build", "-trimpath", "-buildmode=c-shared",
         "-ldflags", f"-s -w -X {MODULE}/internal/plugin.Version={a.version}", "-o", built, "."], cwd=ROOT, env=env)
    hdr = built[:-3] + ".h"
    if os.path.exists(hdr):
        os.remove(hdr)

    with open(cfg_path) as f:
        old_cfg = f.read()
    new_cfg = update_config(old_cfg, a.version, plugins_dir, state_file)
    parsed = yaml.safe_load(new_cfg)
    pc = parsed["plugins"]["configs"][PLUGIN]
    assert parsed["plugins"]["enabled"] is True and pc["enabled"] is True and pc["deployed_version"] == a.version
    if a.dry_run:
        print(new_cfg)
        return

    stamp = dt.datetime.now().strftime("%Y%m%d-%H%M%S")
    backups = os.path.join(a.cpa_dir, "backups")
    os.makedirs(backups, mode=0o700, exist_ok=True)
    backup = os.path.join(backups, f"config.yaml.{stamp}.bak")
    shutil.copy2(cfg_path, backup)
    os.chmod(backup, 0o600)
    print("backed up config to", backup)
    if os.path.exists(state_file):
        shutil.copy2(state_file, os.path.join(backups, f"{PLUGIN}-state.{stamp}.json"))

    os.makedirs(plugins_dir, mode=0o700, exist_ok=True)
    with open(built, "rb") as f:
        atomic_write(os.path.join(plugins_dir, so_name), f.read(), 0o600)
    if a.seed_active and not os.path.exists(state_file):
        seed_state(state_file, a.seed_active)
        print("seeded initial account:", a.seed_active)

    since = dt.datetime.now().strftime("%Y-%m-%d %H:%M:%S")
    offsets = log_offsets(a.cpa_dir, old_cfg)
    time.sleep(1)
    # Rewrite config.yaml in place: CLIProxyAPI watches the file's inode, so
    # replacing it via rename would go unnoticed (and orphan its watch).
    with open(cfg_path, "r+b") as f:
        f.write(new_cfg.encode())
        f.truncate()
        f.flush()
        os.fsync(f.fileno())
    print("config updated; waiting for CLIProxyAPI to hot-load", so_name)
    line = wait_for_load(a.version, since, offsets)
    if not line:
        raise SystemExit("plugin did not report as registered in time; check CLIProxyAPI's log "
                         f"({' or '.join(offsets)} or `journalctl --user -u cliproxyapi`). "
                         f"Roll back with: cp {backup} {cfg_path}")
    print("loaded:", line.strip())
    for name in os.listdir(plugins_dir):
        if re.fullmatch(rf"{PLUGIN}-v.+\.so", name) and name != so_name:
            os.remove(os.path.join(plugins_dir, name))
            print("removed old build", name)
    print(f"rollback: cp {backup} {cfg_path}  (CLIProxyAPI hot-reloads; built-in routing resumes)")


if __name__ == "__main__":
    main()
