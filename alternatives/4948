package runnerauth

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRoutingCacheRoundTrip(t *testing.T) {
	t.Parallel()
	authored := func(name string) *policy.Descriptor {
		return &policy.Descriptor{ID: "policy_" + name, Authored: &policy.Authored{Version: 1, Files: map[string]string{"AGENTS.md": strings.Repeat(name, 70<<10/len(name))}}}
	}
	for _, test := range []struct {
		name          string
		configuration *ProjectConfiguration
		request       *ProjectConfigurationRequest
	}{
		{name: "routing only"},
		{name: "Hub project configuration with two large policies", configuration: &ProjectConfiguration{ProjectID: "prj_home", Authority: "hub_configuration", EffectivePolicy: authored("approved"), SelectedPolicy: authored("observed")}, request: &ProjectConfigurationRequest{ProjectID: "prj_home", RequestID: "request", Desired: &ProjectConfiguration{ProjectID: "prj_home", EffectivePolicy: authored("desired")}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "private", "runner.json")
			file, err := Initialize(path, "https://hub.example.test")
			if err != nil {
				t.Fatal(err)
			}
			snapshot := RoutingSnapshot{RunnerID: file.Identity.RunnerID, Revision: 3, ProjectConfiguration: test.configuration, ProjectConfigurationRequest: test.request, Routing: Routing{
				DisplayName: "Runner", State: "active", CapacityLimit: 2, ProjectIDs: []tracker.ProjectID{"prj_home"},
				IsolationTier: "native-trusted", HostServices: []string{"tcp:127.0.0.1:8080"},
				Availability: Availability{Timezone: "UTC", Windows: []string{"Mon-Fri 09:00-17:00"}},
			}}
			if err := SaveRoutingCache(path, snapshot); err != nil {
				t.Fatal(err)
			}
			got, err := LoadRoutingCache(path)
			if err != nil {
				t.Fatal(err)
			}
			if got.Revision != 3 || got.Routing.IsolationTier != "native-trusted" || got.Routing.ProjectIDs[0] != "prj_home" {
				t.Fatalf("cache = %#v", got)
			}
			if got.ProjectConfiguration != nil || got.ProjectConfigurationRequest != nil {
				t.Fatalf("cache persisted Hub-owned project configuration: %#v %#v", got.ProjectConfiguration, got.ProjectConfigurationRequest)
			}
			info, err := os.Stat(RoutingCachePath(path))
			if err != nil || runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
				t.Fatalf("cache permissions = %v, %v", info, err)
			}
		})
	}
}
