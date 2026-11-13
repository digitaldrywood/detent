package hubserver

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func parityNativeContext(t *testing.T, service *Service, organization tracker.OrganizationID) context.Context {
	t.Helper()
	contexts := make(chan context.Context, 1)
	path := "/api/v2/organizations/:organization/parity-context"
	service.echo.GET(path, func(c echo.Context) error {
		contexts <- operatortool.BindConnection(c.Request().Context(), "parity-"+c.Param("organization"), "test")
		return c.NoContent(http.StatusOK)
	}, service.operatorAuthority)
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodGet, "/api/v2/organizations/"+string(organization)+"/parity-context", testHubAdminToken, nil), http.StatusOK)
	ctx := <-contexts
	if err := (hostedOperatorExecutor{service}).OpenConnection(ctx); err != nil {
		t.Fatal(err)
	}
	return ctx
}

func parityToolCall(t *testing.T, name string, arguments any) operatortool.Call {
	t.Helper()
	definition, ok := operatortool.Lookup(name)
	if !ok {
		t.Fatalf("unknown tool %s", name)
	}
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(definition.InputSchema, &schema); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(arguments)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	for key := range fields {
		if _, ok := schema.Properties[key]; !ok {
			delete(fields, key)
		}
	}
	raw, err = json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return operatortool.Call{Name: name, Arguments: raw}
}

func TestPostgresMCPChangeParity(t *testing.T) {
	first := newChangeFixture(t, nil)
	organization := nativeOrganization{ID: first.project.OrganizationID + "_second"}
	second := newChangeFixture(t, first.service, organization.ID)
	for _, f := range []changeFixture{first, second} {
		t.Run(string(f.project.OrganizationID), func(t *testing.T) {
			version := f.publish(t, "parity-version", "")
			ctx := parityNativeContext(t, f.service, f.project.OrganizationID)
			for _, name := range []string{
				operatortool.GetChange, operatortool.GetChangeVersion, operatortool.DiscussChange,
				operatortool.GetChangeReviewPolicy,
				operatortool.ListWorkItemPullRequests, operatortool.ArtifactServices,
				operatortool.ArtifactReferences,
			} {
				t.Run(name, func(t *testing.T) {
					args := operatortool.ChangeArguments{ProjectID: string(f.project.ID), ItemID: string(f.issue.WorkItemID), ChangeID: f.change.ID, VersionID: version.ID, RequestID: "parity-" + name, Body: "Example discussion", FileSHA256: strings.Repeat("a", 64), Viewed: true, ExpectedRevision: int64(f.change.Revision), ExpectedPolicyID: f.rules.ID, Policy: &f.rules, AttemptID: "attempt", Sequence: 1, Bundle: &tracker.ChangeReviewBundle{ArtifactID: artifact.NewID("artifact"), Revision: 1, SHA256: strings.Repeat("a", 64), HeadSHA: version.HeadSHA}, Binding: &artifact.Binding{ServiceID: artifact.NewID("service"), Origin: "https://artifacts.example.test", PublisherTokenID: "publisher", Mode: "customer"}, ReleasePick: &tracker.ReleasePickInput{SourceIssueID: f.issue.WorkItemID, SourceChangeID: f.change.ID, SourceMergeSHA: strings.Repeat("a", 40), Repository: "example-org/example-project", PRNumber: 1, HeadRef: "release-pick", HeadSHA: strings.Repeat("b", 40), BaseRef: "release/v1.0", BaseSHA: strings.Repeat("a", 40), FrozenSHA: strings.Repeat("a", 40)}}
					call := parityToolCall(t, name, args)
					result, err := (hostedOperatorExecutor{f.service}).Execute(ctx, call)
					paritySuccess(t, result, err)
					var got operatortool.ChangeResult
					if err := json.Unmarshal(result.Content, &got); err != nil {
						t.Fatal(err)
					}
					if got.OrganizationID != string(f.project.OrganizationID) || got.ProjectID != string(f.project.ID) {
						t.Fatal("change result escaped tenant")
					}
					definitions, listErr := (hostedOperatorExecutor{f.service}).ListTools(ctx)
					if listErr != nil {
						t.Fatal(listErr)
					}
					if entryKeyHasDefinition(definitions, name) != (err == nil) {
						t.Fatalf("catalog/execution mismatch for %s: %v", name, err)
					}

				})
			}
		})
	}
}

