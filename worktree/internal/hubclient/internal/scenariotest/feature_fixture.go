package scenariotest

import (
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func newScenarioHub(t *testing.T, factories Constructors, memory ...bool) *nativeChangeHub {
	t.Helper()
	return newNativeChangeHubConfigured(t, "In Review", []tracker.NativeState{{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Done"}}, {Name: "In Progress", Dispatchable: true, Transitions: []string{"In Review", "Done", "Todo"}}, {Name: "In Review", Transitions: []string{"In Progress", "Done"}}, {Name: "Done", Terminal: true, Transitions: []string{"Todo"}}}, len(memory) > 0 && memory[0], false, hubserver.Config{DatabasePath: hubDatabasePath(t), InitialAdminToken: []byte(nativeChangeAdminToken)}, factories)
}
func fixtureArtifactConfiguration(t testing.TB, scheduler *Scheduler, service string, bytes int64) {
	t.Helper()
	fixture := fixtureClient(scheduler.Transport())
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	fixture.config.ArtifactServiceID, fixture.config.ArtifactBytes = service, bytes
}
