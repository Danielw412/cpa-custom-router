package service

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Danielw412/cpa-custom-router/internal/engine"
)

const (
	starterIdleWait = 30 * time.Second
	starterTimeout  = 90 * time.Second
	starterProtocol = "openai-response"
)

// StarterSettleWait is how long to wait before re-reading quota after a
// window start (a variable so tests can shorten it).
var StarterSettleWait = 4 * time.Second

// starterLoop sends scheduled and automatic window starts at their due time.
func (s *Service) starterLoop(ctx context.Context, done chan struct{}) {
	defer close(done)
	select {
	case <-ctx.Done():
		return
	case <-time.After(InitialLoopDelay):
	}
	for {
		s.runDueStarts(ctx)
		wait := starterIdleWait
		if next := s.Engine.NextStartAt(); !next.IsZero() {
			if d := time.Until(next); d < wait {
				wait = d
			}
		}
		if wait < 0 {
			wait = 0
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-s.startWake:
			timer.Stop()
		case <-timer.C:
		}
	}
}

func (s *Service) runDueStarts(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			s.setErr(fmt.Sprintf("window start recovered from panic: %v", r))
		}
	}()
	for _, p := range s.Engine.DueStarts() {
		if ctx.Err() != nil {
			return
		}
		s.Engine.RecordStart(s.startWindow(ctx, p))
	}
	s.logEvents()
	s.Flush()
}

// startWindow sends one tiny request to start an account's 5-hour window,
// unless it is already running, and measures what it cost. The request is not
// part of any session or thread (no session headers, store=false) and its
// text is never kept.
func (s *Service) startWindow(ctx context.Context, p engine.StartPlan) engine.StartResult {
	settings, _ := s.Engine.Settings()
	r := engine.StartResult{At: time.Now(), Account: p.Account, Source: p.Source, Model: settings.WindowStartModel, Effort: settings.WindowStartEffort}
	authIndex, ok := s.Engine.StartTarget(p.Account)
	if !ok {
		r.Status, r.Detail = engine.StartFailed, "account is no longer in CLIProxyAPI"
		return r
	}
	// Fresh numbers first, so a running window is never restarted needlessly.
	s.refreshQuota(ctx, p.Account, authIndex)
	if state, reset := s.Engine.WindowState(p.Account); state == engine.WindowRunning {
		r.Status, r.ResetAt = engine.StartRunning, reset
		return r
	}
	r.ShortBefore, r.LongBefore = s.Engine.UsedPercents(p.Account)

	// A fresh random ID per start, used by CLIProxyAPI as the upstream
	// session/cache key. Without it CLIProxyAPI derives one from the
	// (identical) prompt, so every account's start would share the same ID.
	oneOff := randomUUID()
	body, _ := json.Marshal(map[string]any{
		"prompt_cache_key": oneOff,
		"model":            settings.WindowStartModel,
		"instructions":     "Reply with the single word OK.",
		"input": []any{map[string]any{
			"role":    "user",
			"content": []any{map[string]any{"type": "input_text", "text": "OK"}},
		}},
		"reasoning": map[string]any{"effort": settings.WindowStartEffort},
		"store":     false,
		"stream":    false,
	})
	headers := map[string][]string{"Session_id": {oneOff}}
	// A pause or slow down is for agents' requests, not for starts you scheduled.
	s.held.setExempt(oneOff, true)
	defer s.held.setExempt(oneOff, false)
	sent := time.Now()
	resp, err := s.Host.ModelExecute(starterProtocol, settings.WindowStartModel, body, headers, p.Account, starterTimeout)
	if err == nil && resp.StatusCode != 0 && resp.StatusCode != 200 {
		err = fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if err != nil {
		r.Status, r.Detail = engine.StartFailed, shortError(err)
		return r
	}
	var out struct {
		Usage struct {
			InputTokens        int64 `json:"input_tokens"`
			OutputTokens       int64 `json:"output_tokens"`
			InputTokensDetails struct {
				CachedTokens int64 `json:"cached_tokens"`
			} `json:"input_tokens_details"`
			OutputTokensDetails struct {
				ReasoningTokens int64 `json:"reasoning_tokens"`
			} `json:"output_tokens_details"`
		} `json:"usage"`
	}
	_ = json.Unmarshal(resp.Body, &out)
	r.InputTokens = out.Usage.InputTokens
	r.CachedTokens = out.Usage.InputTokensDetails.CachedTokens
	r.OutputTokens = out.Usage.OutputTokens
	r.ReasoningTokens = out.Usage.OutputTokensDetails.ReasoningTokens

	// Let the provider's counters settle, then measure again.
	select {
	case <-ctx.Done():
	case <-time.After(StarterSettleWait):
	}
	s.refreshQuota(ctx, p.Account, authIndex)
	r.ShortAfter, r.LongAfter = s.Engine.UsedPercents(p.Account)
	r.Status = engine.StartStarted
	r.ResetAt = sent.Add(5 * time.Hour)
	if state, reset := s.Engine.WindowState(p.Account); state == engine.WindowRunning && !reset.IsZero() {
		r.ResetAt = reset
	}
	return r
}

// refreshQuota polls the Codex usage endpoint for one account, if possible.
func (s *Service) refreshQuota(ctx context.Context, id, authIndex string) {
	if authIndex == "" {
		return // e.g. API-key credentials: rely on response headers instead
	}
	q, err := s.pollOne(ctx, engine.PollTarget{ID: id, AuthIndex: authIndex})
	s.Engine.ApplyPolledQuota(id, q, err)
}

func randomUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func shortError(err error) string {
	msg := strings.TrimSpace(err.Error())
	if i := strings.Index(msg, "host_call_failed: "); i >= 0 {
		msg = msg[i+len("host_call_failed: "):]
	}
	if len(msg) > 160 {
		msg = msg[:160] + "…"
	}
	return msg
}
