package service

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Danielw412/cpa-custom-router/internal/engine"
)

// Holding requests for a pause or slow down. scheduler.pick blocks on the
// host's request goroutine until the request may go: the engine says when,
// this file waits, outside the engine lock. CLIProxyAPI sends nothing to the
// client or upstream until pick returns, and puts no time limit on it.

// Hold limits; variables so tests can shorten them.
var (
	// HoldLimit bounds one HTTP request's wait while paused. Codex keeps
	// waiting for the response headers, so a long hold costs nothing; the
	// limit only guarantees every held call returns (CLIProxyAPI waits for
	// them before it unloads the plugin, e.g. when it is turned off). It is
	// longer than the longest auto-resume interval, so with auto-resume on a
	// request is always let through before it would be ended.
	HoldLimit = time.Duration(engine.MaxAutoResumeMinutes+1) * time.Minute
	// HoldLimitWebSocket bounds a WebSocket request's wait. Codex gives up on
	// a WebSocket turn after 5 minutes without an event and reconnects, so
	// holding longer only keeps an abandoned request around; ending it just
	// before gives Codex a clean error to retry.
	HoldLimitWebSocket = 290 * time.Second
	// MaxHeld bounds requests waiting at once. Past it, the longest-waiting
	// request, most likely abandoned by its client, is ended.
	MaxHeld = 200
	// holdRecheck re-reads the pause state while waiting, as a backstop.
	holdRecheck = 5 * time.Second
)

// RejectPaused is the reject code for a paused request that was ended.
const RejectPaused = "quota_router_paused"

const (
	endTooMany = "custom-quota-router: requests are paused and too many are waiting; this one was ended so the client can retry it"
	endStopped = "custom-quota-router: requests are paused and the plugin is reloading; retry the request"
)

type holder struct {
	mu      sync.Mutex
	wake    chan struct{}
	waiting []*heldRequest
	stopped bool
	// exempt holds session IDs of the plugin's own window starts, which are
	// never held.
	exempt map[string]bool
}

type heldRequest struct {
	end chan struct{}
	why string // set before end is closed
}

func newHolder() *holder {
	return &holder{wake: make(chan struct{}), exempt: map[string]bool{}}
}

// Hold waits while requests are paused or slowed down. It returns "" when
// the request may be routed, or a reason to reject it (a paused request that
// waited too long, or was ended to make room or for a reload).
func (s *Service) Hold(req engine.PickRequest) (reject string) {
	defer func() {
		if r := recover(); r != nil {
			reject = "" // a bug here must never hold or fail a request
		}
	}()
	if !hasCodex(req.Candidates) || s.held.isExempt(req.Headers) {
		return ""
	}
	g := s.Engine.Gate()
	if !g.Paused && g.SlowDown <= 0 {
		return ""
	}
	limit := HoldLimit
	if _, ws := req.Metadata["execution_session_id"]; ws {
		limit = HoldLimitWebSocket
	}
	return s.held.wait(s.Engine, time.Now(), limit)
}

// Waiting is how many requests are being held right now.
func (s *Service) Waiting() int {
	s.held.mu.Lock()
	defer s.held.mu.Unlock()
	return len(s.held.waiting)
}

// WakeHeld makes waiting requests re-check the pause state (after a change).
func (s *Service) WakeHeld() { s.held.notify() }

func (h *holder) wait(e *engine.Engine, arrived time.Time, holdLimit time.Duration) string {
	r := &heldRequest{end: make(chan struct{})}
	h.mu.Lock()
	if h.stopped {
		h.mu.Unlock()
		return ""
	}
	h.waiting = append(h.waiting, r)
	if len(h.waiting) > MaxHeld {
		h.endLocked(h.waiting[0], endTooMany)
	}
	h.mu.Unlock()
	defer h.remove(r)

	released := false // let through by auto-resume
	for {
		// Take the wake-up channel before reading the state, so a change in
		// between still wakes this wait.
		h.mu.Lock()
		wake := h.wake
		h.mu.Unlock()
		g := e.Gate()
		now := time.Now()
		paused := g.Paused && !released
		var next time.Time
		if paused {
			if at := g.NextAutoResume(arrived); !at.IsZero() && !now.Before(at) {
				released = true
				e.NoteAutoResume()
				continue
			}
			limit := arrived.Add(holdLimit)
			if !now.Before(limit) {
				return fmt.Sprintf("custom-quota-router: requests are paused; this one waited %s and was ended so the client can retry it", waitText(holdLimit))
			}
			next = earliest(limit, g.NextAutoResume(arrived), g.Until)
		} else {
			// The slow-down wait counts from arrival, so time spent paused
			// already covers it.
			notBefore := arrived.Add(g.SlowDown)
			if !now.Before(notBefore) {
				return ""
			}
			next = notBefore
		}
		d := next.Sub(now)
		if d > holdRecheck {
			d = holdRecheck
		}
		t := time.NewTimer(d)
		select {
		case <-wake:
		case <-t.C:
		case <-r.end:
			t.Stop()
			if paused {
				return r.why
			}
			return "" // only slowed down: let it go rather than fail it
		}
		t.Stop()
	}
}

func (h *holder) notify() {
	h.mu.Lock()
	close(h.wake)
	h.wake = make(chan struct{})
	h.mu.Unlock()
}

// stop ends every waiting request (quiesce or shutdown) so the host can
// finish its calls into this instance. Paused requests are rejected and
// retried by their clients, landing on the successor, which is still paused.
func (h *holder) stop() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.stopped = true
	for len(h.waiting) > 0 {
		h.endLocked(h.waiting[0], endStopped)
	}
}

func (h *holder) start() {
	h.mu.Lock()
	h.stopped = false
	h.mu.Unlock()
}

// endLocked wakes one waiting request with a reason and forgets it.
func (h *holder) endLocked(r *heldRequest, why string) {
	h.removeLocked(r)
	r.why = why
	close(r.end)
}

func (h *holder) remove(r *heldRequest) {
	h.mu.Lock()
	h.removeLocked(r)
	h.mu.Unlock()
}

func (h *holder) removeLocked(r *heldRequest) {
	for i, w := range h.waiting {
		if w == r {
			h.waiting = append(h.waiting[:i], h.waiting[i+1:]...)
			return
		}
	}
}

func (h *holder) setExempt(id string, on bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if on {
		h.exempt[id] = true
	} else {
		delete(h.exempt, id)
	}
}

func (h *holder) isExempt(headers map[string][]string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.exempt) == 0 {
		return false
	}
	for k, vs := range headers {
		if strings.EqualFold(k, "Session_id") || strings.EqualFold(k, "Session-Id") {
			for _, v := range vs {
				if h.exempt[strings.TrimSpace(v)] {
					return true
				}
			}
		}
	}
	return false
}

func hasCodex(cands []engine.Candidate) bool {
	for _, c := range cands {
		if strings.EqualFold(strings.TrimSpace(c.Provider), "codex") && c.ID != "" {
			return true
		}
	}
	return false
}

// earliest returns the earliest non-zero time.
func earliest(ts ...time.Time) time.Time {
	var out time.Time
	for _, t := range ts {
		if !t.IsZero() && (out.IsZero() || t.Before(out)) {
			out = t
		}
	}
	return out
}

func waitText(d time.Duration) string {
	if d >= 2*time.Minute {
		return fmt.Sprintf("%d minutes", int((d+30*time.Second)/time.Minute))
	}
	return d.Round(time.Millisecond).String()
}
