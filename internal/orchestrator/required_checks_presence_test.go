package orchestrator

import (
	"github.com/digitaldrywood/detent/internal/gate"
	"testing"
)

func TestCloneGateRequiredChecksPresence(t *testing.T) {
	for _, checks := range [][]string{nil, {}, {"Native"}} {
		cfg := cloneAutoPromoteConfig(AutoPromoteConfig{Gate: gate.Config{RequiredStatusChecks: checks}})
		if (cfg.Gate.RequiredStatusChecks == nil) != (checks == nil) {
			t.Fatalf("cloned checks=%#v, original=%#v", cfg.Gate.RequiredStatusChecks, checks)
		}
		if len(checks) > 0 {
			cfg.Gate.RequiredStatusChecks[0] = "changed"
			if checks[0] != "Native" {
				t.Fatal("clone shares required checks")
			}
		}
	}
}
