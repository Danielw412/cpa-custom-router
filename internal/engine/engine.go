// Package engine implements quota-aware, cache-preserving account routing for
// Codex credentials. It is pure Go (no cgo) so it can be tested directly; the
// plugin's C ABI layer only translates host calls into engine calls.
package engine

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	ModeAuto   = "auto"
	ModeManual = "manual"

	providerCodex = "codex"

	// detourGrace is how long a session's account may be missing from the
	// host's candidate list (cooldown, retry exclusion) before the session is
	// rebound. Shorter gaps are treated as transient: the request detours but
	// the session keeps its binding (and its prompt cache).
	detourGrace = 3 * time.Minute
	// Ranking tolerances: weekly resets closer together than weeklyTie, or
	// remaining quota within remainingTie points, count as equal so the next
	// criterion decides. Switching back for an earlier 5-hour reset needs at
	// least fiveHourLead.
	weeklyTie = 24 * time.Hour
	// busyWindow: a session that sent a request this recently counts as
	// working now; otherwise it is idle (its cache may still be warm).
	busyWindow   = 5 * time.Minute
	remainingTie = 5.0
	fiveHourLead = 30 * time.Minute
	// familyTTL bounds memory for idle sessions.
	familyTTL   = 24 * time.Hour
	maxFamilies = 4000
	// defaultBlock is used when a quota failure carries no reset time.
	defaultBlock = 15 * time.Minute
	// maxLinks bounds remembered child->parent session links.
	maxLinks = 20000
	// historyCoalesce merges repeated identical events (e.g. several sessions
	// migrating for the same reason) into one entry with a count.
	historyCoalesce = 2 * time.Minute
	// fallbackNoteEvery rate-limits "pinned account unavailable" notes.
	fallbackNoteEvery = 5 * time.Minute
	// provisionalWindow bounds how long an initial choice may be re-ranked.
	provisionalWindow = 15 * time.Minute
)

// Candidate is one credential offered by the host for a request.
type Candidate struct {
	ID       string
	Provider string
	Priority int
	Status   string
}

// PickRequest is the routing context for one scheduler pick.
type PickRequest struct {
	Candidates []Candidate
	Headers    map[string][]string
	Metadata   map[string]any
	Model      string
}

// PickResult is the engine's decision. Handled=false defers to CLIProxyAPI's
// built-in selector (including its own session affinity).
type PickResult struct {
	Handled      bool
	AuthID       string
	Reject       bool
	RejectCode   string
	RejectReason string
	Reason       string
}

// HostAccount is a credential as reported by host.auth.list.
type HostAccount struct {
	ID          string
	AuthIndex   string
	Provider    string
	Label       string
	Email       string
	Plan        string
	Disabled    bool
	Unavailable bool
	Status      string
	NextRetry   time.Time
}

// UsageObservation is the subset of a CLIProxyAPI usage record the engine needs.
type UsageObservation struct {
	AuthID   string
	Provider string
	Model    string
	At       time.Time
	Headers  map[string][]string
	Failed   bool
	Status   int
	Body     string
}

// Account is the engine's view of one Codex credential.
type Account struct {
	ID              string    `json:"id"`
	Label           string    `json:"label,omitempty"`
	Plan            string    `json:"plan,omitempty"`
	Quota           *Quota    `json:"quota,omitempty"`
	BlockedUntil    time.Time `json:"blocked_until,omitempty"`
	BlockReason     string    `json:"block_reason,omitempty"`
	Down            bool      `json:"down,omitempty"` // last logged health transition
	LastPolled      time.Time `json:"last_polled,omitempty"`
	PollError       string    `json:"poll_error,omitempty"`
	authIndex       string
	hostDisabled    bool
	hostUnavailable bool
	hostNextRetry   time.Time
	hostStatus      string
	inRoster        bool
	unavailSince    time.Time
	// EverInRoster: the account came from CLIProxyAPI's auth-file roster
	// (persisted, so a deletion is noticed right after a restart).
	EverInRoster bool `json:"roster,omitempty"`
	lastSeen     time.Time
	forcePoll    bool
	// ModelBlocks holds per-model usage limits (e.g. a model with its own
	// quota) that must not take the whole account out of rotation.
	ModelBlocks map[string]time.Time `json:"model_blocks,omitempty"`
	// WindowUntil: the 5-hour window is known to be running until then
	// (set by real traffic or by a window start).
	WindowUntil time.Time `json:"window_until,omitempty"`
	// LastUsed is when the account last served a successful request.
	LastUsed time.Time `json:"last_used,omitempty"`
}

// Binding pins a session family (a main agent plus its subagents/forks) to an account.
type Binding struct {
	Account     string    `json:"account"`
	BoundAt     time.Time `json:"bound_at"`
	LastSeen    time.Time `json:"last_seen"`
	Requests    int64     `json:"requests"`
	DetourSince time.Time `json:"detour_since,omitempty"`
	// Last is the account the session's most recent request went to. It
	// differs from Account while a request detours or a websocket stays
	// pinned elsewhere.
	Last string `json:"last,omitempty"`
}

// Link records a child session's parent so subagents resolve to their family root.
type Link struct {
	Parent string    `json:"parent"`
	Seen   time.Time `json:"seen"`
}

// HistoryEntry is one routing event shown in the UI.
type HistoryEntry struct {
	At     time.Time `json:"at"`
	Kind   string    `json:"kind"`
	From   string    `json:"from,omitempty"`
	To     string    `json:"to,omitempty"`
	Reason string    `json:"reason"`
	Count  int       `json:"count,omitempty"`
}

// State is everything persisted across restarts and plugin upgrades.
type State struct {
	Version      int       `json:"version"`
	Mode         string    `json:"mode"`
	Pinned       string    `json:"pinned,omitempty"`
	Active       string    `json:"active,omitempty"`
	ActiveManual bool      `json:"active_manual,omitempty"`
	ActiveReason string    `json:"active_reason,omitempty"`
	ActiveSince  time.Time `json:"active_since,omitempty"`
	// ProvisionalUntil marks an initial choice made before every account's
	// quota was known; until then it may be re-ranked once clearly better
	// data arrives. The deadline never extends.
	ProvisionalUntil time.Time           `json:"provisional_until,omitempty"`
	LastSwitch       time.Time           `json:"last_switch,omitempty"`
	History          []HistoryEntry      `json:"history"`
	Families         map[string]*Binding `json:"families"`
	Links            map[string]Link     `json:"links"`
	Accounts         map[string]*Account `json:"accounts"`
	Starts           []StartPlan         `json:"starts,omitempty"`
	StartLog         []StartResult       `json:"start_log,omitempty"`
	// Pause, when set, holds new requests at the router (see pause.go).
	Pause *Pause `json:"pause,omitempty"`
	// SlowDownSeconds makes every request wait this long before it is sent.
	SlowDownSeconds int `json:"slow_down_seconds,omitempty"`
}

