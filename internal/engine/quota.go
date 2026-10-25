package engine

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"time"
)

// Window is one Codex rate-limit window (the 5-hour or the weekly window).
type Window struct {
	UsedPercent   float64   `json:"used_percent"`
	WindowMinutes int       `json:"window_minutes"`
	ResetAt       time.Time `json:"reset_at,omitempty"`
	ObservedAt    time.Time `json:"observed_at"`
}

// Remaining returns the remaining percentage at now. When the window's reset
// time has passed since the observation, the window is assumed to have reset
// (estimated=true) instead of trusting the stale used percentage.
func (w *Window) Remaining(now time.Time) (pct float64, estimated bool) {
	if w == nil {
		return 0, false
	}
	if !w.ResetAt.IsZero() && !now.Before(w.ResetAt) {
		return 100, true
	}
	return clampPct(100 - w.UsedPercent), false
}

// Quota is the latest known rate-limit snapshot for one account.
type Quota struct {
	Short        *Window   `json:"short,omitempty"` // 5-hour window
	Long         *Window   `json:"long,omitempty"`  // weekly window (monthly on some plans)
	LimitReached bool      `json:"limit_reached,omitempty"`
	Plan         string    `json:"plan,omitempty"`
	ObservedAt   time.Time `json:"observed_at"`
	Source       string    `json:"source"` // "response" or "poll"
}

// Known reports whether any window has been observed.
func (q *Quota) Known() bool { return q != nil && (q.Short != nil || q.Long != nil) }

// MinRemaining returns the tighter of the two windows' remaining percentage.
func (q *Quota) MinRemaining(now time.Time) (float64, bool) {
	if !q.Known() {
		return 0, false
	}
	minPct := 101.0
	for _, w := range []*Window{q.Short, q.Long} {
		if w == nil {
			continue
		}
		if r, _ := w.Remaining(now); r < minPct {
			minPct = r
		}
	}
	return minPct, true
}

// NextReset returns when the account recovers above belowPct: the latest
// future reset among the windows currently below that level.
func (q *Quota) NextReset(now time.Time, belowPct float64) time.Time {
	var next time.Time
	if q == nil {
		return next
	}
	for _, w := range []*Window{q.Short, q.Long} {
		if w == nil || w.ResetAt.IsZero() || !w.ResetAt.After(now) {
			continue
		}
		if r, _ := w.Remaining(now); r >= belowPct {
			continue
		}
		if next.IsZero() || w.ResetAt.After(next) {
			next = w.ResetAt
		}
	}
	return next
}

// classify maps a window to short (5h) or long (weekly/monthly) by duration.
func classify(windowMinutes int, primary bool) string {
	switch {
	case windowMinutes <= 0:
		if primary {
			return "short"
		}
		return "long"
	case windowMinutes <= 12*60:
		return "short"
	default:
		return "long"
	}
}

func (q *Quota) put(w *Window, primary bool) {
	if w == nil {
		return
	}
	if classify(w.WindowMinutes, primary) == "short" {
		q.Short = w
	} else {
		q.Long = w
	}
}

// ParseQuotaHeaders extracts Codex rate-limit windows from response headers.
// CLIProxyAPI records both HTTP x-codex-* headers and websocket
// codex.rate_limits events in this same header shape. Header names are
// matched case-insensitively.
func ParseQuotaHeaders(headers map[string][]string, observedAt time.Time) (Quota, bool) {
	if len(headers) == 0 {
		return Quota{}, false
	}
	lower := make(map[string]string, len(headers))
	for k, v := range headers {
		if len(v) == 0 {
			continue
		}
		lk := strings.ToLower(strings.TrimSpace(k))
		if strings.HasPrefix(lk, "x-codex-") {
			lower[lk] = strings.TrimSpace(v[len(v)-1])
		}
	}
	if len(lower) == 0 {
		return Quota{}, false
	}
	q := Quota{ObservedAt: observedAt, Source: "response"}
	for _, name := range []string{"primary", "secondary"} {
		used, okUsed := parseFloat(lower["x-codex-"+name+"-used-percent"])
		if !okUsed {
			continue
		}
		w := &Window{UsedPercent: clampPct(used), ObservedAt: observedAt}
		if mins, ok := parseFloat(lower["x-codex-"+name+"-window-minutes"]); ok {
			w.WindowMinutes = int(mins)
		}
		w.ResetAt = resetTime(lower["x-codex-"+name+"-reset-at"], lower["x-codex-"+name+"-reset-after-seconds"], observedAt)
		q.put(w, name == "primary")
	}
	if v, ok := lower["x-codex-limit-reached"]; ok && strings.EqualFold(v, "true") {
		q.LimitReached = true
	}
	if v, ok := lower["x-codex-allowed"]; ok && strings.EqualFold(v, "false") {
		q.LimitReached = true
	}
	q.Plan = lower["x-codex-plan-type"]
	if !q.Known() && !q.LimitReached {
		return Quota{}, false
	}
	return q, true
}

