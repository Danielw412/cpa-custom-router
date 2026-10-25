package plugin

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/Danielw412/cpa-custom-router/internal/hostapi"
	"github.com/Danielw412/cpa-custom-router/internal/service"
)

const (
	idA         = "codex-aaaa1111-alice@example.com-plus.json"
	idB         = "codex-bbbb2222-bob@example.com-plus.json"
	secretToken = "SECRET-ACCESS-TOKEN-should-never-leak"
)

// fakeHost emulates CLIProxyAPI host callbacks.
type fakeHost struct {
	mu        sync.Mutex
	calls     []string
	usageAuth []string
	logs      []string
	// quota: auth index -> {5-hour used %, seconds until reset}
	quota    map[string][2]float64
	executed []map[string]any
}

var authIndexOf = map[string]string{idA: "a1", idB: "b2"}

func ok(v any) []byte {
	raw, _ := json.Marshal(v)
	out, _ := json.Marshal(map[string]any{"ok": true, "result": json.RawMessage(raw)})
	return out
}

func (f *fakeHost) call(method string, req []byte) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, method)
	f.mu.Unlock()
	switch method {
	case "host.auth.list":
		return ok(map[string]any{"files": []map[string]any{
			{"id": idA, "auth_index": "a1", "provider": "codex", "email": "alice@example.com", "account_type": "plus"},
			{"id": idB, "auth_index": "b2", "provider": "codex", "email": "bob@example.com"},
			{"id": "claude-x.json", "auth_index": "c3", "provider": "claude"},
		}}), nil
	case "host.auth.get":
		var r struct {
			AuthIndex string `json:"auth_index"`
		}
		_ = json.Unmarshal(req, &r)
		return ok(map[string]any{"auth_index": r.AuthIndex, "json": map[string]any{
			"type": "codex", "access_token": secretToken, "account_id": "acct-" + r.AuthIndex, "refresh_token": "R",
		}}), nil
	case "host.http.operation_open":
		return ok(map[string]any{"operation_id": "op1"}), nil
	case "host.http.do":
		var r struct {
			URL     string              `json:"url"`
			Headers map[string][]string `json:"headers"`
		}
		_ = json.Unmarshal(req, &r)
		if r.URL != "https://chatgpt.com/backend-api/wham/usage" || r.Headers["Authorization"][0] != "Bearer "+secretToken {
			return ok(map[string]any{"StatusCode": 400}), nil
		}
		f.mu.Lock()
		acct := r.Headers["Chatgpt-Account-Id"][0]
		f.usageAuth = append(f.usageAuth, acct)
		q, found := f.quota[strings.TrimPrefix(acct, "acct-")]
		f.mu.Unlock()
		if !found {
			q = [2]float64{30, 3000}
		}
		body := fmt.Sprintf(`{"plan_type":"plus","rate_limit":{"primary_window":{"used_percent":%g,"limit_window_seconds":18000,"reset_after_seconds":%g},"secondary_window":{"used_percent":40,"limit_window_seconds":604800,"reset_after_seconds":300000}}}`, q[0], q[1])
		return ok(map[string]any{"StatusCode": 200, "Headers": map[string][]string{}, "Body": []byte(body)}), nil
	case "host.model.execute":
		var r map[string]any
		_ = json.Unmarshal(req, &r)
		f.mu.Lock()
		f.executed = append(f.executed, r)
		if f.quota == nil {
			f.quota = map[string][2]float64{}
		}
		f.quota[authIndexOf[fmt.Sprint(r["auth_id"])]] = [2]float64{1, 17990} // the window has started
		f.mu.Unlock()
		body := `{"output":[{"type":"message","content":[{"type":"output_text","text":"OK"}]}],"usage":{"input_tokens":812,"input_tokens_details":{"cached_tokens":0},"output_tokens":6,"output_tokens_details":{"reasoning_tokens":0}}}`
		return ok(map[string]any{"status_code": 200, "headers": map[string][]string{}, "body": []byte(body)}), nil
	case "host.http.cancel":
		return ok(map[string]any{}), nil
	case "host.log":
		f.mu.Lock()
		f.logs = append(f.logs, string(req))
		f.mu.Unlock()
		return ok(map[string]any{}), nil
	}
	return []byte(`{"ok":false,"error":{"code":"unknown","message":"unknown"}}`), nil
}

