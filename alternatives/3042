package hubserver

import (
	"path/filepath"
	"testing"
)

func TestRequestMetricsRequiresPostgres(t *testing.T) {
	owner := &database{path: filepath.Join(t.TempDir(), "hub.sqlite")}
	collector, err := owner.openRequestMetrics(t.Context(), "org_example")
	if collector != nil {
		t.Cleanup(func() { _ = collector.close() })
	}
	if err == nil {
		t.Fatal("metrics opened without PostgreSQL")
	}
}
