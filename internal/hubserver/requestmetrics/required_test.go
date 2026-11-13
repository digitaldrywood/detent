package requestmetrics

import (
	"testing"
)

func TestRequestMetricsRequiresPostgres(t *testing.T) {
	collector, err := New(Deps{Organization: "org_example"})
	if collector != nil {
		t.Cleanup(func() { _ = collector.Close() })
	}
	if err == nil {
		t.Fatal("metrics opened without PostgreSQL")
	}
}