func TestPostgresMCPProjectParity(t *testing.T) {
	first := newIntegrationFixture(t, &importFixtureBackend{})
	organization := nativeOrganization{ID: first.project.OrganizationID + "_second"}
	second := newNativeFixture(t, first.service, organization.ID, "Second project")
	for _, f := range []nativeFixture{first, second} {
		t.Run(string(f.project.OrganizationID), func(t *testing.T) {
			ctx := parityNativeContext(t, f.service, f.project.OrganizationID)
			e := hostedOperatorExecutor{f.service}
			envelope, err := f.service.config.SecretKeys.Seal([]byte("example-secret"), accessstore.SecretAAD(f.project.OrganizationID, f.project.ID, flySpritesToken))
			if err != nil {
				t.Fatal(err)
			}
			if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
				now := f.service.config.now()
				return w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, Kind: flySpritesToken, Envelope: envelope, UpdatedAt: now}, accessstore.SecretAudit{OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, Kind: flySpritesToken, Actor: "fixture", Event: "create", KeyVersion: envelope.Version, RecordedAt: now})
			}); err != nil {
				t.Fatal(err)
			}

			for _, name := range []string{"project_secret_metadata", "repository_freshness", operatortool.GetSpritePool, operatortool.GetProjectSpriteAllocation, operatortool.LocalProjectConfiguration, "get_cutover_receipt"} {
				t.Run(name, func(t *testing.T) {
					result, err := e.Execute(ctx, parityToolCall(t, name, map[string]any{"project_id": f.project.ID}))
					if name == "project_secret_metadata" && (!strings.Contains(string(result.Content), `"present":true`) || strings.Contains(string(result.Content), "example-secret")) {
						t.Fatal("secret metadata is missing or exposes plaintext")
					}

					switch name {
					case "repository_freshness":
						if f.project.OrganizationID == first.project.OrganizationID {
							if !errors.Is(err, operatortool.ErrServiceUnavailable) {
								t.Fatalf("retired linked repository read: %v", err)
							}
						} else {
							paritySuccess(t, result, err)
						}
					case "get_cutover_receipt":
						var request *operatortool.RequestError
						if !errors.As(err, &request) || request.Code != "not_found" {
							t.Fatalf("missing cutover receipt: %v", err)
						}
					default:
						paritySuccess(t, result, err)
					}

				})
			}
			for _, test := range []struct {
				name  string
				input any
			}{
				{"create_native_project", operatortool.ProjectCreateInput{Name: "Created project", States: nativeFixtureStates()}},
				{"remove_project_secret", struct{}{}},
				{"revoke_project_policy", operatortool.PolicyRevokeInput{ExpectedID: hubTestPolicy().ID}},
				{"bind_native_repository", operatortool.RepositoryInput{Repository: "example-org/example-project", Source: "runner_checkout", ExpectedRevision: 1}},
				{"command_git_hub_batch", operatortool.GitHubBatchInput{Action: "discover"}},
				{operatortool.UpdateSpritePool, operatortool.SpritePoolInput{MinRunners: 0, MaxRunners: 0, IdleSeconds: 600}},
				{operatortool.UpdateProjectSpriteAllocation, operatortool.SpriteAllocationInput{DesiredCount: 0}},
			} {
				t.Run(test.name, func(t *testing.T) {
					project := string(f.project.ID)
					if test.name == "create_native_project" || test.name == operatortool.UpdateSpritePool {
						project = ""
					}
					if test.name == "bind_native_repository" {
						input := test.input.(operatortool.RepositoryInput)
						if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
							record, err := r.ProjectRecord(t.Context(), f.project.ID)
							input.ExpectedRevision = tracker.Revision(record.Settings.Revision)
							return err
						}); err != nil {
							t.Fatal(err)
						}
						test.input = input
					}
					result, err := e.Execute(ctx, projectCall(t, test.name, project, "parity-"+test.name, test.input))
					if test.name == "remove_project_secret" {
						if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
							_, err := r.AccessSecret(t.Context(), f.project.ID, flySpritesToken)
							if !errors.Is(err, corestore.ErrNotFound) {
								return fmt.Errorf("secret survived removal: %v", err)
							}
							return nil
						}); err != nil {
							t.Fatal(err)
						}
					}

					if test.name == "revoke_project_policy" {
						if !errors.Is(err, operatortool.ErrServiceUnavailable) || !strings.Contains(err.Error(), "does not require execution approval") {
							t.Fatalf("policy revocation retirement: %v", err)
						}
					} else {
						paritySuccess(t, result, err)
					}
				})
			}
			for _, name := range []string{"apply_local_project_policy", "drain_local_project", "detach_local_project", "resume_local_project"} {
				t.Run(name, func(t *testing.T) {
					args := operatortool.LocalProjectArguments{ProjectID: string(f.project.ID), RequestID: "parity-" + name, ExpectedConfigRevision: strings.Repeat("a", 64), ExpectedPolicyID: hubTestPolicy().ID, PolicyID: hubTestPolicy().ID, SourceRevision: strings.Repeat("a", 40), Checkpoint: strings.Repeat("a", 64)}
					result, err := e.Execute(ctx, parityToolCall(t, name, args))
					paritySuccess(t, result, err)
					if !strings.Contains(string(result.Content), "not installed or is stopped") {
						t.Fatal("missing configuration owner must be reported")
					}

				})
			}
		})
	}
}

