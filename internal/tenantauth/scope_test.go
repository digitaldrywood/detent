package tenantauth

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestScopeRejectsMissingAndExpiredAuthority(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name   string
		scope  Scope
		denied bool
	}{
		{name: "zero", denied: true},
		{name: "valid", scope: Scope{organization: "first", principal: Principal{CredentialID: "key"}, created: now}},
		{name: "credential boundary", scope: Scope{organization: "first", principal: Principal{CredentialID: "key"}, expires: &now}, denied: true},
		{name: "not created", scope: Scope{organization: "first", principal: Principal{CredentialID: "key"}, created: now.Add(time.Second)}, denied: true},
		{name: "runner boundary", scope: Scope{organization: "first", principal: Principal{CredentialID: "key"}, runner: runnerauth.Identity{Binding: runnerauth.Binding{RunnerID: "runner"}, ExpiresAt: now}}, denied: true},
		{name: "renewal", scope: Scope{organization: "first", principal: Principal{CredentialID: "key"}, expires: &now, renewal: true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.scope.ValidAt(now); errors.Is(err, ErrDenied) != test.denied {
				t.Fatalf("ValidAt() = %v, denied = %v", err, test.denied)
			}
		})
	}
}

func TestScopeDoesNotExposeMutableIdentityOrProof(t *testing.T) {
	scope := Scope{proof: RequestIdentity{CredentialHash: "private-proof"}, runner: runnerauth.Identity{ProjectIDs: []tracker.ProjectID{"first"}, Operations: []string{"read"}}}
	runner := scope.Runner()
	runner.ProjectIDs[0] = "second"
	runner.Operations[0] = "claim"
	if scope.Runner().ProjectIDs[0] != "first" || scope.Runner().Operations[0] != "read" {
		t.Fatal("returned runner identity mutates scope")
	}
	for _, format := range []string{"%v", "%+v", "%#v"} {
		if strings.Contains(fmt.Sprintf(format, scope), "private-proof") {
			t.Fatal("scope formatting discloses credential proof")
		}
	}
}
