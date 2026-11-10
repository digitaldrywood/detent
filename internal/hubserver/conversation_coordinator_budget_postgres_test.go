package hubserver

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/budget"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type coordinatorBudgetProvider struct {
	*hostedSecurityProvider
	organization string
}

func (p coordinatorBudgetProvider) Organization(_ context.Context, id string) (auth.Organization, error) {
	return auth.Organization{ID: id, ExternalID: p.organization, Name: "Example organization"}, nil
}

func coordinatorBudgetPeer(t *testing.T, first hostedSecurityFixture) hostedSecurityFixture {
	t.Helper()
	cfg := first.service.config
	hosted := *cfg.Hosted
	cfg.Hosted = &hosted
	hosted.OrganizationID, hosted.WorkOSOrganizationID, hosted.BootstrapSubject = "org_budget_second", "provider_budget_second", "owner_budget_second"
	provider := newHostedSecurityProvider()
	hosted.Provider = coordinatorBudgetProvider{hostedSecurityProvider: provider, organization: hosted.OrganizationID}
	cfg.InitialAdminToken = []byte("example-second-budget-admin")
	organization := tracker.OrganizationID(hosted.OrganizationID)
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
		if err := w.CreateHubIdentity(t.Context(), accessstore.HubIdentity{OrganizationID: organization, ID: newNativeID("hub"), CursorKey: key[:]}); err != nil {
			return err
		}
		return w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: organization, Kind: accessstore.NativeCursorKeyKind, Envelope: envelope, UpdatedAt: now}, accessstore.SecretAudit{OrganizationID: organization, Kind: accessstore.NativeCursorKeyKind, Actor: "fixture", Event: "create", KeyVersion: envelope.Version, RecordedAt: now})
	}); err != nil {
		t.Fatal(err)
	}
	service := openHostedPostgresService(t, cfg)
	project := tracker.ProjectID("prj_budget_second")
	seedHostedPostgresProject(t, service, project)
	return hostedSecurityFixture{service: service, provider: provider, project: project, base: "/api/v2/organizations/" + hosted.OrganizationID + "/projects/" + string(project)}
}

func coordinatorBudgetUser(t *testing.T, f hostedSecurityFixture, name, role string, manageRunner bool) hostedSecurityUser {
	t.Helper()
	organization := f.service.config.Hosted.OrganizationID
	now := f.service.config.now()
	subject := organization + "_" + name
	identity := auth.Identity{Subject: subject, Email: subject + "@example.test", EmailVerified: true, Hosted: &auth.HostedIdentity{Subject: subject, OrganizationID: f.service.config.Hosted.WorkOSOrganizationID, SessionID: "session_" + subject, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}}
	f.provider.mu.Lock()
	f.provider.sessions[identity.Hosted.SessionID] = *identity.Hosted
	f.provider.mu.Unlock()
	member, err := f.provider.CreateMembership(t.Context(), subject, identity.Hosted.OrganizationID, role)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.addHostedMember(t.Context(), identity, member); err != nil {
		t.Fatal(err)
	}
	token, _, err := f.service.hostedSessions.CreateIdentitySession(t.Context(), identity)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), tracker.OrganizationID(organization), corestore.Serving, func(w tenantstore.Writer) error {
		stored, err := w.AccessMember(t.Context(), subject)
		if err != nil {
			return err
		}
		if err := w.SetAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: tracker.OrganizationID(organization), UserID: subject, ProjectID: f.project, ManageRunner: manageRunner}); err != nil {
			return err
		}
		return w.SetAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: tracker.OrganizationID(organization), TokenID: stored.PrincipalID, ProjectID: f.project})
	}); err != nil {
		t.Fatal(err)
	}
	return hostedSecurityUser{identity: identity, token: token}
}

