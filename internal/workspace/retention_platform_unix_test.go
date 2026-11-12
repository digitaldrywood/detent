//go:build !windows

package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRetentionQuarantineUnremovablePath(t *testing.T) {
	t.Parallel()

	backend := retentionBackend(t)
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	parent := filepath.Join(backend.root, ".detent/quarantine")
	quarantine := filepath.Join(parent, "workspace-"+now.Add(-4*24*time.Hour).Format(quarantineTimestampFormat))
	retentionFixture(t, filepath.Join(quarantine, "file"), now)
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(parent, 0o700); err != nil {
			t.Error(err)
		}
	})
	for range 2 {
		totals, err := backend.SweepRetention(t.Context(), RetentionRequest{Now: now})
		if err == nil || !strings.Contains(err.Error(), filepath.Join(".detent/quarantine", filepath.Base(quarantine))) {
			t.Fatalf("retention error = %v, want quarantine path", err)
		}
		if totals.Quarantine.Count != 0 {
			t.Fatalf("totals=%+v", totals)
		}
	}
}
