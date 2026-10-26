package engine

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"
)

// Window starts: a Codex 5-hour window only begins at an account's first
// request, so sending one tiny request at a chosen moment decides when the
// window resets. The engine keeps the plans and results; the service sends
// the requests.

const (
	DefaultStarterModel  = "gpt-6-luna"
	DefaultStarterEffort = "low"

	maxStartLog   = 60
	maxStartPlans = 200
	// A start that is overdue by more than this (CLIProxyAPI was down) is
	// reported as missed instead of being sent late.
	startMissedAfter = 15 * time.Minute
	// An idle window's reported reset slides to "now + window length"; a
	// window whose reset is within this slack of that is treated as idle.
	windowSlack = 2 * time.Minute
	shortWindow = 5 * time.Hour
	// Auto-start waits this long after a window resets, so its request
	// can't land in the window that just ended.
	autoStartDelay = time.Minute
	// autoStartRetry holds off another automatic start of the same account
	// after one was tried. Failures in a row double it, up to 8 times, so an
	// account that can't be started costs a request now and then, not one
	// every loop.
	autoStartRetry = 15 * time.Minute

	SourceNow     = "now"
	SourceDelay   = "delay"
	SourceStagger = "stagger"
	SourceManual  = "manual"
	SourceAuto    = "auto"

	WindowRunning = "running"
	WindowIdle    = "idle"
	WindowUnknown = "unknown"

	StartStarted = "started"
	StartRunning = "already_running"
	StartFailed  = "failed"
	StartMissed  = "missed"
)

// StartPlan is one scheduled window start for one account.
type StartPlan struct {
	ID      string    `json:"id"`
	Account string    `json:"account"`
	At      time.Time `json:"at"`
	Daily   bool      `json:"daily,omitempty"`
	Source  string    `json:"source"`
}

// StartResult records what happened when a plan came due. Only metadata is
// kept: no prompt or response text.
type StartResult struct {
	At              time.Time `json:"at"`
	Account         string    `json:"account"`
	Source          string    `json:"source,omitempty"`
	Status          string    `json:"status"`
	Detail          string    `json:"detail,omitempty"`
	Model           string    `json:"model,omitempty"`
	Effort          string    `json:"effort,omitempty"`
	InputTokens     int64     `json:"input_tokens,omitempty"`
	CachedTokens    int64     `json:"cached_tokens,omitempty"`
	OutputTokens    int64     `json:"output_tokens,omitempty"`
	ReasoningTokens int64     `json:"reasoning_tokens,omitempty"`
	ShortBefore     *float64  `json:"short_used_before,omitempty"`
	ShortAfter      *float64  `json:"short_used_after,omitempty"`
	LongBefore      *float64  `json:"long_used_before,omitempty"`
	LongAfter       *float64  `json:"long_used_after,omitempty"`
	ResetAt         time.Time `json:"reset_at,omitempty"`
}

var (
	ErrBadSchedule = errors.New("invalid schedule")
	ErrUnknownPlan = errors.New("unknown scheduled start")
)

// WindowState reports whether an account's 5-hour window is running.
func (e *Engine) WindowState(id string) (string, time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.windowState(id, e.now())
}

func (e *Engine) windowState(id string, now time.Time) (string, time.Time) {
	a := e.st.Accounts[id]
	if a == nil {
		return WindowUnknown, time.Time{}
	}
	var w *Window
	if a.Quota != nil {
		w = a.Quota.Short
	}
	if a.WindowUntil.After(now) && !windowReplaced(a.WindowUntil, w) {
		return WindowRunning, a.WindowUntil
	}
	if w == nil || w.ResetAt.IsZero() {
		return WindowUnknown, time.Time{}
	}
	if !now.Before(w.ResetAt) {
		return WindowIdle, time.Time{}
	}
	if w.UsedPercent > 0 {
		return WindowRunning, w.ResetAt
	}
	if w.ResetAt.Sub(w.ObservedAt) >= windowLength(w)-windowSlack {
		return WindowIdle, time.Time{} // reset keeps sliding: not started
	}
	return WindowRunning, w.ResetAt
}

// windowReplaced reports whether a reading taken after the window ending at
// until began shows a different reset: Codex reset the limits early, so that
// window is over. Right after a start a reading at 0% looks idle, which is why
// until is trusted otherwise; a reading of that same window reports until.
func windowReplaced(until time.Time, w *Window) bool {
	if w == nil || w.ResetAt.IsZero() {
		return false
	}
	start := until.Add(-windowLength(w))
	return w.ObservedAt.After(start.Add(windowSlack)) && w.ResetAt.Sub(until).Abs() > windowSlack
}

