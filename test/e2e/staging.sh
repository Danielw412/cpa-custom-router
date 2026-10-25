#!/usr/bin/env bash
# Start an isolated CLIProxyAPI staging instance (port 18317) with the plugin
# and a mock Codex upstream (port 18400). Uses no real credentials.
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
STAGE="${STAGE:-/tmp/cqr-staging}"
BIN="${CPA_BIN:-$HOME/cliproxyapi/cli-proxy-api}"
SO="${SO:-$ROOT/build/custom-quota-router-v0.0.1.so}"

stop() {
  [[ -f "$STAGE/cpa.pid" ]] && kill "$(cat "$STAGE/cpa.pid")" 2>/dev/null || true
  [[ -f "$STAGE/mock.pid" ]] && kill "$(cat "$STAGE/mock.pid")" 2>/dev/null || true
  sleep 1
}
if [[ "${1:-}" == "stop" ]]; then stop; exit 0; fi
stop
rm -rf "$STAGE"; mkdir -p "$STAGE/auths" "$STAGE/plugins"
cp "$SO" "$STAGE/plugins/"
mkdir -p "$STAGE/static" && cp "$HOME/cliproxyapi/static/management.html" "$STAGE/static/"
cat > "$STAGE/config.yaml" <<YAML
config-version: 8
server:
  host: "127.0.0.1"
  port: 18317
management:
  allow-remote: false
  secret-key: "staging-mgmt-key"
  disable-control-panel: false
  disable-auto-update-panel: true
access:
  api-keys:
    - "staging-client-key"
oauth:
  auth-dir: "$STAGE/auths"
routing:
  strategy: "fill-first"
  session-affinity: true
  session-affinity-ttl: "1h"
  session-affinity-rebind-on: "quota"
observability:
  usage:
    usage-statistics-enabled: true
api-keys:
  codex:
    - name: mock
      base-url: "http://127.0.0.1:18400/v1"
      models:
        - name: "gpt-5.5"
          alias: "gpt-5.5"
        - name: "gpt-6-luna"
          alias: "gpt-6-luna"
      keys:
        - api-key: "mock-key-1"
          websockets: true
        - api-key: "mock-key-2"
          websockets: true
        - api-key: "mock-key-3"
          websockets: true
plugins:
  enabled: true
  dir: "$STAGE/plugins"
  configs:
    custom-quota-router:
      enabled: true
      priority: 1
      switch_threshold_percent: 5
      min_dwell_minutes: 20
      quota_poll_minutes: 0
      state_file: "$STAGE/state.json"
YAML
"$ROOT/build/cqr-mock" 127.0.0.1:18400 > "$STAGE/mock.log" 2>&1 & echo $! > "$STAGE/mock.pid"
(cd "$STAGE" && exec "$BIN" -config "$STAGE/config.yaml" -local-model > "$STAGE/cpa.log" 2>&1) & echo $! > "$STAGE/cpa.pid"
for i in $(seq 1 60); do
  if curl -fsS -o /dev/null -H "Authorization: Bearer staging-mgmt-key" http://127.0.0.1:18317/v0/management/custom-quota-router/state 2>/dev/null; then
    echo "staging ready"; exit 0
  fi
  sleep 0.5
done
echo "staging failed to start"; tail -40 "$STAGE/cpa.log"; exit 1
