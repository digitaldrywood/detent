//go:build !windows

package claudecode

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/isolation"
)

func TestEffectiveIsolationRejectsWeakenedSettings(t *testing.T) {
	expected := sandboxSettings(t)
	for _, change := range []string{"none", "disabled", "unsandboxed", "domains", "sockets", "excluded commands"} {
		t.Run(change, func(t *testing.T) {
			encoded, _ := json.Marshal(expected)
			var effective map[string]any
			if err := json.Unmarshal(encoded, &effective); err != nil {
				t.Fatal(err)
			}
			sandbox := effective["sandbox"].(map[string]any)
			switch change {
			case "disabled":
				sandbox["enabled"] = false
			case "unsandboxed":
				sandbox["allowUnsandboxedCommands"] = true
			case "domains":
				sandbox["network"].(map[string]any)["allowedDomains"] = []any{"example.com"}
			case "sockets":
				sandbox["network"].(map[string]any)["allowAllUnixSockets"] = true
			case "excluded commands":
				sandbox["excludedCommands"] = []any{"curl"}
			}
			if err := VerifyEffectiveIsolation(expected, effective); (err == nil) != (change == "none") {
				t.Fatalf("verification = %v", err)
			}
		})
	}
}

func TestSandboxHandshakeVerifiesBeforePrompt(t *testing.T) {
	settings := sandboxSettings(t)
	for _, valid := range []bool{true, false} {
		expected := settings
		var effective any = settings
		if !valid {
			effective = map[string]any{"sandbox": map[string]any{"enabled": false}}
		}
		init := `{"type":"control_response","response":{"subtype":"success","request_id":"initialize","response":{}}}`
		reply, _ := json.Marshal(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": "get_settings", "response": map[string]any{"effective": effective}}})
		var input bytes.Buffer
		_, err := verifySandboxProcess(t.Context(), &input, strings.NewReader(init+"\n"+string(reply)+"\n"), expected)
		if (err == nil) != valid {
			t.Fatalf("handshake error = %v", err)
		}
		if strings.Contains(input.String(), `"type":"user"`) || !strings.Contains(input.String(), "get_settings") {
			t.Fatalf("unexpected handshake input: %s", input.String())
		}
	}
}

func sandboxSettings(t *testing.T) map[string]any {
	t.Helper()
	settings, err := IsolationSettings(isolation.Policy{Tier: isolation.Sandbox, WritableRoots: []string{t.TempDir()}})

	if err != nil {
		t.Fatal(err)
	}
	return settings
}

func TestAgentBackendToolConnectionSandbox(t *testing.T) {
	for _, test := range []toolConnectionCase{{"sandbox", false, false, isolation.Sandbox}, {"sandbox-read-only", true, false, isolation.Sandbox}, {"sandbox-text-only", true, true, isolation.Sandbox}} {
		t.Run(test.name, func(t *testing.T) { testAgentBackendToolConnection(t, test) })
	}
}
