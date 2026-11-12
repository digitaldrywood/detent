package cli

import "testing"

func TestEnsureWindowsCodexProfileDirectoryLink(t *testing.T) {
	t.Parallel()
	testEnsureWindowsCodexProfileLink(t, true)
}
