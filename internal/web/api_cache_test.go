package web

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/buildinfo"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/toolcache"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func TestStateResponseSharedCaches(t *testing.T) {
	for _, count := range []int{1, 2} {
		t.Run(string(rune('0'+count)), func(t *testing.T) {
			snapshot := telemetry.Snapshot{}
			for i := range count {
				snapshot.SharedCaches = append(snapshot.SharedCaches, workspace.CacheUsage{ProjectID: string(rune('a' + i)), BuildBytes: int64(i + 1)})
			}
			response := stateResponse(snapshot, time.Now(), time.Now(), "test", buildinfo.Info{})
			data, err := json.Marshal(response)
			if err != nil {
				t.Fatal(err)
			}
			var decoded struct {
				Caches []workspace.CacheUsage `json:"shared_caches"`
			}
			if err := json.Unmarshal(data, &decoded); err != nil {
				t.Fatal(err)
			}
			if len(decoded.Caches) != count {
				t.Fatalf("caches = %+v", decoded.Caches)
			}
			for i, cache := range decoded.Caches {
				if cache != snapshot.SharedCaches[i] {
					t.Fatalf("cache = %+v", cache)
				}
			}
		})
	}
}

func TestProjectScopedSharedCaches(t *testing.T) {
	snapshot := telemetry.Snapshot{SharedCaches: []workspace.CacheUsage{{ProjectID: "a", BuildBytes: 4}, {ProjectID: "b", BuildBytes: 8}}}
	for _, id := range []string{"a", "b", "missing"} {
		t.Run(id, func(t *testing.T) {
			scoped := projectScopedSnapshotForProject(snapshot, telemetry.Project{ID: id})
			response := stateResponse(scoped, time.Now(), time.Now(), "test", buildinfo.Info{})
			want := 1
			if id == "missing" {
				want = 0
			}
			if len(response.SharedCaches) != want {
				t.Fatalf("caches = %+v", response.SharedCaches)
			}
			for _, cache := range response.SharedCaches {
				if cache.ProjectID != id {
					t.Fatalf("foreign cache = %+v", cache)
				}
			}
			if len(snapshot.SharedCaches) != 2 || snapshot.SharedCaches[0].ProjectID != "a" {
				t.Fatal("scoping mutated fleet snapshot")
			}
		})
	}
}

func TestStateResponseHostCache(t *testing.T) {
	for _, report := range []*toolcache.Report{nil, {BuildPath: "/native/build", BuildBytes: 47 << 30, ModuleBytes: 123}, {Error: "scan interrupted"}} {
		snapshot := telemetry.Snapshot{HostCache: report}
		response := stateResponse(snapshot, time.Now(), time.Now(), "test", buildinfo.Info{})
		data, err := json.Marshal(response)
		if err != nil {
			t.Fatal(err)
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
