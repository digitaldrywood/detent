package recoveryfingerprint

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestToolTracksExecutable(t *testing.T) {
	t.Parallel()

	name := "agent-tool"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("first"), 0o700); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	first, err := Tool(path + " app-server")
	if err != nil {
		t.Fatalf("Tool() error = %v", err)
	}
	if err := os.WriteFile(path, []byte("second-version"), 0o700); err != nil {
		t.Fatalf("WriteFile() update error = %v", err)
	}
	second, err := Tool(path + " app-server")
	if err != nil {
		t.Fatalf("Tool() update error = %v", err)
	}
	if first == second {
		t.Fatalf("fingerprint = %q after executable change", second)
	}
}

func TestToolReportsMissingExecutable(t *testing.T) {
	t.Parallel()

	fingerprint, err := Tool(filepath.Join(t.TempDir(), "missing-agent"))
	if err == nil || fingerprint == "" {
		t.Fatalf("Tool() = %q, %v, want durable missing-tool fingerprint", fingerprint, err)
	}
}