func parityHostedContext(t *testing.T, f hostedSecurityFixture, user hostedSecurityUser) context.Context {
	t.Helper()
	contexts := make(chan context.Context, 1)
	path := "/parity-hosted-context/" + user.identity.Subject
	f.service.echo.GET(path, func(c echo.Context) error {
		contexts <- operatortool.BindConnection(c.Request().Context(), "parity-"+user.identity.Subject, "test")
		return c.NoContent(http.StatusOK)
	}, f.service.operatorAuthority)
	requireNativeStatus(t, f.request(t, user, http.MethodGet, path, nil), http.StatusOK)
	ctx := <-contexts
	if err := (hostedOperatorExecutor{f.service}).OpenConnection(ctx); err != nil {
		t.Fatal(err)
	}
	return ctx
}

func TestPostgresMCPHostedParity(t *testing.T) {
	first := newOperatorSecurityFixture(t).hostedSecurityFixture
	second := parityHostedPeer(t, first, "org_budget_second", "prj_budget_second")
	for _, f := range []hostedSecurityFixture{first, second} {
		t.Run(f.service.config.Hosted.OrganizationID, func(t *testing.T) {
			f.service.config.Hosted.Directory = []HostedDestination{{OrganizationID: f.service.config.Hosted.OrganizationID, WorkOSOrganizationID: f.service.config.Hosted.WorkOSOrganizationID, PublicURL: f.service.config.Hosted.PublicURL}}
			owner := coordinatorBudgetUser(t, f, "parity-owner", "owner", true)
			target := coordinatorBudgetUser(t, f, "parity-target", "member", false)
			var targetID string
			if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.service.accessOrganization(), corestore.Serving, func(r tenantstore.Reader) error {
				member, err := r.AccessMember(t.Context(), target.identity.Subject)
				targetID = member.MembershipID
				return err
			}); err != nil {
				t.Fatal(err)
			}
			ctx := parityHostedContext(t, f, owner)
			e := hostedOperatorExecutor{f.service}
			var createdID string
			for _, test := range []struct {
				name string
				args map[string]any
			}{
				{operatortool.OrganizationList, map[string]any{}},
				{operatortool.CredentialList, map[string]any{}},
				{operatortool.OrganizationProjectRank, map[string]any{}},
				{operatortool.OrganizationProjectRankUpdate, map[string]any{"expected_revision": 1, "project_ids": []string{string(f.project)}}},
				{operatortool.CredentialCreate, map[string]any{"name": "Example key", "scopes": []string{"read"}, "expires_in": "30d"}},
				{operatortool.CredentialRevoke, map[string]any{}},
				{operatortool.MemberRemove, map[string]any{"member_id": targetID}},
				{operatortool.InvitationSend, map[string]any{"email": "invite@example.test", "role": "member"}},
				{operatortool.InvitationRevoke, map[string]any{"invitation_id": "invitation_1"}},
				{"create_hosted_project", map[string]any{"input": operatortool.HostedProjectCreateInput{Name: "Created hosted project", GrantAccess: true}}},
				{operatortool.HostedFleet, map[string]any{}},
				{operatortool.AnalyticsDashboard, map[string]any{}},
				{operatortool.TimeSeries, map[string]any{}},
				{operatortool.BillingUsage, map[string]any{}},
				{operatortool.HostedPlan, map[string]any{}},
				{operatortool.HostedUsage, map[string]any{}},
				{operatortool.OrganizationSwitch, map[string]any{"organization_id": f.service.config.Hosted.OrganizationID}},
				{operatortool.InvitationAccept, map[string]any{}},
				{operatortool.SessionLogout, map[string]any{}},
			} {
				t.Run(test.name, func(t *testing.T) {
					definition, _ := operatortool.Lookup(test.name)
					if !definition.Annotations.ReadOnly {
						test.args["request_id"] = "parity-" + test.name
					}
					if test.name == operatortool.CredentialRevoke {
						test.args["credential_id"] = createdID
					}
					if test.name == operatortool.InvitationAccept {
						invitation, err := f.provider.Invite(t.Context(), f.service.config.Hosted.WorkOSOrganizationID, owner.identity.Email, "owner", "")
						if err != nil {
							t.Fatal(err)
						}
						if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.service.accessOrganization(), corestore.Serving, func(w tenantstore.Writer) error {
							return w.CreateAccessInvitation(t.Context(), accessstore.Invitation{OrganizationID: f.service.accessOrganization(), ID: invitation.ID, Email: invitation.Email, Role: "owner", CreatedAt: f.service.config.now(), ExpiresAt: invitation.ExpiresAt})
						}); err != nil {
							t.Fatal(err)
						}
						test.args["invitation_id"] = invitation.ID
					}
					result, err := e.Execute(ctx, parityToolCall(t, test.name, test.args))
					if test.name == operatortool.MemberRemove || test.name == operatortool.InvitationRevoke {
						paritySuccess(t, result, err)
						if test.name == operatortool.MemberRemove && f.provider.members[targetID].Status != "inactive" {
							t.Fatal("member removal did not commit before result reauthorization")
						}
						if test.name == operatortool.InvitationRevoke && f.provider.invitations["invitation_1"].State != "revoked" {
							t.Fatal("invitation revocation did not commit before result reauthorization")
						}

					} else {
						paritySuccess(t, result, err)
					}

					if test.name == operatortool.CredentialCreate && err == nil {
						var output struct {
							Output struct {
								ResourceID string `json:"resource_id"`
							} `json:"output"`
						}
						if err := json.Unmarshal(result.Content, &output); err != nil {
							t.Fatal(err)
						}
						createdID = output.Output.ResourceID
					}
				})
			}
		})
	}
}

