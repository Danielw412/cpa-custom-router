package engine

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	accA = "codex-aaaa1111-alice@example.com-plus.json"
	accB = "codex-bbbb2222-bob@example.com-plus.json"
	accC = "codex-cccc3333-carol@example.com-plus.json"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newTestEngine(t *testing.T, mutate func(*Settings)) (*Engine, *fakeClock) {
	t.Helper()
	s := DefaultSettings()
	s.StateFile = filepath.Join(t.TempDir(), "state.json")
	if mutate != nil {
		mutate(&s)
	}
	s, warn := s.Normalize(nil)
	if len(warn) > 0 {
		t.Fatalf("unexpected settings warnings: %v", warn)
	}
	clk := &fakeClock{t: t0}
	e := New(s, nil, clk.now)
	e.ObserveAccounts([]HostAccount{
		{ID: accA, AuthIndex: "1", Provider: "codex", Email: "alice@example.com"},
		{ID: accB, AuthIndex: "2", Provider: "codex", Email: "bob@example.com"},
		{ID: accC, AuthIndex: "3", Provider: "codex", Email: "carol@example.com"},
	})
	return e, clk
}

// quota reports usage for an account as CLIProxyAPI would after a response.
// shortUsed/longUsed are used percentages; resets are relative to now.
func quota(e *Engine, id string, shortUsed, longUsed float64, shortReset, longReset time.Duration) {
	e.ObserveUsage(UsageObservation{AuthID: id, Provider: "codex", Headers: map[string][]string{
		"x-codex-primary-used-percent":          {strconv.FormatFloat(shortUsed, 'f', -1, 64)},
		"x-codex-primary-window-minutes":        {"300"},
		"x-codex-primary-reset-after-seconds":   {strconv.Itoa(int(shortReset.Seconds()))},
		"x-codex-secondary-used-percent":        {strconv.FormatFloat(longUsed, 'f', -1, 64)},
		"x-codex-secondary-window-minutes":      {"10080"},
		"x-codex-secondary-reset-after-seconds": {strconv.Itoa(int(longReset.Seconds()))},
	}})
}

// polled records a usage-endpoint reading without any traffic, so an account
// at 0% keeps an idle (not started) 5-hour window.
func polled(e *Engine, id string, shortUsed, longUsed float64, shortReset, longReset time.Duration) {
	now := e.now()
	e.ApplyPolledQuota(id, Quota{
		Short:      &Window{UsedPercent: shortUsed, WindowMinutes: 300, ResetAt: now.Add(shortReset), ObservedAt: now},
		Long:       &Window{UsedPercent: longUsed, WindowMinutes: 10080, ResetAt: now.Add(longReset), ObservedAt: now},
		ObservedAt: now, Source: "poll"}, nil)
}

func healthyAll(e *Engine) {
	for _, id := range []string{accA, accB, accC} {
		quota(e, id, 20, 30, 3*time.Hour, 5*24*time.Hour)
	}
}

func session(id string) map[string][]string { return map[string][]string{"Session_id": {id}} }
func subagent(parent, child string) map[string][]string {
	return map[string][]string{"Session_id": {parent}, "Thread_id": {child}, "X-Openai-Subagent": {"collab_spawn"}}
}

func pickFor(t *testing.T, e *Engine, headers map[string][]string, avail ...string) PickResult {
	t.Helper()
	if len(avail) == 0 {
		avail = []string{accA, accB, accC}
	}
	cands := make([]Candidate, 0, len(avail))
	for _, id := range avail {
		cands = append(cands, Candidate{ID: id, Provider: "codex", Status: "active"})
	}
	return e.Pick(PickRequest{Candidates: cands, Headers: headers})
}

func mustRoute(t *testing.T, r PickResult, want string) {
	t.Helper()
	if !r.Handled || r.Reject || r.AuthID != want {
		t.Fatalf("routed to %q (handled=%v reject=%v reason=%q), want %q", r.AuthID, r.Handled, r.Reject, r.Reason, want)
	}
}

func historyHas(e *Engine, kind, substr string) bool {
	for _, h := range e.View(0).History {
		if h.Kind == kind && strings.Contains(h.Reason, substr) {
			return true
		}
	}
	return false
}