// Engine is safe for concurrent use.
type Engine struct {
	mu             sync.Mutex
	now            func() time.Time
	settings       Settings
	warnings       []string
	st             State
	dirty          bool
	lastNote       time.Time
	picks          int64
	lastPoll       time.Time
	listening      bool // true once a host roster has been received
	listeningSince time.Time
	pending        []HistoryEntry
	stats          map[string]int64
	pickModel      string // model of the pick being decided (under mu)
}

// New creates an engine with the given settings.
func New(settings Settings, warnings []string, clock func() time.Time) *Engine {
	if clock == nil {
		clock = time.Now
	}
	e := &Engine{now: clock, settings: settings, warnings: warnings}
	e.st = emptyState()
	return e
}

func emptyState() State {
	return State{
		Version:  1,
		Mode:     ModeAuto,
		Families: map[string]*Binding{},
		Links:    map[string]Link{},
		Accounts: map[string]*Account{},
	}
}

// Configure applies new settings (from plugin.reconfigure).
func (e *Engine) Configure(settings Settings, warnings []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	old := e.settings
	e.settings = settings
	e.warnings = warnings
	e.trimHistory()
	if !settingsEqual(old, settings) {
		e.addHistory("settings", "", "", "Routing preferences updated")
	}
	e.dirty = true
}

// Settings returns the effective settings and any normalization warnings.
func (e *Engine) Settings() (Settings, []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.settings, append([]string(nil), e.warnings...)
}

// Restore loads persisted state (on startup or after a plugin hot upgrade).
func (e *Engine) Restore(st State) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if st.Families == nil {
		st.Families = map[string]*Binding{}
	}
	if st.Links == nil {
		st.Links = map[string]Link{}
	}
	if st.Accounts == nil {
		st.Accounts = map[string]*Account{}
	}
	if st.Mode != ModeManual {
		st.Mode = ModeAuto
	}
	for id, a := range st.Accounts {
		if a == nil {
			delete(st.Accounts, id)
			continue
		}
		a.ID = id
	}
	if st.Pause != nil && st.Pause.Since.IsZero() {
		st.Pause = nil
	}
	if st.Pause != nil && (st.Pause.AutoResumeMinutes < 0 || st.Pause.AutoResumeMinutes > MaxAutoResumeMinutes) {
		st.Pause.AutoResumeMinutes = 0
	}
	if st.SlowDownSeconds < 0 || st.SlowDownSeconds > MaxSlowDownSeconds {
		st.SlowDownSeconds = 0
	}
	st.Version = 1
	e.st = st
	e.trimHistory()
}

// Export returns a deep-enough copy of state for persistence and whether it
// changed since the last export.
func (e *Engine) Export() (State, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	dirty := e.dirty
	e.dirty = false
	st := e.st
	st.History = append([]HistoryEntry(nil), e.st.History...)
	st.Families = make(map[string]*Binding, len(e.st.Families))
	for k, v := range e.st.Families {
		b := *v
		st.Families[k] = &b
	}
	st.Links = make(map[string]Link, len(e.st.Links))
	for k, v := range e.st.Links {
		st.Links[k] = v
	}
	st.Accounts = make(map[string]*Account, len(e.st.Accounts))
	for k, v := range e.st.Accounts {
		a := *v
		if v.Quota != nil {
			q := *v.Quota
			a.Quota = &q
		}
		if v.ModelBlocks != nil {
			a.ModelBlocks = make(map[string]time.Time, len(v.ModelBlocks))
			for m, t := range v.ModelBlocks {
				a.ModelBlocks[m] = t
			}
		}
		st.Accounts[k] = &a
	}
	st.Starts = append([]StartPlan(nil), e.st.Starts...)
	st.StartLog = append([]StartResult(nil), e.st.StartLog...)
	if e.st.Pause != nil {
		p := *e.st.Pause
		st.Pause = &p
	}
	return st, dirty
}

// MarkDirty forces the next Export to report a change.
func (e *Engine) MarkDirty() {
	e.mu.Lock()
	e.dirty = true
	e.mu.Unlock()
}

// ---------------------------------------------------------------------------
// Scheduler pick
// ---------------------------------------------------------------------------