func call(t *testing.T, p *Plugin, method string, req any) json.RawMessage {
	t.Helper()
	raw, _ := json.Marshal(req)
	out, okCall := p.Handle(method, raw)
	var env struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  *struct{ Message string }
	}
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatalf("%s: bad envelope %s", method, out)
	}
	if !okCall || !env.OK {
		t.Fatalf("%s failed: %s", method, out)
	}
	return env.Result
}

func setup(t *testing.T) (*Plugin, *fakeHost, string) {
	t.Helper()
	service.InitialLoopDelay = 10 * time.Millisecond
	service.TickEvery = 50 * time.Millisecond
	service.StarterSettleWait = 20 * time.Millisecond
	state := filepath.Join(t.TempDir(), "state.json")
	fh := &fakeHost{}
	p := New(hostapi.New(fh.call))
	cfg := "enabled: true\npriority: 1\nswitch_threshold_percent: 5\naccount_order: [" + idB + ", " + idA + "]\nstate_file: " + state + "\n"
	reg := call(t, p, "plugin.register", map[string]any{"config_yaml": []byte(cfg), "schema_version": 6})
	var r struct {
		SchemaVersion uint32             `json:"schema_version"`
		Metadata      pluginapi.Metadata `json:"metadata"`
		Capabilities  map[string]bool    `json:"capabilities"`
	}
	if err := json.Unmarshal(reg, &r); err != nil {
		t.Fatal(err)
	}
	m := r.Metadata
	if m.Name == "" || m.Version == "" || m.Author == "" || m.GitHubRepository == "" || r.SchemaVersion != 6 {
		t.Fatalf("host would reject registration metadata: %+v", r)
	}
	for _, c := range []string{"scheduler", "scheduler_across_priorities", "usage_plugin", "management_api"} {
		if !r.Capabilities[c] {
			t.Fatalf("missing capability %s", c)
		}
	}
	t.Cleanup(p.Shutdown)
	waitFor(t, "account roster", func() bool {
		_, _, body := mgmt(t, p, "GET", "/v0/management/custom-quota-router/state", nil)
		var v struct{ Accounts []any }
		_ = json.Unmarshal(body, &v)
		return len(v.Accounts) == 2
	})
	return p, fh, state
}