func TestInitialSelectionFollowsOrderAndStaysSticky(t *testing.T) {
	e, clk := newTestEngine(t, func(s *Settings) { s.AccountOrder = []string{accB, accA, accC} })
	healthyAll(e)
	mustRoute(t, pickFor(t, e, session("s1")), accB)
	// Even when another account has more quota, the current one is kept.
	quota(e, accC, 0, 0, 5*time.Hour, 7*24*time.Hour)
	for i := 0; i < 5; i++ {
		clk.add(10 * time.Minute)
		mustRoute(t, pickFor(t, e, session(fmt.Sprintf("n%d", i))), accB)
	}
}

func TestSwitchesBelowThresholdAndMigratesWarmSessions(t *testing.T) {
	e, clk := newTestEngine(t, func(s *Settings) { s.AccountOrder = []string{accA, accB, accC} })
	healthyAll(e)
	mustRoute(t, pickFor(t, e, session("s1")), accA)
	clk.add(time.Minute)
	quota(e, accA, 96, 40, 2*time.Hour, 4*24*time.Hour) // 4% left < 5%
	mustRoute(t, pickFor(t, e, session("new")), accB)
	if e.View(0).Active != accB {
		t.Fatalf("active should be B")
	}
	// The warm session on A moves at its next request boundary.
	mustRoute(t, pickFor(t, e, session("s1")), accB)
	if !historyHas(e, "switch", "below the 5% threshold") || !historyHas(e, "migrate", "below 5% remaining") {
		t.Fatalf("missing history: %+v", e.View(0).History)
	}
}

func TestSubagentsFollowTheirParentsAccount(t *testing.T) {
	e, _ := newTestEngine(t, func(s *Settings) { s.AccountOrder = []string{accA, accB, accC} })
	healthyAll(e)
	mustRoute(t, pickFor(t, e, session("parent")), accA)
	if err := e.Select(accB); err != nil {
		t.Fatal(err)
	}
	// New top-level work goes to B, but the parent's subagents stay with A.
	mustRoute(t, pickFor(t, e, session("other")), accB)
	mustRoute(t, pickFor(t, e, subagent("parent", "child-1")), accA)
	mustRoute(t, pickFor(t, e, subagent("parent", "child-2")), accA)
	mustRoute(t, pickFor(t, e, session("parent")), accA)
}

func TestWarmSessionsStayOnSwitchBackColdOnesRejoin(t *testing.T) {
	e, clk := newTestEngine(t, func(s *Settings) { s.AccountOrder = []string{accA, accB, accC} })
	healthyAll(e)
	quota(e, accA, 97, 40, 40*time.Minute, 4*24*time.Hour) // A low, 5h window resets in 40 min
	mustRoute(t, pickFor(t, e, session("busy")), accB)
	mustRoute(t, pickFor(t, e, session("idle")), accB)
	for i := 0; i < 6; i++ { // one hour, "busy" keeps working every 10 min
		clk.add(10 * time.Minute)
		mustRoute(t, pickFor(t, e, session("busy")), accB)
	}
	// A's 5-hour window has reset and dwell time has passed: switch back.
	mustRoute(t, pickFor(t, e, session("fresh")), accA)
	if !historyHas(e, "switch", "Switched back to alice@example.com") {
		t.Fatalf("expected switch-back entry: %+v", e.View(0).History)
	}
	// The busy session keeps B (warm cache); the idle one rejoins A.
	mustRoute(t, pickFor(t, e, session("busy")), accB)
	mustRoute(t, pickFor(t, e, session("idle")), accA)
	if !historyHas(e, "rejoin", "cache expired") {
		t.Fatalf("expected rejoin entry")
	}
}

