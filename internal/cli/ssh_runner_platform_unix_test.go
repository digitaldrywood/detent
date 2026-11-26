//go:build !windows

package cli

import (
	"testing"

	"github.com/digitaldrywood/detent/internal/isolation"
)

func TestSSHLocalTargetIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping SSH local target integration test in -short mode")
	}

	testSSHWorkerLifecycle(t, true, true, isolation.NativeTrusted)
}