func windowLength(w *Window) time.Duration {
	if length := time.Duration(w.WindowMinutes) * time.Minute; length > 0 {
		return length
	}
	return shortWindow
}

// startOrder lists accounts for "all accounts" actions: the current account
// first, then the routing order, then by name.
func (e *Engine) startOrder() []string {
	ids := e.knownIDs()
	sort.SliceStable(ids, func(i, j int) bool {
		a, b := ids[i], ids[j]
		if (a == e.st.Active) != (b == e.st.Active) {
			return a == e.st.Active
		}
		if oa, ob := e.settings.OrderIndex(a), e.settings.OrderIndex(b); oa != ob {
			return oa < ob
		}
		return e.label(a) < e.label(b)
	})
	return ids
}

// PlanStarts schedules window starts for one account (account != "") or for
// all accounts: the first at now+delay, each next one stagger later. Pending
// one-off starts for those accounts are replaced; daily schedules are kept.
func (e *Engine) PlanStarts(account string, delay, stagger time.Duration) ([]StartPlan, error) {
	if delay < 0 || delay > 24*time.Hour || stagger < 0 || stagger > 12*time.Hour {
		return nil, ErrBadSchedule
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	var ids []string
	if account != "" {
		if !e.present(account, now) {
			return nil, ErrUnknownAccount
		}
		ids = []string{account}
	} else {
		ids = e.startOrder()
	}
	source := SourceNow
	switch {
	case stagger > 0 && len(ids) > 1:
		source = SourceStagger
	case delay > 0:
		source = SourceDelay
	}
	var plans []StartPlan
	for i, id := range ids {
		e.dropPlans(id, false)
		p := StartPlan{ID: e.planID(i), Account: id, At: now.Add(delay + time.Duration(i)*stagger), Source: source}
		e.st.Starts = append(e.st.Starts, p)
		plans = append(plans, p)
	}
	e.capPlans()
	var what string
	switch {
	case account != "" && delay > 0:
		what = fmt.Sprintf("%s's 5-hour window set to start %s (in %s)", e.label(account), clockText(now.Add(delay)), durText(delay))
	case account != "":
		what = fmt.Sprintf("Starting %s's 5-hour window now", e.label(account))
	case source == SourceStagger:
		what = fmt.Sprintf("Start 5-hour windows from %s, %s apart, for %d accounts", clockText(now.Add(delay)), durText(stagger), len(ids))
	case source == SourceDelay:
		what = fmt.Sprintf("Start 5-hour windows at %s for %d accounts", clockText(now.Add(delay)), len(ids))
	default:
		what = fmt.Sprintf("Start 5-hour windows now for %d accounts", len(ids))
	}
	e.addHistory("window", "", "", what)
	e.dirty = true
	return plans, nil
}

// PlanManual sets an account's window to start at a specific time, optionally
// every day. It replaces that account's other scheduled starts.
func (e *Engine) PlanManual(account string, at time.Time, daily bool) (StartPlan, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	if !e.present(account, now) {
		return StartPlan{}, ErrUnknownAccount
	}
	if at.Before(now.Add(-time.Minute)) || at.After(now.Add(8*24*time.Hour)) {
		return StartPlan{}, ErrBadSchedule
	}
	e.dropPlans(account, true)
	p := StartPlan{ID: e.planID(0), Account: account, At: at, Daily: daily, Source: SourceManual}
	e.st.Starts = append(e.st.Starts, p)
	e.capPlans()
	repeat := ""
	if daily {
		repeat = ", every day"
	}
	e.addHistory("window", "", account, fmt.Sprintf("%s's 5-hour window set to start %s%s (resets %s)",
		e.label(account), clockText(at), repeat, clockText(at.Add(shortWindow))))
	e.dirty = true
	return p, nil
}

// CancelStarts removes one plan (id), every plan of an account, or every
// plan when both are empty ("turn everything off"). It returns how many.
func (e *Engine) CancelStarts(id, account string) (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	kept := e.st.Starts[:0]
	n := 0
	for _, p := range e.st.Starts {
		if (id == "" && account == "") || (id != "" && p.ID == id) || (id == "" && p.Account == account) {
			n++
			continue
		}
		kept = append(kept, p)
	}
	e.st.Starts = kept
	if id != "" && n == 0 {
		return 0, ErrUnknownPlan
	}
	if n > 0 {
		switch {
		case id == "" && account == "":
			e.addHistory("window", "", "", fmt.Sprintf("All scheduled window starts turned off (%d removed)", n))
		case account != "":
			e.addHistory("window", "", account, "Scheduled window starts cancelled for "+e.label(account))
		default:
			e.addHistory("window", "", "", "A scheduled window start was cancelled")
		}
		e.dirty = true
	}
	return n, nil
}

// DueStarts removes and returns plans that are due. Daily plans are moved to
// their next day; plans overdue by too long are logged as missed. With
// auto-start on, accounts whose next window should start now are added.
func (e *Engine) DueStarts() []StartPlan {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	var due []StartPlan
	kept := e.st.Starts[:0]
	for _, p := range e.st.Starts {
		if p.At.After(now) {
			kept = append(kept, p)
			continue
		}
		if now.Sub(p.At) > startMissedAfter {
			e.logStart(StartResult{At: now, Account: p.Account, Status: StartMissed,
				Detail: "CLIProxyAPI was not running at " + clockText(p.At)})
		} else {
			due = append(due, p)
		}
		if p.Daily {
			next := p
			for !next.At.After(now) {
				next.At = next.At.Add(24 * time.Hour)
			}
			kept = append(kept, next)
		}
		e.dirty = true
	}
	e.st.Starts = kept
	if e.settings.AutoStartWindows {
		planned := map[string]bool{}
		for _, p := range due {
			planned[p.Account] = true
		}
		for _, id := range e.startOrder() {
			if at, ok := e.autoStartAt(id, now); ok && !at.After(now) && !planned[id] {
				// Hold off the next try now, so nothing can send twice.
				e.st.Accounts[id].autoRetryAt = now.Add(autoStartRetry)
				due = append(due, StartPlan{ID: e.planID(len(due)), Account: id, At: now, Source: SourceAuto})
			}
		}
	}
	return due
}

// NextStartAt returns when the next start falls due (a scheduled start, or
// an automatic one), or zero.
func (e *Engine) NextStartAt() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	var next time.Time
	for _, p := range e.st.Starts {
		if next.IsZero() || p.At.Before(next) {
			next = p.At
		}
	}
	if e.settings.AutoStartWindows {
		now := e.now()
		for _, id := range e.knownIDs() {
			if at, ok := e.autoStartAt(id, now); ok && (next.IsZero() || at.Before(next)) {
				next = at
			}
		}
	}
	return next
}

