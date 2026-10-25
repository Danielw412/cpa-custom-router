package engine

import (
	"errors"
	"fmt"
	"time"
)

// Pause and slow down. While paused, new Codex requests wait at the router:
// nothing is sent upstream, replies already streaming finish, and every
// session keeps its account (and so its prompt cache). Slow down makes each
// request wait a few seconds before it is sent. The engine keeps the state and
// says when a waiting request may go; the service does the waiting, outside
// the engine lock.

const (
	// MaxPause bounds a timed pause. "Until resumed" has no end.
	MaxPause = 24 * time.Hour
	// MaxAutoResumeMinutes keeps auto-resume inside the time Codex keeps
	// retrying a request that gets no reply (about 25 minutes over HTTP), so
	// a waiting turn is let through before Codex gives up on it.
	MaxAutoResumeMinutes = 24
	// MaxSlowDownSeconds stays below Codex's 5-minute wait for a reply.
	MaxSlowDownSeconds = 240
)

var (
	ErrBadPause    = errors.New("invalid pause")
	ErrBadSlowDown = errors.New("invalid slow down")
)

// Pause is a pause in progress (persisted).
type Pause struct {
	Since time.Time `json:"since"`
	// Until ends the pause on its own; zero means until resumed by hand.
	Until time.Time `json:"until,omitempty"`
	// AutoResumeMinutes > 0 lets each waiting request go every that many
	// minutes (one step per conversation), then holds the next one again.
	AutoResumeMinutes int `json:"auto_resume_minutes,omitempty"`
	// AutoResumeFrom anchors the auto-resume cycle: the pause start, or when
	// the interval last changed.
	AutoResumeFrom time.Time `json:"auto_resume_from,omitempty"`
}

// Gate is what a request about to be routed checks before it may go.
type Gate struct {
	Paused         bool
	Until          time.Time
	AutoResume     time.Duration
	AutoResumeFrom time.Time
	SlowDown       time.Duration
}

// NextAutoResume is when a request that arrived at the given time is let
// through by auto-resume: the first cycle boundary after it arrived. Zero
// when auto-resume is off.
func (g Gate) NextAutoResume(arrived time.Time) time.Time {
	if g.AutoResume <= 0 {
		return time.Time{}
	}
	n := int64(0)
	if arrived.After(g.AutoResumeFrom) {
		n = int64(arrived.Sub(g.AutoResumeFrom) / g.AutoResume)
	}
	return g.AutoResumeFrom.Add(time.Duration(n+1) * g.AutoResume)
}

// Gate reports the pause and slow-down state for a request about to be
// routed, ending a timed pause whose time is up.
func (e *Engine) Gate() Gate {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.expirePause(e.now())
	g := Gate{SlowDown: time.Duration(e.st.SlowDownSeconds) * time.Second}
	if p := e.st.Pause; p != nil {
		g.Paused = true
		g.Until = p.Until
		g.AutoResume = time.Duration(p.AutoResumeMinutes) * time.Minute
		g.AutoResumeFrom = p.AutoResumeFrom
	}
	return g
}

// Pause holds new requests until Resume, or for d when d > 0. Calling it
// while paused changes the timer and auto-resume but keeps the start time.
func (e *Engine) Pause(d time.Duration, autoResumeMinutes int) error {
	if d < 0 || d > MaxPause || autoResumeMinutes < 0 || autoResumeMinutes > MaxAutoResumeMinutes {
		return ErrBadPause
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	e.expirePause(now)
	p := e.st.Pause
	fresh := p == nil
	if fresh {
		p = &Pause{Since: now}
	}
	p.Until = time.Time{}
	if d > 0 {
		p.Until = now.Add(d)
	}
	if fresh || p.AutoResumeMinutes != autoResumeMinutes {
		p.AutoResumeFrom = now
	}
	p.AutoResumeMinutes = autoResumeMinutes
	e.st.Pause = p
	e.dirty = true

	what := "Paused: new requests wait here until you resume them"
	if !p.Until.IsZero() {
		what = fmt.Sprintf("Paused until %s: new requests wait here", p.Until.Local().Format("3:04 PM"))
	}
	if !fresh {
		what = "Pause changed. " + what
	}
	if p.AutoResumeMinutes > 0 {
		what += fmt.Sprintf("; auto-resume lets each waiting conversation take one step every %d min", p.AutoResumeMinutes)
	}
	e.addHistory("pause", "", "", what)
	return nil
}

// Resume ends a pause and lets every waiting request go. It reports whether
// a pause was in progress.
func (e *Engine) Resume() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	e.expirePause(now)
	p := e.st.Pause
	if p == nil {
		return false
	}
	e.endPause(now, fmt.Sprintf("Resumed after %s: waiting requests continue", shortDuration(now.Sub(p.Since))))
	return true
}

