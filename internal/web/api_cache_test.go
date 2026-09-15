package web

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/buildinfo"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/toolcache"
)

func TestStateResponseHostCache(t *testing.T) {
	for _, report := range []*toolcache.Report{nil, {BuildPath: "/native/build", BuildBytes: 47 << 30, ModuleBytes: 123}, {Error: "scan interrupted"}} {
		snapshot := telemetry.Snapshot{HostCache: report}
		response := stateResponse(snapshot, time.Now(), time.Now(), "test", buildinfo.Info{})
		data, err := json.Marshal(response)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(data, &fields); err != nil {
			t.Fatal(err)
		}
		if _, ok := fields["shared_caches"]; ok {
			t.Fatal("obsolete shared_caches exposed")
		}
		var decoded struct {
			HostCache *toolcache.Report `json:"host_cache"`
		}
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatal(err)
		}
		if report == nil {
			if decoded.HostCache != nil {
				t.Fatal("unexpected report")
			}
			continue
		}
		if decoded.HostCache == nil || *decoded.HostCache != *report {
			t.Fatalf("report = %+v", decoded.HostCache)
		}
	}
}
