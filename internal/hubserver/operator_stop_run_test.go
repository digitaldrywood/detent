package hubserver

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHostedStopRunAuthority(t *testing.T) {
	t.Parallel()
	for _, deployment := range []string{"dedicated", "shared"} {
		for _, scenario := range []struct {
			name, role     string
			scope          apikey.Scope
			denied, revoke bool
		}{
			{name: "member write", role: "member", scope: apikey.ScopeWrite},
			{name: "owner read", role: "owner", scope: apikey.ScopeRead, denied: true},
			{name: "viewer", role: "viewer", denied: true},
			{name: "reporter", role: "reporter", denied: true},
			{name: "revoked grant", role: "member", scope: apikey.ScopeWrite, revoke: true},
		} {
			t.Run(deployment+"/"+scenario.name, func(t *testing.T) {
				f, ctx := newHostedKeyMCPFixture(t, deployment, scenario.role)
				item := f.seedIssue(t, 1)
				seedCoordinatorArchiveLease(t, f.service, string(item))
				coordinatorFixtureWrite(t, f.service, "org_security", func(w tenantstore.Writer) error {
					project, err := w.ProjectRecord(t.Context(), f.project)
					if err != nil {
						return err
					}
					project.States = []tracker.NativeState{{Name: "Todo", Dispatchable: true, Transitions: []string{"Cancelled"}}, {Name: "Cancelled", Terminal: true}}
					_, err = w.UpdateProjectRecord(t.Context(), project)
					return err
				})
				readLease := func() leasestore.Lease {
					t.Helper()
					var lease leasestore.Lease
					if err := f.service.config.TenantRepository.ReadTenant(t.Context(), "org_security", corestore.Serving, func(r tenantstore.Reader) error {
						var err error
						lease, err = r.Lease(t.Context(), "archive-lease")
						return err
					}); err != nil {
						t.Fatal(err)
					}
					return lease
				}
				fence := tracker.FencingToken(readLease().FencingToken)
				if scenario.scope != "" {
					issuer := f.issuer(t)
					expires := time.Now().Add(time.Hour)
					key, err := f.service.createAPITokenFor(t.Context(), hubapi.TokenRequest{Name: "stop-client", Scope: hubapi.APIScopeOperator, KeyScope: scenario.scope, Issuer: &issuer, ProjectIDs: []string{string(f.project)}, ProjectAccess: hubapi.HostedProjectsSelected, ExpiresAt: &expires})
					if err != nil {
						t.Fatal(err)
					}
					ctx = changeOperatorContext(t, f.service, key.Token, "org_security")
					connection := operatortool.CurrentConnection(ctx)
					connection.ID = "stop-key-connection"
					ctx = operatortool.WithConnection(ctx, connection)
				}
				executor := hostedOperatorExecutor{f.service}
				if err := executor.OpenConnection(ctx); err != nil {
					t.Fatal(err)
				}
				raw, err := json.Marshal(operatortool.NativeStopRunArguments{ProjectID: string(f.project), RequestID: "stop", Identifier: string(item), ExpectedRevision: 1, LeaseID: "archive-lease", FencingToken: fence, Destination: "Cancelled"})
				if err != nil {
					t.Fatal(err)
				}
				call := operatortool.Call{Name: operatortool.StopRun, Arguments: raw}
				result, err := executor.Execute(ctx, call)
				if scenario.denied {
					if !errors.Is(err, operatortool.ErrAccessDenied) {
						t.Fatalf("unauthorized stop: %s %v", result.Content, err)
					}
				} else {
					var receipt struct {
						ID     string            `json:"action_id"`
						Status chat.ActionStatus `json:"status"`
					}
					if err != nil || json.Unmarshal(result.Content, &receipt) != nil || receipt.ID == "" || receipt.Status != chat.ActionSucceeded {
						t.Fatalf("authorized stop: %s %v", result.Content, err)
					}
					if scenario.revoke {
						coordinatorFixtureWrite(t, f.service, "org_security", func(w tenantstore.Writer) error {
							return w.DeleteAccessProjectGrant(t.Context(), f.user.identity.Subject, f.project)
						})
						for _, revoked := range []operatortool.Call{call, {Name: operatortool.ActionResult, Arguments: json.RawMessage(`{"action_id":"` + receipt.ID + `"}`)}} {
							if _, err := executor.Execute(ctx, revoked); !errors.Is(err, operatortool.ErrAccessDenied) {
								t.Fatalf("revoked authority accepted %s: %v", revoked.Name, err)
							}
						}
					}
				}
				released := readLease().ReleasedAt != nil
				if released == scenario.denied {
					t.Fatalf("released=%t denied=%t", released, scenario.denied)
				}
			})
		}
	}
}
