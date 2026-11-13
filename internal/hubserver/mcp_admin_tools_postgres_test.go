package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresMCPAdminChangeTools(t *testing.T) {
	t.Parallel()
	first := newReleaseTenantFixture(t)
	peer, peerOwner := newChangePolicyPostgresTenantFixture(t, first.repository, "org_second", &memoryRunDetails{})
	second := newReleaseTenantFixtureFor(t, first.repository, peer, peerOwner)
	for _, f := range []*releaseTenantFixture{first, second} {
		t.Run(f.f.service.config.Hosted.OrganizationID, func(t *testing.T) {
			seedReleaseTenantAudit(t, f)
			publisher := &releasePublisherFixture{releaseVerifierFixture: f.verifier}
			f.f.service.config.ReconcileBackend = publisher
			ownerCtx := parityHostedContext(t, f.f, f.owner)
			viewer := coordinatorBudgetUser(t, f.f, "viewer", "viewer", false)
			viewerCtx := parityHostedContext(t, f.f, viewer)
			outsider := coordinatorBudgetUser(t, f.f, "outsider", "member", false)
			organization := tracker.OrganizationID(f.f.service.config.Hosted.OrganizationID)
			if err := f.repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				return w.ClearAccessMemberGrants(t.Context(), outsider.identity.Subject)
			}); err != nil {
				t.Fatal(err)
			}
			outsiderCtx := parityHostedContext(t, f.f, outsider)
			if err := f.repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				now := f.f.service.config.now()
				if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: "artifact-publisher", Name: "Example publisher", Hash: apikey.HashToken(string(organization) + "-publisher"), Scope: "worker", Kind: "service", ProjectAccess: "selected", CreatedAt: now, UpdatedAt: now}); err != nil {
					return err
				}
				return w.SetAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization, TokenID: "artifact-publisher", ProjectID: f.f.project})
			}); err != nil {
				t.Fatal(err)
			}
			binding := artifact.Binding{ServiceID: artifact.NewID("service"), Origin: "https://artifacts.example.test", Mode: "customer", PublisherTokenID: "artifact-publisher"}
			for _, name := range []string{operatortool.PublishReleasePick, operatortool.AssociateReleasePick, operatortool.BindArtifactService} {
				t.Run(name, func(t *testing.T) {
					args := operatortool.ChangeArguments{ProjectID: string(f.f.project), ItemID: string(f.delivery.WorkItemID), ReleasePick: &f.request.ReleasePickInput, ExpectedRevision: int64(f.delivery.Revision), RequestID: "admin-" + name, Binding: &binding}
					call := parityToolCall(t, name, args)
					if name == operatortool.PublishReleasePick {
						var fields map[string]json.RawMessage
						if err := json.Unmarshal(call.Arguments, &fields); err != nil {
							t.Fatal(err)
						}
						var pick map[string]json.RawMessage
						if err := json.Unmarshal(fields["release_pick"], &pick); err != nil {
							t.Fatal(err)
						}
						delete(pick, "pr_number")
						raw, err := json.Marshal(pick)
						if err != nil {
							t.Fatal(err)
						}
						fields["release_pick"] = raw
						call.Arguments, err = json.Marshal(fields)
						if err != nil {
							t.Fatal(err)
						}
					}

					e := hostedOperatorExecutor{f.f.service}
					for _, denied := range []struct {
						name string
						ctx  context.Context
					}{{"invisible", outsiderCtx}, {"insufficient role", viewerCtx}} {
						t.Run(denied.name, func(t *testing.T) {
							if _, err := e.Execute(denied.ctx, call); !errors.Is(err, operatortool.ErrAccessDenied) {
								t.Fatalf("denied call: %v", err)
							}
						})
					}
					result, err := e.Execute(ownerCtx, call)
					paritySuccess(t, result, err)
					var got operatortool.ChangeResult
					if err := json.Unmarshal(result.Content, &got); err != nil {
						t.Fatal(err)
					}
					if got.OrganizationID != string(organization) || got.ProjectID != string(f.f.project) || got.URL == "" || got.GeneratedAt.IsZero() || got.Freshness != "live" || len(got.Receipt) == 0 {
						t.Fatalf("missing result fields: %s", result.Content)
					}
					switch name {
					case operatortool.PublishReleasePick:
						var publication tracker.ReleasePickPublication
						if err := json.Unmarshal(got.Receipt, &publication); err != nil {
							t.Fatal(err)
						}
						if publication.ReleasePickInput != f.request.ReleasePickInput || publication.WorkItemID != f.delivery.WorkItemID {
							t.Fatalf("publication: %+v", publication)
						}
					case operatortool.AssociateReleasePick:
						var delivery tracker.ReleaseDelivery
						if err := json.Unmarshal(got.Receipt, &delivery); err != nil {
							t.Fatal(err)
						}
						if delivery.ReleasePickInput != f.request.ReleasePickInput || delivery.ChangeID == "" {
							t.Fatalf("delivery: %+v", delivery)
						}
					case operatortool.BindArtifactService:
						var gotBinding artifact.Binding
						if err := json.Unmarshal(got.Receipt, &gotBinding); err != nil {
							t.Fatal(err)
						}
						if gotBinding.ServiceID != binding.ServiceID || gotBinding.Origin != binding.Origin || gotBinding.Mode != binding.Mode || gotBinding.PublisherTokenID != "" {
							t.Fatalf("binding: %+v", gotBinding)
						}
					}
					definitions, err := e.ListTools(ownerCtx)
					if err != nil || !entryKeyHasDefinition(definitions, name) {
						t.Fatalf("tool absent: %v", err)
					}
					replay, err := e.Execute(ownerCtx, call)
					paritySuccess(t, replay, err)
					var replayed operatortool.ChangeResult
					if err := json.Unmarshal(replay.Content, &replayed); err != nil {
						t.Fatal(err)
					}
					if string(replayed.Receipt) != string(got.Receipt) {
						t.Fatal("replay changed receipt")
					}
				})
			}
		})
	}
}

