package engine

import (
	"strings"
	"testing"
	"time"
)

// poll simulates a Codex usage-endpoint reading for the 5-hour window.
func poll(e *Engine, id string, used float64, resetIn time.Duration) {
	now := e.now()
	e.ApplyPolledQuota(id, Quota{Short: &Window{UsedPercent: used, WindowMinutes: 300, ResetAt: now.Add(resetIn), ObservedAt: now},
		ObservedAt: now, Source: "poll"}, nil)
}

func TestWindowStateDetection(t *testing.T) {
	e, clk := newTestEngine(t, nil)
	if s, _ := e.WindowState(accA); s != WindowUnknown {
		t.Fatalf("no data: %s", s)
	}
	poll(e, accA, 0, 5*time.Hour) // reset keeps sliding: not started
	if s, _ := e.WindowState(accA); s != WindowIdle {
		t.Fatalf("sliding reset should be idle: %s", s)
	}
	poll(e, accB, 0, 4*time.Hour) // 0% but the clock is running
	if s, _ := e.WindowState(accB); s != WindowRunning {
		t.Fatalf("fixed reset should be running: %s", s)
	}
	poll(e, accC, 12, 2*time.Hour)
	if s, at := e.WindowState(accC); s != WindowRunning || !at.Equal(clk.now().Add(2*time.Hour)) {
		t.Fatalf("used window: %s %v", s, at)
	}
	clk.add(3 * time.Hour)
	if s, _ := e.WindowState(accC); s != WindowIdle {
		t.Fatalf("after its reset the window is idle: %s", s)
	}
	// Real traffic marks the window running even while the reading shows 0%.
	quota(e, accA, 0, 10, 4*time.Hour+59*time.Minute, 5*24*time.Hour)
	if s, _ := e.WindowState(accA); s != WindowRunning {
		t.Fatalf("successful request should mark running: %s", s)
	}
}

func TestOnlyALaterReadingWithANewResetEndsAStartedWindow(t *testing.T) {
	e, clk := newTestEngine(t, nil)
	start := clk.now()
	until := start.Add(5 * time.Hour)
	e.RecordStart(StartResult{At: start, Account: accA, Status: StartStarted, ResetAt: until})
	// A tiny start leaves 0% used; readings of that window report its reset.
	clk.add(40 * time.Minute)
	poll(e, accA, 0, until.Sub(clk.now()))
	if s, at := e.WindowState(accA); s != WindowRunning || !at.Equal(until) {
		t.Fatalf("started window read at 0%%: %s %v", s, at)
	}
	// Codex resets the limits early: the reset slides again.
	clk.add(time.Hour)
	poll(e, accA, 0, 5*time.Hour)
	if s, _ := e.WindowState(accA); s != WindowIdle {
		t.Fatalf("early reset should leave the window idle: %s", s)
	}
	// The next request starts a fresh window.
	clk.add(time.Hour)
	quota(e, accA, 1, 10, 5*time.Hour, 5*24*time.Hour)
	if s, at := e.WindowState(accA); s != WindowRunning || !at.Equal(clk.now().Add(5*time.Hour)) {
		t.Fatalf("new window after the early reset: %s %v", s, at)
	}

	// A window that was already running before a start reports its real,
	// earlier reset; that wins over the start's estimate.
	clk.add(time.Minute)
	poll(e, accB, 30, 2*time.Hour)
	e.RecordStart(StartResult{At: clk.now(), Account: accB, Status: StartStarted, ResetAt: clk.now().Add(5 * time.Hour)})
	clk.add(10 * time.Minute)
	poll(e, accB, 31, 110*time.Minute)
	if s, at := e.WindowState(accB); s != WindowRunning || !at.Equal(clk.now().Add(110*time.Minute)) {
		t.Fatalf("reading should correct the estimate: %s %v", s, at)
	}
}

