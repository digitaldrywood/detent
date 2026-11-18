package claudecode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/runner"
)

func TestInitBillingObservation(t *testing.T) {
	t.Parallel()
	for _, test := range []struct{ source, auth, want string }{
		{"oauth", "", "subscription"}, {"", "claude.ai", "subscription"},
		{"ANTHROPIC_API_KEY", "", "api_key"}, {"apiKeyHelper", "", "api_key"},
		{"/login managed key", "", "api_key"}, {"user", "", "api_key"},
		{"project", "", "api_key"}, {"org", "", "api_key"}, {"temporary", "", "api_key"},
		{"none", "bedrock", "api_key"}, {"none", "", "unknown"},
		{"", "", "unknown"}, {"unrecognized", "", "unknown"},
	} {
		t.Run(test.source+"/"+test.auth, func(t *testing.T) {
			state := &turnState{}
			var got string
			err := state.apply(claudeEvent{Type: "system", Subtype: "init", SessionID: "session", APIKeySource: test.source, AuthSource: test.auth}, false, func(update runner.AgentUpdate) error { got = update.Billing; return nil })
			if err != nil || got != test.want {
				t.Fatalf("billing = %q, err = %v, want %q", got, err, test.want)
			}
		})
	}
}

func TestNativeClaudeBilling(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, raw, want string
		fails           bool
	}{
		{"subscription", `{"loggedIn":true,"authMethod":"claude.ai","apiKeySource":"none","email":"private@example.com"}`, "subscription", false},
		{"oauth token", `{"loggedIn":true,"authMethod":"oauth_token"}`, "subscription", false},
		{"key overrides login", `{"loggedIn":true,"authMethod":"claude.ai","apiKeySource":"ANTHROPIC_API_KEY"}`, "api_key", false},
		{"third party", `{"loggedIn":true,"authMethod":"third_party"}`, "api_key", false},
		{"not signed in", `{"loggedIn":false,"authMethod":"none"}`, "unknown", false},
		{"old client", `{}`, "unknown", false},
		{"invalid", `private@example.com`, "unknown", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "status.json")
			if err := os.WriteFile(path, []byte(test.raw), 0o600); err != nil {
				t.Fatal(err)
			}
			backend, err := NewAgentBackend(Options{CommandFactory: catCommand(path, "", 0)})
			if err != nil {
				t.Fatal(err)
			}
			got, err := backend.BillingBasis(t.Context(), runner.AgentProcessRequest{})
			if got != test.want || (err != nil) != test.fails {
				t.Fatalf("billing = %q, err = %v, want %q", got, err, test.want)
			}
			if err != nil && strings.Contains(err.Error(), "private@example.com") {
				t.Fatal("native content leaked into error")
			}
		})
	}
}