func TestNoSwitchBackBeforeDwellOrWithoutBenefit(t *testing.T) {
	e, clk := newTestEngine(t, func(s *Settings) { s.AccountOrder = []string{accA, accB} })
	healthyAll(e)
	quota(e, accA, 97, 40, 5*time.Minute, 4*24*time.Hour)
	mustRoute(t, pickFor(t, e, session("x")), accB)
	clk.add(10 * time.Minute) // A has reset, but dwell (20 min) not reached
	mustRoute(t, pickFor(t, e, session("y")), accB)

	// Equal priority: a healthier peer is not a reason to move.
	e2, clk2 := newTestEngine(t, nil)
	quota(e2, accA, 50, 50, 3*time.Hour, 5*24*time.Hour)
	quota(e2, accB, 50, 50, 3*time.Hour, 5*24*time.Hour)
	quota(e2, accC, 50, 50, 3*time.Hour, 5*24*time.Hour)
	first := pickFor(t, e2, session("a")).AuthID
	for _, id := range []string{accA, accB, accC} {
		if id != first {
			polled(e2, id, 0, 0, 5*time.Hour, 5*24*time.Hour)
		}
	}
	for i := 0; i < 12; i++ {
		clk2.add(30 * time.Minute)
		mustRoute(t, pickFor(t, e2, session(fmt.Sprintf("p%d", i))), first)
	}
}

func TestBalancedUsesExpiringWeeklyQuotaFirst(t *testing.T) {
	e, clk := newTestEngine(t, nil)
	quota(e, accA, 97, 30, 30*time.Minute, 20*time.Hour) // weekly expires soon, 5h low
	quota(e, accB, 10, 30, 3*time.Hour, 6*24*time.Hour)  // weekly far away
	quota(e, accC, 10, 90, 3*time.Hour, 6*24*time.Hour)  // nearly out for the week
	mustRoute(t, pickFor(t, e, session("s")), accB)
	clk.add(45 * time.Minute) // A's 5h window resets; its weekly quota expires in ~19h
	mustRoute(t, pickFor(t, e, session("t")), accA)
	if !historyHas(e, "switch", "resets sooner") {
		t.Fatalf("expected use-it-or-lose-it switch: %+v", e.View(0).History)
	}
}

func TestBalancedPrefersExpiring5hWindowWhenWeeklyResetsAreClose(t *testing.T) {
	e, _ := newTestEngine(t, nil)
	healthyAll(e)
	if err := e.Select(accA); err != nil {
		t.Fatal(err)
	}
	// B's weekly quota resets 44 minutes before C's, but C's 5-hour window
	// expires an hour earlier: use C's headroom before it is lost.
	quota(e, accB, 10, 58, 3*time.Hour+52*time.Minute, 5*24*time.Hour)
	quota(e, accC, 14, 54, 2*time.Hour+44*time.Minute, 5*24*time.Hour+44*time.Minute)
	quota(e, accA, 96, 40, 2*time.Hour, 4*24*time.Hour) // A drops below 5%
	mustRoute(t, pickFor(t, e, session("new")), accC)
	if e.View(0).Active != accC {
		t.Fatalf("active should be C, got %s", e.View(0).Active)
	}

	now := e.now()
	ranked := func() []string { e.mu.Lock(); defer e.mu.Unlock(); return e.rank([]string{accB, accC}, now, "") }
	// More than a day apart, the weekly reset still decides.
	quota(e, accC, 14, 54, 2*time.Hour+44*time.Minute, 6*24*time.Hour+time.Hour)
	if r := ranked(); r[0] != accB {
		t.Fatalf("weekly reset a day sooner should win: %v", r)
	}
	// A running window beats one that has not started, even if the idle
	// account's weekly quota resets slightly sooner.
	quota(e, accB, 0, 58, 5*time.Hour, 5*24*time.Hour)
	quota(e, accC, 14, 54, 4*time.Hour, 5*24*time.Hour+3*time.Hour)
	if r := ranked(); r[0] != accC {
		t.Fatalf("running window should be used before starting an idle one: %v", r)
	}
}

