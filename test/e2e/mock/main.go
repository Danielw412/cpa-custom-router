// Mock Codex upstream for end-to-end tests of custom-quota-router.
//
// Serves POST /v1/responses (SSE) and GET /v1/responses (WebSocket upgrade)
// like chatgpt.com's Codex backend, including x-codex-* rate-limit headers,
// codex.rate_limits websocket events and usage_limit_reached errors, per API
// key. Drivers control quotas via POST /_control and read GET /_log to see
// which key served each turn. No real accounts are involved.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type keyState struct {
	Short     float64 `json:"short"`
	Long      float64 `json:"long"`
	Exhausted bool    `json:"exhausted"`
}

type logEntry struct {
	Key       string  `json:"key"`
	Session   string  `json:"session"`
	Transport string  `json:"transport"`
	Status    int     `json:"status"`
	Conn      int     `json:"conn,omitempty"`
	Model     string  `json:"model,omitempty"`
	Effort    string  `json:"effort,omitempty"`
	Store     *bool   `json:"store,omitempty"`
	CacheKey  string  `json:"cache_key,omitempty"`
	T         float64 `json:"t"`
}

type reqInfo struct {
	Model     string `json:"model"`
	Store     *bool  `json:"store"`
	CacheKey  string `json:"prompt_cache_key"`
	Reasoning struct {
		Effort string `json:"effort"`
	} `json:"reasoning"`
}

var (
	mu       sync.Mutex
	states   = map[string]*keyState{}
	entries  []logEntry
	connSeq  int
	upgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
)

func st(key string) *keyState {
	if states[key] == nil {
		states[key] = &keyState{Short: 10, Long: 20}
	}
	return states[key]
}

func record(e logEntry) {
	e.T = float64(time.Now().UnixMilli()) / 1000
	entries = append(entries, e)
}

func quotaHeaders(s keyState) map[string]string {
	short := s.Short
	if s.Exhausted {
		short = 100
	}
	return map[string]string{
		"x-codex-primary-used-percent":          fmt.Sprint(short),
		"x-codex-primary-window-minutes":        "300",
		"x-codex-primary-reset-after-seconds":   "1800",
		"x-codex-secondary-used-percent":        fmt.Sprint(s.Long),
		"x-codex-secondary-window-minutes":      "10080",
		"x-codex-secondary-reset-after-seconds": "400000",
		"x-codex-plan-type":                     "plus",
	}
}

func events(key, model string) []map[string]any {
	id := fmt.Sprintf("resp_%d", time.Now().UnixNano())
	text := "ok from " + key
	msg := map[string]any{"type": "message", "id": "msg_1", "role": "assistant", "status": "completed",
		"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}}}
	return []map[string]any{
		{"type": "response.created", "sequence_number": 0, "response": map[string]any{"id": id, "object": "response", "status": "in_progress", "model": model, "output": []any{}}},
		{"type": "response.output_item.added", "sequence_number": 1, "output_index": 0, "item": map[string]any{"type": "message", "id": "msg_1", "role": "assistant", "status": "in_progress", "content": []any{}}},
		{"type": "response.output_text.delta", "sequence_number": 2, "item_id": "msg_1", "output_index": 0, "content_index": 0, "delta": text},
		{"type": "response.output_item.done", "sequence_number": 3, "output_index": 0, "item": msg},
		{"type": "response.completed", "sequence_number": 4, "response": map[string]any{"id": id, "object": "response", "status": "completed", "model": model,
			"output": []any{msg}, "usage": map[string]any{"input_tokens": 12, "output_tokens": 3, "total_tokens": 15,
				"input_tokens_details": map[string]any{"cached_tokens": 0}, "output_tokens_details": map[string]any{"reasoning_tokens": 0}}}},
	}
}

func usageLimitError() map[string]any {
	return map[string]any{"type": "usage_limit_reached", "message": "The usage limit has been reached", "resets_in_seconds": 1800}
}

func sessionOf(r *http.Request) string {
	for _, h := range []string{"Session_id", "Session-Id"} {
		if v := r.Header.Get(h); v != "" {
			return v
		}
	}
	return ""
}

func handleResponses(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if websocket.IsWebSocketUpgrade(r) {
		serveWS(w, r, key)
		return
	}
	var req reqInfo
	_ = json.NewDecoder(r.Body).Decode(&req)
	mu.Lock()
	s := *st(key)
	if s.Exhausted {
		record(logEntry{Key: key, Session: sessionOf(r), Transport: "http", Status: 429, Model: req.Model, Effort: req.Reasoning.Effort, Store: req.Store})
		mu.Unlock()
		for k, v := range quotaHeaders(s) {
			w.Header().Set(k, v)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(429)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": usageLimitError()})
		return
	}
	record(logEntry{Key: key, Session: sessionOf(r), Transport: "http", Status: 200, Model: req.Model, Effort: req.Reasoning.Effort, Store: req.Store, CacheKey: req.CacheKey})
	st(key).Short += 0.5
	mu.Unlock()
	for k, v := range quotaHeaders(s) {
		w.Header().Set(k, v)
	}
	w.Header().Set("Content-Type", "text/event-stream")
	fl, _ := w.(http.Flusher)
	for _, ev := range events(key, req.Model) {
		raw, _ := json.Marshal(ev)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev["type"], raw)
		if fl != nil {
			fl.Flush()
		}
	}
}

func serveWS(w http.ResponseWriter, r *http.Request, key string) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	mu.Lock()
	connSeq++
	id := connSeq
	mu.Unlock()
	session := sessionOf(r)
	for {
		_, payload, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var req struct {
			Type  string `json:"type"`
			Model string `json:"model"`
		}
		_ = json.Unmarshal(payload, &req)
		if req.Type != "response.create" {
			continue
		}
		mu.Lock()
		s := *st(key)
		if s.Exhausted {
			record(logEntry{Key: key, Session: session, Transport: "ws", Status: 429, Conn: id})
			mu.Unlock()
			hdr := map[string]any{}
			for k, v := range quotaHeaders(s) {
				hdr[k] = v
			}
			_ = conn.WriteJSON(map[string]any{"type": "error", "status": 429, "error": usageLimitError(), "headers": hdr})
			continue
		}
		record(logEntry{Key: key, Session: session, Transport: "ws", Status: 200, Conn: id})
		st(key).Short += 0.5
		mu.Unlock()
		_ = conn.WriteJSON(map[string]any{"type": "codex.rate_limits", "plan_type": "plus", "rate_limits": map[string]any{
			"allowed": true, "limit_reached": false,
			"primary":   map[string]any{"used_percent": s.Short, "window_minutes": 300, "reset_after_seconds": 1800},
			"secondary": map[string]any{"used_percent": s.Long, "window_minutes": 10080, "reset_after_seconds": 400000},
		}})
		for _, ev := range events(key, req.Model) {
			if err := conn.WriteJSON(ev); err != nil {
				return
			}
		}
	}
}

func main() {
	addr := "127.0.0.1:18400"
	if len(os.Args) > 1 {
		addr = os.Args[1]
	}
	http.HandleFunc("/v1/responses", handleResponses)
	http.HandleFunc("/_control", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		for k, v := range body {
			if k == "_reset_log" {
				entries = nil
				continue
			}
			_ = json.Unmarshal(v, st(k))
		}
		mu.Unlock()
		w.Write([]byte(`{"ok":true}`))
	})
	http.HandleFunc("/_log", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		_ = json.NewEncoder(w).Encode(entries)
	})
	log.Fatal(http.ListenAndServe(addr, nil))
}