// ParseUsageResponse parses the JSON returned by Codex's usage endpoint
// (the same endpoint CLIProxyAPI's dashboard uses for quota checks).
func ParseUsageResponse(body []byte, observedAt time.Time) (Quota, bool) {
	var raw struct {
		PlanType  string `json:"plan_type"`
		RateLimit *struct {
			Allowed      *bool          `json:"allowed"`
			LimitReached *bool          `json:"limit_reached"`
			Primary      *usageWindowJS `json:"primary_window"`
			Secondary    *usageWindowJS `json:"secondary_window"`
		} `json:"rate_limit"`
	}
	if err := json.Unmarshal(body, &raw); err != nil || raw.RateLimit == nil {
		return Quota{}, false
	}
	q := Quota{ObservedAt: observedAt, Source: "poll", Plan: raw.PlanType}
	for i, uw := range []*usageWindowJS{raw.RateLimit.Primary, raw.RateLimit.Secondary} {
		if uw == nil || uw.UsedPercent == nil {
			continue
		}
		w := &Window{UsedPercent: clampPct(*uw.UsedPercent), ObservedAt: observedAt}
		if uw.LimitWindowSeconds != nil {
			w.WindowMinutes = int(*uw.LimitWindowSeconds / 60)
		}
		w.ResetAt = resetTime(numString(uw.ResetAt), numString(uw.ResetAfterSeconds), observedAt)
		q.put(w, i == 0)
	}
	if raw.RateLimit.LimitReached != nil && *raw.RateLimit.LimitReached {
		q.LimitReached = true
	}
	if raw.RateLimit.Allowed != nil && !*raw.RateLimit.Allowed {
		q.LimitReached = true
	}
	if !q.Known() {
		return Quota{}, false
	}
	return q, true
}

type usageWindowJS struct {
	UsedPercent        *float64 `json:"used_percent"`
	LimitWindowSeconds *float64 `json:"limit_window_seconds"`
	ResetAfterSeconds  *float64 `json:"reset_after_seconds"`
	ResetAt            *float64 `json:"reset_at"`
}

// QuotaError describes an upstream "usage limit reached" failure.
type QuotaError struct {
	ResetAt time.Time
}

// ParseQuotaError recognizes Codex usage-limit failures in an upstream error
// body and extracts when the limit resets. ok=false means the failure is not
// a quota exhaustion (e.g. a plain transient 429 or a 5xx).
func ParseQuotaError(status int, body string, now time.Time) (QuotaError, bool) {
	lb := strings.ToLower(body)
	isQuota := strings.Contains(lb, "usage_limit_reached") ||
		strings.Contains(lb, "usage limit") ||
		strings.Contains(lb, "rate_limit_exceeded") && strings.Contains(lb, "resets_") ||
		strings.Contains(lb, "insufficient_quota")
	if !isQuota {
		return QuotaError{}, false
	}
	if status != 0 && status != 429 && status != 403 && status != 402 {
		return QuotaError{}, false
	}
	qe := QuotaError{}
	var raw struct {
		Error struct {
			ResetsAt        *float64 `json:"resets_at"`
			ResetsInSeconds *float64 `json:"resets_in_seconds"`
		} `json:"error"`
		ResetsAt        *float64 `json:"resets_at"`
		ResetsInSeconds *float64 `json:"resets_in_seconds"`
	}
	if err := json.Unmarshal([]byte(body), &raw); err == nil {
		at := firstNonNil(raw.Error.ResetsAt, raw.ResetsAt)
		in := firstNonNil(raw.Error.ResetsInSeconds, raw.ResetsInSeconds)
		qe.ResetAt = resetTime(numString(at), numString(in), now)
	}
	return qe, true
}

func firstNonNil(vals ...*float64) *float64 {
	for _, v := range vals {
		if v != nil {
			return v
		}
	}
	return nil
}

func numString(v *float64) string {
	if v == nil {
		return ""
	}
	return strconv.FormatFloat(*v, 'f', -1, 64)
}

// resetTime prefers an absolute unix timestamp and falls back to a relative
// "seconds until reset" measured from the observation time.
func resetTime(resetAt, resetAfter string, observedAt time.Time) time.Time {
	if v, ok := parseFloat(resetAt); ok && v > 1e9 {
		sec, frac := math.Modf(v)
		if v > 1e12 { // milliseconds
			return time.UnixMilli(int64(v))
		}
		return time.Unix(int64(sec), int64(frac*1e9))
	}
	if v, ok := parseFloat(resetAfter); ok && v >= 0 {
		return observedAt.Add(time.Duration(v * float64(time.Second)))
	}
	return time.Time{}
}

func parseFloat(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false
	}
	return v, true
}

func clampPct(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}