func TestRankByOrdersTheCriteria(t *testing.T) {
	e, _ := newTestEngine(t, func(s *Settings) { s.AccountOrder = []string{accB, accC} })
	healthyAll(e)
	quota(e, accB, 10, 50, 4*time.Hour, 5*24*time.Hour)
	quota(e, accC, 10, 20, 1*time.Hour, 5*24*time.Hour+2*time.Hour)
	now := e.now()
	top := func(rank ...string) string {
		e.mu.Lock()
		defer e.mu.Unlock()
		s, _ := Settings{RankBy: rank}.Normalize(nil)
		e.settings.RankBy = s.RankBy
		return e.rank([]string{accB, accC}, now, "")[0]
	}
	if got := top(RankAccountOrder); got != accB {
		t.Fatalf("account order first should pick B: %s", got)
	}
	if got := top(RankFiveHourReset, RankAccountOrder); got != accC {
		t.Fatalf("5-hour reset first should pick C: %s", got)
	}
	if got := top(RankMostRemaining); got != accC {
		t.Fatalf("most remaining first should pick C (80%% vs 50%%): %s", got)
	}
	// Weekly resets two hours apart are a tie, so the next criterion decides.
	if got := top(RankWeeklyReset, RankAccountOrder); got != accB {
		t.Fatalf("close weekly resets should fall through to order: %s", got)
	}
	quota(e, accB, 10, 50, 4*time.Hour, 3*24*time.Hour)
	if got := top(RankWeeklyReset, RankFiveHourReset); got != accB {
		t.Fatalf("a weekly reset two days sooner should win: %s", got)
	}
}

func TestRankBySettingsAndPresets(t *testing.T) {
	in := DefaultSettings()
	in.RankBy = []string{" Five_Hour_Reset", "bogus", "five_hour_reset"}
	s, warn := in.Normalize(nil)
	want := []string{RankFiveHourReset, RankAccountOrder, RankWeeklyReset, RankMostRemaining}
	if fmt.Sprint(s.RankBy) != fmt.Sprint(want) || len(warn) != 1 {
		t.Fatalf("rank_by normalized to %v (warnings %v), want %v", s.RankBy, warn, want)
	}
	in = DefaultSettings()
	in.Strategy = StrategyMostRemaining
	s, _ = in.Normalize(nil)
	if s.RankBy[0] != RankMostRemaining || len(s.RankBy) != len(RankCriteria) {
		t.Fatalf("legacy strategy preset: %v", s.RankBy)
	}
	if d, _ := DefaultSettings().Normalize(nil); fmt.Sprint(d.RankBy) != fmt.Sprint(RankCriteria) {
		t.Fatalf("default ranking: %v", d.RankBy)
	}
}

func TestSwitchBackFollowsTheRanking(t *testing.T) {
	// 5-hour reset ranked first: move to the window that expires sooner.
	e, clk := newTestEngine(t, func(s *Settings) {
		s.RankBy = []string{RankFiveHourReset}
		s.AccountOrder = []string{accA, accB, accC}
	})
	quota(e, accA, 10, 30, 4*time.Hour, 5*24*time.Hour)
	polled(e, accB, 0, 30, 5*time.Hour, 5*24*time.Hour) // idle windows
	polled(e, accC, 0, 30, 5*time.Hour, 5*24*time.Hour)
	mustRoute(t, pickFor(t, e, session("s")), accA)
	quota(e, accB, 20, 30, 90*time.Minute, 5*24*time.Hour) // B's window now runs out first
	clk.add(25 * time.Minute)
	mustRoute(t, pickFor(t, e, session("t")), accB)
	if !historyHas(e, "switch", "5-hour window resets sooner") {
		t.Fatalf("expected a 5-hour switch: %+v", e.View(0).History)
	}

	// Most quota left only moves the account when it is ranked first.
	for _, tc := range []struct {
		rank []string
		want string
	}{{[]string{RankAccountOrder}, accA}, {[]string{RankMostRemaining}, accB}} {
		e, clk := newTestEngine(t, func(s *Settings) { s.RankBy = tc.rank })
		quota(e, accA, 50, 50, 3*time.Hour, 5*24*time.Hour)
		quota(e, accB, 50, 50, 3*time.Hour, 5*24*time.Hour)
		quota(e, accC, 50, 50, 3*time.Hour, 5*24*time.Hour)
		if err := e.Select(accA); err != nil {
			t.Fatal(err)
		}
		mustRoute(t, pickFor(t, e, session("a")), accA)
		e.mu.Lock()
		e.st.ActiveManual = false // as if the router had chosen A itself
		e.mu.Unlock()
		quota(e, accB, 10, 10, 3*time.Hour, 5*24*time.Hour)
		clk.add(25 * time.Minute)
		mustRoute(t, pickFor(t, e, session("b")), tc.want)
	}
}