// Pick chooses the credential for one request.
func (e *Engine) Pick(req PickRequest) PickResult {
	codex := make([]string, 0, len(req.Candidates))
	others := 0
	for _, c := range req.Candidates {
		if strings.EqualFold(strings.TrimSpace(c.Provider), providerCodex) && c.ID != "" {
			codex = append(codex, c.ID)
		} else {
			others++
		}
	}
	if len(codex) == 0 {
		return PickResult{Reason: "no codex candidates"}
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	res := e.pickLocked(req, codex, others)
	if e.stats == nil {
		e.stats = map[string]int64{}
	}
	e.stats[statKey(res)]++
	return res
}

func statKey(r PickResult) string {
	switch {
	case r.Reject:
		return "rejected"
	case !r.Handled:
		return "deferred"
	}
	for _, k := range []struct{ substr, key string }{
		{"websocket pinned", "websocket_pinned"}, {"session affinity", "affinity"}, {"new session", "new_session"},
		{"migrated", "migrated"}, {"detour", "detour"}, {"manual pin", "manual"}, {"cold session", "rejoined"},
		{"below threshold but", "low_no_alternative"},
	} {
		if strings.Contains(r.Reason, k.substr) {
			return k.key
		}
	}
	return "other"
}

func (e *Engine) pickLocked(req PickRequest, codex []string, others int) (res PickResult) {
	now := e.now()
	e.picks++
	e.pickModel = normalizeModel(req.Model)
	defer func() { e.pickModel = "" }()
	avail := make(map[string]bool, len(codex))
	for _, id := range codex {
		a := e.account(id)
		a.lastSeen = now
		avail[id] = true
	}

	family := ""
	if ref, ok := ExtractSession(req.Headers, req.Metadata); ok {
		family = e.familyRoot(ref, now)
	}
	defer func() {
		if b := e.binding(family); b != nil && res.Handled && !res.Reject && res.AuthID != "" {
			b.Last = res.AuthID
		}
	}()

	// A Responses websocket that already committed to an account is pinned by
	// CLIProxyAPI; switching it would force a replay, so it always continues.
	if pinned := metaString(req.Metadata, "pinned_auth_id"); pinned != "" {
		if avail[pinned] {
			e.touchFamily(family, now)
			return PickResult{Handled: true, AuthID: pinned, Reason: "websocket pinned"}
		}
		return PickResult{Reason: "pinned auth unavailable"}
	}

	if e.st.Mode == ModeManual && e.st.Pinned != "" {
		pin := e.st.Pinned
		if avail[pin] {
			e.bindFamily(family, pin, now, "migrate", "pinned account")
			return PickResult{Handled: true, AuthID: pin, Reason: "manual pin"}
		}
		if e.settings.ManualFallback == FallbackStrict {
			if others > 0 {
				return PickResult{Reason: "pinned account unavailable; deferring non-codex route to built-in selector"}
			}
			return PickResult{
				Handled: true, Reject: true, RejectCode: "quota_router_pinned_unavailable",
				RejectReason: fmt.Sprintf("custom-quota-router: pinned account %s is unavailable (manual mode, strict fallback)", e.label(pin)),
			}
		}
		if now.Sub(e.lastNote) > fallbackNoteEvery {
			e.lastNote = now
			e.addHistory("fallback", pin, "", fmt.Sprintf("Pinned account %s is unavailable; routing automatically until it returns", e.label(pin)))
		}
	} else {
		e.evaluateActive(now)
	}

	target, reason := e.decide(family, avail, now)
	if target != "" {
		return PickResult{Handled: true, AuthID: target, Reason: reason}
	}
	// Nothing passes the router's own checks (e.g. every offered account is
	// marked exhausted). The host still offers them, so never reject on the
	// router's say-so: try the least-bad account the user has not switched Off.
	if t := e.lenientPick(family, avail, now); t != "" {
		return PickResult{Handled: true, AuthID: t, Reason: "lenient: every offered account looks exhausted"}
	}
	if others > 0 {
		return PickResult{Reason: "no routable codex account; deferring to built-in selector"}
	}
	return PickResult{
		Handled: true, Reject: true, RejectCode: "quota_router_no_account",
		RejectReason: "custom-quota-router: every available Codex account is set to Off for automatic routing",
	}
}

// lenientPick chooses among offered accounts that are not switched Off,
// ignoring the router's own quota blocks: the session's bound account if
// possible, otherwise the one with the most remaining quota.
func (e *Engine) lenientPick(family string, avail map[string]bool, now time.Time) string {
	if b := e.binding(family); b != nil && avail[b.Account] && e.settings.Role(b.Account) != RoleOff {
		return b.Account
	}
	best, bestRem := "", -1.0
	for _, id := range keys(avail) {
		if e.settings.Role(id) == RoleOff {
			continue
		}
		if r := e.mustRemaining(id, now); r > bestRem {
			best, bestRem = id, r
		}
	}
	return best
}

// normalizeModel strips thinking suffixes so pick and usage model names match.
func normalizeModel(m string) string {
	m = strings.ToLower(strings.TrimSpace(m))
	if i := strings.IndexByte(m, '('); i > 0 {
		m = m[:i]
	}
	return strings.TrimSpace(m)
}

// decide picks an account for a family given the request's available set.
func (e *Engine) decide(family string, avail map[string]bool, now time.Time) (string, string) {
	active := e.st.Active
	if e.st.Mode == ModeManual {
		// Manual mode with the pin unavailable: behave like auto but never
		// move the global active account.
		active = ""
	}
	usable := func(id string) bool { return e.usableAuto(id, now) && !e.modelBlocked(id, now) }
	rankReq := func(exclude string) []string {
		var out []string
		for _, id := range e.rank(keys(avail), now, exclude) {
			if !e.modelBlocked(id, now) {
				out = append(out, id)
			}
		}
		return out
	}
	fallback := func() string {
		if active != "" && avail[active] && usable(active) {
			return active
		}
		if ranked := rankReq(""); len(ranked) > 0 {
			return ranked[0]
		}
		return ""
	}

	b := e.binding(family)
	if b == nil {
		t := fallback()
		if t != "" && family != "" {
			e.st.Families[family] = &Binding{Account: t, BoundAt: now, LastSeen: now, Requests: 1}
			e.dirty = true
		}
		return t, "new session → current account"
	}

	x := b.Account
	idle := e.idleFor(b, now)
	b.LastSeen = now
	b.Requests++
	switch {
	case avail[x] && usable(x) && !e.low(x, now):
		b.DetourSince = time.Time{}
		// A session idle past the cache window has nothing left to preserve,
		// so it rejoins the current account instead of lingering elsewhere.
		if idle > e.cacheWarm() && active != "" && active != x && avail[active] &&
			usable(active) && !e.low(active, now) {
			e.rebind(family, b, active, now, "rejoin", "idle long enough that its cache expired")
			return active, "cold session joined current account"
		}
		return x, "session affinity"
	case avail[x] && usable(x) && e.low(x, now):
		b.DetourSince = time.Time{}
		ranked := rankReq(x)
		if len(ranked) > 0 && !e.low(ranked[0], now) {
			to := ranked[0]
			if active != "" && avail[active] && usable(active) && !e.low(active, now) {
				to = active
			}
			e.rebind(family, b, to, now, "migrate", fmt.Sprintf("below %.0f%% remaining", e.settings.SwitchThresholdPercent))
			return to, "migrated: below threshold"
		}
		return x, "below threshold but no healthier account"
	default:
		// x is unavailable for this request, blocked, or excluded by the user.
		blocked := e.isBlocked(x, now)
		off := e.settings.Role(x) == RoleOff
		t := fallback()
		if t == "" {
			return "", ""
		}
		// Quota exhaustion, an Off role, removal from CLIProxyAPI or a long
		// outage move the session now. A model-specific limit or a short host
		// cooldown/retry exclusion only detours this request: the binding (and
		// its cache) survives for detourGrace.
		if blocked || off || !e.hostEnabled(x, now) || e.hostDownLong(x, now) {
			why := "unavailable in CLIProxyAPI"
			switch {
			case off:
				why = "set to Off for automatic routing"
			case !e.hostEnabled(x, now):
				why = "removed or disabled in CLIProxyAPI"
			case blocked:
				why = "quota exhausted"
				if a := e.st.Accounts[x]; a != nil && a.BlockReason != "" {
					why = a.BlockReason
				}
			}
			e.rebind(family, b, t, now, "migrate", why)
			return t, "migrated: " + why
		}
		if b.DetourSince.IsZero() {
			b.DetourSince = now
		}
		if now.Sub(b.DetourSince) >= detourGrace {
			e.rebind(family, b, t, now, "migrate", fmt.Sprintf("unavailable for over %d minutes", int(detourGrace.Minutes())))
			return t, "migrated: unavailable"
		}
		return t, "temporary detour (binding kept)"
	}
}

// touchFamily records activity on a session without changing its binding.
func (e *Engine) touchFamily(family string, now time.Time) {
	if b := e.binding(family); b != nil {
		b.LastSeen = now
		b.Requests++
	}
}

func (e *Engine) bindFamily(family, account string, now time.Time, kind, reason string) {
	if family == "" {
		return
	}
	b := e.binding(family)
	if b == nil {
		e.st.Families[family] = &Binding{Account: account, BoundAt: now, LastSeen: now, Requests: 1}
		e.dirty = true
		return
	}
	b.LastSeen = now
	b.Requests++
	if b.Account != account {
		e.rebind(family, b, account, now, kind, reason)
	}
}

func (e *Engine) rebind(family string, b *Binding, to string, now time.Time, kind, reason string) {
	if b == nil || b.Account == to {
		return
	}
	from := b.Account
	b.Account = to
	b.BoundAt = now
	b.DetourSince = time.Time{}
	e.dirty = true
	e.addHistory(kind, from, to, fmt.Sprintf("Session moved from %s to %s (%s)", e.label(from), e.label(to), reason))
}

func (e *Engine) binding(family string) *Binding {
	if family == "" {
		return nil
	}
	return e.st.Families[family]
}

// familyRoot records parent links and resolves a session to its family root.
func (e *Engine) familyRoot(ref SessionRef, now time.Time) string {
	if ref.ParentID != "" && ref.ParentID != ref.ID {
		if l, ok := e.st.Links[ref.ID]; !ok || l.Parent != ref.ParentID || now.Sub(l.Seen) > time.Minute {
			e.st.Links[ref.ID] = Link{Parent: ref.ParentID, Seen: now}
			e.dirty = true
		}
	}
	id := ref.ID
	seen := map[string]bool{id: true}
	for i := 0; i < 16; i++ {
		l, ok := e.st.Links[id]
		if !ok || l.Parent == "" || seen[l.Parent] {
			break
		}
		id = l.Parent
		seen[id] = true
	}
	// A child seen before its family root was bound inherits nothing yet;
	// whichever member arrives first binds the family.
	return id
}

// ---------------------------------------------------------------------------
// Active-account policy
// ---------------------------------------------------------------------------

// evaluateActive keeps the global "current account" sticky: it only changes
// on emergencies (exhausted, unavailable, below threshold) or, after the
// minimum dwell time, when a clearly preferable account has recovered.
func (e *Engine) evaluateActive(now time.Time) {
	if e.st.Mode == ModeManual {
		return
	}
	all := e.knownIDs()
	cur := e.st.Active
	if cur != "" {
		if _, ok := e.st.Accounts[cur]; !ok {
			cur = ""
		}
	}
	if cur == "" {
		ranked := e.rank(all, now, "")
		if len(ranked) > 0 {
			e.setActive(ranked[0], false, now, "switch", "Started with "+e.describeChoice(ranked[0], now))
			if e.anyUnknown(all, now) {
				e.st.ProvisionalUntil = now.Add(provisionalWindow)
			}
		}
		return
	}
	if !e.st.ProvisionalUntil.IsZero() {
		if !e.st.ActiveManual {
			if ranked := e.rank(all, now, ""); len(ranked) > 0 && ranked[0] != cur && e.clearlyBetter(ranked[0], cur, now) {
				until := e.st.ProvisionalUntil
				e.setActive(ranked[0], false, now, "switch", "Started with "+e.describeChoice(ranked[0], now)+" once more quota data arrived")
				e.st.ProvisionalUntil = until
				cur = ranked[0]
			}
		}
		if now.After(e.st.ProvisionalUntil) || !e.anyUnknown(all, now) {
			e.st.ProvisionalUntil = time.Time{}
			e.dirty = true
		}
	}

	emergency := ""
	switch {
	case e.settings.Role(cur) == RoleOff:
		emergency = "disabled for automatic routing"
	case e.isBlocked(cur, now):
		emergency = "quota exhausted"
		if a := e.st.Accounts[cur]; a != nil && a.BlockReason != "" {
			emergency = a.BlockReason
		}
	case !e.hostEnabled(cur, now):
		emergency = "removed or disabled in CLIProxyAPI"
	case e.hostDownLong(cur, now):
		emergency = "unavailable in CLIProxyAPI for several minutes"
	case e.low(cur, now):
		r, _ := e.remaining(cur, now)
		emergency = fmt.Sprintf("%.0f%% remaining, below the %.0f%% threshold", r, e.settings.SwitchThresholdPercent)
	}
	if emergency != "" {
		ranked := e.rank(all, now, cur)
		if len(ranked) == 0 {
			return
		}
		next := ranked[0]
		// Only trade a low-but-usable account for one that is actually better.
		if e.usableAuto(cur, now) && e.low(next, now) {
			if r1, _ := e.remaining(next, now); r1 <= e.mustRemaining(cur, now) {
				return
			}
		}
		e.setActive(next, false, now, "switch", fmt.Sprintf("Switched from %s to %s: %s", e.label(cur), e.describeChoice(next, now), emergency))
		return
	}

	if !e.settings.SwitchBack || e.st.ActiveManual {
		return
	}
	if now.Sub(e.st.LastSwitch) < time.Duration(e.settings.MinDwellMinutes)*time.Minute {
		return
	}
	ranked := e.rank(all, now, "")
	if len(ranked) == 0 || ranked[0] == cur {
		return
	}
	best := ranked[0]
	if !e.strong(best, now) {
		return
	}
	if ok, why := e.preferredOver(best, cur, now); ok {
		e.setActive(best, false, now, "switch", why)
	}
}

func (e *Engine) setActive(id string, manual bool, now time.Time, kind, reason string) {
	from := e.st.Active
	if from == id && e.st.ActiveManual == manual {
		return
	}
	e.st.Active = id
	e.st.ActiveManual = manual
	e.st.ActiveReason = reason
	e.st.ActiveSince = now
	e.st.LastSwitch = now
	e.dirty = true
	if from != id {
		e.addHistory(kind, from, id, reason)
	}
}

// preferredOver reports whether switching the current account to best is
// worth it under the user's ranking. The first criterion on which the two
// differ decides; the switch happens only if best wins it by a clear margin.
// More quota left is not use-it-or-lose-it, so it only moves the account when
// ranked first; lower down it just breaks ties. Only new and cache-cold
// sessions follow the active account, so warm sessions keep their cache.
func (e *Engine) preferredOver(best, cur string, now time.Time) (bool, string) {
	rb, rc := e.roleRank(best), e.roleRank(cur)
	if rb < rc {
		return true, fmt.Sprintf("Switched back to %s: it recovered, and %s is a reserve account", e.describeChoice(best, now), e.label(cur))
	}
	if rb > rc {
		return false, ""
	}
	b, c := e.rankItem(best, now), e.rankItem(cur, now)
	for i, crit := range e.settings.RankBy {
		if rankTied(crit, b, c) {
			continue
		}
		if rankCompare(crit, b, c) > 0 {
			return false, ""
		}
		switch crit {
		case RankAccountOrder:
			return true, fmt.Sprintf("Switched back to %s: it recovered and is above %s in your order", e.describeChoice(best, now), e.label(cur))
		case RankWeeklyReset:
			if c.weekly.IsZero() {
				return false, ""
			}
			return true, fmt.Sprintf("Switched to %s: its weekly quota resets sooner (%s), so it is used before it expires", e.describeChoice(best, now), b.weekly.Local().Format("Mon 3:04 PM"))
		case RankFiveHourReset:
			if !c.window.IsZero() && c.window.Sub(b.window) < fiveHourLead {
				return false, ""
			}
			return true, fmt.Sprintf("Switched to %s: its 5-hour window resets sooner (%s), so its headroom is used before it expires", e.describeChoice(best, now), b.window.Local().Format("3:04 PM"))
		case RankMostRemaining:
			if i > 0 || b.rem-c.rem < e.settings.SwitchBackMarginPercent {
				return false, ""
			}
			return true, fmt.Sprintf("Switched to %s: much more quota left than %s (%.0f%%)", e.describeChoice(best, now), e.label(cur), c.rem)
		}
		return false, ""
	}
	return false, ""
}

// ---------------------------------------------------------------------------
// Account health and ranking
// ---------------------------------------------------------------------------

func (e *Engine) account(id string) *Account {
	a := e.st.Accounts[id]
	if a == nil {
		a = &Account{ID: id, Label: labelFromID(id)}
		e.st.Accounts[id] = a
		e.dirty = true
	}
	return a
}

func (e *Engine) label(id string) string {
	if id == "" {
		return "—"
	}
	if a := e.st.Accounts[id]; a != nil && a.Label != "" {
		return a.Label
	}
	return labelFromID(id)
}

func (e *Engine) knownIDs() []string {
	now := e.now()
	ids := make([]string, 0, len(e.st.Accounts))
	for id := range e.st.Accounts {
		if e.present(id, now) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// present: the account still exists in CLIProxyAPI. Auth-file accounts must
// be in the latest roster; credentials that never appear in the roster (API
// keys) count as present while recently offered as candidates.
func (e *Engine) present(id string, now time.Time) bool {
	a := e.st.Accounts[id]
	if a == nil {
		return false
	}
	if !e.listening {
		return true
	}
	if a.EverInRoster {
		return a.inRoster
	}
	seen := a.lastSeen
	if e.listeningSince.After(seen) {
		seen = e.listeningSince // grace after a restart or upgrade
	}
	return now.Sub(seen) <= time.Hour
}

// hostEnabled: present and not disabled in CLIProxyAPI.
func (e *Engine) hostEnabled(id string, now time.Time) bool {
	a := e.st.Accounts[id]
	return a != nil && e.present(id, now) && !a.hostDisabled
}

// hostDownLong: CLIProxyAPI has kept the account unavailable (cooldown) for
// longer than the detour grace and has not offered it recently.
func (e *Engine) hostDownLong(id string, now time.Time) bool {
	a := e.st.Accounts[id]
	if a == nil || !a.hostUnavailable || a.unavailSince.IsZero() {
		return false
	}
	if !a.hostNextRetry.IsZero() && !a.hostNextRetry.After(now) {
		return false
	}
	return now.Sub(a.unavailSince) >= detourGrace && now.Sub(a.lastSeen) >= time.Minute
}

// modelBlocked: the account hit a usage limit specific to the current model.
func (e *Engine) modelBlocked(id string, now time.Time) bool {
	a := e.st.Accounts[id]
	if a == nil || e.pickModel == "" || len(a.ModelBlocks) == 0 {
		return false
	}
	return a.ModelBlocks[e.pickModel].After(now)
}

func (e *Engine) isBlocked(id string, now time.Time) bool {
	a := e.st.Accounts[id]
	return a != nil && a.BlockedUntil.After(now)
}

func (e *Engine) hostOK(id string, now time.Time) bool {
	return e.hostEnabled(id, now) && !e.hostDownLong(id, now)
}

func (e *Engine) remaining(id string, now time.Time) (float64, bool) {
	a := e.st.Accounts[id]
	if a == nil {
		return 0, false
	}
	return a.Quota.MinRemaining(now)
}

// mustRemaining treats unknown quota as full for comparisons.
func (e *Engine) mustRemaining(id string, now time.Time) float64 {
	if r, ok := e.remaining(id, now); ok {
		return r
	}
	return 100
}

func (e *Engine) low(id string, now time.Time) bool {
	r, ok := e.remaining(id, now)
	return ok && r < e.settings.SwitchThresholdPercent
}

// strong: comfortably above the threshold (threshold + switch-back margin).
func (e *Engine) strong(id string, now time.Time) bool {
	r, ok := e.remaining(id, now)
	return ok && r >= e.settings.SwitchThresholdPercent+e.settings.SwitchBackMarginPercent
}

// anyUnknown reports whether a routable account has no quota data yet.
func (e *Engine) anyUnknown(ids []string, now time.Time) bool {
	for _, id := range ids {
		if a := e.st.Accounts[id]; a != nil && e.usableAuto(id, now) && !a.Quota.Known() {
			return true
		}
	}
	return false
}

func (e *Engine) usableAuto(id string, now time.Time) bool {
	return e.settings.Role(id) != RoleOff && !e.isBlocked(id, now) && e.hostOK(id, now)
}

func (e *Engine) roleRank(id string) int {
	if e.settings.Role(id) == RoleReserve {
		return 1
	}
	return 0
}

func (e *Engine) longReset(id string, now time.Time) time.Time {
	a := e.st.Accounts[id]
	if a == nil || a.Quota == nil || a.Quota.Long == nil {
		return time.Time{}
	}
	if a.Quota.Long.ResetAt.After(now) {
		return a.Quota.Long.ResetAt
	}
	return time.Time{}
}

// windowReset is when the account's running 5-hour window resets, or zero if
// no window is running (an idle window loses nothing while it waits).
func (e *Engine) windowReset(id string, now time.Time) time.Time {
	if st, at := e.windowState(id, now); st == WindowRunning {
		return at
	}
	return time.Time{}
}

// rankItem is what the ranking criteria compare.
type rankItem struct {
	id     string
	tier   int
	order  int
	rem    float64
	weekly time.Time // weekly reset; zero if unknown
	window time.Time // running 5-hour window's reset; zero if none is running
	label  string
}

func (e *Engine) rankItem(id string, now time.Time) rankItem {
	return rankItem{id: id, tier: e.tier(id, now), order: e.settings.OrderIndex(id), rem: e.mustRemaining(id, now),
		weekly: e.longReset(id, now), window: e.windowReset(id, now), label: e.label(id)}
}

// soonerFirst orders known times before unknown (zero) ones, earlier first.
func soonerFirst(a, b time.Time) int {
	switch {
	case a.IsZero() && b.IsZero():
		return 0
	case a.IsZero():
		return 1
	case b.IsZero():
		return -1
	}
	return a.Compare(b)
}

// rankCompare orders two accounts on one criterion: negative if a is better.
func rankCompare(crit string, a, b rankItem) int {
	switch crit {
	case RankAccountOrder:
		return a.order - b.order
	case RankWeeklyReset:
		return soonerFirst(a.weekly, b.weekly)
	case RankFiveHourReset:
		return soonerFirst(a.window, b.window)
	case RankMostRemaining:
		switch {
		case a.rem > b.rem:
			return -1
		case a.rem < b.rem:
			return 1
		}
	}
	return 0
}

// rankTied reports whether two accounts count as equal on a criterion, so the
// next criterion decides: weekly resets within a day, quota within a few
// points, or neither 5-hour window running.
func rankTied(crit string, a, b rankItem) bool {
	switch crit {
	case RankWeeklyReset:
		if a.weekly.IsZero() || b.weekly.IsZero() {
			return a.weekly.IsZero() && b.weekly.IsZero()
		}
		return a.weekly.Sub(b.weekly).Abs() < weeklyTie
	case RankMostRemaining:
		return math.Abs(a.rem-b.rem) < remainingTie
	}
	return rankCompare(crit, a, b) == 0
}

// rank orders usable accounts from best to worst. Tiers keep healthy accounts
// ahead of thin or low ones, normal ahead of reserve; the user's criteria
// (rank_by) order accounts within a tier. Unusable accounts are excluded.
func (e *Engine) rank(ids []string, now time.Time, exclude string) []string {
	items := make([]rankItem, 0, len(ids))
	for _, id := range ids {
		if id == exclude || !e.usableAuto(id, now) {
			continue
		}
		items = append(items, e.rankItem(id, now))
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].tier != items[j].tier {
			return items[i].tier < items[j].tier
		}
		return items[i].label < items[j].label
	})
	// Sort each run of tied accounts by the next criterion. A run is anchored
	// at its best member, so "within a day" never chains across a week.
	var refine func(run []rankItem, k int)
	refine = func(run []rankItem, k int) {
		if len(run) < 2 || k >= len(e.settings.RankBy) {
			return
		}
		crit := e.settings.RankBy[k]
		sort.SliceStable(run, func(i, j int) bool { return rankCompare(crit, run[i], run[j]) < 0 })
		for start := 0; start < len(run); {
			end := start + 1
			for end < len(run) && rankTied(crit, run[start], run[end]) {
				end++
			}
			refine(run[start:end], k+1)
			start = end
		}
	}
	for start := 0; start < len(items); {
		end := start + 1
		for end < len(items) && items[end].tier == items[start].tier {
			end++
		}
		refine(items[start:end], 0)
		start = end
	}
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.id
	}
	return out
}

