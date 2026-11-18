//go:build !windows

package artifact

import "testing"

func TestCaptureGitLiteralColonPaths(t *testing.T) {
	testCaptureGitLiteralPaths(t, []string{":(exclude)secret.txt"})
}