func TestTransientUnavailabilityDetoursWithoutRebinding(t *testing.T) {
	e, clk := newTestEngine(t, func(s *Settings) { s.AccountOrder = []string{accA, accB, accC} })
	healthyAll(e)
	mustRoute(t, pickFor(t, e, session("s")), accA)
	// A briefly missing (cooldown or retry exclusion): detour, keep binding.
	r := pickFor(t, e, session("s"), accB, accC)
	mustRoute(t, r, accB)
	if !strings.Contains(r.Reason, "detour") {
		t.Fatalf("reason = %q", r.Reason)
	}
	clk.add(time.Minute)
	mustRoute(t, pickFor(t, e, session("s")), accA)
	// Missing for longer than the grace period: the session is rebound.
	pickFor(t, e, session("s"), accB, accC)
	clk.add(4 * time.Minute)
	mustRoute(t, pickFor(t, e, session("s"), accB, accC), accB)
	mustRoute(t, pickFor(t, e, session("s")), accB)
}

func TestQuotaExhaustionFrom429AndRecovery(t *testing.T) {
	e, clk := newTestEngine(t, func(s *Settings) { s.AccountOrder = []string{accA, accB} })
	healthyAll(e)
	mustRoute(t, pickFor(t, e, session("s")), accA)
	e.ObserveUsage(UsageObservation{AuthID: accA, Provider: "codex", Failed: true, Status: 429,
		Body: `{"error":{"type":"usage_limit_reached","resets_in_seconds":1200}}`})
	// Even though the host still offers A, the plugin knows it is exhausted.
	mustRoute(t, pickFor(t, e, session("s")), accB)
	v := e.View(0)
	if v.Active != accB {
		t.Fatalf("active = %s", v.Active)
	}
	for _, a := range v.Accounts {
		if a.ID == accA && a.State != "exhausted" {
			t.Fatalf("A state = %s", a.State)
		}
	}
	clk.add(21 * time.Minute)
	e.Tick()
	if !historyHas(e, "recovered", "alice@example.com recovered") {
		t.Fatalf("expected recovery: %+v", e.View(0).History)
	}
}

func TestFreshQuotaEndsBlockEarly(t *testing.T) {
	e, clk := newTestEngine(t, nil)
	healthyAll(e)
	e.ObserveUsage(UsageObservation{AuthID: accA, Provider: "codex", Failed: true, Status: 429,
		Body: `{"error":{"type":"usage_limit_reached","resets_in_seconds":7200}}`})
	if !e.isBlocked(accA, clk.now()) {
		t.Fatal("A should be blocked")
	}
	clk.add(time.Minute)
	e.ApplyPolledQuota(accA, Quota{Short: &Window{UsedPercent: 5, ObservedAt: clk.now()}, ObservedAt: clk.now(), Source: "poll"}, nil)
	if e.isBlocked(accA, clk.now()) {
		t.Fatal("fresh healthy quota should lift the block")
	}
}

func TestManualPinFallbackStrictAndResume(t *testing.T) {
	e, _ := newTestEngine(t, func(s *Settings) { s.AccountOrder = []string{accA, accB, accC} })
	healthyAll(e)
	mustRoute(t, pickFor(t, e, session("s")), accA)
	if err := e.Pin(accC); err != nil {
		t.Fatal(err)
	}
	mustRoute(t, pickFor(t, e, session("s")), accC) // warm session moves too: pin overrides
	mustRoute(t, pickFor(t, e, session("n")), accC)
	// Pinned account unavailable: route automatically (default) and note it.
	mustRoute(t, pickFor(t, e, session("n"), accA, accB), accA)
	if !historyHas(e, "fallback", "is unavailable") {
		t.Fatal("expected fallback note")
	}
	// Strict: reject instead.
	s, _ := e.Settings()
	s.ManualFallback = FallbackStrict
	e.Configure(s, nil)
	r := pickFor(t, e, session("n"), accA, accB)
	if !r.Handled || !r.Reject {
		t.Fatalf("strict mode should reject: %+v", r)
	}
	if err := e.Pin("nope"); err != ErrUnknownAccount {
		t.Fatalf("pin unknown: %v", err)
	}
	e.SetAuto()
	v := e.View(0)
	if v.Mode != ModeAuto || v.Active != accC {
		t.Fatalf("resume auto should keep pinned account current: mode=%s active=%s", v.Mode, v.Active)
	}
}