func parityHostedPeer(t *testing.T, first hostedSecurityFixture, organization tracker.OrganizationID, project tracker.ProjectID) hostedSecurityFixture {
	t.Helper()
	cfg := first.service.config
	hosted := *cfg.Hosted
	cfg.Hosted = &hosted
	cfg.ProjectParticipants = nil
	cfg.NativeRunEvents = nil
	hosted.OrganizationID, hosted.WorkOSOrganizationID, hosted.BootstrapSubject = string(organization), "provider_"+string(organization), "owner_"+string(organization)
	provider := newHostedSecurityProvider()
	hosted.Provider = coordinatorBudgetProvider{hostedSecurityProvider: provider, organization: hosted.OrganizationID}
	cfg.InitialAdminToken = []byte("example-second-budget-admin")
	now := cfg.now()
	key := sha256.Sum256([]byte(hosted.OrganizationID))
	envelope, err := cfg.SecretKeys.Seal(key[:], accessstore.SecretAAD(organization, "", accessstore.NativeCursorKeyKind))
	if err != nil {
		t.Fatal(err)
	}
	deployment, generation := hosted.deployment()
	if err := first.service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: organization, Name: "Example second organization", Slug: "budget-second", CreatedAt: now}); err != nil {
			return err
		}
		if err := w.AddAccessOrganizationSlug(t.Context(), "budget-second"); err != nil {
			return err
		}
		if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: organization, ProviderID: hosted.WorkOSOrganizationID, BootstrapSubject: hosted.BootstrapSubject, Deployment: deployment, PublicURL: hosted.PublicURL, AllocationGeneration: generation}); err != nil {
			return err
		}
		if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: "admin_" + string(organization), Name: "Example administrator", Hash: apikey.HashToken(string(cfg.InitialAdminToken)), Scope: "admin", CreatedAt: now, UpdatedAt: now, KeyScope: "admin", ProjectAccess: "all", Kind: "service"}); err != nil {
			return err
		}
		if err := w.CreateHubIdentity(t.Context(), accessstore.HubIdentity{OrganizationID: organization, ID: hubapi.NewID("hub"), CursorKey: key[:]}); err != nil {
			return err
		}
		return w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: organization, Kind: accessstore.NativeCursorKeyKind, Envelope: envelope, UpdatedAt: now}, accessstore.SecretAudit{OrganizationID: organization, Kind: accessstore.NativeCursorKeyKind, Actor: "fixture", Event: "create", KeyVersion: envelope.Version, RecordedAt: now})
	}); err != nil {
		t.Fatal(err)
	}
	service := openHostedPostgresService(t, cfg)
	seedHostedPostgresProject(t, service, project)
	return hostedSecurityFixture{service: service, provider: provider, project: project, base: "/api/v2/organizations/" + hosted.OrganizationID + "/projects/" + string(project)}
}