func mgmt(t *testing.T, p *Plugin, method, path string, body any) (int, http.Header, []byte) {
	t.Helper()
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	res := call(t, p, "management.handle", map[string]any{"Method": method, "Path": path, "Body": raw, "Headers": map[string][]string{"Authorization": {"Bearer k"}}})
	var resp struct {
		StatusCode int
		Headers    http.Header
		Body       []byte
	}
	if err := json.Unmarshal(res, &resp); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Headers, resp.Body
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestEndToEndThroughRPC(t *testing.T) {
	p, fh, statePath := setup(t)

	// Background loop: roster from host.auth.list, then active quota polls.
	waitFor(t, "quota polls for both codex accounts", func() bool {
		fh.mu.Lock()
		defer fh.mu.Unlock()
		return len(fh.usageAuth) >= 2
	})

	// Scheduler pick, exactly as the host serializes it.
	pick := pluginapi.SchedulerPickRequest{
		Provider: "codex", Model: "gpt-6.1-sol",
		Options: pluginapi.SchedulerOptions{Headers: map[string][]string{"Session_id": {"sess-1"}}},
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: idA, Provider: "codex", Status: "active"}, {ID: idB, Provider: "codex", Status: "active"},
		},
	}
	var resp pluginapi.SchedulerPickResponse
	if err := json.Unmarshal(call(t, p, "scheduler.pick", pick), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Handled || resp.AuthID != idB {
		t.Fatalf("pick = %+v, want B (first in account_order)", resp)
	}

	// Usage record with quota headers showing B nearly exhausted.
	call(t, p, "usage.handle", pluginapi.UsageRecord{
		AuthID: idB, Provider: "codex", RequestedAt: time.Now(),
		ResponseHeaders: http.Header{"X-Codex-Primary-Used-Percent": {"98"}, "X-Codex-Primary-Window-Minutes": {"300"}, "X-Codex-Primary-Reset-After-Seconds": {"600"}},
	})
	if err := json.Unmarshal(call(t, p, "scheduler.pick", pick), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.AuthID != idA {
		t.Fatalf("after B dropped below threshold the session should move to A, got %+v", resp)
	}

	// Management API: state, manual pin, resume.
	status, hdr, body := mgmt(t, p, "GET", "/v0/management/custom-quota-router/state", nil)
	if status != 200 || !strings.HasPrefix(hdr.Get("Content-Type"), "application/json") || hdr.Get("Cache-Control") != "no-store" {
		t.Fatalf("state: %d %v", status, hdr)
	}
	if strings.Contains(string(body), secretToken) || strings.Contains(string(body), "acct-a1") {
		t.Fatal("state response leaks credential material")
	}
	var view struct {
		Mode     string
		Routing  string
		Accounts []struct {
			ID, State string
		}
		History []struct{ Kind, Reason string }
	}
	_ = json.Unmarshal(body, &view)
	if view.Routing != idA || len(view.Accounts) != 2 {
		t.Fatalf("view: %+v", view)
	}
	status, _, _ = mgmt(t, p, "POST", "/v0/management/custom-quota-router/mode", map[string]string{"mode": "manual", "account": idB})
	if status != 200 {
		t.Fatalf("pin status %d", status)
	}
	if err := json.Unmarshal(call(t, p, "scheduler.pick", pick), &resp); err != nil || resp.AuthID != idB {
		t.Fatalf("manual pin not honored: %+v", resp)
	}
	if status, _, _ := mgmt(t, p, "POST", "/v0/management/custom-quota-router/mode", map[string]string{"mode": "manual", "account": "missing.json"}); status != 404 {
		t.Fatalf("unknown account should 404, got %d", status)
	}
	if status, _, _ := mgmt(t, p, "POST", "/v0/management/custom-quota-router/mode", map[string]any{"mode": "auto", "extra": 1}); status != 400 {
		t.Fatalf("unknown fields should be rejected, got %d", status)
	}
	if status, _, _ := mgmt(t, p, "POST", "/v0/management/custom-quota-router/mode", map[string]string{"mode": "auto"}); status != 200 {
		t.Fatalf("resume auto failed")
	}
	if status, _, _ := mgmt(t, p, "POST", "/v0/management/custom-quota-router/select", map[string]string{"account": idA}); status != 200 {
		t.Fatalf("select failed")
	}
	if status, _, _ := mgmt(t, p, "POST", "/v0/management/custom-quota-router/refresh", map[string]string{}); status != 200 {
		t.Fatalf("refresh failed")
	}
	if status, _, _ := mgmt(t, p, "DELETE", "/v0/management/custom-quota-router/state", nil); status != 404 {
		t.Fatalf("unexpected method should 404")
	}

	// Static panel with a strict CSP.
	status, hdr, body = mgmt(t, p, "GET", "/v0/resource/plugins/custom-quota-router/ui", nil)
	csp := hdr.Get("Content-Security-Policy")
	if status != 200 || !strings.Contains(csp, "script-src 'self'") || !strings.Contains(csp, "frame-ancestors 'self'") || !strings.Contains(string(body), "ui.js?v=") {
		t.Fatalf("ui: %d %q", status, csp)
	}
	if status, hdr, _ := mgmt(t, p, "GET", "/v0/resource/plugins/custom-quota-router/ui.js", nil); status != 200 || !strings.HasPrefix(hdr.Get("Content-Type"), "text/javascript") {
		t.Fatalf("ui.js: %d", status)
	}

	// Routing events are written to the CLIProxyAPI log.
	waitFor(t, "host log entries", func() bool {
		fh.mu.Lock()
		defer fh.mu.Unlock()
		return len(fh.logs) > 0
	})

	// Quiesce (hot upgrade): state is flushed and the file never holds secrets.
	call(t, p, "plugin.quiesce", map[string]any{})
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("state not flushed: %v", err)
	}
	if strings.Contains(string(raw), secretToken) {
		t.Fatal("state file contains the access token")
	}
	if info, _ := os.Stat(statePath); info.Mode().Perm() != 0o600 {
		t.Fatalf("state file perms = %v", info.Mode().Perm())
	}
}