func TestManualSelectionIsNotUndoneBySwitchBack(t *testing.T) {
	e, clk := newTestEngine(t, func(s *Settings) { s.AccountOrder = []string{accA, accB} })
	healthyAll(e)
	mustRoute(t, pickFor(t, e, session("s")), accA)
	if err := e.Select(accB); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		clk.add(30 * time.Minute)
		mustRoute(t, pickFor(t, e, session(fmt.Sprintf("n%d", i))), accB)
	}
	quota(e, accB, 99, 50, time.Hour, 3*24*time.Hour)
	mustRoute(t, pickFor(t, e, session("later")), accA)
}

func TestWebsocketPinnedRequestsAreNeverMoved(t *testing.T) {
	e, _ := newTestEngine(t, nil)
	healthyAll(e)
	if err := e.Pin(accC); err != nil {
		t.Fatal(err)
	}
	r := e.Pick(PickRequest{
		Candidates: []Candidate{{ID: accA, Provider: "codex"}},
		Headers:    session("ws"),
		Metadata:   map[string]any{"pinned_auth_id": accA},
	})
	mustRoute(t, r, accA)
}

func TestRolesReserveAndOff(t *testing.T) {
	e, _ := newTestEngine(t, func(s *Settings) {
		s.AccountOrder = []string{accA, accB, accC}
		s.DisabledAccounts = []string{accA}
		s.ReserveAccounts = []string{accC}
	})
	healthyAll(e)
	mustRoute(t, pickFor(t, e, session("s")), accB)
	quota(e, accB, 99, 50, time.Hour, 3*24*time.Hour)
	mustRoute(t, pickFor(t, e, session("t")), accC) // reserve used once B is low
	r := pickFor(t, e, session("u"), accA)
	if !r.Reject {
		t.Fatalf("only an Off account available: expected rejection, got %+v", r)
	}
	// Manual pin still works for an Off account.
	if err := e.Pin(accA); err != nil {
		t.Fatal(err)
	}
	mustRoute(t, pickFor(t, e, session("u")), accA)
}

func TestNonCodexCandidatesAreLeftToCLIProxyAPI(t *testing.T) {
	e, _ := newTestEngine(t, nil)
	r := e.Pick(PickRequest{Candidates: []Candidate{{ID: "claude-1.json", Provider: "claude"}}})
	if r.Handled {
		t.Fatalf("must not handle non-codex picks: %+v", r)
	}
}

func TestStatePersistsAcrossRestart(t *testing.T) {
	e, clk := newTestEngine(t, func(s *Settings) { s.AccountOrder = []string{accA, accB} })
	healthyAll(e)
	mustRoute(t, pickFor(t, e, session("keep")), accA)
	if err := e.Pin(accB); err != nil {
		t.Fatal(err)
	}
	st, dirty := e.Export()
	if !dirty {
		t.Fatal("expected dirty state")
	}
	s, _ := e.Settings()
	if err := SaveState(s.StateFile, st); err != nil {
		t.Fatal(err)
	}
	loaded, ok, err := LoadState(s.StateFile)
	if err != nil || !ok {
		t.Fatalf("load: %v %v", ok, err)
	}
	e2 := New(s, nil, clk.now)
	e2.Restore(loaded)
	v := e2.View(0)
	if v.Mode != ModeManual || v.Pinned != accB || len(v.History) == 0 {
		t.Fatalf("restored view wrong: %+v", v)
	}
	if e2.st.Families["codex:keep"] == nil {
		t.Fatalf("session binding lost: %+v", e2.st.Families)
	}
}

func TestPollTargets(t *testing.T) {
	e, clk := newTestEngine(t, nil)
	if got := len(e.PollTargets()); got != 3 {
		t.Fatalf("all accounts without data are due, got %d", got)
	}
	healthyAll(e)
	if got := len(e.PollTargets()); got != 0 {
		t.Fatalf("fresh passive data needs no poll, got %d", got)
	}
	clk.add(11 * time.Minute)
	if got := len(e.PollTargets()); got != 3 {
		t.Fatalf("stale data should be due, got %d", got)
	}
	e2, _ := newTestEngine(t, func(s *Settings) { s.QuotaPollMinutes = 0 })
	if got := len(e2.PollTargets()); got != 0 {
		t.Fatalf("polling disabled, got %d", got)
	}
}