func TestPostgresMCPArtifactParity(t *testing.T) {
	first := newPostgresArtifactFixture(t)
	peer := parityHostedPeer(t, first.hostedSecurityFixture, tracker.OrganizationID("org_"+strings.Repeat("c", 32)), tracker.ProjectID("prj_"+strings.Repeat("d", 32)))
	second := postgresArtifactFixture{hostedSecurityFixture: peer, admin: hostedPostgresAdmin(t, peer.service)}
	for _, f := range []postgresArtifactFixture{first, second} {
		t.Run(f.service.config.Hosted.OrganizationID, func(t *testing.T) {
			reference, _, _ := seedPostgresHostedArtifact(t, f)
			owner := coordinatorBudgetUser(t, f.hostedSecurityFixture, "artifact-reader", "owner", false)
			ctx := parityHostedContext(t, f.hostedSecurityFixture, owner)
			for _, name := range []string{operatortool.ArtifactServices, operatortool.ArtifactReferences, operatortool.GetArtifactReference, operatortool.ArtifactAccess} {
				t.Run(name, func(t *testing.T) {
					args := operatortool.ChangeArguments{ProjectID: string(f.project), ItemID: reference.WorkItemID, ArtifactID: reference.ArtifactID, Revision: reference.Revision, SHA256: reference.SHA256, RequestID: "artifact-parity"}
					result, err := (hostedOperatorExecutor{f.service}).Execute(ctx, parityToolCall(t, name, args))
					if err != nil {
						t.Fatal(err)
					}
					if !strings.Contains(string(result.Content), reference.ServiceID) {
						t.Fatalf("artifact identity missing: %s", result.Content)
					}
					definitions, err := (hostedOperatorExecutor{f.service}).ListTools(ctx)
					if err != nil || !entryKeyHasDefinition(definitions, name) {
						t.Fatalf("working artifact tool absent: %v", err)
					}
				})
			}
		})
	}
}

func TestPostgresMCPInstanceParity(t *testing.T) {
	first := newDefaultNativeFixture(t, Config{GitHubRequestCounts: func() []GitHubRequestCount { return []GitHubRequestCount{} }})
	second := newNativeFixture(t, first.service, first.project.OrganizationID+"_second", "Instance parity")
	for _, f := range []nativeFixture{first, second} {
		t.Run(string(f.project.OrganizationID), func(t *testing.T) {
			r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
			r.enroll(t)
			ctx := parityNativeContext(t, f.service, f.project.OrganizationID)
			for _, name := range []string{operatortool.InstanceHealth, operatortool.NativeCapabilities, operatortool.GetRunnerRouting, operatortool.GitHubRequestCounts, operatortool.CredentialList, operatortool.OrganizationList, operatortool.OrganizationSwitch} {
				t.Run(name, func(t *testing.T) {
					call := parityToolCall(t, name, map[string]any{"runner_id": r.binding.RunnerID, "request_id": "instance-parity", "organization_id": f.project.OrganizationID})
					result, err := (hostedOperatorExecutor{f.service}).Execute(ctx, call)
					paritySuccess(t, result, err)
				})
			}
		})
	}
}

func TestPostgresMCPAccountParity(t *testing.T) {
	for _, staff := range []bool{false, true} {
		t.Run(fmt.Sprintf("staff=%t", staff), func(t *testing.T) {
			first := newOperatorSecurityFixture(t).hostedSecurityFixture
			second := parityHostedPeer(t, first, "org_account_second", "prj_account_second")
			for _, f := range []hostedSecurityFixture{first, second} {
				t.Run(f.service.config.Hosted.OrganizationID, func(t *testing.T) {
					subject := f.service.config.Hosted.BootstrapSubject
					email := "account@example.test"
					if staff {
						email = "support@example.test"
					}
					now := f.service.config.now()
					identity := auth.Identity{Subject: subject, Email: email, EmailVerified: true, Hosted: &auth.HostedIdentity{Subject: subject, OrganizationID: f.service.config.Hosted.WorkOSOrganizationID, SessionID: "parity-account", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}}
					f.provider.sessions[identity.Hosted.SessionID] = *identity.Hosted
					token, _, err := f.service.hostedSessions.CreateIdentitySession(t.Context(), identity)
					if err != nil {
						t.Fatal(err)
					}
					user := hostedSecurityUser{identity: identity, token: token}
					ctx := parityHostedContext(t, f, user)
					name := operatortool.OrganizationCreate
					if staff {
						name = operatortool.SupportStart
					}
					definitions, listErr := (hostedOperatorExecutor{f.service}).ListTools(ctx)
					if listErr != nil || !entryKeyHasDefinition(definitions, name) {
						t.Fatalf("conditional account tool absent: %v", listErr)
					}
					result, err := (hostedOperatorExecutor{f.service}).Execute(ctx, parityToolCall(t, name, map[string]any{"name": "Example organization", "organization_id": f.service.config.Hosted.OrganizationID, "reason": "Example assistance", "request_id": "account-parity"}))
					paritySuccess(t, result, err)
				})
			}
		})
	}
}

