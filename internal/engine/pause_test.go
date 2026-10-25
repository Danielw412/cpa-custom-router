package engine

import (
	"testing"
	"time"
)

func TestPausedTimeDoesNotMoveASessionOffItsAccount(t *testing.T) {
	e, clk := newTestEngine(t, nil)
	healthyAll(e)
	mustRoute(t, pickFor(t, e, session("s1")), accA)
	if err := e.Select(accB); err != nil {
		t.Fatal(err)
	}
	clk.add(10 * time.Minute)
	if err := e.Pause(0, 0); err != nil {
		t.Fatal(err)
	}
	clk.add(2 * time.Hour)
	if !e.Resume() {
		t.Fatal("Resume reported no pause in progress")
	}
	// 10 minutes idle before the pause: still inside the 30-minute cache
	// window, so the session keeps its account instead of rejoining B.
	mustRoute(t, pickFor(t, e, session("s1")), accA)

	// After the pause, idle time counts again as usual.
	clk.add(31 * time.Minute)
	mustRoute(t, pickFor(t, e, session("s1")), accB)
	if !historyHas(e, "pause", "Resumed after 2 h") {
		t.Fatal("resume not recorded in history")
	}
}

func TestSessionLetThroughByAutoResumeKeepsItsAccount(t *testing.T) {
	e, clk := newTestEngine(t, nil)
	healthyAll(e)
	mustRoute(t, pickFor(t, e, session("s1")), accA)
	if err := e.Select(accB); err != nil {
		t.Fatal(err)
	}
	clk.add(20 * time.Minute)
	if err := e.Pause(0, 20); err != nil {
		t.Fatal(err)
	}
	// The request released 20 minutes into the pause: 40 minutes since the
	// last request, but only 20 of them idle.
	clk.add(20 * time.Minute)
	mustRoute(t, pickFor(t, e, session("s1")), accA)
	clk.add(25 * time.Minute)
	mustRoute(t, pickFor(t, e, session("s1")), accA)
}

func TestTimedPauseEndsOnItsOwn(t *testing.T) {
	e, clk := newTestEngine(t, nil)
	if err := e.Pause(30*time.Minute, 0); err != nil {
		t.Fatal(err)
	}
	if g := e.Gate(); !g.Paused || !g.Until.Equal(t0.Add(30*time.Minute)) {
		t.Fatalf("gate = %+v, want paused until +30m", g)
	}
	clk.add(29 * time.Minute)
	if !e.Gate().Paused {
		t.Fatal("pause ended early")
	}
	clk.add(time.Minute)
	if e.Gate().Paused {
		t.Fatal("pause still in progress after its timer ran out")
	}
	if e.View(0).Pause != nil {
		t.Fatal("view still shows a pause")
	}
	if !historyHas(e, "pause", "timer ran out") {
		t.Fatal("timer expiry not recorded")
	}
}

func TestTickEndsATimedPause(t *testing.T) {
	e, clk := newTestEngine(t, nil)
	if err := e.Pause(5*time.Minute, 0); err != nil {
		t.Fatal(err)
	}
	clk.add(6 * time.Minute)
	e.Tick()
	if st, _ := e.Export(); st.Pause != nil {
		t.Fatal("Tick left an expired pause in state")
	}
}

func TestAutoResumeLetsEachRequestGoAtTheNextCycle(t *testing.T) {
	e, clk := newTestEngine(t, nil)
	if err := e.Pause(0, 20); err != nil {
		t.Fatal(err)
	}
	g := e.Gate()
	cases := []struct {
		arrived, want time.Duration
	}{
		{0, 20 * time.Minute},
		{19*time.Minute + 59*time.Second, 20 * time.Minute},
		{20 * time.Minute, 40 * time.Minute}, // just missed the cycle
		{55 * time.Minute, 60 * time.Minute},
		{-5 * time.Minute, 20 * time.Minute}, // waiting since before the pause
	}
	for _, c := range cases {
		if got := g.NextAutoResume(t0.Add(c.arrived)); !got.Equal(t0.Add(c.want)) {
			t.Errorf("arrived %v: released at %v, want %v", c.arrived, got.Sub(t0), c.want)
		}
	}

	// Changing the interval restarts the cycle from now; changing only the
	// timer keeps it.
	clk.add(7 * time.Minute)
	if err := e.Pause(60*time.Minute, 20); err != nil {
		t.Fatal(err)
	}
	if got := e.Gate().NextAutoResume(t0.Add(7 * time.Minute)); !got.Equal(t0.Add(20 * time.Minute)) {
		t.Fatalf("timer change moved the cycle to %v", got.Sub(t0))
	}
	if err := e.Pause(60*time.Minute, 10); err != nil {
		t.Fatal(err)
	}
	if got := e.Gate().NextAutoResume(t0.Add(7 * time.Minute)); !got.Equal(t0.Add(17 * time.Minute)) {
		t.Fatalf("new interval released at %v, want 17m", got.Sub(t0))
	}
	if v := e.View(0).Pause; v == nil || !v.Since.Equal(t0) || !v.NextAutoResume.Equal(t0.Add(17*time.Minute)) {
		t.Fatalf("view = %+v, want pause since t0 with next auto-resume at +17m", v)
	}

	if (Gate{}).NextAutoResume(t0) != (time.Time{}) {
		t.Fatal("auto-resume off should never release")
	}
}

