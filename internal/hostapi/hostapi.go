// Package hostapi wraps CLIProxyAPI host callbacks (host.auth.*, host.http.*,
// host.log) behind a small, testable client. The transport (the C function
// pointer supplied at plugin init) is injected as a Caller.
package hostapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Danielw412/cpa-custom-router/internal/engine"
)

// Caller invokes one host callback and returns the raw JSON envelope.
type Caller func(method string, request []byte) ([]byte, error)

// Client is a typed host callback client.
type Client struct {
	call Caller
}

// New returns a client using the given transport.
func New(call Caller) *Client { return &Client{call: call} }

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (c *Client) invoke(method string, req any, out any) error {
	if c == nil || c.call == nil {
		return errors.New("host callbacks unavailable")
	}
	raw, err := json.Marshal(req)
	if err != nil {
		return err
	}
	resp, err := c.call(method, raw)
	if err != nil {
		return err
	}
	var env envelope
	if err := json.Unmarshal(resp, &env); err != nil {
		return fmt.Errorf("%s: decode envelope: %w", method, err)
	}
	if !env.OK {
		if env.Error != nil {
			return fmt.Errorf("%s: %s: %s", method, env.Error.Code, env.Error.Message)
		}
		return fmt.Errorf("%s: host returned failure", method)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(env.Result, out); err != nil {
		return fmt.Errorf("%s: decode result: %w", method, err)
	}
	return nil
}

type authFile struct {
	ID             string    `json:"id"`
	AuthIndex      string    `json:"auth_index"`
	Name           string    `json:"name"`
	Type           string    `json:"type"`
	Provider       string    `json:"provider"`
	Label          string    `json:"label"`
	Status         string    `json:"status"`
	Disabled       bool      `json:"disabled"`
	Unavailable    bool      `json:"unavailable"`
	NextRetryAfter time.Time `json:"next_retry_after"`
	Email          string    `json:"email"`
	AccountType    string    `json:"account_type"`
}

// AuthList returns credentials from host.auth.list (no secrets are included
// in this callback's payload).
func (c *Client) AuthList() ([]engine.HostAccount, error) {
	var resp struct {
		Files []authFile `json:"files"`
	}
	if err := c.invoke("host.auth.list", map[string]any{}, &resp); err != nil {
		return nil, err
	}
	out := make([]engine.HostAccount, 0, len(resp.Files))
	for _, f := range resp.Files {
		provider := f.Provider
		if provider == "" {
			provider = f.Type
		}
		id := f.ID
		if id == "" {
			id = f.Name
		}
		out = append(out, engine.HostAccount{
			ID:          id,
			AuthIndex:   f.AuthIndex,
			Provider:    provider,
			Label:       f.Label,
			Email:       f.Email,
			Plan:        f.AccountType,
			Disabled:    f.Disabled,
			Unavailable: f.Unavailable,
			Status:      f.Status,
			NextRetry:   f.NextRetryAfter,
		})
	}
	return out, nil
}

// CodexCredential holds the two fields needed for the usage endpoint. It is
// kept only for the duration of one poll and is never logged or returned.
type CodexCredential struct {
	AccessToken string
	AccountID   string
}

// CodexCredential reads the credential file through host.auth.get.
func (c *Client) CodexCredential(authIndex string) (CodexCredential, error) {
	var resp struct {
		JSON json.RawMessage `json:"json"`
	}
	if err := c.invoke("host.auth.get", map[string]string{"auth_index": authIndex}, &resp); err != nil {
		return CodexCredential{}, err
	}
	var cred struct {
		AccessToken string `json:"access_token"`
		AccountID   string `json:"account_id"`
		Type        string `json:"type"`
	}
	if err := json.Unmarshal(resp.JSON, &cred); err != nil {
		return CodexCredential{}, errors.New("credential is not JSON")
	}
	if cred.Type != "" && !strings.EqualFold(cred.Type, "codex") {
		return CodexCredential{}, errors.New("not a codex credential")
	}
	if cred.AccessToken == "" {
		return CodexCredential{}, errors.New("credential has no access token")
	}
	return CodexCredential{AccessToken: cred.AccessToken, AccountID: cred.AccountID}, nil
}

// HTTPResponse is the subset of host.http.do's result the plugin uses.
type HTTPResponse struct {
	StatusCode int
	Headers    map[string][]string
	Body       []byte
}

// HTTPGet performs a GET through CLIProxyAPI's transport (honouring its proxy
// settings). host.http.do has no built-in deadline, so the call is bounded by
// opening a cancelable operation and cancelling it on timeout.
func (c *Client) HTTPGet(ctx context.Context, url string, headers map[string][]string, timeout time.Duration) (HTTPResponse, error) {
	var op struct {
		OperationID string `json:"operation_id"`
	}
	if err := c.invoke("host.http.operation_open", map[string]any{}, &op); err != nil {
		return HTTPResponse{}, err
	}
	type result struct {
		resp HTTPResponse
		err  error
	}
	done := make(chan result, 1)
	go func() {
		var resp HTTPResponse
		err := c.invoke("host.http.do", map[string]any{
			"operation_id": op.OperationID,
			"method":       "GET",
			"url":          url,
			"headers":      headers,
		}, &resp)
		done <- result{resp, err}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case r := <-done:
		return r.resp, r.err
	case <-timer.C:
	case <-ctx.Done():
	}
	_ = c.invoke("host.http.cancel", map[string]any{"operation_id": op.OperationID}, nil)
	select {
	case r := <-done:
		if r.err == nil {
			return r.resp, nil
		}
	case <-time.After(5 * time.Second):
	}
	return HTTPResponse{}, fmt.Errorf("request timed out after %s", timeout)
}

// Log writes to CLIProxyAPI's log.
func (c *Client) Log(level, message string, fields map[string]any) {
	_ = c.invoke("host.log", map[string]any{"level": level, "message": message, "fields": fields}, nil)
}

// ModelResponse is the subset of host.model.execute's result the plugin uses.
type ModelResponse struct {
	StatusCode int                 `json:"status_code"`
	Headers    map[string][]string `json:"headers"`
	Body       []byte              `json:"body"`
}

// ModelExecute runs one non-streaming model request through CLIProxyAPI's
// own pipeline (routing, translation, usage accounting), locked to authID.
// host.model.execute cannot be cancelled, so the wait is bounded instead; a
// request that outlives the timeout finishes in the background.
func (c *Client) ModelExecute(protocol, model string, body []byte, headers map[string][]string, authID string, timeout time.Duration) (ModelResponse, error) {
	type result struct {
		resp ModelResponse
		err  error
	}
	done := make(chan result, 1)
	go func() {
		var resp ModelResponse
		err := c.invoke("host.model.execute", map[string]any{
			"entry_protocol": protocol,
			"exit_protocol":  protocol,
			"model":          model,
			"stream":         false,
			"body":           body,
			"headers":        headers,
			"auth_id":        authID,
		}, &resp)
		done <- result{resp, err}
	}()
	select {
	case r := <-done:
		return r.resp, r.err
	case <-time.After(timeout):
		return ModelResponse{}, fmt.Errorf("no response within %s", timeout)
	}
}
