//go:build !windows

package cli

import (
	"testing"

	"github.com/digitaldrywood/detent/internal/isolation"
)

func TestSSHLocalTargetIntegration(t *testing.T) {

	testSSHWorkerLifecycle(t, true, true, isolation.NativeTrusted)
}