func TestPauseAndSlowDownSurviveARestart(t *testing.T) {
	e, clk := newTestEngine(t, nil)
	if err := e.Pause(90*time.Minute, 15); err != nil {
		t.Fatal(err)
	}
	if err := e.SetSlowDown(45); err != nil {
		t.Fatal(err)
	}
	st, dirty := e.Export()
	if !dirty {
		t.Fatal("pause did not mark state dirty")
	}
	st.Pause.Until = time.Time{} // the export is a copy
	if g := e.Gate(); g.Until.IsZero() {
		t.Fatal("Export shared the pause with the engine")
	}
	st, _ = e.Export()
	path := e.settings.StateFile
	if err := SaveState(path, st); err != nil {
		t.Fatal(err)
	}
	loaded, ok, err := LoadState(path)
	if err != nil || !ok {
		t.Fatalf("load: ok=%v err=%v", ok, err)
	}
	e2 := New(e.settings, nil, clk.now)
	e2.Restore(loaded)
	g := e2.Gate()
	if !g.Paused || !g.Until.Equal(t0.Add(90*time.Minute)) || g.AutoResume != 15*time.Minute || g.SlowDown != 45*time.Second {
		t.Fatalf("restored gate = %+v", g)
	}
}

func TestRestoreDropsInvalidPauseAndSlowDown(t *testing.T) {
	e, _ := newTestEngine(t, nil)
	e.Restore(State{Pause: &Pause{}, SlowDownSeconds: 9999})
	if g := e.Gate(); g.Paused || g.SlowDown != 0 {
		t.Fatalf("gate = %+v, want an invalid pause and slow down dropped", g)
	}
	e.Restore(State{Pause: &Pause{Since: t0, AutoResumeMinutes: 500}})
	if g := e.Gate(); !g.Paused || g.AutoResume != 0 {
		t.Fatalf("gate = %+v, want paused with auto-resume off", g)
	}
}

func TestPauseAndSlowDownRejectOutOfRangeValues(t *testing.T) {
	e, _ := newTestEngine(t, nil)
	for _, c := range []struct {
		d    time.Duration
		auto int
	}{{-time.Minute, 0}, {MaxPause + time.Minute, 0}, {0, -1}, {0, MaxAutoResumeMinutes + 1}} {
		if err := e.Pause(c.d, c.auto); err != ErrBadPause {
			t.Errorf("Pause(%v, %d) = %v, want ErrBadPause", c.d, c.auto, err)
		}
	}
	if e.Gate().Paused {
		t.Fatal("a rejected pause took effect")
	}
	for _, s := range []int{-1, MaxSlowDownSeconds + 1} {
		if err := e.SetSlowDown(s); err != ErrBadSlowDown {
			t.Errorf("SetSlowDown(%d) = %v, want ErrBadSlowDown", s, err)
		}
	}
	if e.Resume() {
		t.Fatal("Resume reported a pause that never started")
	}
}

func TestSlowDownIsRecordedAndShown(t *testing.T) {
	e, _ := newTestEngine(t, nil)
	if err := e.SetSlowDown(30); err != nil {
		t.Fatal(err)
	}
	if e.Gate().SlowDown != 30*time.Second || e.View(0).SlowDownSeconds != 30 {
		t.Fatal("slow down not applied")
	}
	if !historyHas(e, "slowdown", "waits 30 s") {
		t.Fatal("slow down not recorded")
	}
	if err := e.SetSlowDown(0); err != nil {
		t.Fatal(err)
	}
	if !historyHas(e, "slowdown", "full speed") {
		t.Fatal("slow down off not recorded")
	}
}

func TestPausedSessionsAreNotForgotten(t *testing.T) {
	e, clk := newTestEngine(t, nil)
	healthyAll(e)
	mustRoute(t, pickFor(t, e, session("s1")), accA)
	if err := e.Pause(0, 0); err != nil {
		t.Fatal(err)
	}
	clk.add(familyTTL + time.Hour)
	e.Tick()
	if e.View(0).Sessions != 1 {
		t.Fatal("a paused session was pruned as idle")
	}
	if v := e.View(0); v.WarmSessions != 1 {
		t.Fatalf("warm sessions during a pause = %d, want 1", v.WarmSessions)
	}
}

func TestAutoResumeReleasesAreMergedInHistory(t *testing.T) {
	e, _ := newTestEngine(t, nil)
	for i := 0; i < 3; i++ {
		e.NoteAutoResume()
	}
	h := e.View(0).History
	if len(h) != 1 || h[0].Count != 3 {
		t.Fatalf("history = %+v, want one entry counted 3 times", h)
	}
}