func TestUnknownMethodAndBadInputAreSafe(t *testing.T) {
	p, _, _ := setup(t)
	if _, okCall := p.Handle("nope", nil); okCall {
		t.Fatal("unknown method should fail")
	}
	// Malformed pick payloads defer to the built-in selector instead of failing requests.
	out, okCall := p.Handle("scheduler.pick", []byte("{not json"))
	if !okCall || !strings.Contains(string(out), `"Handled":false`) {
		t.Fatalf("bad pick should be unhandled: %s", out)
	}
	if _, okCall := p.Handle("usage.handle", []byte("garbage")); !okCall {
		t.Fatal("bad usage record should be ignored")
	}
}

func TestReconfigureKeepsRuntimeState(t *testing.T) {
	p, _, state := setup(t)
	call(t, p, "management.handle", map[string]any{"Method": "POST", "Path": "/v0/management/custom-quota-router/mode",
		"Body": []byte(`{"mode":"manual","account":"` + idA + `"}`)})
	call(t, p, "plugin.reconfigure", map[string]any{"config_yaml": []byte("switch_threshold_percent: 12\nstate_file: " + state + "\n")})
	_, _, body := mgmt(t, p, "GET", "/v0/management/custom-quota-router/state", nil)
	var v struct {
		Mode     string
		Pinned   string
		Settings struct {
			SwitchThresholdPercent float64 `json:"switch_threshold_percent"`
		}
	}
	_ = json.Unmarshal(body, &v)
	if v.Mode != "manual" || v.Pinned != idA || v.Settings.SwitchThresholdPercent != 12 {
		t.Fatalf("reconfigure lost state or settings: %+v", v)
	}
}

