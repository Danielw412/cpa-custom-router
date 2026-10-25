package engine

import (
	"encoding/json"
	"strings"
	"unicode"
)

// SessionRef identifies the conversation a request belongs to and, for
// subagents and forks, the parent conversation whose account it should share.
type SessionRef struct {
	ID       string
	ParentID string
	Subagent bool
}

// Header-only port of CLIProxyAPI's session.ExtractSessionInfo
// (sdk/cliproxy/session/info.go, v8.0.42). The scheduler capability receives
// request headers and metadata but not the body, so only header and metadata
// sources are available. IDs are produced in the same "<kind>:<id>" form so
// they line up with CLIProxyAPI's own logs; session_test.go cross-checks the
// two implementations.
func ExtractSession(headers map[string][]string, metadata map[string]any) (SessionRef, bool) {
	h := headerLookup(headers)

	// 1. Claude Code headers.
	if sid := h.get("X-Claude-Code-Session-Id"); sid != "" {
		agentID := h.get("X-Claude-Code-Agent-Id")
		parentAgentID := h.get("X-Claude-Code-Parent-Agent-Id")
		if agentID != "" && agentID != "main" {
			ref := SessionRef{ID: "claude:" + sid + ":agent:" + agentID, ParentID: "claude:" + sid, Subagent: true}
			if parentAgentID != "" && parentAgentID != "main" && parentAgentID != agentID {
				ref.ParentID = "claude:" + sid + ":agent:" + parentAgentID
			}
			return finalize(ref)
		}
		return finalize(SessionRef{ID: "claude:" + sid})
	}

	// 3. Codex / OpenAI CLI headers, including multi-agent v2 turn metadata.
	sid := h.get("Session-Id")
	if sid == "" {
		sid = h.get("Session_id")
	}
	tid := h.get("Thread-Id")
	if tid == "" {
		tid = h.get("Thread_id")
	}
	var turn map[string]any
	if raw := h.raw("X-Codex-Turn-Metadata"); raw != "" {
		_ = json.Unmarshal([]byte(raw), &turn)
	}
	turnStr := func(key string) string {
		if turn == nil {
			return ""
		}
		if v, ok := turn[key].(string); ok {
			return normalizeID(v)
		}
		return ""
	}
	if sid == "" {
		sid = turnStr("session_id")
	}
	if tid == "" {
		tid = turnStr("thread_id")
	}
	if sid != "" || tid != "" {
		parentThread := h.get("X-Codex-Parent-Thread-Id")
		if parentThread == "" {
			parentThread = turnStr("parent_thread_id")
		}
		forkedFrom := turnStr("forked_from_thread_id")
		if forkedFrom == "" {
			forkedFrom = turnStr("forked_from_id")
		}
		agentName := ""
		if turn != nil {
			if raw, ok := turn["agent_name"].(string); ok {
				raw = strings.TrimPrefix(raw, "/root/")
				raw = strings.TrimPrefix(raw, "/")
				raw = normalizeID(strings.TrimSpace(raw))
				if raw != "" && raw != "root" && raw != "main" {
					agentName = raw
				}
			}
		}
		sub := h.get("X-Openai-Subagent")
		subagentSignal := sub != "" && !strings.EqualFold(sub, "false") && sub != "0"
		if turn != nil {
			if kind, _ := turn["subagent_kind"].(string); kind == "thread_spawn" {
				subagentSignal = true
			}
		}

		if forkedFrom != "" {
			forkID := tid
			if forkID == "" {
				forkID = sid
			}
			if forkID == forkedFrom && sid != "" && sid != forkedFrom {
				forkID = sid
			}
			return finalize(SessionRef{ID: "codex:" + forkID, ParentID: "codex:" + forkedFrom})
		}
		if subagentSignal || (tid != "" && sid != "" && tid != sid) || (parentThread != "" && parentThread != tid && parentThread != sid) {
			child := tid
			if child == "" {
				child = sid
			}
			parent := parentThread
			if parent == "" {
				parent = sid
			}
			if agentName != "" && sid != "" {
				ref := SessionRef{ID: "codex:" + sid + ":agent:" + agentName, Subagent: true}
				if parent != "" {
					ref.ParentID = "codex:" + parent
				}
				return finalize(ref)
			}
			ref := SessionRef{ID: "codex:" + child, Subagent: true}
			if parent != "" && parent != child {
				ref.ParentID = "codex:" + parent
			}
			return finalize(ref)
		}
		id := sid
		if id == "" {
			id = tid
		}
		ref := SessionRef{ID: "codex:" + id}
		if parentThread != "" && parentThread != id {
			ref.ParentID = "codex:" + parentThread
			ref.Subagent = true
		}
		return finalize(ref)
	}

	// 4. Antigravity CLI.
	if sid := h.get("X-Http-Session-Id"); sid != "" {
		return finalize(withParent("agy:", sid, h.first("X-Parent-Session-ID", "X-Parent-ID")))
	}
	// 5./6. Generic session headers, in CLIProxyAPI's precedence order.
	generic := []struct {
		header  string
		prefix  string
		parents []string
	}{
		{"X-Session-ID", "header:", []string{"X-Parent-Session-ID", "X-Parent-ID"}},
		{"X-Session-Affinity", "affinity:", []string{"X-Parent-Session-Affinity", "X-Parent-Session-ID", "X-Parent-ID"}},
		{"X-Slot-Session-Id", "slot:", []string{"X-Parent-Slot-Session-Id", "X-Parent-Session-ID", "X-Parent-ID"}},
		{"X-Task-ID", "task:", []string{"X-Parent-Task-ID", "X-Parent-ID"}},
		{"X-Conversation-Id", "conv:", []string{"X-Parent-Conversation-Id", "X-Parent-ID"}},
		{"X-Thread-Id", "thread:", []string{"X-Parent-Thread-Id", "X-Parent-ID"}},
		{"X-Client-Request-Id", "clientreq:", []string{"X-Parent-Session-ID", "X-Parent-ID"}},
	}
	for _, g := range generic {
		if sid := h.get(g.header); sid != "" {
			return finalize(withParent(g.prefix, sid, h.first(g.parents...)))
		}
	}

	// 12. Long-lived downstream execution session (e.g. one Responses websocket).
	if metadata != nil {
		if v, ok := metadata["execution_session_id"].(string); ok {
			if id := normalizeID(v); id != "" {
				return finalize(SessionRef{ID: "execution:" + id})
			}
		}
	}
	return SessionRef{}, false
}

