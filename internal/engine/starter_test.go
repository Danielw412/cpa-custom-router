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
