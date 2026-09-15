package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	servicepkg "github.com/digitaldrywood/detent/internal/service"
	"github.com/digitaldrywood/detent/internal/toolcache"
)

func TestStatusHostCacheReport(t *testing.T) {
	for _, tt := range []struct {
		name   string
		report toolcache.Report
		want   string
	}{
		{"sizes", toolcache.Report{BuildPath: "/build", BuildBytes: 12, ModulePath: "/modules", ModuleBytes: 34}, "GOCACHE=/build (12 bytes); GOMODCACHE=/modules (34 bytes)"},
		{"unavailable", toolcache.Report{Error: "no Go toolchain"}, "no Go toolchain"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			runner := statusServiceRunner{ServiceRunner: &serviceRunnerStub{status: servicepkg.Status{}}, inspectCaches: func(context.Context) toolcache.Report { return tt.report }}
			status, err := runner.Status(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if status.HostCache == nil || *status.HostCache != tt.report {
				t.Fatalf("report=%+v", status.HostCache)
			}
			var out bytes.Buffer
			if err := writeServiceStatusText(&out, status); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), tt.want) {
				t.Fatalf("output=%s", out.String())
			}
		})
	}
}
