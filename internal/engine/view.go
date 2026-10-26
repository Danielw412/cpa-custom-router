package engine

import (
	"fmt"
	"sort"
	"time"
)

// WindowView is a display-ready quota window.
type WindowView struct {
	RemainingPercent float64   `json:"remaining_percent"`
	Estimated        bool      `json:"estimated,omitempty"`
	WindowMinutes    int       `json:"window_minutes,omitempty"`
	ResetAt          time.Time `json:"reset_at,omitempty"`
}

// AccountView is one account row in the UI. It deliberately contains no
// credential material: only the auth file ID, email label, plan and quota.
type AccountView struct {
	ID           string      `json:"id"`
	Label        string      `json:"label"`
	Plan         string      `json:"plan,omitempty"`
	Role         string      `json:"role"`
	Order        int         `json:"order"`
	State        string      `json:"state"` // ok|low|exhausted|unavailable|unknown|off
	Detail       string      `json:"detail,omitempty"`
	Active       bool        `json:"active"`
	Pinned       bool        `json:"pinned"`
	Short        *WindowView `json:"short,omitempty"`
	Long         *WindowView `json:"long,omitempty"`
	ObservedAt   time.Time   `json:"observed_at,omitempty"`
	Source       string      `json:"source,omitempty"`
	Stale        bool        `json:"stale,omitempty"`
	BlockedUntil time.Time   `json:"blocked_until,omitempty"`
	PollError    string      `json:"poll_error,omitempty"`
	Sessions     int         `json:"sessions"`
	WarmSessions int         `json:"warm_sessions"`
	// BusySessions sent a request to this account in the last few minutes;
	// IdleSessions are bound here with a warm cache but are not working now.
	BusySessions int       `json:"busy_sessions"`
	IdleSessions int       `json:"idle_sessions"`
	LastUsed     time.Time `json:"last_used,omitempty"`
	// Window is the 5-hour window state: running, idle or unknown.
	Window        string      `json:"window"`
	WindowResetAt time.Time   `json:"window_reset_at,omitempty"`
	Plans         []StartPlan `json:"plans"`
	// AutoStartAt is when auto-start will start this account's next window
	// (now or earlier: about to); zero when auto-start is off or skips it.
	AutoStartAt time.Time `json:"auto_start_at,omitempty"`
}

// StarterView describes the window-start feature.
type StarterView struct {
	Model  string        `json:"model"`
	Effort string        `json:"effort"`
	Plans  []StartPlan   `json:"plans"`
	Log    []StartResult `json:"log"`
	// LastCost is the most recent start that actually sent a request.
	LastCost *StartResult `json:"last_cost,omitempty"`
}

// View is the full UI/API state.
type View struct {
	Now          time.Time        `json:"now"`
	Mode         string           `json:"mode"`
	Pinned       string           `json:"pinned,omitempty"`
	Active       string           `json:"active,omitempty"`
	ActiveManual bool             `json:"active_manual,omitempty"`
	ActiveReason string           `json:"active_reason,omitempty"`
	ActiveSince  time.Time        `json:"active_since,omitempty"`
	Routing      string           `json:"routing,omitempty"` // account new requests use right now
	RoutingNote  string           `json:"routing_note,omitempty"`
	Accounts     []AccountView    `json:"accounts"`
	History      []HistoryEntry   `json:"history"`
	Settings     Settings         `json:"settings"`
	Warnings     []string         `json:"warnings,omitempty"`
	Sessions     int              `json:"sessions"`
	WarmSessions int              `json:"warm_sessions"`
	Starter      StarterView      `json:"starter"`
	Picks        int64            `json:"picks"`
	PickStats    map[string]int64 `json:"pick_stats"`
	LastPoll     time.Time        `json:"last_poll,omitempty"`
	// Pause is set while new requests are held; SlowDownSeconds is the wait
	// added before each request (0 = off).
	Pause           *PauseView `json:"pause,omitempty"`
	SlowDownSeconds int        `json:"slow_down_seconds"`
}