func withParent(prefix, sid, parent string) SessionRef {
	ref := SessionRef{ID: prefix + sid}
	if parent != "" && parent != sid {
		ref.ParentID = prefix + parent
		ref.Subagent = true
	}
	return ref
}

func finalize(ref SessionRef) (SessionRef, bool) {
	if ref.ID == "" {
		return SessionRef{}, false
	}
	if ref.ParentID == ref.ID {
		ref.ParentID = ""
	}
	return ref, true
}

type headerLookup map[string][]string

// raw returns the first value of a header, matched case-insensitively.
func (h headerLookup) raw(name string) string {
	for k, vs := range h {
		if strings.EqualFold(k, name) && len(vs) > 0 {
			return strings.TrimSpace(vs[0])
		}
	}
	return ""
}

func (h headerLookup) get(name string) string {
	for k, vs := range h {
		if !strings.EqualFold(k, name) {
			continue
		}
		for _, v := range vs {
			if n := normalizeID(v); n != "" {
				return n
			}
		}
	}
	return ""
}

func (h headerLookup) first(names ...string) string {
	for _, n := range names {
		if v := h.get(n); v != "" {
			return v
		}
	}
	return ""
}

// normalizeID mirrors CLIProxyAPI's NormalizeExplicitID.
func normalizeID(raw string) string {
	for _, r := range raw {
		if unicode.IsControl(r) {
			return ""
		}
	}
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 256 {
		return ""
	}
	return raw
}