func TestPlanNowDelayAndStagger(t *testing.T) {
	e, clk := newTestEngine(t, func(s *Settings) { s.AccountOrder = []string{accC, accB, accA} })
	healthyAll(e)
	if err := e.Select(accB); err != nil {
		t.Fatal(err)
	}
	plans, err := e.PlanStarts("", 0, 0)
	if err != nil || len(plans) != 3 {
		t.Fatalf("start all now: %v %d", err, len(plans))
	}
	if plans[0].Account != accB || plans[1].Account != accC || plans[2].Account != accA {
		t.Fatalf("current account first, then routing order: %v", []string{plans[0].Account, plans[1].Account, plans[2].Account})
	}
	for _, p := range plans {
		if !p.At.Equal(clk.now()) || p.Source != SourceNow {
			t.Fatalf("now plan wrong: %+v", p)
		}
	}
	plans, _ = e.PlanStarts("", 0, 45*time.Minute)
	for i, p := range plans {
		if !p.At.Equal(clk.now().Add(time.Duration(i)*45*time.Minute)) || p.Source != SourceStagger {
			t.Fatalf("stagger plan %d wrong: %+v", i, p)
		}
	}
	if n := len(e.View(0).Starter.Plans); n != 3 {
		t.Fatalf("new plans replace pending one-offs; got %d", n)
	}
	plans, _ = e.PlanStarts("", 30*time.Minute, 0)
	for _, p := range plans {
		if !p.At.Equal(clk.now().Add(30*time.Minute)) || p.Source != SourceDelay {
			t.Fatalf("delay plan wrong: %+v", p)
		}
	}
	if _, err := e.PlanStarts("nope.json", 0, 0); err != ErrUnknownAccount {
		t.Fatalf("unknown account: %v", err)
	}
	if _, err := e.PlanStarts("", -time.Minute, 0); err != ErrBadSchedule {
		t.Fatalf("negative delay: %v", err)
	}
}

