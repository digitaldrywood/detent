package hubscope_test

import (
	"testing"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeKeyActor(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		credential hubapi.APICredential
		want       tracker.Actor
	}{
		{"browser", hubapi.APICredential{ID: "human", SessionHash: "session"}, tracker.Actor{Kind: "human", PrincipalID: "human"}},
		{"personal entry", hubapi.APICredential{ID: "human", EntryKey: &apikey.KeyAuthority{ID: "key", Kind: "personal"}}, tracker.Actor{Kind: "human", PrincipalID: "human", ViaKey: "key"}},
		{"service entry", hubapi.APICredential{ID: "service_key", EntryKey: &apikey.KeyAuthority{ID: "key", Kind: "service"}}, tracker.Actor{Kind: "integration", PrincipalID: "service_key", ViaKey: "key"}},
		{"personal hosted", hubapi.APICredential{ID: "key", HostedPrincipal: "human", HostedKeyKind: "personal", HostedKeyScope: apikey.ScopeWrite}, tracker.Actor{Kind: "human", PrincipalID: "human", ViaKey: "key"}},
		{"service hosted", hubapi.APICredential{ID: "key", HostedPrincipal: "human", HostedKeyKind: "service", HostedKeyScope: apikey.ScopeWrite}, tracker.Actor{Kind: "integration", PrincipalID: "key", ViaKey: "key"}},
		{"worker", hubapi.APICredential{ID: "worker", Scope: hubapi.APIScopeWorker}, tracker.Actor{Kind: "runner", PrincipalID: "worker"}},
		{"hosted worker", hubapi.APICredential{ID: "worker", Scope: hubapi.APIScopeWorker, HostedKeyKind: "service"}, tracker.Actor{Kind: "runner", PrincipalID: "worker"}},
		{"runner", hubapi.APICredential{ID: "runner", Scope: hubapi.APIScopeOperator, Runner: runnerauth.Identity{Binding: runnerauth.Binding{RunnerID: "runner"}}}, tracker.Actor{Kind: "runner", PrincipalID: "runner"}},
		{"hosted runner", hubapi.APICredential{ID: "runner", Scope: hubapi.APIScopeOperator, HostedKeyKind: "service", Runner: runnerauth.Identity{Binding: runnerauth.Binding{RunnerID: "runner"}}}, tracker.Actor{Kind: "runner", PrincipalID: "runner"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := (hubscope.NativeScope{Credential: test.credential}).Actor(); got != test.want {
				t.Fatalf("actor=%+v want=%+v", got, test.want)
			}
		})
	}
}
