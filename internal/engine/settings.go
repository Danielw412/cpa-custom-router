package engine

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// Strategy names control how a replacement account is ranked and when a
// switch back to a recovered account is considered worthwhile.
const (
	StrategyBalanced      = "balanced"
	StrategyPriority      = "priority"
	StrategySoonestReset  = "soonest-reset"
	StrategyMostRemaining = "most-remaining"

	FallbackAuto   = "auto"
	FallbackStrict = "strict"
)

// Ranking criteria, applied in the order listed in rank_by. Health always
// comes first: accounts below the threshold and reserve accounts rank last
// whatever the criteria say.
const (
	RankAccountOrder  = "account_order"   // higher in your account order
	RankFiveHourReset = "five_hour_reset" // running 5-hour window resets sooner
	RankWeeklyReset   = "weekly_reset"    // weekly quota resets sooner
	RankMostRemaining = "most_remaining"  // more quota left
)

// RankCriteria lists every criterion in the default (balanced) order.
var RankCriteria = []string{RankAccountOrder, RankWeeklyReset, RankFiveHourReset, RankMostRemaining}

// rankPreset maps the legacy strategy setting to a ranking, used when
// rank_by is not set.
func rankPreset(strategy string) []string {
	switch strategy {
	case StrategyPriority:
		return []string{RankAccountOrder, RankMostRemaining, RankWeeklyReset, RankFiveHourReset}
	case StrategySoonestReset:
		return []string{RankWeeklyReset, RankFiveHourReset, RankAccountOrder, RankMostRemaining}
	case StrategyMostRemaining:
		return []string{RankMostRemaining, RankAccountOrder, RankWeeklyReset, RankFiveHourReset}
	default:
		return append([]string(nil), RankCriteria...)
	}
}

// Settings are the user-tunable knobs. They live in config.yaml under
// plugins.configs.custom-quota-router and are hot-applied via plugin.reconfigure.
type Settings struct {
	// SwitchThresholdPercent: leave an account once its 5-hour or weekly
	// remaining quota drops below this percentage.
	SwitchThresholdPercent float64 `yaml:"switch_threshold_percent" json:"switch_threshold_percent"`
	// SwitchBack enables returning to a preferred account after it recovers.
	SwitchBack bool `yaml:"switch_back" json:"switch_back"`
	// SwitchBackMarginPercent is the extra headroom (above the threshold) a
	// recovered account needs before automatic routing returns to it.
	SwitchBackMarginPercent float64 `yaml:"switch_back_margin_percent" json:"switch_back_margin_percent"`
	// MinDwellMinutes is the minimum time between non-emergency switches.
	MinDwellMinutes int `yaml:"min_dwell_minutes" json:"min_dwell_minutes"`
	// CacheWarmMinutes: a session idle for longer than this is considered to
	// have lost its prompt cache, so it may move without a cache penalty.
	CacheWarmMinutes int `yaml:"cache_warm_minutes" json:"cache_warm_minutes"`
	// Strategy is the legacy preset (balanced, priority, soonest-reset,
	// most-remaining); it only picks the ranking when rank_by is empty.
	Strategy string `yaml:"strategy" json:"strategy"`
	// RankBy orders the criteria used to choose the next account and to decide
	// switching back, most important first. Missing criteria are appended in
	// the default order, so every criterion always has a place.
	RankBy []string `yaml:"rank_by" json:"rank_by"`
	// AccountOrder lists auth IDs from most to least preferred. Unlisted accounts
	// share the lowest priority.
	AccountOrder []string `yaml:"account_order" json:"account_order"`
	// ReserveAccounts are only used automatically when no normal account is healthy.
	ReserveAccounts []string `yaml:"reserve_accounts" json:"reserve_accounts"`
	// DisabledAccounts are never chosen by automatic routing (manual pin still works).
	DisabledAccounts []string `yaml:"disabled_accounts" json:"disabled_accounts"`
	// ManualFallback decides what happens when the pinned account is unavailable:
	// "auto" routes automatically until it returns, "strict" rejects requests.
	ManualFallback string `yaml:"manual_fallback" json:"manual_fallback"`
	// QuotaPollMinutes is how often idle accounts are refreshed from the Codex
	// usage endpoint. 0 disables active polling (passive observation only).
	QuotaPollMinutes int `yaml:"quota_poll_minutes" json:"quota_poll_minutes"`
	// StaleAfterMinutes marks quota data as stale in the UI and for decisions.
	StaleAfterMinutes int `yaml:"stale_after_minutes" json:"stale_after_minutes"`
	// ModelLevelLimits scopes usage-limit failures to the model that hit them
	// instead of the whole account. Enable it together with CLIProxyAPI's
	// upstream.codex.model-level-cooling; leave it off otherwise (default).
	ModelLevelLimits bool `yaml:"model_level_limits" json:"model_level_limits"`
	// WindowStartModel / WindowStartEffort are used for the single tiny
	// request that starts an account's 5-hour window.
	WindowStartModel  string `yaml:"window_start_model" json:"window_start_model"`
	WindowStartEffort string `yaml:"window_start_effort" json:"window_start_effort"`
	// HistoryLimit bounds the persisted routing history.
	HistoryLimit int `yaml:"history_limit" json:"history_limit"`
	// StateFile stores runtime state (mode, pins, bindings, history).
	StateFile string `yaml:"state_file" json:"state_file"`
}

