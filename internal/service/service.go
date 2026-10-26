// Package service wires the routing engine to CLIProxyAPI host callbacks:
// account roster refresh, active quota polling, persistence and logging.
package service

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/Danielw412/cpa-custom-router/internal/engine"
	"github.com/Danielw412/cpa-custom-router/internal/hostapi"
)

const (
	tickEvery       = 15 * time.Second
	rosterEvery     = 30 * time.Second
	pollTimeout     = 20 * time.Second
	maxPollsPerTick = 3
	codexUsageURL   = "https://chatgpt.com/backend-api/wham/usage"
	codexUserAgent  = "codex_cli_rs/0.164.0"
)

// Loop timing; variables so tests can shorten them.
var (
	InitialLoopDelay = 2 * time.Second
	TickEvery        = tickEvery
)

// Service owns the engine and its background loop.
type Service struct {
	Engine *engine.Engine
	Host   *hostapi.Client

	mu        sync.Mutex
	statePath string
	cancel    context.CancelFunc
	done      chan struct{}
	pollNow   chan struct{}
	startWake chan struct{}
	lastErr   string
	retired   bool
	held      *holder
}

// New creates a service from the plugin's YAML config and restores state.
func New(configYAML []byte, host *hostapi.Client) *Service {
	settings, warnings := engine.ParseSettings(configYAML)
	s := &Service{
		Engine:    engine.New(settings, warnings, time.Now),
		Host:      host,
		statePath: settings.StateFile,
		pollNow:   make(chan struct{}, 1),
		startWake: make(chan struct{}, 1),
		held:      newHolder(),
	}
	if st, ok, err := engine.LoadState(settings.StateFile); err != nil {
		s.lastErr = err.Error()
	} else if ok {
		s.Engine.Restore(st)
	}
	return s
}

// Reconfigure applies new YAML settings without losing runtime state.
func (s *Service) Reconfigure(configYAML []byte) {
	settings, warnings := engine.ParseSettings(configYAML)
	s.Engine.Configure(settings, warnings)
	s.mu.Lock()
	if settings.StateFile != s.statePath {
		s.statePath = settings.StateFile
		s.Engine.MarkDirty()
	}
	s.mu.Unlock()
	// Auto-start may have just been turned on: start idle windows now.
	s.WakeStarter()
}

// Start launches the background loop (idempotent).
func (s *Service) Start() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		return
	}
	s.retired = false
	s.held.start()
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.done = make(chan struct{})
	loopDone, starterDone := make(chan struct{}), make(chan struct{})
	go s.loop(ctx, loopDone)
	go s.starterLoop(ctx, starterDone)
	go func(done chan struct{}) {
		<-loopDone
		<-starterDone
		close(done)
	}(s.done)
}

// Stop halts the loop and flushes state (quiesce/shutdown). Held requests
// are let go first: the host waits for every call into this instance to
// return before it finishes shutting it down.
func (s *Service) Stop() {
	s.held.stop()
	s.mu.Lock()
	cancel, done := s.cancel, s.done
	s.cancel, s.done = nil, nil
	s.mu.Unlock()
	if cancel != nil {
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
		}
		// Events recorded since the last tick would otherwise never reach the log.
		s.logEvents()
	}
	s.Flush()
}

// Retire stops the loop, persists state one last time, and prevents any
// later flush, so a successor plugin instance (hot upgrade) owns the state
// file from now on. Start() undoes this if the host rolls the upgrade back.
func (s *Service) Retire() {
	s.Stop()
	s.mu.Lock()
	s.retired = true
	s.mu.Unlock()
}

// WakeStarter re-evaluates scheduled window starts now (after a change).
func (s *Service) WakeStarter() {
	select {
	case s.startWake <- struct{}{}:
	default:
	}
}

// RequestPoll asks the loop to refresh quotas for all accounts now.
func (s *Service) RequestPoll() {
	s.Engine.ForcePollAll()
	select {
	case s.pollNow <- struct{}{}:
	default:
	}
}