// tier orders accounts for ranking: healthy normal accounts (strong, then
// unknown, then thin) before any reserve account, and any account above the
// threshold before low ones.
func (e *Engine) tier(id string, now time.Time) int {
	r, known := e.remaining(id, now)
	health := 0
	switch {
	case known && r >= e.settings.SwitchThresholdPercent+e.settings.SwitchBackMarginPercent:
		health = 0
	case !known:
		health = 1
	case r >= e.settings.SwitchThresholdPercent:
		health = 2
	default:
		return 8 + e.roleRank(id)
	}
	return health + 3*e.roleRank(id)
}

// clearlyBetter guards provisional re-ranking against flapping: a better
// tier, or a clear win on the user's ranking.
func (e *Engine) clearlyBetter(b, c string, now time.Time) bool {
	if tb, tc := e.tier(b, now), e.tier(c, now); tb != tc {
		return tb < tc
	}
	ok, _ := e.preferredOver(b, c, now)
	return ok
}

func (e *Engine) describeChoice(id string, now time.Time) string {
	r, ok := e.remaining(id, now)
	if !ok {
		return e.label(id) + " (quota not yet observed)"
	}
	return fmt.Sprintf("%s (%.0f%% left)", e.label(id), r)
}

// quotaSummary renders "62% left" or "quota not yet observed".
func (e *Engine) quotaSummary(id string, now time.Time) string {
	if r, ok := e.remaining(id, now); ok {
		return fmt.Sprintf("%.0f%% left", r)
	}
	return "quota not yet observed"
}