func TestPostgresMCPAdminDeletionTools(t *testing.T) {
	t.Parallel()
	fixture := newOperatorSecurityFixture(t)
	first := fixture.hostedSecurityFixture
	second := parityHostedPeer(t, first, "org_second", "prj_second")
	fixtures := []hostedSecurityFixture{first, second}
	owners := make([]hostedSecurityUser, len(fixtures))
	contexts := make([]context.Context, len(fixtures))
	for i, f := range fixtures {
		owners[i] = coordinatorBudgetUser(t, f, "admin-owner", "owner", false)
		contexts[i] = parityHostedContext(t, f, owners[i])
	}
	for index, f := range fixtures {
		t.Run(f.service.config.Hosted.OrganizationID, func(t *testing.T) {
			owner := owners[index]
			viewer := coordinatorBudgetUser(t, f, "admin-viewer", "viewer", false)
			target := coordinatorBudgetUser(t, f, "admin-target", "member", false)
			ownerCtx := contexts[index]
			viewerCtx := parityHostedContext(t, f, viewer)
			organization := tracker.OrganizationID(f.service.config.Hosted.OrganizationID)
			admin := fixture.admin
			for _, path := range []string{"/projects", "/members"} {
				requireNativeStatus(t, f.request(t, target, http.MethodGet, "/api/v2/organizations/"+string(organization)+path, nil), http.StatusOK)
			}

			var targetID string
			if err := f.service.config.TenantRepository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
				m, err := r.AccessMember(t.Context(), target.identity.Subject)
				targetID = m.MembershipID
				return err
			}); err != nil {
				t.Fatal(err)
			}
			invitation, err := f.provider.Invite(t.Context(), f.service.config.Hosted.WorkOSOrganizationID, "invite@example.test", "member", "")
			if err != nil {
				t.Fatal(err)
			}
			if err := f.service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				return w.CreateAccessInvitation(t.Context(), accessstore.Invitation{OrganizationID: organization, ID: invitation.ID, Email: invitation.Email, Role: "member", CreatedAt: f.service.config.now(), ExpiresAt: invitation.ExpiresAt})
			}); err != nil {
				t.Fatal(err)
			}

			var ownerID string
			if err := f.service.config.TenantRepository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
				m, err := r.AccessMember(t.Context(), owner.identity.Subject)
				ownerID = m.MembershipID
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := (hostedOperatorExecutor{f.service}).Execute(ownerCtx, parityToolCall(t, operatortool.MemberRemove, map[string]any{"member_id": ownerID, "request_id": "remove-last-owner"})); !errors.Is(err, operatortool.ErrAccessDenied) {
				t.Fatalf("last owner removal: %v", err)
			}
			for _, test := range []struct{ name, field, id string }{{operatortool.InvitationRevoke, "invitation_id", invitation.ID}, {operatortool.MemberRemove, "member_id", targetID}} {
				t.Run(test.name, func(t *testing.T) {
					e := hostedOperatorExecutor{f.service}
					path, route := "/api/v2/organizations/"+string(organization)+"/members/"+test.id, "DELETE "+hostedOrganizationBase+"/members/:member"
					if test.name == operatortool.InvitationRevoke {
						path, route = "/api/v2/organizations/"+string(organization)+"/members/invitations/"+test.id, "DELETE "+hostedOrganizationBase+"/members/invitations/:invitation"
					}
					t.Run("audit failure rolls back deletion", func(t *testing.T) {
						repository := f.service.config.AccessRepository
						failed := &routingAuditFailureRepository{Repository: repository}
						f.service.config.AccessRepository = failed
						defer func() { f.service.config.AccessRepository = repository }()
						requireNativeStatus(t, f.request(t, owner, http.MethodDelete, path, nil), http.StatusServiceUnavailable)
						if failed.attempts != 1 {
							t.Fatalf("audit attempts=%d", failed.attempts)
						}
						if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
							if test.name == operatortool.InvitationRevoke {
								_, err := r.AccessInvitation(t.Context(), test.id)
								return err
							}
							member, err := r.AccessMember(t.Context(), target.identity.Subject)
							if !member.Active {
								t.Error("audit failure committed member removal")
							}
							return err
						}); err != nil {
							t.Fatal(err)
						}
					})

					args := map[string]any{test.field: test.id, "request_id": "admin-" + test.name}
					call := parityToolCall(t, test.name, args)
					if _, err := e.Execute(contexts[1-index], call); !errors.Is(err, operatortool.ErrAccessDenied) {
						t.Fatalf("foreign organization caller: %v", err)
					}

					if _, err := e.Execute(viewerCtx, call); !errors.Is(err, operatortool.ErrAccessDenied) {
						t.Fatalf("role denial: %v", err)
					}
					hidden := parityToolCall(t, test.name, map[string]any{test.field: "other-organization-resource", "request_id": "hidden-" + test.name})
					if _, err := e.Execute(ownerCtx, hidden); !errors.Is(err, operatortool.ErrAccessDenied) {
						t.Fatalf("invisible resource: %v", err)
					}
					result, err := e.Execute(ownerCtx, call)
					paritySuccess(t, result, err)
					var got struct {
						Action chat.Action
						Output struct {
							ResourceID string `json:"resource_id"`
						}
						ResultTool string `json:"result_tool"`
						FreshAt    string `json:"fresh_at"`
					}
					if err := json.Unmarshal(result.Content, &got); err != nil {
						t.Fatal(err)
					}
					if got.Action.Status != chat.ActionSucceeded || got.Output.ResourceID != test.id || got.ResultTool != operatortool.ActionResult || got.FreshAt == "" {
						t.Fatalf("result: %s", result.Content)
					}
					replay, err := e.Execute(ownerCtx, call)
					paritySuccess(t, replay, err)

					connection := operatortool.CurrentConnection(ownerCtx)
					connection.ID += "-" + test.name + "-reconnected"
					reconnected := operatortool.WithConnection(ownerCtx, connection)
					if err := e.OpenConnection(reconnected); err != nil {
						t.Fatal(err)
					}
					retried, err := e.Execute(reconnected, call)
					paritySuccess(t, retried, err)
					if _, err := e.Execute(viewerCtx, call); !errors.Is(err, operatortool.ErrAccessDenied) {
						t.Fatalf("committed result bypassed role: %v", err)
					}
					changed := parityToolCall(t, test.name, map[string]any{test.field: "different-resource", "request_id": "admin-" + test.name})
					if _, err := e.Execute(ownerCtx, changed); !errors.Is(err, operatortool.ErrAccessDenied) {
						t.Fatalf("changed receipt input accepted: %v", err)
					}
					fetched, err := e.Execute(ownerCtx, parityToolCall(t, operatortool.ActionResult, map[string]any{"action_id": got.Action.ID}))
					paritySuccess(t, fetched, err)
					definitions, err := e.ListTools(ownerCtx)
					if err != nil || !entryKeyHasDefinition(definitions, test.name) {
						t.Fatalf("tool absent: %v", err)
					}

					var audits int
					if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM hosted_audit WHERE organization_id=$1 AND route=$2 AND event='action' AND status=$3 AND effective_user=$4 AND mutation_object_key IS NOT NULL`, organization, route, http.StatusNoContent, owner.identity.Subject).Scan(&audits); err != nil {
						t.Fatal(err)
					}
					if audits != 1 {
						t.Fatalf("committed deletion audit rows=%d", audits)
					}
					if test.name == operatortool.MemberRemove {
						requireNativeStatus(t, f.request(t, target, http.MethodGet, "/api/v2/organizations/"+string(organization)+"/projects", nil), http.StatusForbidden)
						requireNativeStatus(t, f.request(t, target, http.MethodGet, "/api/v2/organizations/"+string(organization)+"/members", nil), http.StatusForbidden)
						if err := f.service.config.TenantRepository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
							m, err := r.AccessMember(t.Context(), target.identity.Subject)
							if m.Active {
								t.Error("removed member active")
							}
							return err
						}); err != nil {
							t.Fatal(err)
						}
					}
				})
			}
		})
	}
}