// View builds the UI state. historyLimit<=0 returns all history (newest first).
func (e *Engine) View(historyLimit int) View {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	e.expirePause(now)
	warm := e.cacheWarm()

	sessions := map[string]int{}
	warmSessions := map[string]int{}
	busySessions := map[string]int{}
	idleSessions := map[string]int{}
	totalWarm := 0
	for _, b := range e.st.Families {
		sessions[b.Account]++
		idle := now.Sub(b.LastSeen)
		cold := e.idleFor(b, now) > warm // a pause does not cool a session
		if !cold {
			warmSessions[b.Account]++
			totalWarm++
		}
		switch {
		case idle <= busyWindow:
			// Count it where its requests actually go (a pinned websocket
			// can stay on another account than the session's binding).
			at := b.Last
			if at == "" {
				at = b.Account
			}
			busySessions[at]++
		case !cold:
			idleSessions[b.Account]++
		}
	}

	v := View{
		Now:             now,
		Mode:            e.st.Mode,
		Pinned:          e.st.Pinned,
		Active:          e.st.Active,
		ActiveManual:    e.st.ActiveManual,
		ActiveReason:    e.st.ActiveReason,
		ActiveSince:     e.st.ActiveSince,
		Settings:        e.settings,
		Warnings:        append([]string(nil), e.warnings...),
		Sessions:        len(e.st.Families),
		WarmSessions:    totalWarm,
		Picks:           e.picks,
		LastPoll:        e.lastPoll,
		Accounts:        []AccountView{},
		PickStats:       map[string]int64{},
		Pause:           e.pauseView(now),
		SlowDownSeconds: e.st.SlowDownSeconds,
	}
	for k, n := range e.stats {
		v.PickStats[k] = n
	}

	// Which account would a brand-new session use right now?
	switch {
	case e.st.Mode == ModeManual && e.st.Pinned != "":
		// Same rule as Pick: a pin is honoured whenever CLIProxyAPI offers the
		// account, even if the router believes its quota is exhausted.
		if e.hostOK(e.st.Pinned, now) {
			v.Routing = e.st.Pinned
			v.RoutingNote = "Pinned manually"
			if a := e.st.Accounts[e.st.Pinned]; a != nil && a.BlockedUntil.After(now) {
				v.RoutingNote = fmt.Sprintf("Pinned manually. Its quota looks exhausted until %s, so requests may fail until then", a.BlockedUntil.Local().Format("Mon 3:04 PM"))
			}
		} else if e.settings.ManualFallback == FallbackStrict {
			v.RoutingNote = "Pinned account unavailable — requests are rejected (strict)"
		} else if ranked := e.rank(e.knownIDs(), now, ""); len(ranked) > 0 {
			v.Routing = ranked[0]
			v.RoutingNote = "Pinned account unavailable — routing automatically until it returns"
		}
	default:
		v.Routing = e.st.Active
		v.RoutingNote = e.st.ActiveReason
	}

	stale := time.Duration(e.settings.StaleAfterMinutes) * time.Minute
	for _, id := range e.knownIDs() {
		a := e.st.Accounts[id]
		av := AccountView{
			ID:           id,
			Label:        e.label(id),
			Plan:         a.Plan,
			Role:         e.settings.Role(id),
			Order:        e.settings.OrderIndex(id),
			Active:       id == v.Routing,
			Pinned:       e.st.Mode == ModeManual && id == e.st.Pinned,
			PollError:    a.PollError,
			Sessions:     sessions[id],
			WarmSessions: warmSessions[id],
			BusySessions: busySessions[id],
			IdleSessions: idleSessions[id],
			LastUsed:     a.LastUsed,
		}
		if a.Quota != nil {
			av.ObservedAt = a.Quota.ObservedAt
			av.Source = a.Quota.Source
			av.Stale = !a.Quota.ObservedAt.IsZero() && now.Sub(a.Quota.ObservedAt) > stale
			av.Short = windowView(a.Quota.Short, now)
			av.Long = windowView(a.Quota.Long, now)
		}
		switch {
		case av.Role == RoleOff:
			av.State, av.Detail = "off", "Excluded from automatic routing"
		case a.BlockedUntil.After(now):
			av.State = "exhausted"
			av.BlockedUntil = a.BlockedUntil
			av.Detail = fmt.Sprintf("%s until %s", a.BlockReason, a.BlockedUntil.Local().Format("Mon 3:04 PM"))
		case !e.hostOK(id, now):
			av.State, av.Detail = "unavailable", "Unavailable in CLIProxyAPI"
			if a.hostDisabled {
				av.Detail = "Disabled in CLIProxyAPI"
			}
		case a.Quota == nil || !a.Quota.Known():
			av.State, av.Detail = "unknown", "Waiting for quota data"
		case e.low(id, now):
			r, _ := e.remaining(id, now)
			av.State, av.Detail = "low", fmt.Sprintf("%.0f%% left — below the %.0f%% switch threshold", r, e.settings.SwitchThresholdPercent)
		default:
			av.State = "ok"
		}
		av.Window, av.WindowResetAt = e.windowState(id, now)
		if e.settings.AutoStartWindows {
			if at, ok := e.autoStartAt(id, now); ok {
				av.AutoStartAt = at
				if at.Before(now) {
					av.AutoStartAt = now
				}
			}
		}
		av.Plans = []StartPlan{}
		for _, p := range e.st.Starts {
			if p.Account == id {
				av.Plans = append(av.Plans, p)
			}
		}
		sort.Slice(av.Plans, func(i, j int) bool { return av.Plans[i].At.Before(av.Plans[j].At) })
		v.Accounts = append(v.Accounts, av)
	}
	v.Starter = StarterView{
		Model:  e.settings.WindowStartModel,
		Effort: e.settings.WindowStartEffort,
		Plans:  append([]StartPlan{}, e.st.Starts...),
		Log:    []StartResult{},
	}
	sort.Slice(v.Starter.Plans, func(i, j int) bool { return v.Starter.Plans[i].At.Before(v.Starter.Plans[j].At) })
	for i := len(e.st.StartLog) - 1; i >= 0; i-- {
		r := e.st.StartLog[i]
		if len(v.Starter.Log) < 20 {
			v.Starter.Log = append(v.Starter.Log, r)
		}
		if v.Starter.LastCost == nil && r.Status == StartStarted && (r.InputTokens > 0 || r.OutputTokens > 0) {
			rc := r
			v.Starter.LastCost = &rc
		}
	}
	sort.SliceStable(v.Accounts, func(i, j int) bool {
		a, b := v.Accounts[i], v.Accounts[j]
		if a.Order != b.Order {
			return a.Order < b.Order
		}
		return a.Label < b.Label
	})

	n := len(e.st.History)
	limit := n
	if historyLimit > 0 && historyLimit < n {
		limit = historyLimit
	}
	v.History = make([]HistoryEntry, 0, limit)
	for i := n - 1; i >= 0 && len(v.History) < limit; i-- {
		v.History = append(v.History, e.st.History[i])
	}
	return v
}

func windowView(w *Window, now time.Time) *WindowView {
	if w == nil {
		return nil
	}
	r, est := w.Remaining(now)
	wv := &WindowView{RemainingPercent: r, Estimated: est, WindowMinutes: w.WindowMinutes}
	if w.ResetAt.After(now) {
		wv.ResetAt = w.ResetAt
	}
	return wv
}
