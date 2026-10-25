// Package plugin implements the CLIProxyAPI RPC methods on top of the engine.
// It is independent of cgo so the full request path can be unit tested.
package plugin

import (
	"encoding/json"
	"net/http"
	"net/url"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/Danielw412/cpa-custom-router/internal/engine"
	"github.com/Danielw412/cpa-custom-router/internal/hostapi"
	"github.com/Danielw412/cpa-custom-router/internal/service"
	"github.com/Danielw412/cpa-custom-router/internal/web"
)

// Version is overridden at build time with -ldflags "-X ...plugin.Version=...".
var Version = "1.0.0"

// ABIVersion is the native ABI this plugin implements.
const ABIVersion = pluginabi.ABIVersion

// Plugin holds the lazily created service.
type Plugin struct {
	host *hostapi.Client
	mu   sync.Mutex
	svc  *service.Service
}

// New creates the plugin with a host callback client.
func New(host *hostapi.Client) *Plugin { return &Plugin{host: host} }

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ErrorEnvelope builds a failed RPC envelope.
func ErrorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}

func okEnvelope(v any) ([]byte, bool) {
	raw, err := json.Marshal(v)
	if err != nil {
		return ErrorEnvelope("encode_failed", err.Error()), false
	}
	out, _ := json.Marshal(envelope{OK: true, Result: raw})
	return out, true
}

// Handle dispatches one RPC call. The bool reports success to the C layer.
func (p *Plugin) Handle(method string, req []byte) ([]byte, bool) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		var lr struct {
			ConfigYAML []byte `json:"config_yaml"`
		}
		_ = json.Unmarshal(req, &lr)
		p.configure(lr.ConfigYAML)
		return okEnvelope(registration())
	case pluginabi.MethodPluginQuiesce:
		// A newer version of the plugin is about to replace this one: stop
		// background work and persist state so the successor resumes it.
		if svc := p.service(); svc != nil {
			svc.Retire()
		}
		return okEnvelope(struct{}{})
	case pluginabi.MethodPluginShutdown:
		p.Shutdown()
		return okEnvelope(struct{}{})
	case pluginabi.MethodSchedulerPick:
		return p.pick(req)
	case pluginabi.MethodUsageHandle:
		p.usage(req)
		return okEnvelope(struct{}{})
	case pluginabi.MethodManagementRegister:
		return okEnvelope(map[string]any{
			"routes":    web.ManagementRoutes(),
			"resources": web.ResourceRoutes(),
		})
	case pluginabi.MethodManagementHandle:
		return p.management(req)
	default:
		return ErrorEnvelope("unknown_method", "unknown method: "+method), false
	}
}

// Shutdown stops background work and flushes state.
func (p *Plugin) Shutdown() {
	if svc := p.service(); svc != nil {
		svc.Stop()
	}
}

func (p *Plugin) service() *service.Service {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.svc
}

func (p *Plugin) configure(configYAML []byte) {
	p.mu.Lock()
	if p.svc == nil {
		p.svc = service.New(configYAML, p.host)
	} else {
		p.svc.Reconfigure(configYAML)
	}
	svc := p.svc
	p.mu.Unlock()
	svc.Start()
}

func (p *Plugin) pick(raw []byte) (out []byte, ok bool) {
	// Whatever happens in here, never fail the request: defer to the host.
	defer func() {
		if r := recover(); r != nil {
			out, ok = okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
		}
	}()
	svc := p.service()
	if svc == nil {
		return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
	}
	var req pluginapi.SchedulerPickRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		// Never fail a request because of the plugin: defer to the built-in selector.
		return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
	}
	cands := make([]engine.Candidate, 0, len(req.Candidates))
	for _, c := range req.Candidates {
		cands = append(cands, engine.Candidate{ID: c.ID, Provider: c.Provider, Priority: c.Priority, Status: c.Status})
	}
	preq := engine.PickRequest{
		Candidates: cands,
		Headers:    req.Options.Headers,
		Metadata:   req.Options.Metadata,
		Model:      req.Model,
	}
	// While paused or slowed down this waits; it never holds the engine lock.
	if reason := svc.Hold(preq); reason != "" {
		return okEnvelope(pluginapi.SchedulerPickResponse{Handled: true, Reject: true, RejectCode: service.RejectPaused, RejectReason: reason})
	}
	res := svc.Engine.Pick(preq)
	return okEnvelope(pluginapi.SchedulerPickResponse{
		AuthID:       res.AuthID,
		Handled:      res.Handled,
		Reject:       res.Reject,
		RejectCode:   res.RejectCode,
		RejectReason: res.RejectReason,
	})
}

func (p *Plugin) usage(raw []byte) {
	svc := p.service()
	if svc == nil {
		return
	}
	var rec pluginapi.UsageRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return
	}
	// Rate-limit data reflects the start of the upstream response.
	at := rec.RequestedAt
	if rec.TTFT > 0 {
		at = at.Add(rec.TTFT)
	}
	svc.Engine.ObserveUsage(engine.UsageObservation{
		AuthID:   rec.AuthID,
		Provider: rec.Provider,
		Model:    rec.Model,
		At:       at,
		Headers:  rec.ResponseHeaders,
		Failed:   rec.Failed,
		Status:   rec.Failure.StatusCode,
		Body:     rec.Failure.Body,
	})
}

type mgmtRequest struct {
	Method  string
	Path    string
	Headers http.Header
	Query   url.Values
	Body    []byte
}

type mgmtResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}

type backend struct{ svc *service.Service }