func TestWindowStartsThroughRPC(t *testing.T) {
	p, fh, _ := setup(t)
	fh.mu.Lock()
	fh.quota = map[string][2]float64{"a1": {0, 18000}, "b2": {25, 3600}} // A idle, B running
	fh.mu.Unlock()

	if status, _, body := mgmt(t, p, "POST", "/v0/management/custom-quota-router/windows/start", map[string]int{}); status != 200 {
		t.Fatalf("start all now: %d %s", status, body)
	}
	var v struct {
		Accounts []struct {
			ID, Window string
		}
		Starter struct {
			Log []struct {
				Account, Status string
			}
			LastCost *struct {
				InputTokens     int64    `json:"input_tokens"`
				OutputTokens    int64    `json:"output_tokens"`
				ShortUsedBefore *float64 `json:"short_used_before"`
				ShortUsedAfter  *float64 `json:"short_used_after"`
			} `json:"last_cost"`
			Plans []struct {
				ID, Account string
				Daily       bool
			}
		}
	}
	readState := func() {
		_, _, body := mgmt(t, p, "GET", "/v0/management/custom-quota-router/state", nil)
		v.Starter.Log, v.Starter.Plans, v.Starter.LastCost = nil, nil, nil
		if err := json.Unmarshal(body, &v); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, "both start results", func() bool { readState(); return len(v.Starter.Log) >= 2 })

	fh.mu.Lock()
	executed := append([]map[string]any(nil), fh.executed...)
	fh.mu.Unlock()
	if len(executed) != 1 {
		t.Fatalf("exactly one request (idle account only) expected, got %d", len(executed))
	}
	x := executed[0]
	if x["auth_id"] != idA || x["model"] != "gpt-6-luna" || x["entry_protocol"] != "openai-response" || x["stream"] != false {
		t.Fatalf("wrong execute request: %v", x)
	}
	raw, _ := base64.StdEncoding.DecodeString(fmt.Sprint(x["body"]))
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if body["store"] != false || body["reasoning"].(map[string]any)["effort"] != "low" {
		t.Fatalf("request must be luna-low with store=false: %s", raw)
	}
	if k, _ := body["prompt_cache_key"].(string); len(k) != 36 {
		t.Fatalf("each start needs its own random cache/session key: %s", raw)
	}
	for _, k := range []string{"previous_response_id", "conversation", "metadata"} {
		if _, has := body[k]; has {
			t.Fatalf("request must not reference a thread/session (%s): %s", k, raw)
		}
	}
	hdr, _ := x["headers"].(map[string]any)
	sid, _ := hdr["Session_id"].([]any)
	if len(hdr) != 1 || len(sid) != 1 || len(fmt.Sprint(sid[0])) != 36 {
		t.Fatalf("expected only a random one-off session ID header: %v", x["headers"])
	}
	statuses := map[string]string{}
	for _, l := range v.Starter.Log {
		statuses[l.Account] = l.Status
	}
	if statuses[idA] != "started" || statuses[idB] != "already_running" {
		t.Fatalf("start results: %v", statuses)
	}
	c := v.Starter.LastCost
	if c == nil || c.InputTokens != 812 || c.OutputTokens != 6 || c.ShortUsedBefore == nil || *c.ShortUsedBefore != 0 || c.ShortUsedAfter == nil || *c.ShortUsedAfter != 1 {
		t.Fatalf("cost measurement wrong: %+v", c)
	}
	for _, a := range v.Accounts {
		if a.Window != "running" {
			t.Fatalf("%s window should now be running: %s", a.ID, a.Window)
		}
	}

	// Manual daily start, then turn everything off.
	at := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
	if status, _, b := mgmt(t, p, "POST", "/v0/management/custom-quota-router/windows/schedule", map[string]any{"account": idB, "at": at, "daily": true}); status != 200 {
		t.Fatalf("schedule: %d %s", status, b)
	}
	readState()
	if len(v.Starter.Plans) != 1 || !v.Starter.Plans[0].Daily || v.Starter.Plans[0].Account != idB {
		t.Fatalf("plans after schedule: %+v", v.Starter.Plans)
	}
	if status, _, _ := mgmt(t, p, "POST", "/v0/management/custom-quota-router/windows/start", map[string]int{"delay_minutes": 30, "stagger_minutes": 15}); status != 200 {
		t.Fatal("delayed staggered start rejected")
	}
	if status, _, _ := mgmt(t, p, "POST", "/v0/management/custom-quota-router/windows/start", map[string]int{"delay_minutes": -1}); status != 400 {
		t.Fatal("negative delay accepted")
	}
	if status, _, _ := mgmt(t, p, "POST", "/v0/management/custom-quota-router/windows/cancel", map[string]string{}); status != 200 {
		t.Fatal("turn off failed")
	}
	readState()
	if len(v.Starter.Plans) != 0 {
		t.Fatalf("turn everything off left plans: %+v", v.Starter.Plans)
	}
}

