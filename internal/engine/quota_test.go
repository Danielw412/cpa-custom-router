package engine

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

func TestParseQuotaHeadersHTTPAndWebsocketShapes(t *testing.T) {
	// HTTP responses carry lowercase x-codex-* headers.
	q, ok := ParseQuotaHeaders(map[string][]string{
		"x-codex-primary-used-percent":        {"37.5"},
		"x-codex-primary-window-minutes":      {"300"},
		"x-codex-primary-reset-after-seconds": {"3600"},
		"x-codex-secondary-used-percent":      {"80"},
		"x-codex-secondary-window-minutes":    {"10080"},
		"x-codex-secondary-reset-at":          {"1761400000"},
		"x-codex-plan-type":                   {"plus"},
		"content-type":                        {"text/event-stream"},
	}, t0)
	if !ok || q.Short == nil || q.Long == nil {
		t.Fatalf("expected both windows, got %+v ok=%v", q, ok)
	}
	if r, _ := q.Short.Remaining(t0); r != 62.5 {
		t.Fatalf("short remaining = %v", r)
	}
	if !q.Short.ResetAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("short reset = %v", q.Short.ResetAt)
	}
	if q.Long.ResetAt.Unix() != 1761400000 || q.Plan != "plus" {
		t.Fatalf("long window/plan wrong: %+v", q)
	}

	// CLIProxyAPI converts websocket codex.rate_limits events into canonical
	// X-Codex-* headers, including Limit-Reached.
	q, ok = ParseQuotaHeaders(map[string][]string{
		"X-Codex-Primary-Used-Percent":   {"100"},
		"X-Codex-Primary-Window-Minutes": {"300"},
		"X-Codex-Primary-Reset-At":       {"1761300000"},
		"X-Codex-Limit-Reached":          {"true"},
	}, t0)
	if !ok || !q.LimitReached || q.Short == nil || q.Long != nil {
		t.Fatalf("websocket shape parsed wrong: %+v", q)
	}

	if _, ok := ParseQuotaHeaders(map[string][]string{"content-type": {"x"}}, t0); ok {
		t.Fatal("headers without quota data must not parse")
	}
}

func TestWindowClassificationByDuration(t *testing.T) {
	// A plan whose primary window is weekly must land in Long, not Short.
	q, ok := ParseQuotaHeaders(map[string][]string{
		"x-codex-primary-used-percent":   {"10"},
		"x-codex-primary-window-minutes": {"10080"},
	}, t0)
	if !ok || q.Long == nil || q.Short != nil {
		t.Fatalf("weekly primary misclassified: %+v", q)
	}
}

func TestRemainingAssumesResetAfterResetTime(t *testing.T) {
	w := &Window{UsedPercent: 99, ResetAt: t0.Add(time.Minute), ObservedAt: t0}
	if r, est := w.Remaining(t0); r != 1 || est {
		t.Fatalf("before reset: %v %v", r, est)
	}
	if r, est := w.Remaining(t0.Add(2 * time.Minute)); r != 100 || !est {
		t.Fatalf("after reset should be estimated full: %v %v", r, est)
	}
}

func TestParseUsageResponse(t *testing.T) {
	body := []byte(`{"plan_type":"plus","rate_limit":{"allowed":true,"limit_reached":false,
	  "primary_window":{"used_percent":12,"limit_window_seconds":18000,"reset_after_seconds":600,"reset_at":1761310000},
	  "secondary_window":{"used_percent":55,"limit_window_seconds":604800,"reset_after_seconds":86400,"reset_at":1761390000}},
	  "credits":{"has_credits":false}}`)
	q, ok := ParseUsageResponse(body, t0)
	if !ok || q.Short.UsedPercent != 12 || q.Long.UsedPercent != 55 || q.Short.WindowMinutes != 300 || q.Source != "poll" {
		t.Fatalf("usage parse wrong: %+v ok=%v", q, ok)
	}
	if _, ok := ParseUsageResponse([]byte(`{"detail":"Unauthorized"}`), t0); ok {
		t.Fatal("error body must not parse")
	}
}

func TestParseQuotaError(t *testing.T) {
	body := `{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","resets_in_seconds":1800}}`
	qe, ok := ParseQuotaError(429, body, t0)
	if !ok || !qe.ResetAt.Equal(t0.Add(30*time.Minute)) {
		t.Fatalf("quota error: %+v ok=%v", qe, ok)
	}
	if _, ok := ParseQuotaError(429, `{"error":{"type":"server_overloaded"}}`, t0); ok {
		t.Fatal("transient 429 must not count as quota exhaustion")
	}
	if _, ok := ParseQuotaError(500, body, t0); ok {
		t.Fatal("5xx must not count as quota exhaustion")
	}
}