// autoStartAt is when auto-start should start an account's next 5-hour
// window: a minute after the last one ended (right away if it ended longer
// ago). ok is false for accounts it leaves alone: Off, blocked, unavailable
// or out of weekly quota; quota it can't read before sending (API keys); a
// window whose state is unknown; and an account with a scheduled start due
// before that window would end, so your own start times still decide.
func (e *Engine) autoStartAt(id string, now time.Time) (time.Time, bool) {
	a := e.st.Accounts[id]
	if a == nil || a.authIndex == "" || !e.usableAuto(id, now) {
		return time.Time{}, false
	}
	if a.Quota != nil && a.Quota.Long != nil {
		if r, _ := a.Quota.Long.Remaining(now); r <= 0 {
			return time.Time{}, false
		}
	}
	var ended time.Time
	switch state, reset := e.windowState(id, now); state {
	case WindowRunning:
		ended = reset
	case WindowIdle:
		ended = lastWindowEnd(a, now)
	default:
		return time.Time{}, false
	}
	at := a.autoRetryAt
	if !ended.IsZero() && ended.Add(autoStartDelay).After(at) {
		at = ended.Add(autoStartDelay)
	}
	from := at
	if from.Before(now) {
		from = now
	}
	for _, p := range e.st.Starts {
		if p.Account == id && p.At.Before(from.Add(shortWindow)) {
			return time.Time{}, false
		}
	}
	return at, true
}

// lastWindowEnd is when an idle account's last 5-hour window ended, or zero
// if that isn't known (for example after Codex reset the limits early).
func lastWindowEnd(a *Account, now time.Time) time.Time {
	var end time.Time
	if !a.WindowUntil.After(now) {
		end = a.WindowUntil
	}
	if a.Quota != nil && a.Quota.Short != nil {
		if r := a.Quota.Short.ResetAt; !r.After(now) && r.After(end) {
			end = r
		}
	}
	return end
}