// DefaultSettings returns conservative, cache-friendly defaults.
func DefaultSettings() Settings {
	return Settings{
		SwitchThresholdPercent:  5,
		SwitchBack:              true,
		SwitchBackMarginPercent: 20,
		MinDwellMinutes:         20,
		CacheWarmMinutes:        30,
		Strategy:                StrategyBalanced,
		ManualFallback:          FallbackAuto,
		QuotaPollMinutes:        10,
		StaleAfterMinutes:       30,
		HistoryLimit:            200,
		WindowStartModel:        DefaultStarterModel,
		WindowStartEffort:       DefaultStarterEffort,
		StateFile:               "plugins/custom-quota-router/state.json",
	}
}

// ParseSettings decodes the plugin's YAML block on top of defaults and
// normalizes it. Invalid values are clamped and reported as warnings rather
// than failing, so a bad edit can never take routing offline.
func ParseSettings(raw []byte) (Settings, []string) {
	s := DefaultSettings()
	var warnings []string
	if len(strings.TrimSpace(string(raw))) > 0 {
		// Decode into a scratch copy so a type error in one field does not
		// leave a half-applied struct; on failure fall back to defaults.
		decoded := DefaultSettings()
		if err := yaml.Unmarshal(raw, &decoded); err != nil {
			warnings = append(warnings, fmt.Sprintf("config could not be parsed, defaults in use: %v", err))
		} else {
			s = decoded
		}
	}
	return s.Normalize(warnings)
}

