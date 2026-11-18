//go:build darwin || linux

package cli

import (
	"testing"
)

func TestLocalWorkerReleaseAndPolicyCoexistence(t *testing.T) {
	for _, scenario := range []string{"coexistence", "failures", "authority"} {
		t.Run(scenario, func(t *testing.T) { testLocalWorkerScenario(t, scenario) })
	}
}
