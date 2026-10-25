package engine

import (
	"fmt"
	"testing"
	"time"
)

// Regression tests for issues found in code review.

func exhaust(e *Engine, id string, resetIn time.Duration) {
	e.ObserveUsage(UsageObservation{AuthID: id, Provider: "codex", Failed: true, Status: 429,
		Headers: map[string][]string{"x-codex-primary-used-percent": {"100"}, "x-codex-primary-window-minutes": {"300"},
			"x-codex-primary-reset-after-seconds": {fmt.Sprint(int(resetIn.Seconds()))}},
		Body: fmt.Sprintf(`{"error":{"type":"usage_limit_reached","resets_in_seconds":%d}}`, int(resetIn.Seconds()))})
}

func TestNeverRejectWhenRouterBlocksEveryOfferedAccount(t *testing.T) {
	e, _ := newTestEngine(t, nil)
	healthyAll(e)
	for _, id := range []string{accA, accB, accC} {
		exhaust(e, id, time.Hour)
	}
	// The host still offers A (e.g. its cooldown ended early): try it rather than reject.
	r := pickFor(t, e, session("s"), accA)
	mustRoute(t, r, accA)
	// A 401 no longer creates a router-side block at all.
	e2, _ := newTestEngine(t, nil)
	healthyAll(e2)
	e2.ObserveUsage(UsageObservation{AuthID: accA, Provider: "codex", Failed: true, Status: 401, Body: "unauthorized"})
	mustRoute(t, pickFor(t, e2, session("s"), accA), accA)
	// Only an explicit Off role leads to rejection (codex-only route) or deferral (mixed route).
	e3, _ := newTestEngine(t, func(s *Settings) { s.DisabledAccounts = []string{accA} })
	if r := pickFor(t, e3, session("s"), accA); !r.Reject {
		t.Fatalf("all-Off codex route should reject: %+v", r)
	}
	r = e3.Pick(PickRequest{Candidates: []Candidate{{ID: accA, Provider: "codex"}, {ID: "claude.json", Provider: "claude"}}})
	if r.Handled {
		t.Fatalf("mixed route should defer: %+v", r)
	}
}

func TestUsageLimitIsAccountWideByDefault(t *testing.T) {
	e, _ := newTestEngine(t, nil)
	healthyAll(e)
	// A websocket failure record carries the error body but no quota headers.
	e.ObserveUsage(UsageObservation{AuthID: accA, Provider: "codex", Model: "gpt-5.5", Failed: true, Status: 429,
		Body: `{"error":{"type":"usage_limit_reached","resets_in_seconds":3600}}`})
	if !e.isBlocked(accA, e.now()) {
		t.Fatal("usage limit should block the account (CLIProxyAPI default cools the whole credential)")
	}
}

func TestAPIKeyAccountsSurviveRestartRoster(t *testing.T) {
	e, clk := newTestEngine(t, nil)
	api := "codex:apikey:0123456789ab"
	mustRoute(t, pickFor(t, e, session("s"), api), api)
	st, _ := e.Export()
	s, _ := e.Settings()
	e2 := New(s, nil, clk.now)
	e2.Restore(st)
	e2.ObserveAccounts(nil) // roster never lists API-key credentials
	clk.add(10 * time.Minute)
	e2.Tick()
	if v := e2.View(0); v.Active != api {
		t.Fatalf("API-key account treated as removed after restart: active=%q", v.Active)
	}
}

func TestModelSpecificLimitDoesNotBlockTheAccount(t *testing.T) {
	e, _ := newTestEngine(t, func(s *Settings) {
		s.AccountOrder = []string{accA, accB, accC}
		s.ModelLevelLimits = true
	})
	healthyAll(e)
	mustRoute(t, e.Pick(PickRequest{Candidates: cands(accA, accB, accC), Headers: session("s"), Model: "gpt-5.5"}), accA)
	// A per-model limit while A's main windows have plenty left.
	e.ObserveUsage(UsageObservation{AuthID: accA, Provider: "codex", Model: "gpt-5.3-codex-spark", Failed: true, Status: 429,
		Body: `{"error":{"type":"usage_limit_reached","resets_in_seconds":3600}}`})
	if e.isBlocked(accA, e.now()) {
		t.Fatal("account-wide block for a model-specific limit")
	}
	// Spark requests detour (binding kept); other models stay on A.
	r := e.Pick(PickRequest{Candidates: cands(accA, accB, accC), Headers: session("s"), Model: "gpt-5.3-codex-spark(high)"})
	if r.AuthID == accA {
		t.Fatalf("spark should avoid A: %+v", r)
	}
	mustRoute(t, e.Pick(PickRequest{Candidates: cands(accA, accB, accC), Headers: session("s"), Model: "gpt-5.5"}), accA)
	if e.View(0).Active != accA {
		t.Fatal("active account should not change for a per-model limit")
	}
}

func TestRemovedActiveAccountIsReplaced(t *testing.T) {
	e, clk := newTestEngine(t, func(s *Settings) { s.AccountOrder = []string{accA, accB, accC} })
	healthyAll(e)
	mustRoute(t, pickFor(t, e, session("s"), accA, accB, accC), accA)
	e.ObserveAccounts([]HostAccount{
		{ID: accB, AuthIndex: "2", Provider: "codex", Email: "bob@example.com"},
		{ID: accC, AuthIndex: "3", Provider: "codex", Email: "carol@example.com"},
	})
	clk.add(time.Minute)
	e.Tick()
	v := e.View(0)
	if v.Active != accB || v.Routing != accB {
		t.Fatalf("active/routing should move off the removed account: %s %s", v.Active, v.Routing)
	}
	for _, a := range v.Accounts {
		if a.ID == accA {
			t.Fatal("removed account still listed")
		}
	}
	mustRoute(t, pickFor(t, e, session("s"), accB, accC), accB)
}

