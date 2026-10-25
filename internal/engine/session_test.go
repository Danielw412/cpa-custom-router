package engine

import (
	"net/http"
	"testing"

	cpasession "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
)

// TestSessionParityWithCLIProxyAPI checks the header-only port against
// CLIProxyAPI's own extractor so plugin session IDs match the host's.
func TestSessionParityWithCLIProxyAPI(t *testing.T) {
	cases := []struct {
		name    string
		headers http.Header
		meta    map[string]any
	}{
		{"codex main", http.Header{"Session_id": {"019a-main"}}, nil},
		{"codex thread equals session", http.Header{"Session_id": {"s1"}, "Thread_id": {"s1"}}, nil},
		{"codex child thread", http.Header{"Session_id": {"s1"}, "Thread_id": {"t2"}}, nil},
		{"codex subagent header", http.Header{"Session_id": {"s1"}, "X-Openai-Subagent": {"review"}}, nil},
		{"codex parent thread header", http.Header{"Session_id": {"t9"}, "X-Codex-Parent-Thread-Id": {"s1"}}, nil},
		{"codex turn metadata spawn", http.Header{"X-Codex-Turn-Metadata": {`{"session_id":"s1","thread_id":"t3","subagent_kind":"thread_spawn","agent_name":"/root/explorer"}`}}, nil},
		{"codex turn metadata fork", http.Header{"X-Codex-Turn-Metadata": {`{"session_id":"s1","thread_id":"t4","forked_from_thread_id":"s1"}`}}, nil},
		{"claude main", http.Header{"X-Claude-Code-Session-Id": {"c1"}}, nil},
		{"claude agent", http.Header{"X-Claude-Code-Session-Id": {"c1"}, "X-Claude-Code-Agent-Id": {"a7"}}, nil},
		{"claude nested agent", http.Header{"X-Claude-Code-Session-Id": {"c1"}, "X-Claude-Code-Agent-Id": {"a7"}, "X-Claude-Code-Parent-Agent-Id": {"a3"}}, nil},
		{"generic session", http.Header{"X-Session-Id": {"g1"}, "X-Parent-Session-Id": {"g0"}}, nil},
		{"opencode affinity", http.Header{"X-Session-Affinity": {"o1"}}, nil},
		{"conversation", http.Header{"X-Conversation-Id": {"conv1"}}, nil},
		{"execution session", http.Header{}, map[string]any{"execution_session_id": "ws-123"}},
		{"none", http.Header{"User-Agent": {"x"}}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want, wantOK := cpasession.ExtractSessionInfo(tc.headers, nil, tc.meta)
			got, gotOK := ExtractSession(tc.headers, tc.meta)
			if wantOK != gotOK {
				t.Fatalf("ok mismatch: cpa=%v plugin=%v (cpa=%+v plugin=%+v)", wantOK, gotOK, want, got)
			}
			if !wantOK {
				return
			}
			if want.SessionID != got.ID || want.ParentSessionID != got.ParentID {
				t.Fatalf("mismatch:\n cpa    id=%q parent=%q\n plugin id=%q parent=%q", want.SessionID, want.ParentSessionID, got.ID, got.ParentID)
			}
		})
	}
}
