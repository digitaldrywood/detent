package cli

import (
	"encoding/json"
	"runtime"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/buildinfo"
)

func TestRunnerRunningBuildProvenance(t *testing.T) {
	for _, test := range []struct {
		name string
		info buildinfo.Info
	}{
		{"private patched source", buildinfo.Info{Version: "v1.2.3", Commit: strings.Repeat("a", 40), Dirty: true}},
		{"version is not release approval", buildinfo.Info{Version: "v1.2.3", Commit: strings.Repeat("a", 40)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			evidence := runnerRunningBuild(test.info, "fallback")
			if evidence.Validate() != nil || evidence.Version != test.info.Version || evidence.Commit != test.info.Commit || evidence.OS != runtime.GOOS || evidence.Architecture != runtime.GOARCH || len(evidence.SHA256) != 64 || evidence.VerifiedRelease {
				t.Fatalf("running build=%+v", evidence)
			}
			if test.info.Dirty && evidence.Source != "private_patched_source" {
				t.Fatalf("lost private composition=%+v", evidence)
			}
			raw, err := json.Marshal(evidence)
			if err != nil || strings.Contains(string(raw), "/Users/") || strings.Contains(string(raw), "/private/") {
				t.Fatalf("private path in evidence=%s error=%v", raw, err)
			}
		})
	}
}