// StartTarget returns what the service needs to start an account's window.
func (e *Engine) StartTarget(id string) (authIndex string, ok bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	a := e.st.Accounts[id]
	if a == nil || !e.present(id, e.now()) {
		return "", false
	}
	return a.authIndex, true
}

// UsedPercents returns the latest known used percentages (nil if unknown).
func (e *Engine) UsedPercents(id string) (short, long *float64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	a := e.st.Accounts[id]
	if a == nil || a.Quota == nil {
		return nil, nil
	}
	now := e.now()
	pick := func(w *Window) *float64 {
		if w == nil {
			return nil
		}
		r, _ := w.Remaining(now)
		v := 100 - r
		return &v
	}
	return pick(a.Quota.Short), pick(a.Quota.Long)
}

// RecordStart stores a start result and marks a started window as running.
func (e *Engine) RecordStart(r StartResult) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if a := e.st.Accounts[r.Account]; a != nil && r.Source == SourceAuto {
		switch r.Status {
		case StartRunning:
			// A request got there first and nothing was sent, so there is
			// nothing to report. The next try follows that window's reset.
			a.autoRetryAt = time.Time{}
			return
		case StartFailed:
			a.autoFails++
			a.autoRetryAt = e.now().Add(autoStartRetry << min(a.autoFails-1, 3))
		default:
			a.autoFails = 0
		}
	}
	if r.Status == StartStarted && !r.ResetAt.IsZero() {
		a := e.account(r.Account)
		if r.ResetAt.After(a.WindowUntil) {
			a.WindowUntil = r.ResetAt
		}
	}
	e.logStart(r)
	e.dirty = true
}

func (e *Engine) logStart(r StartResult) {
	e.st.StartLog = append(e.st.StartLog, r)
	if over := len(e.st.StartLog) - maxStartLog; over > 0 {
		e.st.StartLog = append([]StartResult(nil), e.st.StartLog[over:]...)
	}
	name := e.label(r.Account)
	if r.Source == SourceAuto {
		switch r.Status {
		case StartStarted:
			e.addHistory("window", "", r.Account, fmt.Sprintf("Auto-start: started %s's next 5-hour window (resets %s)", name, clockText(r.ResetAt)))
			return
		case StartFailed:
			retry := "soon"
			if a := e.st.Accounts[r.Account]; a != nil && a.autoRetryAt.After(e.now()) {
				retry = "in " + durText(a.autoRetryAt.Sub(e.now()).Round(time.Minute))
			}
			e.addHistory("fallback", "", r.Account, fmt.Sprintf("Auto-start could not start %s's 5-hour window: %s. It tries again %s", name, r.Detail, retry))
			return
		}
	}
	switch r.Status {
	case StartStarted:
		e.addHistory("window", "", r.Account, fmt.Sprintf("Started %s's 5-hour window (resets %s)", name, clockText(r.ResetAt)))
	case StartRunning:
		e.addHistory("window", "", r.Account, fmt.Sprintf("%s's 5-hour window was already running (resets %s); nothing sent", name, clockText(r.ResetAt)))
	case StartFailed:
		e.addHistory("fallback", "", r.Account, fmt.Sprintf("Could not start %s's 5-hour window: %s", name, r.Detail))
	case StartMissed:
		e.addHistory("fallback", "", r.Account, fmt.Sprintf("Missed %s's window start: %s", name, r.Detail))
	}
}

// dropPlans removes an account's pending plans (one-off only unless all).
func (e *Engine) dropPlans(account string, all bool) {
	kept := e.st.Starts[:0]
	for _, p := range e.st.Starts {
		if p.Account == account && (all || !p.Daily) {
			continue
		}
		kept = append(kept, p)
	}
	e.st.Starts = kept
}

func (e *Engine) capPlans() {
	if over := len(e.st.Starts) - maxStartPlans; over > 0 {
		e.st.Starts = append([]StartPlan(nil), e.st.Starts[over:]...)
	}
}

func (e *Engine) planID(i int) string {
	return strconv.FormatInt(e.now().UnixNano()+int64(i), 36)
}

func clockText(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return t.Local().Format("Mon 3:04 PM")
}

func durText(d time.Duration) string {
	if d >= time.Hour && d%time.Hour == 0 {
		return fmt.Sprintf("%d h", int(d.Hours()))
	}
	return fmt.Sprintf("%d min", int(d.Minutes()))
}
