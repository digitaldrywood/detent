package web

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/buildinfo"
	"github.com/digitaldrywood/detent/internal/telemetry"
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
