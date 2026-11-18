package hubserver

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeActorContractFixture(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join(conversationFixtureDirectory, "native-actors.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var want []tracker.Actor
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	got := []tracker.Actor{
		(hubscope.NativeScope{Credential: hubapi.APICredential{ID: "actor-contract", Scope: hubapi.APIScopeOperator}}).Actor(),
		(hubscope.NativeScope{Credential: hubapi.APICredential{ID: "actor-contract", Scope: hubapi.APIScopeWorker}}).Actor(),
		*nativeIntegrationActor("github"),
		healthIssueScope("", "").Actor(),
	}
	if !slices.Equal(got, want) {
		t.Fatalf("server actors = %+v, client fixture = %+v", got, want)
	}
}