func TestPauseAndSlowDownThroughRPC(t *testing.T) {
	p, _, statePath := setup(t)
	const api = "/v0/management/custom-quota-router"
	pick := pluginapi.SchedulerPickRequest{
		Provider: "codex", Model: "gpt-6.1-sol",
		Options:    pluginapi.SchedulerOptions{Headers: map[string][]string{"Session_id": {"sess-p"}}},
		Candidates: []pluginapi.SchedulerAuthCandidate{{ID: idA, Provider: "codex"}, {ID: idB, Provider: "codex"}},
	}
	pickAsync := func() <-chan pluginapi.SchedulerPickResponse {
		out := make(chan pluginapi.SchedulerPickResponse, 1)
		go func() {
			raw, _ := json.Marshal(pick)
			res, _ := p.Handle("scheduler.pick", raw)
			var env struct {
				Result pluginapi.SchedulerPickResponse
			}
			_ = json.Unmarshal(res, &env)
			out <- env.Result
		}()
		return out
	}
	type view struct {
		Pause *struct {
			AutoResumeMinutes int `json:"auto_resume_minutes"`
		}
		SlowDownSeconds int `json:"slow_down_seconds"`
		Waiting         int
	}
	state := func() view {
		_, _, body := mgmt(t, p, "GET", api+"/state", nil)
		var v view
		_ = json.Unmarshal(body, &v)
		return v
	}

	for _, bad := range []struct {
		path string
		body any
	}{
		{"/pause", map[string]int{"minutes": 2000}},
		{"/pause", map[string]int{"auto_resume_minutes": 25}},
		{"/pause", map[string]any{"minutes": 5, "extra": true}},
		{"/slowdown", map[string]int{"seconds": 241}},
		{"/slowdown", map[string]int{"seconds": -1}},
	} {
		if status, _, _ := mgmt(t, p, "POST", api+bad.path, bad.body); status != 400 {
			t.Errorf("POST %s %v = %d, want 400", bad.path, bad.body, status)
		}
	}

	if status, _, _ := mgmt(t, p, "POST", api+"/pause", map[string]int{"minutes": 0, "auto_resume_minutes": 20}); status != 200 {
		t.Fatalf("pause status %d", status)
	}
	if v := state(); v.Pause == nil || v.Pause.AutoResumeMinutes != 20 {
		t.Fatalf("state after pause: %+v", v)
	}
	if raw, err := os.ReadFile(statePath); err != nil || !strings.Contains(string(raw), `"pause"`) {
		t.Fatalf("pause not saved right away: %v", err)
	}

	held := pickAsync()
	waitFor(t, "a held request", func() bool { return state().Waiting == 1 })
	select {
	case r := <-held:
		t.Fatalf("pick returned while paused: %+v", r)
	default:
	}
	if status, _, _ := mgmt(t, p, "POST", api+"/resume", nil); status != 200 {
		t.Fatalf("resume status %d", status)
	}
	select {
	case r := <-held:
		if !r.Handled || r.Reject || r.AuthID == "" {
			t.Fatalf("resumed pick = %+v", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pick still held after resume")
	}

	if status, _, _ := mgmt(t, p, "POST", api+"/slowdown", map[string]int{"seconds": 1}); status != 200 {
		t.Fatalf("slowdown status %d", status)
	}
	start := time.Now()
	if r := <-pickAsync(); !r.Handled || r.Reject || time.Since(start) < 900*time.Millisecond {
		t.Fatalf("slowed pick = %+v after %v", r, time.Since(start))
	}
	mgmt(t, p, "POST", api+"/slowdown", map[string]int{"seconds": 0})

	// A hot upgrade ends held requests with a retryable rejection, so the
	// host can retire this instance and the retries reach the successor.
	mgmt(t, p, "POST", api+"/pause", map[string]int{"minutes": 30})
	held = pickAsync()
	waitFor(t, "a held request", func() bool { return state().Waiting == 1 })
	call(t, p, "plugin.quiesce", map[string]any{})
	select {
	case r := <-held:
		if !r.Handled || !r.Reject || r.RejectCode != "quota_router_paused" {
			t.Fatalf("held pick on quiesce = %+v, want a paused rejection", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("quiesce left a request held")
	}
}