// Flush persists state if it changed.
func (s *Service) Flush() {
	st, dirty := s.Engine.Export()
	if !dirty {
		return
	}
	s.mu.Lock()
	path, retired := s.statePath, s.retired
	s.mu.Unlock()
	if retired {
		return
	}
	if err := engine.SaveState(path, st); err != nil {
		s.Engine.MarkDirty()
		s.setErr(fmt.Sprintf("save state: %v", err))
	}
}

// LastError returns the most recent background error (for the UI).
func (s *Service) LastError() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastErr
}

func (s *Service) setErr(msg string) {
	s.mu.Lock()
	s.lastErr = msg
	s.mu.Unlock()
}

func (s *Service) loop(ctx context.Context, done chan struct{}) {
	defer close(done)
	select {
	case <-ctx.Done():
		return
	case <-time.After(InitialLoopDelay):
	}
	var lastRoster time.Time
	ticker := time.NewTicker(TickEvery)
	defer ticker.Stop()
	for {
		s.iteration(ctx, &lastRoster)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.pollNow:
			lastRoster = time.Time{}
		}
	}
}

// iteration runs one maintenance pass; a panic is contained to this pass so
// the loop keeps refreshing, polling and persisting afterwards.
func (s *Service) iteration(ctx context.Context, lastRoster *time.Time) {
	defer func() {
		if r := recover(); r != nil {
			s.setErr(fmt.Sprintf("background task recovered from panic: %v", r))
		}
	}()
	if time.Since(*lastRoster) >= rosterEvery {
		s.refreshRoster()
		*lastRoster = time.Now()
	}
	s.Engine.Tick()
	s.pollDue(ctx)
	s.logEvents()
	s.Flush()
}

func (s *Service) refreshRoster() {
	list, err := s.Host.AuthList()
	if err != nil {
		s.setErr(fmt.Sprintf("refresh accounts: %v", err))
		return
	}
	s.Engine.ObserveAccounts(list)
}

func (s *Service) pollDue(ctx context.Context) {
	targets := s.Engine.PollTargets()
	for i, t := range targets {
		if i >= maxPollsPerTick || ctx.Err() != nil {
			return
		}
		q, err := s.pollOne(ctx, t)
		s.Engine.ApplyPolledQuota(t.ID, q, err)
	}
}

// pollOne reads the account's quota from the Codex usage endpoint (the same
// read-only endpoint CLIProxyAPI's dashboard uses). The access token is held
// only for this call; it is never logged, stored, or returned to clients.
func (s *Service) pollOne(ctx context.Context, t engine.PollTarget) (engine.Quota, error) {
	cred, err := s.Host.CodexCredential(t.AuthIndex)
	if err != nil {
		return engine.Quota{}, err
	}
	headers := map[string][]string{
		"Authorization": {"Bearer " + cred.AccessToken},
		"Accept":        {"application/json"},
		"User-Agent":    {codexUserAgent},
	}
	if cred.AccountID != "" {
		headers["Chatgpt-Account-Id"] = []string{cred.AccountID}
	}
	cred = hostapi.CodexCredential{}
	resp, err := s.Host.HTTPGet(ctx, codexUsageURL, headers, pollTimeout)
	headers = nil
	if err != nil {
		return engine.Quota{}, err
	}
	if resp.StatusCode != 200 {
		return engine.Quota{}, fmt.Errorf("usage endpoint returned HTTP %d", resp.StatusCode)
	}
	q, ok := engine.ParseUsageResponse(resp.Body, time.Now())
	if !ok {
		return engine.Quota{}, fmt.Errorf("usage endpoint response had no rate-limit windows")
	}
	return q, nil
}

func (s *Service) logEvents() {
	for _, ev := range s.Engine.DrainEvents() {
		fields := map[string]any{"plugin": "custom-quota-router", "kind": ev.Kind}
		if ev.From != "" {
			fields["from"] = ev.From
		}
		if ev.To != "" {
			fields["to"] = ev.To
		}
		s.Host.Log("info", "quota-router: "+ev.Reason, fields)
	}
}