func (b backend) Engine() *engine.Engine { return b.svc.Engine }
func (b backend) RequestPoll()           { b.svc.RequestPoll() }
func (b backend) WakeStarter()           { b.svc.WakeStarter() }
func (b backend) LastError() string      { return b.svc.LastError() }
func (b backend) WakeHeld()              { b.svc.WakeHeld() }
func (b backend) Waiting() int           { return b.svc.Waiting() }
func (b backend) Persist()               { b.svc.Flush() }

func (p *Plugin) management(raw []byte) ([]byte, bool) {
	svc := p.service()
	if svc == nil {
		return okEnvelope(mgmtResponse{StatusCode: http.StatusServiceUnavailable, Body: []byte(`{"error":"plugin not configured yet"}`)})
	}
	var req mgmtRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return okEnvelope(mgmtResponse{StatusCode: http.StatusBadRequest, Body: []byte(`{"error":"bad request"}`)})
	}
	resp := web.Handle(backend{svc}, Version, req.Method, req.Path, req.Query, req.Body)
	return okEnvelope(mgmtResponse{StatusCode: resp.StatusCode, Headers: resp.Headers, Body: resp.Body})
}

type registrationCaps struct {
	Scheduler                 bool `json:"scheduler"`
	SchedulerAcrossPriorities bool `json:"scheduler_across_priorities"`
	UsagePlugin               bool `json:"usage_plugin"`
	ManagementAPI             bool `json:"management_api"`
}

type registrationBody struct {
	SchemaVersion uint32             `json:"schema_version"`
	Metadata      pluginapi.Metadata `json:"metadata"`
	Capabilities  registrationCaps   `json:"capabilities"`
}

func registration() registrationBody {
	d := engine.DefaultSettings()
	str, num, integer, boolean, enum, array := pluginapi.ConfigFieldTypeString, pluginapi.ConfigFieldTypeNumber,
		pluginapi.ConfigFieldTypeInteger, pluginapi.ConfigFieldTypeBoolean, pluginapi.ConfigFieldTypeEnum, pluginapi.ConfigFieldTypeArray
	return registrationBody{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             "custom-quota-router",
			Version:          Version,
			Author:           "Danielw412",
			GitHubRepository: "https://github.com/Danielw412/cpa-custom-router",
			Logo:             logo,
			ConfigFields: []pluginapi.ConfigField{
				{Name: "switch_threshold_percent", Type: num, Default: d.SwitchThresholdPercent, Description: "Leave an account when its 5-hour or weekly remaining quota falls below this percentage."},
				{Name: "strategy", Type: enum, Default: d.Strategy, EnumValues: []string{engine.StrategyBalanced, engine.StrategyPriority, engine.StrategySoonestReset, engine.StrategyMostRemaining}, Description: "Legacy preset (balanced, priority, soonest-reset, most-remaining); used only when rank_by is empty."},
				{Name: "rank_by", Type: array, Default: engine.RankCriteria, Description: "How the next account is chosen, most important first: account_order, weekly_reset, five_hour_reset, most_remaining. Missing criteria are added at the end."},
				{Name: "switch_back", Type: boolean, Default: d.SwitchBack, Description: "Return to a preferred account after its quota resets."},
				{Name: "switch_back_margin_percent", Type: num, Default: d.SwitchBackMarginPercent, Description: "Headroom above the threshold a recovered account needs before switching back."},
				{Name: "min_dwell_minutes", Type: integer, Default: d.MinDwellMinutes, Description: "Minimum minutes between non-emergency switches (prevents flapping)."},
				{Name: "cache_warm_minutes", Type: integer, Default: d.CacheWarmMinutes, Description: "Idle sessions keep their account this long to preserve prompt cache."},
				{Name: "account_order", Type: array, Default: []string{}, Description: "Auth IDs from most to least preferred."},
				{Name: "reserve_accounts", Type: array, Default: []string{}, Description: "Auth IDs used only when no other account has quota."},
				{Name: "disabled_accounts", Type: array, Default: []string{}, Description: "Auth IDs never chosen automatically (manual pin still works)."},
				{Name: "manual_fallback", Type: enum, Default: d.ManualFallback, EnumValues: []string{engine.FallbackAuto, engine.FallbackStrict}, Description: "When the pinned account is unavailable: auto routes automatically, strict rejects requests."},
				{Name: "quota_poll_minutes", Type: integer, Default: d.QuotaPollMinutes, Description: "How often idle accounts' quota is checked. 0 = live traffic only."},
				{Name: "model_level_limits", Type: boolean, Default: false, Description: "Scope usage-limit failures to the model that hit them (enable together with CLIProxyAPI's upstream.codex.model-level-cooling)."},
				{Name: "stale_after_minutes", Type: integer, Default: d.StaleAfterMinutes, Description: "Quota data older than this is flagged as stale."},
				{Name: "history_limit", Type: integer, Default: d.HistoryLimit, Description: "Routing history entries to keep."},
				{Name: "state_file", Type: str, Default: d.StateFile, Description: "Runtime state file (mode, pins, session bindings, history)."},
			},
		},
		Capabilities: registrationCaps{
			Scheduler:                 true,
			SchedulerAcrossPriorities: true,
			UsagePlugin:               true,
			ManagementAPI:             true,
		},
	}
}

// logo is a small route-lamp mark for the dashboard's plugin list.
const logo = "data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 32 32'%3E%3Crect width='32' height='32' rx='8' fill='%230b7a86'/%3E%3Cpath d='M7 22h7c3 0 4-2 5-6s2-6 6-6' stroke='%23fff' stroke-width='2.4' fill='none' stroke-linecap='round'/%3E%3Ccircle cx='25' cy='10' r='3' fill='%23fff'/%3E%3C/svg%3E"