func (e *Engine) cacheWarm() time.Duration {
	return time.Duration(e.settings.CacheWarmMinutes) * time.Minute
}

// ---------------------------------------------------------------------------
// Observations
// ---------------------------------------------------------------------------

// ObserveAccounts refreshes the roster from host.auth.list.
func (e *Engine) ObserveAccounts(list []HostAccount) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.listening {
		e.listening = true
		e.listeningSince = e.now()
	}
	present := make(map[string]bool, len(list))
	for _, h := range list {
		if !strings.EqualFold(h.Provider, providerCodex) || h.ID == "" {
			continue
		}
		present[h.ID] = true
		a := e.account(h.ID)
		a.inRoster = true
		if !a.EverInRoster {
			a.EverInRoster = true
			e.dirty = true
		}
		if h.Unavailable {
			if a.unavailSince.IsZero() {
				a.unavailSince = e.now()
			}
		} else {
			a.unavailSince = time.Time{}
		}
		a.authIndex = h.AuthIndex
		a.hostDisabled = h.Disabled
		a.hostUnavailable = h.Unavailable
		a.hostNextRetry = h.NextRetry
		a.hostStatus = h.Status
		if label := firstNonEmpty(h.Email, h.Label); label != "" && a.Label != label {
			a.Label = label
			e.dirty = true
		}
		if h.Plan != "" && a.Plan == "" {
			a.Plan = h.Plan
		}
	}
	for id, a := range e.st.Accounts {
		if !present[id] {
			a.inRoster = false
		}
	}
}

