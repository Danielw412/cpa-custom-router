package service

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Danielw412/cpa-custom-router/internal/engine"
)

func testService(t *testing.T) *Service {
	t.Helper()
	return New([]byte("state_file: "+filepath.Join(t.TempDir(), "state.json")+"\n"), nil)
}

func codexRequest(session string) engine.PickRequest {
	return engine.PickRequest{
		Candidates: []engine.Candidate{{ID: "codex-a.json", Provider: "codex"}},
		Headers:    map[string][]string{"Session_id": {session}},
	}
}

// holdAsync runs Hold in the background and returns its result channel.
func holdAsync(s *Service, req engine.PickRequest) <-chan string {
	out := make(chan string, 1)
	go func() { out <- s.Hold(req) }()
	return out
}

func waitFor(t *testing.T, ch <-chan string, within time.Duration) string {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(within):
		t.Fatalf("request still held after %v", within)
		return ""
	}
}

func stillHeld(t *testing.T, ch <-chan string, for_ time.Duration) {
	t.Helper()
	select {
	case r := <-ch:
		t.Fatalf("request went early (reject=%q)", r)
	case <-time.After(for_):
	}
}

func waitingCount(t *testing.T, s *Service, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for s.Waiting() != want {
		if time.Now().After(deadline) {
			t.Fatalf("waiting = %d, want %d", s.Waiting(), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestPausedRequestGoesWhenResumed(t *testing.T) {
	s := testService(t)
	if err := s.Engine.Pause(0, 0); err != nil {
		t.Fatal(err)
	}
	ch := holdAsync(s, codexRequest("s1"))
	stillHeld(t, ch, 150*time.Millisecond)
	waitingCount(t, s, 1)
	s.Engine.Resume()
	s.WakeHeld()
	if r := waitFor(t, ch, time.Second); r != "" {
		t.Fatalf("resumed request rejected: %q", r)
	}
	waitingCount(t, s, 0)
}

func TestTimedPauseLetsRequestsGoWhenItEnds(t *testing.T) {
	s := testService(t)
	if err := s.Engine.Pause(300*time.Millisecond, 0); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if r := waitFor(t, holdAsync(s, codexRequest("s1")), 2*time.Second); r != "" {
		t.Fatalf("rejected: %q", r)
	}
	if time.Since(start) < 250*time.Millisecond {
		t.Fatal("request went before the pause ended")
	}
}

func TestSlowDownDelaysEachRequest(t *testing.T) {
	s := testService(t)
	if err := s.Engine.SetSlowDown(1); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if r := waitFor(t, holdAsync(s, codexRequest("s1")), 3*time.Second); r != "" {
		t.Fatalf("slowed request rejected: %q", r)
	}
	if d := time.Since(start); d < 950*time.Millisecond {
		t.Fatalf("request went after %v, want about 1s", d)
	}
	// Turning slow down off releases a waiting request at once.
	if err := s.Engine.SetSlowDown(2); err != nil {
		t.Fatal(err)
	}
	ch := holdAsync(s, codexRequest("s1"))
	stillHeld(t, ch, 100*time.Millisecond)
	if err := s.Engine.SetSlowDown(0); err != nil {
		t.Fatal(err)
	}
	s.WakeHeld()
	waitFor(t, ch, 500*time.Millisecond)
}

func TestPausedRequestIsEndedAfterTheHoldLimit(t *testing.T) {
	s := testService(t)
	oldHTTP, oldWS := HoldLimit, HoldLimitWebSocket
	HoldLimit, HoldLimitWebSocket = 600*time.Millisecond, 150*time.Millisecond
	t.Cleanup(func() { HoldLimit, HoldLimitWebSocket = oldHTTP, oldWS })
	if err := s.Engine.Pause(0, 0); err != nil {
		t.Fatal(err)
	}
	ws := codexRequest("s1")
	ws.Metadata = map[string]any{"execution_session_id": "sock-1"}
	start := time.Now()
	r := waitFor(t, holdAsync(s, ws), 2*time.Second)
	if !strings.Contains(r, "paused") || time.Since(start) > 500*time.Millisecond {
		t.Fatalf("websocket request: %q after %v, want ended by the shorter limit", r, time.Since(start))
	}
	httpReq := holdAsync(s, codexRequest("s2"))
	stillHeld(t, httpReq, 300*time.Millisecond)
	if r := waitFor(t, httpReq, 2*time.Second); !strings.Contains(r, "paused") {
		t.Fatalf("http request: %q, want a paused explanation", r)
	}
}

func TestAutoResumeIsShorterThanTheHoldLimit(t *testing.T) {
	// A request held while auto-resume is on must be let through before it
	// would be ended.
	if HoldLimit <= time.Duration(engine.MaxAutoResumeMinutes)*time.Minute {
		t.Fatalf("HoldLimit %v must exceed the longest auto-resume interval", HoldLimit)
	}
}

func TestStopEndsPausedRequestsAndLetsSlowedOnesGo(t *testing.T) {
	s := testService(t)
	if err := s.Engine.Pause(0, 0); err != nil {
		t.Fatal(err)
	}
	paused := holdAsync(s, codexRequest("s1"))
	waitingCount(t, s, 1)
	s.held.stop()
	if r := waitFor(t, paused, time.Second); r != endStopped {
		t.Fatalf("paused request on stop: %q, want %q", r, endStopped)
	}
	// After a stop nothing is held; a restart (rollback) holds again.
	if r := waitFor(t, holdAsync(s, codexRequest("s2")), time.Second); r != "" {
		t.Fatalf("request after stop rejected: %q", r)
	}
	s.held.start()
	s.Engine.Resume()
	if err := s.Engine.SetSlowDown(5); err != nil {
		t.Fatal(err)
	}
	slowed := holdAsync(s, codexRequest("s3"))
	waitingCount(t, s, 1)
	s.held.stop()
	if r := waitFor(t, slowed, time.Second); r != "" {
		t.Fatalf("slowed request failed on stop: %q", r)
	}
}

func TestTooManyWaitingEndsTheLongestWaiting(t *testing.T) {
	s := testService(t)
	old := MaxHeld
	MaxHeld = 2
	t.Cleanup(func() { MaxHeld = old })
	if err := s.Engine.Pause(0, 0); err != nil {
		t.Fatal(err)
	}
	first := holdAsync(s, codexRequest("s1"))
	waitingCount(t, s, 1)
	second := holdAsync(s, codexRequest("s2"))
	waitingCount(t, s, 2)
	third := holdAsync(s, codexRequest("s3"))
	if r := waitFor(t, first, time.Second); r != endTooMany {
		t.Fatalf("oldest request: %q, want %q", r, endTooMany)
	}
	stillHeld(t, second, 50*time.Millisecond)
	stillHeld(t, third, 10*time.Millisecond)
	s.Engine.Resume()
	s.WakeHeld()
	waitFor(t, second, time.Second)
	waitFor(t, third, time.Second)
}

func TestWindowStartsAndOtherProvidersAreNeverHeld(t *testing.T) {
	s := testService(t)
	if err := s.Engine.Pause(0, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.Engine.SetSlowDown(30); err != nil {
		t.Fatal(err)
	}
	s.held.setExempt("start-123", true)
	start := codexRequest("start-123")
	start.Headers = map[string][]string{"session_id": {"start-123"}}
	if r := waitFor(t, holdAsync(s, start), 200*time.Millisecond); r != "" {
		t.Fatalf("window start rejected: %q", r)
	}
	claude := engine.PickRequest{Candidates: []engine.Candidate{{ID: "claude.json", Provider: "claude"}}}
	if r := waitFor(t, holdAsync(s, claude), 200*time.Millisecond); r != "" {
		t.Fatalf("non-codex request rejected: %q", r)
	}
	s.held.setExempt("start-123", false)
	stillHeld(t, holdAsync(s, start), 100*time.Millisecond)
}