// expirePause ends a timed pause whose time is up.
func (e *Engine) expirePause(now time.Time) {
	if p := e.st.Pause; p != nil && !p.Until.IsZero() && !now.Before(p.Until) {
		e.endPause(p.Until, "Pause ended: its timer ran out, so waiting requests continue")
	}
}

// endPause clears the pause. Paused time does not count as idle: each
// session's idle clock resumes where it stopped, so the pause alone never
// moves a session off its account and its prompt cache.
func (e *Engine) endPause(end time.Time, reason string) {
	p := e.st.Pause
	for _, b := range e.st.Families {
		from := p.Since
		if b.LastSeen.After(from) {
			from = b.LastSeen
		}
		if end.After(from) {
			b.LastSeen = b.LastSeen.Add(end.Sub(from))
		}
	}
	e.st.Pause = nil
	e.dirty = true
	e.addHistory("pause", "", "", reason)
}

// idleFor is how long a session has been idle, not counting a pause in
// progress.
func (e *Engine) idleFor(b *Binding, now time.Time) time.Duration {
	if p := e.st.Pause; p != nil {
		if !b.LastSeen.Before(p.Since) {
			return 0
		}
		return p.Since.Sub(b.LastSeen)
	}
	return now.Sub(b.LastSeen)
}

// NoteAutoResume records a waiting request that auto-resume let through.
// Requests released together are merged into one history entry with a count.
func (e *Engine) NoteAutoResume() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.addHistory("pause", "", "", "Auto-resume let a waiting request through so its conversation stays alive")
}

// SetSlowDown makes every request wait this many seconds before it is
// sent. 0 turns slow down off.
func (e *Engine) SetSlowDown(seconds int) error {
	if seconds < 0 || seconds > MaxSlowDownSeconds {
		return ErrBadSlowDown
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.st.SlowDownSeconds == seconds {
		return nil
	}
	e.st.SlowDownSeconds = seconds
	e.dirty = true
	if seconds == 0 {
		e.addHistory("slowdown", "", "", "Slow down off: requests are sent at full speed")
	} else {
		e.addHistory("slowdown", "", "", fmt.Sprintf("Slow down: each request now waits %d s before it is sent", seconds))
	}
	return nil
}

// PauseView describes a pause in progress for the panel.
type PauseView struct {
	Since             time.Time `json:"since"`
	Until             time.Time `json:"until,omitempty"`
	AutoResumeMinutes int       `json:"auto_resume_minutes,omitempty"`
	NextAutoResume    time.Time `json:"next_auto_resume,omitempty"`
}

func (e *Engine) pauseView(now time.Time) *PauseView {
	p := e.st.Pause
	if p == nil {
		return nil
	}
	v := &PauseView{Since: p.Since, Until: p.Until, AutoResumeMinutes: p.AutoResumeMinutes}
	if p.AutoResumeMinutes > 0 {
		g := Gate{AutoResume: time.Duration(p.AutoResumeMinutes) * time.Minute, AutoResumeFrom: p.AutoResumeFrom}
		v.NextAutoResume = g.NextAutoResume(now)
	}
	return v
}

// shortDuration renders "45 s", "12 min" or "2 h 5 min".
func shortDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d s", int(d.Round(time.Second)/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Round(time.Minute)/time.Minute))
	}
	d = d.Round(time.Minute)
	h, m := int(d/time.Hour), int(d%time.Hour/time.Minute)
	if m == 0 {
		return fmt.Sprintf("%d h", h)
	}
	return fmt.Sprintf("%d h %d min", h, m)
}