func coordinatorBudgetTools(t *testing.T, f hostedSecurityFixture, user hostedSecurityUser) (*coordinatorToolset, context.Context, conversationRecord) {
	t.Helper()
	organization := tracker.OrganizationID(f.service.config.Hosted.OrganizationID)
	now := f.service.config.now()
	var record conversationRecord
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		member, err := w.AccessMember(t.Context(), user.identity.Subject)
		if err != nil {
			return err
		}
		row, err := w.CreateConversation(t.Context(), conversationstore.Conversation{OrganizationID: organization, ID: conversation.NewConversationID(), ProjectID: f.project, OwnerPrincipalID: member.PrincipalID, OwnerSubject: user.identity.Subject, Visibility: conversation.VisibilityPrivate, Status: conversation.StatusActive, Revision: 1, Preferences: conversation.DefaultPreferences(), Execution: conversation.Execution{Status: conversation.ExecutionIdle, UpdatedAt: now}, CreatedAt: now, UpdatedAt: now})
		record = conversationRecordFromStore(row)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var actorCtx context.Context
	path := "/api/v2/organizations/:organization/budget-context/" + record.ID
	f.service.echo.GET(path, func(e echo.Context) error {
		var err error
		actorCtx, err = f.service.bindCoordinatorConnection(e.Request().Context(), record.ID)
		if err != nil {
			return err
		}
		return e.NoContent(http.StatusOK)
	}, f.service.operatorAuthority)
	response := f.request(t, user, http.MethodGet, "/api/v2/organizations/"+string(organization)+"/budget-context/"+record.ID, nil)
	requireNativeStatus(t, response, http.StatusOK)
	if actorCtx == nil {
		t.Fatal("budget coordinator context is missing")
	}
	coordinator := f.service.conversations.coordinator.(*conversationTurnCoordinator)
	state := &coordinatorTurnState{coordinator: coordinator, conversationID: record.ID, users: []conversationMessageRecord{{Role: conversation.RoleUser, CreatedAt: now, Data: coordinatorConnectionData(actorCtx)}}}
	return newCoordinatorToolset(coordinator, state), actorCtx, record
}

type coordinatorBudgetReadRepository struct {
	tenantstore.Repository
	policyError error
	policyReads int
}

func (r *coordinatorBudgetReadRepository) ReadTenant(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, read func(tenantstore.Reader) error) error {
	return r.Repository.ReadTenant(ctx, organization, workload, func(reader tenantstore.Reader) error {
		return read(coordinatorBudgetReadReader{Reader: reader, repository: r})
	})
}

type coordinatorBudgetReadReader struct {
	tenantstore.Reader
	repository *coordinatorBudgetReadRepository
}

func (r coordinatorBudgetReadReader) BillingMonthlyPolicy(ctx context.Context, project tracker.ProjectID) (billingstore.MonthlyPolicy, error) {
	r.repository.policyReads++
	if r.repository.policyError != nil {
		return billingstore.MonthlyPolicy{}, r.repository.policyError
	}
	return r.Reader.BillingMonthlyPolicy(ctx, project)
}

