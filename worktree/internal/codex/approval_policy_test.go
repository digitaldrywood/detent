package codex

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
)

func TestWireApprovalPolicyMapsDetentVocabularyOntoCodex(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		policy any
		want   any
	}{
		{name: "nil stays absent", policy: nil, want: nil},
		{name: "reject word becomes never", policy: "reject", want: "never"},
		{name: "reject word ignores case and spacing", policy: "  Reject ", want: "never"},
		{name: "default reject mapping becomes never", policy: map[string]any{
			"reject": map[string]any{"sandbox_approval": true, "rules": true, "mcp_elicitations": true},
		}, want: "never"},
		{name: "reject mapping ignores case", policy: map[string]any{"REJECT": map[string]any{}}, want: "never"},
		{name: "encoded reject mapping becomes never", policy: json.RawMessage(`{"reject":{"rules":true}}`), want: json.RawMessage(`"never"`)},
		{name: "never passes through", policy: "never", want: "never"},
		{name: "untrusted passes through", policy: "untrusted", want: "untrusted"},
		{name: "granular passes through", policy: "granular", want: "granular"},
		{name: "on-request passes through", policy: "on-request", want: "on-request"},
		{name: "yaml underscore spelling is canonicalised", policy: "on_request", want: "on-request"},
		{name: "encoded never keeps its bytes", policy: json.RawMessage(`"never"`), want: json.RawMessage(`"never"`)},
		{name: "unknown word passes through untouched", policy: "bypass", want: "bypass"},
		{name: "unknown mapping passes through untouched", policy: map[string]any{"allow": []any{map[string]any{"tool": "shell"}}},
			want: map[string]any{"allow": []any{map[string]any{"tool": "shell"}}}},
		{name: "multi key mapping passes through untouched", policy: map[string]any{"reject": map[string]any{}, "allow": []any{}},
			want: map[string]any{"reject": map[string]any{}, "allow": []any{}}},
		{name: "granular mapping passes through untouched", policy: json.RawMessage(`{"granular":{"rules":[]}}`), want: json.RawMessage(`{"granular":{"rules":[]}}`)},
		{name: "empty raw message passes through untouched", policy: json.RawMessage(``), want: json.RawMessage(``)},
		{name: "invalid json passes through untouched", policy: json.RawMessage(`{`), want: json.RawMessage(`{`)},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got := wireApprovalPolicy(test.policy)
			if !equalPolicies(got, test.want) {
				t.Fatalf("wireApprovalPolicy(%#v) = %#v, want %#v", test.policy, got, test.want)
			}
		})
	}
}

// equalPolicies compares two policies by their JSON encoding so a raw message
// and the value it decodes to are judged on what reaches Codex.
func equalPolicies(got any, want any) bool {
	if got == nil || want == nil {
		return got == nil && want == nil
	}
	gotRaw, gotIsRaw := got.(json.RawMessage)
	wantRaw, wantIsRaw := want.(json.RawMessage)
	if gotIsRaw || wantIsRaw {
		return gotIsRaw && wantIsRaw && string(gotRaw) == string(wantRaw)
	}
	gotEncoded, gotErr := json.Marshal(got)
	wantEncoded, wantErr := json.Marshal(want)
	if gotErr != nil || wantErr != nil {
		return false
	}
	return string(gotEncoded) == string(wantEncoded)
}

// TestAppServerTranslatesDefaultApprovalPolicy proves the default project
// config, which carries Detent's `reject` mapping, reaches Codex 0.154 as
// `never` on every request that carries an approval policy. Codex 0.154
// answers thread/start with "unknown variant `reject`" otherwise.
func TestAppServerTranslatesDefaultApprovalPolicy(t *testing.T) {
	t.Parallel()

	options := OptionsFromConfig(config.CodexOptions{
		ApprovalPolicy: config.Default().Codex.ApprovalPolicy,
		ThreadSandbox:  "workspace-write",
	})

	transport := newFakeAppServerTransport([]Message{
		responseMessage(t, 1, `{"userAgent":"codex-cli/0.154.0"}`),
		responseMessage(t, 2, `{"thread":{"id":"thread-1"},"model":"gpt-5-codex"}`),
		responseMessage(t, 3, `{"turn":{"id":"turn-1"}}`),
		notificationMessage(t, "turn/completed", `{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed"}}`),
	})
	server, err := NewAppServer(staticTransportFactory{transport: transport},
		WithReadTimeout(time.Second),
		WithTurnTimeout(time.Second),
	)
	if err != nil {
		t.Fatalf("NewAppServer() error = %v", err)
	}

	if _, err := server.RunTurn(t.Context(), RunTurnRequest{
		Workspace:      t.TempDir(),
		Prompt:         "hello",
		ApprovalPolicy: options.ApprovalPolicy,
		ThreadSandbox:  options.ThreadSandbox,
	}, nil); err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	sent := transport.sentMessages()
	if len(sent) != 4 {
		t.Fatalf("sent messages = %d, want 4", len(sent))
	}
	assertRequest(t, sent[2], 2, "thread/start")
	assertJSONContains(t, sent[2].Params, "approvalPolicy", "never")
	assertRequest(t, sent[3], 3, "turn/start")
	assertJSONContains(t, sent[3].Params, "approvalPolicy", "never")
}