// Normalize clamps values into safe ranges and de-duplicates account lists.
func (s Settings) Normalize(warnings []string) (Settings, []string) {
	clampF := func(name string, v *float64, lo, hi float64) {
		if *v < lo || *v > hi {
			warnings = append(warnings, fmt.Sprintf("%s=%g out of range [%g,%g]; clamped", name, *v, lo, hi))
			if *v < lo {
				*v = lo
			} else {
				*v = hi
			}
		}
	}
	clampI := func(name string, v *int, lo, hi int) {
		if *v < lo || *v > hi {
			warnings = append(warnings, fmt.Sprintf("%s=%d out of range [%d,%d]; clamped", name, *v, lo, hi))
			if *v < lo {
				*v = lo
			} else {
				*v = hi
			}
		}
	}
	clampF("switch_threshold_percent", &s.SwitchThresholdPercent, 0, 90)
	clampF("switch_back_margin_percent", &s.SwitchBackMarginPercent, 0, 90)
	clampI("min_dwell_minutes", &s.MinDwellMinutes, 0, 24*60)
	clampI("cache_warm_minutes", &s.CacheWarmMinutes, 0, 24*60)
	clampI("quota_poll_minutes", &s.QuotaPollMinutes, 0, 24*60)
	if s.QuotaPollMinutes > 0 && s.QuotaPollMinutes < 2 {
		warnings = append(warnings, "quota_poll_minutes below 2 is too aggressive; using 2")
		s.QuotaPollMinutes = 2
	}
	clampI("stale_after_minutes", &s.StaleAfterMinutes, 1, 24*60)
	clampI("history_limit", &s.HistoryLimit, 10, 5000)

	s.Strategy = strings.ToLower(strings.TrimSpace(s.Strategy))
	switch s.Strategy {
	case StrategyBalanced, StrategyPriority, StrategySoonestReset, StrategyMostRemaining:
	case "":
		s.Strategy = StrategyBalanced
	default:
		warnings = append(warnings, fmt.Sprintf("unknown strategy %q; using %s", s.Strategy, StrategyBalanced))
		s.Strategy = StrategyBalanced
	}
	if len(s.RankBy) == 0 {
		s.RankBy = rankPreset(s.Strategy)
	} else {
		known := toSet(RankCriteria)
		var rank []string
		for _, c := range dedupe(s.RankBy) {
			c = strings.ToLower(c)
			if _, ok := known[c]; !ok {
				warnings = append(warnings, fmt.Sprintf("unknown rank_by criterion %q ignored", c))
				continue
			}
			rank = append(rank, c)
		}
		s.RankBy = dedupe(append(rank, RankCriteria...))
	}
	s.ManualFallback = strings.ToLower(strings.TrimSpace(s.ManualFallback))
	switch s.ManualFallback {
	case FallbackAuto, FallbackStrict:
	case "":
		s.ManualFallback = FallbackAuto
	default:
		warnings = append(warnings, fmt.Sprintf("unknown manual_fallback %q; using %s", s.ManualFallback, FallbackAuto))
		s.ManualFallback = FallbackAuto
	}
	s.WindowStartModel = strings.TrimSpace(s.WindowStartModel)
	if s.WindowStartModel == "" {
		s.WindowStartModel = DefaultStarterModel
	}
	s.WindowStartEffort = strings.ToLower(strings.TrimSpace(s.WindowStartEffort))
	switch s.WindowStartEffort {
	case "none", "minimal", "low", "medium", "high", "xhigh":
	case "":
		s.WindowStartEffort = DefaultStarterEffort
	default:
		warnings = append(warnings, fmt.Sprintf("unknown window_start_effort %q; using %s", s.WindowStartEffort, DefaultStarterEffort))
		s.WindowStartEffort = DefaultStarterEffort
	}
	s.StateFile = strings.TrimSpace(s.StateFile)
	if s.StateFile == "" {
		s.StateFile = DefaultSettings().StateFile
	}
	s.AccountOrder = dedupe(s.AccountOrder)
	s.ReserveAccounts = dedupe(s.ReserveAccounts)
	s.DisabledAccounts = dedupe(s.DisabledAccounts)
	// An account cannot be both reserve and disabled; disabled wins.
	disabled := toSet(s.DisabledAccounts)
	kept := s.ReserveAccounts[:0]
	for _, id := range s.ReserveAccounts {
		if _, off := disabled[id]; off {
			warnings = append(warnings, fmt.Sprintf("account %q is both reserve and disabled; treating as disabled", id))
			continue
		}
		kept = append(kept, id)
	}
	s.ReserveAccounts = kept
	return s, warnings
}

// Role reports how automatic routing treats an account.
func (s Settings) Role(id string) string {
	for _, d := range s.DisabledAccounts {
		if d == id {
			return RoleOff
		}
	}
	for _, r := range s.ReserveAccounts {
		if r == id {
			return RoleReserve
		}
	}
	return RoleNormal
}

// OrderIndex returns the user priority of an account; lower is preferred.
// Unlisted accounts share len(AccountOrder).
func (s Settings) OrderIndex(id string) int {
	for i, a := range s.AccountOrder {
		if a == id {
			return i
		}
	}
	return len(s.AccountOrder)
}

const (
	RoleNormal  = "normal"
	RoleReserve = "reserve"
	RoleOff     = "off"
)

func dedupe(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

func toSet(in []string) map[string]struct{} {
	out := make(map[string]struct{}, len(in))
	for _, v := range in {
		out[v] = struct{}{}
	}
	return out
}