func TestCoordinatorProjectBudgetPostgres(t *testing.T) {
	first := newHostedSecurityFixture(t, func(cfg *Config) {
		cfg.Conversation = &ConversationConfig{Enabled: true, Backend: newFakeCoordinatorBackend(), Workspace: t.TempDir(), ArchiveContents: &conversationContentFixture{}}
	})
	second := coordinatorBudgetPeer(t, first)
	fixtures := []hostedSecurityFixture{first, second}
	for index, f := range fixtures {
		organization := tracker.OrganizationID(f.service.config.Hosted.OrganizationID)
		total := int64(1000 * (index + 1))
		policy := budget.MonthlyPolicy{Enabled: true, Mode: budget.HardStop, TotalMicros: &total}
		now := f.service.config.now()
		if err := f.service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			return w.SetBillingMonthlyPolicy(t.Context(), billingstore.MonthlyPolicy{OrganizationID: organization, Policy: budget.MonthlyPolicy{Enabled: true, Mode: budget.HardStop, TotalMicros: new(int64(9000))}, Actor: "example-operator", UpdatedAt: now}, 0)
		}); err != nil {
			t.Fatal(err)
		}
		defaultUser := coordinatorBudgetUser(t, f, "default-owner", "owner", false)
		defaultTools, defaultContext, defaultRecord := coordinatorBudgetTools(t, f, defaultUser)
		defaultValue, err := defaultTools.screenRead(defaultContext, defaultRecord, runner.AgentToolCall{Name: "get_project_budget", Arguments: json.RawMessage(`{}`)})
		if err != nil || !reflect.DeepEqual(defaultValue, monthlyBudgetSettings{}) {
			t.Fatalf("absent project policy inherited another scope: value=%+v error=%v", defaultValue, err)
		}
		if err := f.service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			return w.SetBillingMonthlyPolicy(t.Context(), billingstore.MonthlyPolicy{OrganizationID: organization, ProjectID: f.project, Policy: policy, Actor: "example-operator", UpdatedAt: now}, 0)
		}); err != nil {
			t.Fatal(err)
		}
		for _, role := range []struct {
			name, role string
			runner     bool
		}{{"owner", "owner", false}, {"admin", "admin", false}, {"member", "member", false}, {"runner_member", "member", true}, {"viewer", "viewer", false}} {
			t.Run(string(organization)+"/"+role.name, func(t *testing.T) {
				user := coordinatorBudgetUser(t, f, role.name, role.role, role.runner)
				tools, ctx, record := coordinatorBudgetTools(t, f, user)
				call := runner.AgentToolCall{Name: "get_project_budget", Arguments: json.RawMessage(`{}`)}
				original := f.service.config.TenantRepository
				probe := &coordinatorBudgetReadRepository{Repository: original}
				f.service.config.TenantRepository = probe
				t.Cleanup(func() { f.service.config.TenantRepository = original })
				if f.service.database.reader != nil {
					t.Fatal("budget fixture unexpectedly has a legacy SQL reader")
				}
				value, err := tools.screenRead(ctx, record, call)
				want := monthlyBudgetSettings{Revision: 1, Policy: policy}
				if err != nil || !reflect.DeepEqual(value, want) {
					t.Fatalf("project budget role=%s value=%+v want=%+v error=%v", role.role, value, want, err)
				}
				for _, args := range []string{`{"project_id":"prj_foreign"}`, `{"organization_id":"org_foreign"}`, `{"scope":"organization"}`, `{"unknown":true}`} {
					probe.policyReads = 0
					_, err := tools.screenRead(ctx, record, runner.AgentToolCall{Name: call.Name, Arguments: json.RawMessage(args)})
					if err == nil || probe.policyReads != 0 {
						t.Fatalf("budget arguments crossed project boundary: args=%s reads=%d error=%v", args, probe.policyReads, err)
					}
				}
				storageError := errors.New("example budget storage failure")
				probe.policyError = storageError
				if _, err := tools.screenRead(ctx, record, call); !errors.Is(err, storageError) {
					t.Fatalf("budget storage error = %v, want original error", err)
				}
				probe.policyError = nil
				if role.name == "member" {
					if err := original.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
						return w.DeleteAccessProjectGrant(t.Context(), user.identity.Subject, f.project)
					}); err != nil {
						t.Fatal(err)
					}
					probe.policyReads = 0
					if _, err := tools.screenRead(ctx, record, call); !errors.Is(err, operatortool.ErrAccessDenied) || probe.policyReads != 0 {
						t.Fatalf("revoked project access read budget: reads=%d error=%v", probe.policyReads, err)
					}
				}
			})
		}
	}
	var tenants int
	if err := hostedPostgresAdmin(t, first.service).QueryRowContext(t.Context(), `SELECT count(DISTINCT organization_id) FROM monthly_budget_policies WHERE organization_id IN ($1,$2) AND project_id<>''`, first.service.accessOrganization(), second.service.accessOrganization()).Scan(&tenants); err != nil || tenants != 2 {
		t.Fatalf("budgets did not share isolated PG database: tenants=%d error=%v", tenants, err)
	}
}