func TestConcurrentPicksAndObservations(t *testing.T) {
	e, _ := newTestEngine(t, nil)
	healthyAll(e)
	var wg sync.WaitGroup
	for g := 0; g < 32; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				r := pickFor(t, e, subagent(fmt.Sprintf("p%d", g%5), fmt.Sprintf("c%d-%d", g, i%7)))
				if !r.Handled {
					t.Errorf("unhandled pick: %+v", r)
					return
				}
				if i%20 == 0 {
					quota(e, r.AuthID, float64(i%90), 40, time.Hour, 3*24*time.Hour)
					_ = e.View(10)
					e.Tick()
				}
			}
		}(g)
	}
	wg.Wait()
	// All subagents of a parent must share one account.
	fam := map[string]string{}
	for k, b := range e.st.Families {
		fam[k] = b.Account
	}
	if len(fam) > 5 {
		t.Fatalf("expected at most 5 families (one per parent), got %d", len(fam))
	}
}

func TestSettingsNormalization(t *testing.T) {
	s, warn := ParseSettings([]byte("switch_threshold_percent: 150\nstrategy: weird\nquota_poll_minutes: 1\naccount_order: [a, a, b]\nreserve_accounts: [b]\ndisabled_accounts: [b]\n"))
	if s.SwitchThresholdPercent != 90 || s.Strategy != StrategyBalanced || s.QuotaPollMinutes != 2 {
		t.Fatalf("normalized settings wrong: %+v", s)
	}
	if len(s.AccountOrder) != 2 || len(s.ReserveAccounts) != 0 || len(warn) < 4 {
		t.Fatalf("lists/warnings wrong: %+v %v", s, warn)
	}
	if _, warn := ParseSettings([]byte("switch_threshold_percent: [nope")); len(warn) != 1 {
		t.Fatalf("broken YAML should fall back with one warning: %v", warn)
	}
	// Host passes enabled/priority keys too; they must be ignored, not rejected.
	if _, warn := ParseSettings([]byte("enabled: true\npriority: 1\n")); len(warn) != 0 {
		t.Fatalf("host keys should be accepted: %v", warn)
	}
}

func TestViewSeparatesWorkingAndIdleSessions(t *testing.T) {
	e, clk := newTestEngine(t, func(s *Settings) { s.AccountOrder = []string{accA, accB, accC} })
	healthyAll(e)
	mustRoute(t, pickFor(t, e, session("main")), accA)
	if err := e.Select(accB); err != nil {
		t.Fatal(err)
	}
	clk.add(time.Minute)
	mustRoute(t, pickFor(t, e, session("main")), accA) // busy session keeps its account
	mustRoute(t, pickFor(t, e, session("new")), accB)
	// Its next request arrives on a websocket CLIProxyAPI pinned to C: the
	// session stays bound to B but is working on C, so that is where it counts.
	e.Pick(PickRequest{Candidates: []Candidate{{ID: accC, Provider: "codex"}}, Headers: session("new"),
		Metadata: map[string]any{"pinned_auth_id": accC}})

	counts := func() map[string][2]int {
		out := map[string][2]int{}
		for _, a := range e.View(0).Accounts {
			out[a.ID] = [2]int{a.BusySessions, a.IdleSessions}
		}
		return out
	}
	if c := counts(); c[accA] != [2]int{1, 0} || c[accB] != [2]int{0, 0} || c[accC] != [2]int{1, 0} {
		t.Fatalf("working now (busy, idle): %v", c)
	}
	clk.add(10 * time.Minute)
	if c := counts(); c[accA] != [2]int{0, 1} || c[accB] != [2]int{0, 1} || c[accC] != [2]int{0, 0} {
		t.Fatalf("after 10 quiet minutes (busy, idle): %v", c)
	}
	quota(e, accA, 20, 30, 3*time.Hour, 5*24*time.Hour)
	for _, a := range e.View(0).Accounts {
		if a.ID == accA && !a.LastUsed.Equal(clk.now()) {
			t.Fatalf("last used not recorded: %v", a.LastUsed)
		}
	}
}