// ObserveUsage ingests a completed request: quota headers, quota failures and
// auth failures. Called asynchronously by CLIProxyAPI's usage queue.
func (e *Engine) ObserveUsage(o UsageObservation) {
	if o.AuthID == "" {
		return
	}
	if o.Provider != "" && !strings.EqualFold(o.Provider, providerCodex) {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	// Quota headers describe the moment the upstream response started, which
	// can be well before the (async) usage record arrives.
	at := o.At
	if at.IsZero() || at.After(now) || now.Sub(at) > 6*time.Hour {
		at = now
	}
	a := e.account(o.AuthID)
	if !o.Failed && at.After(a.LastUsed) {
		a.LastUsed = at
	}
	if q, ok := ParseQuotaHeaders(o.Headers, at); ok {
		e.mergeQuota(a, q, now)
		// A request just succeeded, so the 5-hour window is running.
		if !o.Failed && q.Short != nil && q.Short.ResetAt.After(a.WindowUntil) {
			a.WindowUntil = q.Short.ResetAt
		}
	}
	if o.Failed {
		if qe, ok := ParseQuotaError(o.Status, o.Body, now); ok {
			until := qe.ResetAt
			if until.IsZero() && a.Quota != nil {
				until = a.Quota.NextReset(now, e.settings.SwitchThresholdPercent)
			}
			if !until.After(now) {
				until = now.Add(defaultBlock)
			}
			model := normalizeModel(o.Model)
			if e.settings.ModelLevelLimits && model != "" {
				// Mirrors CLIProxyAPI's codex model-level-cooling: the limit
				// applies to this model only; other models keep the account.
				if a.ModelBlocks == nil {
					a.ModelBlocks = map[string]time.Time{}
				}
				if a.ModelBlocks[model].Before(until) {
					a.ModelBlocks[model] = until
					e.addHistory("exhausted", a.ID, "", fmt.Sprintf("%s reached the %s usage limit until %s; other models keep using it",
						e.label(a.ID), model, until.Local().Format("Mon 3:04 PM")))
				}
				e.dirty = true
			} else {
				e.block(a, until, "quota exhausted", now)
			}
		}
		// Other failures (401 refresh, 5xx, timeouts) are CLIProxyAPI's to
		// handle: it cools the credential down and stops offering it, which
		// the router treats as a transient detour.
	}
	e.noteHealth(a, now)
	e.evaluateActive(now)
}

// ApplyPolledQuota stores a snapshot fetched from the Codex usage endpoint.
func (e *Engine) ApplyPolledQuota(id string, q Quota, pollErr error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	a := e.account(id)
	a.LastPolled = now
	a.forcePoll = false
	e.lastPoll = now
	if pollErr != nil {
		a.PollError = pollErr.Error()
		e.dirty = true
		return
	}
	a.PollError = ""
	e.mergeQuota(a, q, now)
	e.noteHealth(a, now)
	e.evaluateActive(now)
}

func (e *Engine) mergeQuota(a *Account, q Quota, now time.Time) {
	if a.Quota == nil {
		a.Quota = &Quota{}
	}
	// Usage records can arrive out of order (a long stream finishes after a
	// later short request), so never let older numbers replace newer ones.
	if q.Short != nil && (a.Quota.Short == nil || !q.Short.ObservedAt.Before(a.Quota.Short.ObservedAt)) {
		a.Quota.Short = q.Short
	}
	if q.Long != nil && (a.Quota.Long == nil || !q.Long.ObservedAt.Before(a.Quota.Long.ObservedAt)) {
		a.Quota.Long = q.Long
	}
	if q.ObservedAt.Before(a.Quota.ObservedAt) {
		e.dirty = true
		return
	}
	a.Quota.LimitReached = q.LimitReached
	a.Quota.ObservedAt = q.ObservedAt
	a.Quota.Source = q.Source
	if q.Plan != "" {
		a.Quota.Plan = q.Plan
		a.Plan = q.Plan
	}
	e.dirty = true
	switch {
	case q.LimitReached:
		until := a.Quota.NextReset(now, 0.5)
		if until.IsZero() {
			until = a.Quota.NextReset(now, e.settings.SwitchThresholdPercent)
		}
		if !until.After(now) {
			until = now.Add(defaultBlock)
		}
		e.block(a, until, "quota exhausted", now)
	case a.BlockReason == "quota exhausted" && a.BlockedUntil.After(now):
		// Fresh data proving the account has quota again ends the block early
		// (e.g. a reset happened sooner than predicted).
		if r, ok := a.Quota.MinRemaining(now); ok && r >= e.settings.SwitchThresholdPercent {
			a.BlockedUntil = time.Time{}
			a.BlockReason = ""
		}
	}
}

func (e *Engine) block(a *Account, until time.Time, reason string, now time.Time) {
	if a.BlockedUntil.After(now) && a.BlockReason == reason && !until.After(a.BlockedUntil) {
		return
	}
	a.BlockedUntil = until
	a.BlockReason = reason
	e.dirty = true
}

// noteHealth logs one history entry per down/up transition of an account.
func (e *Engine) noteHealth(a *Account, now time.Time) {
	down := a.BlockedUntil.After(now) || e.low(a.ID, now)
	if down == a.Down {
		return
	}
	a.Down = down
	e.dirty = true
	if down {
		detail := "below threshold"
		if a.BlockedUntil.After(now) {
			detail = fmt.Sprintf("%s until %s", a.BlockReason, a.BlockedUntil.Local().Format("Mon 3:04 PM"))
		} else if r, ok := a.Quota.MinRemaining(now); ok {
			detail = fmt.Sprintf("%.0f%% remaining", r)
		}
		e.addHistory("exhausted", a.ID, "", fmt.Sprintf("%s is down: %s", e.label(a.ID), detail))
	} else {
		e.addHistory("recovered", "", a.ID, fmt.Sprintf("%s recovered: %s", e.label(a.ID), e.quotaSummary(a.ID, now)))
	}
}

// Tick runs periodic maintenance: recoveries, switch-back evaluation, and
// bounded cleanup of idle sessions.
func (e *Engine) Tick() {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	e.expirePause(now)
	for _, a := range e.st.Accounts {
		if !a.BlockedUntil.IsZero() && !a.BlockedUntil.After(now) {
			a.BlockedUntil = time.Time{}
			a.BlockReason = ""
			e.dirty = true
		}
		for m, until := range a.ModelBlocks {
			if !until.After(now) {
				delete(a.ModelBlocks, m)
				e.dirty = true
			}
		}
		e.noteHealth(a, now)
	}
	e.evaluateActive(now)
	// Paused sessions are not idle; forgetting them would cost their cache.
	// The size caps below still apply.
	if e.st.Pause == nil {
		for k, b := range e.st.Families {
			if now.Sub(b.LastSeen) > familyTTL {
				delete(e.st.Families, k)
				e.dirty = true
			}
		}
		for k, l := range e.st.Links {
			if now.Sub(l.Seen) > familyTTL {
				delete(e.st.Links, k)
				e.dirty = true
			}
		}
	}
	if len(e.st.Links) > maxLinks {
		type kv struct {
			k string
			t time.Time
		}
		all := make([]kv, 0, len(e.st.Links))
		for k, l := range e.st.Links {
			all = append(all, kv{k, l.Seen})
		}
		sort.Slice(all, func(i, j int) bool { return all[i].t.Before(all[j].t) })
		for _, item := range all[:len(all)-maxLinks] {
			delete(e.st.Links, item.k)
		}
		e.dirty = true
	}
	if len(e.st.Families) > maxFamilies {
		type kv struct {
			k string
			t time.Time
		}
		all := make([]kv, 0, len(e.st.Families))
		for k, b := range e.st.Families {
			all = append(all, kv{k, b.LastSeen})
		}
		sort.Slice(all, func(i, j int) bool { return all[i].t.Before(all[j].t) })
		for _, item := range all[:len(all)-maxFamilies] {
			delete(e.st.Families, item.k)
		}
		e.dirty = true
	}
}

// PollTarget is an account whose quota should be refreshed actively.
type PollTarget struct {
	ID        string
	AuthIndex string
}

// PollTargets returns accounts whose quota data is missing or older than the
// poll interval (or whose window reset has passed since the last observation).
func (e *Engine) PollTargets() []PollTarget {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.settings.QuotaPollMinutes <= 0 {
		return nil
	}
	now := e.now()
	interval := time.Duration(e.settings.QuotaPollMinutes) * time.Minute
	var out []PollTarget
	for _, id := range e.knownIDs() {
		a := e.st.Accounts[id]
		if a.authIndex == "" || !a.inRoster || a.hostDisabled {
			continue
		}
		if !a.forcePoll && !a.LastPolled.IsZero() && now.Sub(a.LastPolled) < interval/2 {
			continue // includes back-off after errors
		}
		due := a.forcePoll || a.Quota == nil || now.Sub(a.Quota.ObservedAt) >= interval
		if !due && a.Quota != nil {
			for _, w := range []*Window{a.Quota.Short, a.Quota.Long} {
				if w != nil && !w.ResetAt.IsZero() && !now.Before(w.ResetAt) && w.ObservedAt.Before(w.ResetAt) {
					due = true
				}
			}
		}
		if due {
			out = append(out, PollTarget{ID: id, AuthIndex: a.authIndex})
		}
	}
	return out
}

// ForcePollAll makes every account due for polling on the next poll cycle.
func (e *Engine) ForcePollAll() {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, a := range e.st.Accounts {
		a.forcePoll = true
	}
}

// ---------------------------------------------------------------------------
// Manual control
// ---------------------------------------------------------------------------

var ErrUnknownAccount = errors.New("unknown account")

// SetAuto returns to automatic routing.
func (e *Engine) SetAuto() {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	if e.st.Mode == ModeAuto {
		return
	}
	pinned := e.st.Pinned
	e.st.Mode = ModeAuto
	e.st.Pinned = ""
	e.dirty = true
	e.addHistory("manual", pinned, "", "Automatic routing resumed")
	// Keep the previously pinned account as the current account (no forced
	// switch, so its warm sessions keep their cache) unless it needs to change.
	if pinned != "" && e.usableAuto(pinned, now) {
		e.st.Active = pinned
		e.st.ActiveManual = false
		e.st.ActiveReason = "Kept after leaving manual mode: " + e.describeChoice(pinned, now)
		e.st.ActiveSince = now
		e.st.LastSwitch = now
	}
	e.evaluateActive(now)
}

// Pin switches to manual mode, routing every request to id.
func (e *Engine) Pin(id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.st.Accounts[id]; !ok {
		return ErrUnknownAccount
	}
	if e.st.Mode == ModeManual && e.st.Pinned == id {
		return nil
	}
	from := e.st.Pinned
	if from == "" {
		from = e.st.Active
	}
	e.st.Mode = ModeManual
	e.st.Pinned = id
	e.st.Active = id
	e.st.ProvisionalUntil = time.Time{}
	e.st.ActiveSince = e.now()
	e.st.ActiveReason = "Pinned manually"
	e.dirty = true
	e.addHistory("manual", from, id, "Pinned "+e.label(id)+"; manual mode")
	return nil
}

// Select makes id the current account while staying in automatic mode. The
// choice is sticky: switch-back will not move away from it, only the
// threshold/exhaustion rules can.
func (e *Engine) Select(id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.st.Accounts[id]; !ok {
		return ErrUnknownAccount
	}
	now := e.now()
	if e.st.Mode == ModeManual {
		e.st.Mode = ModeAuto
		e.st.Pinned = ""
	}
	from := e.st.Active
	e.st.Active = id
	e.st.ActiveManual = true
	e.st.ProvisionalUntil = time.Time{}
	e.st.ActiveReason = "Selected manually"
	e.st.ActiveSince = now
	e.st.LastSwitch = now
	e.dirty = true
	e.addHistory("manual", from, id, "Switched to "+e.label(id)+" by hand; automatic routing continues from here")
	return nil
}

// ---------------------------------------------------------------------------
// History
// ---------------------------------------------------------------------------

func (e *Engine) addHistory(kind, from, to, reason string) {
	now := e.now()
	if n := len(e.st.History); n > 0 {
		last := &e.st.History[n-1]
		if last.Kind == kind && last.From == from && last.To == to && last.Reason == reason && now.Sub(last.At) < historyCoalesce {
			if last.Count == 0 {
				last.Count = 1
			}
			last.Count++
			last.At = now
			e.dirty = true
			return
		}
	}
	entry := HistoryEntry{At: now, Kind: kind, From: from, To: to, Reason: reason}
	e.st.History = append(e.st.History, entry)
	e.trimHistory()
	if len(e.pending) < 100 {
		e.pending = append(e.pending, entry)
	}
	e.dirty = true
}

// DrainEvents returns history entries added since the last call (for logging).
func (e *Engine) DrainEvents() []HistoryEntry {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := e.pending
	e.pending = nil
	return out
}

func (e *Engine) trimHistory() {
	limit := e.settings.HistoryLimit
	if limit <= 0 {
		limit = DefaultSettings().HistoryLimit
	}
	if over := len(e.st.History) - limit; over > 0 {
		e.st.History = append([]HistoryEntry(nil), e.st.History[over:]...)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func metaString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// labelFromID turns "codex-0898cc86-name@example.com-plus.json" into
// "name@example.com" for display when the host has not supplied an email.
func labelFromID(id string) string {
	s := strings.TrimSuffix(id, ".json")
	parts := strings.Split(s, "-")
	for _, p := range parts {
		if strings.Contains(p, "@") {
			return p
		}
	}
	return s
}

func settingsEqual(a, b Settings) bool {
	return fmt.Sprintf("%+v", a) == fmt.Sprintf("%+v", b)
}
