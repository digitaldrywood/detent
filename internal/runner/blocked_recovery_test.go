package runner

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestBlockedRecoveryToolFingerprintTracksExecutable(t *testing.T) {
	t.Parallel()

	name := "agent-tool"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("first"), 0o700); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	first, err := blockedRecoveryToolFingerprint(path + " app-server")
	if err != nil {
		t.Fatalf("blockedRecoveryToolFingerprint() error = %v", err)
	}
	if err := os.WriteFile(path, []byte("second-version"), 0o700); err != nil {
		t.Fatalf("WriteFile() update error = %v", err)
	}
	second, err := blockedRecoveryToolFingerprint(path + " app-server")
	if err != nil {
		t.Fatalf("blockedRecoveryToolFingerprint() update error = %v", err)
	}
	if first == second {
		t.Fatalf("fingerprint = %q after executable change", second)
	}
}

func TestBlockedRecoveryToolFingerprintReportsMissingExecutable(t *testing.T) {
	t.Parallel()

	fingerprint, err := blockedRecoveryToolFingerprint(filepath.Join(t.TempDir(), "missing-agent"))
	if err == nil || fingerprint == "" {
		t.Fatalf("blockedRecoveryToolFingerprint() = %q, %v, want durable missing-tool fingerprint", fingerprint, err)
	}
}
