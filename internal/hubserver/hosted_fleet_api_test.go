package hubserver

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/runnerauth"
)

func TestHostedFleetRunnerViewBackendIsolationJSON(t *testing.T) {
	tests := []struct {
		name      string
		isolation map[string][]string
		want      string
	}{
		{name: "nil tiers become empty list", isolation: map[string][]string{"native/codex": nil}, want: `{"native/codex":[]}`},
		{name: "populated tiers kept", isolation: map[string][]string{"codex": {"sandbox", "native-trusted"}}, want: `{"codex":["sandbox","native-trusted"]}`},
		{name: "no backends", isolation: nil, want: `{}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			view := hostedFleetRunnerView(runnerauth.Runner{BackendIsolation: test.isolation}, "test", nil, time.Unix(0, 0))
			got, err := json.Marshal(view.BackendIsolation)
			if err != nil {
				t.Fatalf("marshal backend isolation: %v", err)
			}
			if string(got) != test.want {
				t.Fatalf("backend_isolation = %s, want %s", got, test.want)
			}
		})
	}
}