func paritySuccess(t *testing.T, result operatortool.Result, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	var output struct {
		Status string `json:"status"`
		Action struct {
			Status string `json:"status"`
		} `json:"action"`
	}
	if err := json.Unmarshal(result.Content, &output); err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{output.Status, output.Action.Status} {
		if status != "" && status != "succeeded" {
			t.Fatalf("action did not succeed: %s", result.Content)
		}
	}
}

func TestPostgresMCPCatalogAuthorities(t *testing.T) {
	cases := []struct{ name, authorities string }{
		{"billing_usage.hosted_billing", "owner shared"},
		{"billing_usage.hosted_plan_page", "owner shared admin"},
		{"billing_usage.hosted_usage_report", "owner shared member viewer staff admin"},
		{"board_receipt", "local owner shared member viewer admin"},
		{"board_session", "local owner shared member viewer admin"},
		{"approve_change_review_policy", "local owner shared staff admin"},
		{"artifact_access", "local owner shared member viewer staff admin"},
		{"artifact_references", "local owner shared member viewer staff admin"},
		{"artifact_services", "local owner shared member viewer staff admin"},
		{"associate_release_pick", "local owner shared member admin"},
		{"bind_artifact_service", "local owner shared staff admin"},
		{"change_viewed_files", "local owner shared member viewer staff admin"},
		{"discuss_change", "local owner shared member admin"},
		{"get_artifact_reference", "local owner shared member viewer staff admin"},
		{"get_attempt_diff", "local owner shared member viewer staff admin"},
		{"get_change", "local owner shared member viewer staff admin"},
		{"get_change_review_policy", "local owner shared member viewer staff admin"},
		{"get_change_version", "local owner shared member viewer staff admin"},
		{"get_native_run", "local owner shared member viewer staff admin"},
		{"get_work_item_diff", "local owner shared member viewer staff admin"},
		{"list_work_item_pull_requests", "local owner shared member viewer staff admin"},
		{"publish_release_pick", "local owner shared member admin"},
		{"view_change_file", "local owner shared member admin"},
		{"session_logout", "owner member viewer staff admin"},
		{"credential_create", "owner shared member viewer admin"},
		{"credential_list", "local owner shared member viewer admin"},
		{"credential_revoke", "owner shared member viewer admin"},
		{"analytics_dashboard", "owner shared member viewer staff admin"},
		{"get_runner_routing", "local"},
		{"github_request_counts", "local"},
		{"health", "local"},
		{"hosted_fleet", "owner shared member viewer staff admin"},
		{"native_capabilities", "local"},
		{"time_series", "owner shared member viewer staff admin"},
		{"apply_local_project_policy", "local owner shared staff admin"},
		{"detach_local_project", "local owner shared staff admin"},
		{"drain_local_project", "local owner shared staff admin"},
		{"local_project_configuration", "local owner shared member viewer staff admin"},
		{"resume_local_project", "local owner shared admin"},
		{"invitation_accept", "owner member viewer staff admin"},
		{"invitation_revoke", "owner shared admin"},
		{"member_remove", "owner shared admin"},
		{"organization_list", "local owner shared member viewer staff admin"},
		{"organization_project_rank", "owner shared admin"},
		{"organization_project_rank_update", "owner shared admin"},
		{"organization_switch", "local owner member viewer staff admin"},
		{"advance_git_hub_import", "local owner shared member admin"},
		{"bind_native_repository", "local owner shared staff admin"},
		{"command_git_hub_batch", "local owner shared staff admin"},
		{"create_hosted_project", "owner shared staff admin"},
		{"create_native_project", "local"},
		{"cutover_project", "local owner shared staff admin"},
		{"get_cutover_receipt", "local owner shared member viewer staff admin"},
		{"get_git_hub_import", "local owner shared member viewer staff admin"},
		{"get_project_sprite_allocation", "local owner shared member viewer staff admin"},
		{"get_sprite_pool", "local owner shared member viewer staff admin"},
		{"list_git_hub_import_records", "local owner shared member viewer staff admin"},
		{"project_native_summary", "local owner shared member admin"},
		{"project_secret_metadata", "local owner shared member viewer staff admin"},
		{"remove_project_secret", "local owner shared staff admin"},
		{"repository_freshness", ""},
		{"revoke_project_policy", ""},
		{"start_git_hub_import", "local owner shared member admin"},
		{"update_project_sprite_allocation", "local owner shared staff admin"},
		{"update_sprite_pool", "local owner shared staff admin"},
		{"organization_create", ""},
		{"support_start", "staff"},
	}
	for _, authority := range []string{"local", "owner", "shared", "member", "viewer", "shared_member", "shared_viewer", "admin", "staff"} {
		t.Run(authority, func(t *testing.T) {
			var service *Service
			var ctx context.Context
			switch authority {
			case "local":
				f := newDefaultNativeFixture(t, Config{})
				service = f.service
				ctx = parityNativeContext(t, service, f.project.OrganizationID)
			case "staff":
				f := newOperatorSecurityFixture(t)
				user := f.user(t, "staff", "admin", "support@example.test", "write", "")
				service = f.service
				ctx = parityHostedContext(t, f.hostedSecurityFixture, user)
			default:
				deployment, role := "dedicated", authority
				if authority == "shared" {
					deployment, role = "shared", "owner"
				}
				if strings.HasPrefix(authority, "shared_") {
					deployment, role = "shared", strings.TrimPrefix(authority, "shared_")
				}
				fixture, context := newOperatorKeyFixture(t, deployment, role)
				service, ctx = fixture.service, context
			}
			definitions, err := (hostedOperatorExecutor{service}).ListTools(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, test := range cases {
				t.Run(test.name, func(t *testing.T) {
					want := slices.Contains(strings.Fields(test.authorities), strings.TrimPrefix(authority, "shared_"))
					if strings.HasPrefix(authority, "shared_") && (test.name == operatortool.SessionLogout || test.name == operatortool.OrganizationSwitch || test.name == operatortool.InvitationAccept) {
						want = false
					}
					if entryKeyHasDefinition(definitions, test.name) != want {
						t.Fatalf("%s visibility for %s: want %t", test.name, authority, want)
					}
					allowed, err := service.entryKeyToolAvailable(ctx, test.name)
					if err != nil || allowed != want {
						t.Fatalf("%s call gate for %s: allowed=%t want=%t error=%v", test.name, authority, allowed, want, err)
					}
				})
			}
		})
	}
}

func TestPostgresMCPRuntimeParity(t *testing.T) {
	first := newNativeFixture(t, nil, "", "Runtime parity")
	second := newNativeFixture(t, first.service, first.project.OrganizationID+"_second", "Runtime parity")
	for _, f := range []nativeFixture{first, second} {
		t.Run(string(f.project.OrganizationID), func(t *testing.T) {
			issue := f.create(t, "Recorded runtime")
			ctx := parityNativeContext(t, f.service, f.project.OrganizationID)
			for _, name := range []string{operatortool.BoardReceipt, operatortool.BoardSession} {
				t.Run(name, func(t *testing.T) {
					result, err := (hostedOperatorExecutor{f.service}).Execute(ctx, parityToolCall(t, name, map[string]any{"project_id": f.project.ID, "reference": issue.WorkItemID}))
					paritySuccess(t, result, err)
					if !strings.Contains(string(result.Content), string(issue.WorkItemID)) {
						t.Fatal("runtime result lost work item identity")
					}
				})
			}
		})
	}
}

func TestPostgresMCPImportParity(t *testing.T) {
	first := newCutoverIntegrationFixture(t, &importFixtureBackend{omitDependency: true})
	second := newNativeFixture(t, first.service, first.project.OrganizationID+"_second", "Import parity")
	if _, err := hostedPostgresAdmin(t, first.service).ExecContext(t.Context(), "UPDATE projects SET profile='github_compatible' WHERE organization_id=$1 AND id=$2", second.project.OrganizationID, second.project.ID); err != nil {
		t.Fatal(err)
	}

	for _, f := range []nativeFixture{first, second} {
		t.Run(string(f.project.OrganizationID), func(t *testing.T) {
			if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
				record, err := w.ProjectRecord(t.Context(), f.project.ID)
				if err != nil {
					return err
				}
				record.States = hubrecords.NativeTriageStates(nativeFixtureStates())
				record.Settings.Repository = "example-org/example-project"
				record.Settings.CheckoutRepository = "example-org/example-project"
				record.Settings.Intake = "manual"
				record.Settings.DuplicateChallenge = false
				_, err = w.UpdateProjectRecord(t.Context(), record)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			ctx := parityNativeContext(t, f.service, f.project.OrganizationID)
			e := hostedOperatorExecutor{f.service}
			execute := func(call operatortool.Call) json.RawMessage {
				t.Helper()
				result, err := e.Execute(ctx, call)
				if err != nil {
					var request *operatortool.RequestError
					if errors.As(err, &request) {
						t.Fatalf("%s: %s: %s", call.Name, request.Code, request.Message)
					}
					t.Fatalf("%s: %v", call.Name, err)
				}
				paritySuccess(t, result, err)
				var action struct {
					Result json.RawMessage `json:"result"`
				}
				var value struct {
					Result string `json:"result"`
				}
				if err := json.Unmarshal(result.Content, &value); err != nil {
					t.Fatal(err)
				}
				action.Result = json.RawMessage(value.Result)
				return action.Result
			}
			var job GitHubImport
			raw := execute(projectCall(t, "start_git_hub_import", string(f.project.ID), "parity-start", operatortool.ImportStartInput{IssueNumber: 1}))
			if err := json.Unmarshal(raw, &job); err != nil {
				t.Fatal(err)
			}
			for step := 0; job.Stage != "finished"; step++ {
				raw = execute(projectCall(t, "advance_git_hub_import", string(f.project.ID), fmt.Sprintf("parity-advance-%d", step), operatortool.ImportAdvanceInput{ImportID: job.ID, ExpectedRevision: job.Revision}))
				if err := json.Unmarshal(raw, &job); err != nil {
					t.Fatal(err)
				}
				if step > 20 {
					t.Fatal("import failed to progress")
				}
			}
			for _, name := range []string{"get_git_hub_import", "list_git_hub_import_records"} {
				result, err := e.Execute(ctx, parityToolCall(t, name, map[string]any{"project_id": f.project.ID, "import_id": job.ID}))
				paritySuccess(t, result, err)
				if name == "get_git_hub_import" {
					if !strings.Contains(string(result.Content), job.ID) {
						t.Fatal("import identity missing")
					}
				} else {
					var page struct {
						Data projectImportRecordPage `json:"data"`
					}
					if err := json.Unmarshal(result.Content, &page); err != nil {
						t.Fatal(err)
					}
					if len(page.Data.Records) == 0 || page.Data.Records[0].SourceKey != "issue:I_issue" {
						t.Fatalf("persisted import records missing: %s", result.Content)
					}
				}
			}
			input := operatortool.CutoverInput{DryRun: true, InitialState: "Todo", ClosedState: "Done", States: nativeFixtureStates()}
			raw = execute(projectCall(t, "cutover_project", string(f.project.ID), "parity-preview", input))
			var receipt CutoverReceipt
			if err := json.Unmarshal(raw, &receipt); err != nil {
				t.Fatal(err)
			}
			input.DryRun, input.Checkpoint = false, receipt.Checkpoint
			raw = execute(projectCall(t, "cutover_project", string(f.project.ID), "parity-cutover", input))
			if err := json.Unmarshal(raw, &receipt); err != nil {
				t.Fatal(err)
			}
			result, err := e.Execute(ctx, parityToolCall(t, "get_cutover_receipt", map[string]any{"project_id": f.project.ID}))
			paritySuccess(t, result, err)
			if !strings.Contains(string(result.Content), receipt.Checkpoint) {
				t.Fatal("cutover receipt changed")
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/integration", testHubAdminToken, map[string]any{"idempotency_key": "parity-summary-mode", "expected_revision": fmt.Sprint(receipt.Integration.Revision), "intake": "disabled", "projection": "summary", "repository_enabled": false}), http.StatusOK)
			raw = execute(projectCall(t, "project_native_summary", string(f.project.ID), "parity-summary", operatortool.SummaryInput{ItemID: job.WorkItemID, Body: "Example summary"}))
			if !strings.Contains(string(raw), `"status":"pending"`) {
				t.Fatal("summary was not queued through the PostgreSQL owner")
			}
		})
	}
}