func TestProvisionalPhaseDoesNotFlap(t *testing.T) {
	e, clk := newTestEngine(t, func(s *Settings) { s.Strategy = StrategyMostRemaining })
	quota(e, accA, 40, 40, 3*time.Hour, 5*24*time.Hour) // 60% left
	quota(e, accB, 39, 39, 3*time.Hour, 5*24*time.Hour) // 61% left; C stays unknown
	first := pickFor(t, e, session("s0")).AuthID
	switches := 0
	prev := first
	for i := 0; i < 40; i++ {
		clk.add(2 * time.Minute)
		a, b := 40.0, 39.0
		if i%2 == 1 {
			a, b = 39, 40
		}
		quota(e, accA, a, a, 3*time.Hour, 5*24*time.Hour)
		quota(e, accB, b, b, 3*time.Hour, 5*24*time.Hour)
		got := pickFor(t, e, session(fmt.Sprintf("n%d", i)), accA, accB).AuthID
		if got != prev {
			switches++
			prev = got
		}
	}
	if switches > 0 {
		t.Fatalf("active flapped %d times during provisional phase", switches)
	}
	if !e.st.ProvisionalUntil.IsZero() {
		t.Fatalf("provisional phase should end after its window")
	}
}

func TestHostCooldownGetsFullDetourGrace(t *testing.T) {
	e, clk := newTestEngine(t, func(s *Settings) { s.AccountOrder = []string{accA, accB, accC} })
	healthyAll(e)
	mustRoute(t, pickFor(t, e, session("s")), accA)
	coolA := func(unavailable bool) {
		e.ObserveAccounts([]HostAccount{
			{ID: accA, AuthIndex: "1", Provider: "codex", Unavailable: unavailable, NextRetry: t0.Add(time.Hour)},
			{ID: accB, AuthIndex: "2", Provider: "codex"}, {ID: accC, AuthIndex: "3", Provider: "codex"},
		})
	}
	coolA(true) // CLIProxyAPI cools A down for ~2 minutes
	mustRoute(t, pickFor(t, e, session("s"), accB, accC), accB)
	clk.add(70 * time.Second)
	coolA(true)
	r := pickFor(t, e, session("s"), accB, accC)
	mustRoute(t, r, accB)
	if e.st.Families["codex:s"].Account != accA {
		t.Fatal("binding lost during a short cooldown")
	}
	if e.View(0).Active != accA {
		t.Fatal("a short cooldown must not move the current account")
	}
	clk.add(60 * time.Second)
	coolA(false)
	mustRoute(t, pickFor(t, e, session("s")), accA)
}

func TestReserveIsUsedOnlyAfterNormalAccounts(t *testing.T) {
	e, _ := newTestEngine(t, func(s *Settings) {
		s.AccountOrder = []string{accA, accB, accC}
		s.ReserveAccounts = []string{accC}
	})
	quota(e, accA, 98, 40, time.Hour, 5*24*time.Hour) // A low
	quota(e, accC, 0, 0, 5*time.Hour, 7*24*time.Hour) // reserve C strong; B unknown
	mustRoute(t, pickFor(t, e, session("s")), accB)
}

func TestStrictManualDefersOnMixedRoutes(t *testing.T) {
	e, _ := newTestEngine(t, func(s *Settings) { s.ManualFallback = FallbackStrict })
	healthyAll(e)
	if err := e.Pin(accC); err != nil {
		t.Fatal(err)
	}
	r := e.Pick(PickRequest{Candidates: []Candidate{{ID: accA, Provider: "codex"}, {ID: "claude.json", Provider: "claude"}}})
	if r.Handled || r.Reject {
		t.Fatalf("strict pin on a mixed route should defer: %+v", r)
	}
}

func TestViewMatchesPickForExhaustedPin(t *testing.T) {
	e, _ := newTestEngine(t, func(s *Settings) { s.ManualFallback = FallbackStrict })
	healthyAll(e)
	exhaust(e, accB, time.Hour)
	if err := e.Pin(accB); err != nil {
		t.Fatal(err)
	}
	mustRoute(t, pickFor(t, e, session("s")), accB)
	if v := e.View(0); v.Routing != accB {
		t.Fatalf("view says %q while pick routes to the pin", v.Routing)
	}
}

func TestOutOfOrderUsageDoesNotOverwriteNewerQuota(t *testing.T) {
	e, clk := newTestEngine(t, nil)
	newer := clk.now()
	e.ObserveUsage(UsageObservation{AuthID: accA, Provider: "codex", At: newer, Headers: map[string][]string{
		"x-codex-primary-used-percent": {"80"}, "x-codex-primary-window-minutes": {"300"}}})
	e.ObserveUsage(UsageObservation{AuthID: accA, Provider: "codex", At: newer.Add(-5 * time.Minute), Headers: map[string][]string{
		"x-codex-primary-used-percent": {"20"}, "x-codex-primary-window-minutes": {"300"}}})
	if r, _ := e.remaining(accA, clk.now()); r != 20 {
		t.Fatalf("older record overwrote newer data: remaining=%v", r)
	}
}

func cands(ids ...string) []Candidate {
	out := make([]Candidate, 0, len(ids))
	for _, id := range ids {
		out = append(out, Candidate{ID: id, Provider: "codex"})
	}
	return out
}