func TestDueStartsDailyMissedAndCancel(t *testing.T) {
	e, clk := newTestEngine(t, nil)
	at := clk.now().Add(time.Hour)
	daily, err := e.PlanManual(accA, at, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.PlanManual(accB, clk.now().Add(2*time.Hour), false); err != nil {
		t.Fatal(err)
	}
	if len(e.DueStarts()) != 0 {
		t.Fatal("nothing due yet")
	}
	clk.add(time.Hour)
	due := e.DueStarts()
	if len(due) != 1 || due[0].ID != daily.ID {
		t.Fatalf("daily plan should be due: %+v", due)
	}
	v := e.View(0)
	if len(v.Starter.Plans) != 2 {
		t.Fatalf("daily plan should be rescheduled, one-off kept: %+v", v.Starter.Plans)
	}
	for _, p := range v.Starter.Plans {
		if p.Account == accA && !p.At.Equal(at.Add(24*time.Hour)) {
			t.Fatalf("daily not moved to tomorrow: %v", p.At)
		}
	}
	// CLIProxyAPI down for 3 hours: B's start is missed, not sent late.
	clk.add(3 * time.Hour)
	if due := e.DueStarts(); len(due) != 0 {
		t.Fatalf("overdue start must not run late: %+v", due)
	}
	if v := e.View(0); v.Starter.Log[0].Status != StartMissed {
		t.Fatalf("missed start not logged: %+v", v.Starter.Log)
	}
	// Cancel by id, then "turn everything off".
	if _, err := e.CancelStarts("bogus", ""); err != ErrUnknownPlan {
		t.Fatalf("cancel unknown: %v", err)
	}
	e.PlanStarts("", 10*time.Minute, 0)
	if n, _ := e.CancelStarts("", accB); n != 1 {
		t.Fatalf("cancel account: %d", n)
	}
	if n, _ := e.CancelStarts("", ""); n != 3 {
		t.Fatalf("turn everything off should remove the rest (daily included): %d", n)
	}
	if len(e.View(0).Starter.Plans) != 0 {
		t.Fatal("plans remain after turning everything off")
	}
	if _, err := e.PlanManual(accA, clk.now().Add(-time.Hour), false); err != ErrBadSchedule {
		t.Fatalf("past time: %v", err)
	}
}

func TestRecordStartMarksWindowAndLogs(t *testing.T) {
	e, clk := newTestEngine(t, nil)
	poll(e, accA, 0, 5*time.Hour)
	before, after := 0.0, 1.0
	e.RecordStart(StartResult{At: clk.now(), Account: accA, Status: StartStarted, InputTokens: 900, OutputTokens: 7,
		ShortBefore: &before, ShortAfter: &after, ResetAt: clk.now().Add(5 * time.Hour)})
	if s, _ := e.WindowState(accA); s != WindowRunning {
		t.Fatalf("started window should be running: %s", s)
	}
	v := e.View(0)
	if v.Starter.LastCost == nil || v.Starter.LastCost.InputTokens != 900 {
		t.Fatalf("cost not exposed: %+v", v.Starter.LastCost)
	}
	if !strings.Contains(v.History[0].Reason, "Started alice@example.com's 5-hour window") {
		t.Fatalf("history: %+v", v.History[0])
	}
	e.RecordStart(StartResult{At: clk.now(), Account: accB, Status: StartFailed, Detail: "HTTP 429"})
	if !strings.Contains(e.View(0).History[0].Reason, "Could not start") {
		t.Fatal("failure not in history")
	}
}

func TestStartOneAccountInMinutes(t *testing.T) {
	e, clk := newTestEngine(t, nil)
	healthyAll(e)
	if _, err := e.PlanManual(accB, clk.now().Add(5*time.Hour), true); err != nil {
		t.Fatal(err)
	}
	if _, err := e.PlanStarts(accB, 45*time.Minute, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := e.PlanStarts(accB, 20*time.Minute, 0); err != nil { // replaces the 45-minute one
		t.Fatal(err)
	}
	v := e.View(0)
	var oneOff, daily int
	for _, p := range v.Starter.Plans {
		if p.Account != accB {
			t.Fatalf("only bob's account should be scheduled: %+v", p)
		}
		if p.Daily {
			daily++
		} else {
			oneOff++
			if !p.At.Equal(clk.now().Add(20*time.Minute)) || p.Source != SourceDelay {
				t.Fatalf("wrong delayed plan: %+v", p)
			}
		}
	}
	if oneOff != 1 || daily != 1 {
		t.Fatalf("want one delayed start plus the kept daily one, got %d/%d", oneOff, daily)
	}
	if !strings.Contains(v.History[0].Reason, "bob@example.com's 5-hour window set to start") || !strings.Contains(v.History[0].Reason, "(in 20 min)") {
		t.Fatalf("history: %q", v.History[0].Reason)
	}
	clk.add(20 * time.Minute)
	if due := e.DueStarts(); len(due) != 1 || due[0].Account != accB {
		t.Fatalf("bob's start should be due after 20 minutes: %+v", due)
	}
}

func autoStart(s *Settings) { s.AutoStartWindows = true }

func TestAutoStartStartsTheNextWindowAMinuteAfterAReset(t *testing.T) {
	e, clk := newTestEngine(t, autoStart)
	quota(e, accA, 20, 30, time.Hour, 5*24*time.Hour)
	quota(e, accB, 20, 30, 3*time.Hour, 5*24*time.Hour)
	quota(e, accC, 20, 30, 4*time.Hour, 5*24*time.Hour)
	if due := e.DueStarts(); len(due) != 0 {
		t.Fatalf("every window is running: %+v", due)
	}
	reset := clk.now().Add(time.Hour)
	if next := e.NextStartAt(); !next.Equal(reset.Add(time.Minute)) {
		t.Fatalf("the starter should wake a minute after alice's reset: %v", next)
	}
	clk.add(time.Hour)
	if due := e.DueStarts(); len(due) != 0 {
		t.Fatalf("a start right at the reset could land in the old window: %+v", due)
	}
	clk.add(time.Minute)
	due := e.DueStarts()
	if len(due) != 1 || due[0].Account != accA || due[0].Source != SourceAuto || !due[0].At.Equal(clk.now()) {
		t.Fatalf("alice's next window should start now: %+v", due)
	}
	if again := e.DueStarts(); len(again) != 0 {
		t.Fatalf("a start in flight must not be sent twice: %+v", again)
	}
	e.RecordStart(StartResult{At: clk.now(), Account: accA, Source: SourceAuto, Status: StartStarted, ResetAt: clk.now().Add(5 * time.Hour)})
	if s, _ := e.WindowState(accA); s != WindowRunning {
		t.Fatalf("started window should be running: %s", s)
	}
	if !historyHas(e, "window", "Auto-start: started alice@example.com's next 5-hour window") {
		t.Fatalf("history: %+v", e.View(0).History)
	}
	bobAt := t0.Add(3*time.Hour + time.Minute)
	if next := e.NextStartAt(); !next.Equal(bobAt) {
		t.Fatalf("next automatic start should follow bob's reset: %v", next)
	}
	for _, a := range e.View(0).Accounts {
		if a.ID == accB && !a.AutoStartAt.Equal(bobAt) {
			t.Fatalf("panel should show bob's next automatic start: %v", a.AutoStartAt)
		}
	}
}

func TestAutoStartIsOffByDefault(t *testing.T) {
	e, clk := newTestEngine(t, nil)
	polled(e, accA, 0, 10, 5*time.Hour, 5*24*time.Hour)
	clk.add(time.Hour)
	if due := e.DueStarts(); len(due) != 0 {
		t.Fatalf("nothing starts on its own unless auto-start is on: %+v", due)
	}
	if next := e.NextStartAt(); !next.IsZero() {
		t.Fatalf("nothing to wake for: %v", next)
	}
	for _, a := range e.View(0).Accounts {
		if !a.AutoStartAt.IsZero() {
			t.Fatalf("no automatic start to show: %+v", a)
		}
	}
}

func TestAutoStartStartsIdleWindowsRightAway(t *testing.T) {
	e, clk := newTestEngine(t, autoStart)
	for _, id := range []string{accA, accB, accC} {
		polled(e, id, 0, 10, 5*time.Hour, 5*24*time.Hour)
	}
	due := e.DueStarts()
	if len(due) != 3 {
		t.Fatalf("every idle window should start at once: %+v", due)
	}

	for _, p := range due {
		e.RecordStart(StartResult{At: clk.now(), Account: p.Account, Source: SourceAuto, Status: StartStarted, ResetAt: clk.now().Add(5 * time.Hour)})
	}
	if due := e.DueStarts(); len(due) != 0 {
		t.Fatalf("every window is running now: %+v", due)
	}

	// Codex resets the limits early: the next window starts right away.
	clk.add(time.Hour)
	poll(e, accA, 0, 5*time.Hour)
	due = e.DueStarts()
	if len(due) != 1 || due[0].Account != accA {
		t.Fatalf("after an early reset alice's next window should start now: %+v", due)
	}
}

func TestAutoStartSkipsAccountsItShouldNotTouch(t *testing.T) {
	e, _ := newTestEngine(t, func(s *Settings) {
		s.AutoStartWindows = true
		s.DisabledAccounts = []string{accB}
	})
	if due := e.DueStarts(); len(due) != 0 {
		t.Fatalf("no quota data yet, so no window is known to be idle: %+v", due)
	}
	polled(e, accA, 0, 100, 5*time.Hour, 2*24*time.Hour) // weekly quota used up
	polled(e, accB, 0, 10, 5*time.Hour, 5*24*time.Hour)  // Off
	polled(e, accC, 0, 10, 5*time.Hour, 5*24*time.Hour)
	// Carol's quota can't be read before sending (as with an API key).
	e.ObserveAccounts([]HostAccount{
		{ID: accA, AuthIndex: "1", Provider: "codex", Email: "alice@example.com"},
		{ID: accB, AuthIndex: "2", Provider: "codex", Email: "bob@example.com"},
		{ID: accC, Provider: "codex", Email: "carol@example.com"},
	})
	if due := e.DueStarts(); len(due) != 0 {
		t.Fatalf("out of weekly quota, Off and unreadable accounts are left alone: %+v", due)
	}
	e.ObserveAccounts([]HostAccount{
		{ID: accA, AuthIndex: "1", Provider: "codex", Email: "alice@example.com"},
		{ID: accB, AuthIndex: "2", Provider: "codex", Email: "bob@example.com"},
		{ID: accC, AuthIndex: "3", Provider: "codex", Email: "carol@example.com"},
	})
	if due := e.DueStarts(); len(due) != 1 || due[0].Account != accC {
		t.Fatalf("only carol should start: %+v", due)
	}
}

func TestAutoStartWaitsForAScheduledStart(t *testing.T) {
	e, clk := newTestEngine(t, autoStart)
	polled(e, accA, 0, 10, 5*time.Hour, 5*24*time.Hour)
	quota(e, accB, 20, 30, 3*time.Hour, 5*24*time.Hour)
	quota(e, accC, 20, 30, 3*time.Hour, 5*24*time.Hour)
	plans, _ := e.PlanStarts(accA, 2*time.Hour, 0)
	if due := e.DueStarts(); len(due) != 0 {
		t.Fatalf("a window started now would still run at alice's scheduled start: %+v", due)
	}
	if next := e.NextStartAt(); !next.Equal(plans[0].At) {
		t.Fatalf("the scheduled start comes next: %v", next)
	}
	if _, err := e.PlanManual(accA, clk.now().Add(6*time.Hour), true); err != nil {
		t.Fatal(err)
	}
	due := e.DueStarts()
	if len(due) != 1 || due[0].Account != accA {
		t.Fatalf("a window started now ends before the 6-hour start, so it starts: %+v", due)
	}
}

func TestAutoStartBacksOffWhenAStartFails(t *testing.T) {
	e, clk := newTestEngine(t, autoStart)
	polled(e, accA, 0, 10, 5*time.Hour, 5*24*time.Hour)
	quota(e, accB, 20, 30, 3*time.Hour, 5*24*time.Hour)
	quota(e, accC, 20, 30, 3*time.Hour, 5*24*time.Hour)
	fail := func() {
		t.Helper()
		due := e.DueStarts()
		if len(due) != 1 || due[0].Account != accA {
			t.Fatalf("alice should be due: %+v", due)
		}
		e.RecordStart(StartResult{At: clk.now(), Account: accA, Source: SourceAuto, Status: StartFailed, Detail: "HTTP 500"})
	}
	fail()
	if !historyHas(e, "fallback", "Auto-start could not start alice@example.com's 5-hour window: HTTP 500. It tries again in 15 min") {
		t.Fatalf("history: %+v", e.View(0).History)
	}
	clk.add(14 * time.Minute)
	if due := e.DueStarts(); len(due) != 0 {
		t.Fatalf("retried too soon: %+v", due)
	}
	clk.add(time.Minute)
	fail()
	if !historyHas(e, "fallback", "It tries again in 30 min") {
		t.Fatalf("a second failure in a row should wait longer: %+v", e.View(0).History)
	}
	clk.add(29 * time.Minute)
	if due := e.DueStarts(); len(due) != 0 {
		t.Fatalf("retried too soon: %+v", due)
	}
	clk.add(time.Minute)
	if due := e.DueStarts(); len(due) != 1 {
		t.Fatalf("alice should be tried again: %+v", due)
	}
}

func TestAutoStartSaysNothingWhenAWindowIsAlreadyRunning(t *testing.T) {
	e, clk := newTestEngine(t, autoStart)
	polled(e, accA, 0, 10, 5*time.Hour, 5*24*time.Hour)
	quota(e, accB, 20, 30, 3*time.Hour, 5*24*time.Hour)
	quota(e, accC, 20, 30, 3*time.Hour, 5*24*time.Hour)
	if due := e.DueStarts(); len(due) != 1 {
		t.Fatalf("alice should be due: %+v", due)
	}
	// The fresh reading before sending shows live traffic started the window.
	poll(e, accA, 3, 2*time.Hour)
	history := len(e.View(0).History)
	e.RecordStart(StartResult{At: clk.now(), Account: accA, Source: SourceAuto, Status: StartRunning, ResetAt: clk.now().Add(2 * time.Hour)})
	v := e.View(0)
	if len(v.Starter.Log) != 0 || len(v.History) != history {
		t.Fatalf("nothing was sent, so nothing is reported: %+v %+v", v.Starter.Log, v.History)
	}
	if next := e.NextStartAt(); !next.Equal(clk.now().Add(2*time.Hour + time.Minute)) {
		t.Fatalf("the next try follows that window's reset: %v", next)
	}
}

func TestTurningAutoStartOnAndOffIsInHistory(t *testing.T) {
	e, _ := newTestEngine(t, nil)
	s, warn := ParseSettings([]byte("auto_start_windows: true\n"))
	if len(warn) != 0 || !s.AutoStartWindows {
		t.Fatalf("auto_start_windows should parse: %v %v", s.AutoStartWindows, warn)
	}
	s.StateFile = e.settings.StateFile
	e.Configure(s, nil)
	if !historyHas(e, "window", "Auto-start on") || historyHas(e, "settings", "Routing preferences updated") {
		t.Fatalf("history: %+v", e.View(0).History)
	}
	s.AutoStartWindows = false
	e.Configure(s, nil)
	if !historyHas(e, "window", "Auto-start off") {
		t.Fatalf("history: %+v", e.View(0).History)
	}
}
