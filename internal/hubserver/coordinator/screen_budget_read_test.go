package coordinator_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/budget"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/conversations"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/coordinator"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestCoordinatorProjectBudgetPropagatesStorageError(t *testing.T) {
	t.Parallel()
	storageErr := errors.New("example budget storage failure")
	identity := operatortool.Identity{PrincipalID: "member", OrganizationID: "org_budget", CredentialID: "credential"}
	connection := operatortool.Connection{Identity: identity, Resolve: func(context.Context) (operatortool.Authority, error) {
		return operatortool.Authority{Identity: identity, Check: func(context.Context, operatortool.Requirement) error { return nil }}, nil
	}}
	deps := coordinator.ScreenReadDeps{
		ActionContext: func(ctx context.Context, _ conversations.ConversationRecord) (context.Context, error) {
			return operatortool.WithConnection(ctx, connection), nil
		},
		Operator: func(context.Context) (hubapi.APICredential, error) {
			return hubapi.APICredential{ID: identity.CredentialID}, nil
		},
		AuthorizeRecord: func(context.Context, hubscope.NativeScope, conversations.ConversationRecord, bool) error { return nil },
		ReadBudget: func(_ context.Context, scope hubscope.NativeScope) (any, error) {
			if scope.Organization != "org_budget" || scope.Project != "prj_budget" {
				t.Fatalf("budget read escaped the conversation scope: %+v", scope)
			}
			return nil, storageErr
		},
	}
	record := conversations.ConversationRecord{OrganizationID: "org_budget", ProjectID: "prj_budget"}
	call := runner.AgentToolCall{Name: "get_project_budget", Arguments: json.RawMessage(`{}`)}
	value, err := deps.Read(t.Context(), record, call)
	if value != nil || !errors.Is(err, storageErr) {
		t.Fatalf("read returned %v with %v, want the storage error", value, err)
	}
}

func TestCoordinatorProjectBudgetReadOnly(t *testing.T) {
	t.Parallel()
	repository := hubtest.Repository(t)
	organization := hubtest.Organization(t, repository, hubtest.OrganizationOptions{})
	project := hubtest.Project(t, repository, organization.OrganizationID, hubtest.ProjectOptions{})
	org := project.OrganizationID
	now := time.Now().UTC().Truncate(time.Microsecond)
	const cap = int64(1000)
	policy := budget.MonthlyPolicy{Enabled: true, Mode: budget.HardStop, TotalMicros: new(cap)}
	if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
		return w.SetBillingMonthlyPolicy(t.Context(), billingstore.MonthlyPolicy{OrganizationID: org, ProjectID: project.ID, Policy: policy, Actor: "example-operator", UpdatedAt: now}, 0)
	}); err != nil {
		t.Fatal(err)
	}
	read := func(ctx context.Context, organization tracker.OrganizationID, fn func(tenantstore.Reader) error) error {
		return repository.ReadTenant(ctx, organization, corestore.Serving, fn)
	}
	authenticator := hubauth.New(hubauth.Deps{Organization: org, Hosted: &hubauth.Hosted{OrganizationID: string(org)}, Now: func() time.Time { return now }})
	records := coordinator.RecordDeps{ReadTenant: read, Authorize: func(ctx context.Context, r accessstore.Reader, scope hubscope.NativeScope, write bool) error {
		return authenticator.Authorize(ctx, r, conversations.ScopeInput(scope), write)
	}}
	for _, test := range []struct {
		name, role   string
		manageRunner bool
	}{{"member", "member", false}, {"runner member", "member", true}, {"viewer", "viewer", false}} {
		t.Run(test.name, func(t *testing.T) {
			member := hubtest.Member(t, repository, org, hubtest.MemberOptions{Role: test.role, ReadOnly: true, ManageRunners: test.manageRunner, Projects: []tracker.ProjectID{project.ID}, Now: now})
			key := hubtest.Credential(t, repository, org, hubtest.CredentialOptions{KeyScope: string(apikey.ScopeRead), Member: &member, Projects: []tracker.ProjectID{project.ID}, Now: now})
			credential := hubapi.APICredential{ID: key.ID, Hash: key.Hash, Scope: hubapi.APIScopeOperator, HostedRole: test.role, HostedMembership: member.Member.MembershipID, HostedKeyScope: apikey.ScopeRead, Hosted: &auth.HostedIdentity{Subject: member.Member.UserID}}
			var record conversations.ConversationRecord
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				audience, err := conversations.ConversationReadAudienceRecord(t.Context(), w, org, hubscope.AuthCredential(credential))
				if err != nil {
					return err
				}
				row, err := w.CreateConversation(t.Context(), conversationstore.Conversation{
					OrganizationID: org, ProjectID: project.ID, ID: conversation.NewConversationID(), OwnerPrincipalID: key.ID,
					OwnerSubject: member.Member.UserID, Visibility: conversation.VisibilityPrivate, Status: conversation.StatusActive,
					Revision: 1, Preferences: conversation.DefaultPreferences(), ReadAudience: audience, CreatedAt: now, UpdatedAt: now,
				})
				record = conversations.ConversationRecordFromStore(row)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			identity := operatortool.Identity{PrincipalID: key.ID, OrganizationID: string(org), CredentialID: key.ID}
			scope := hubscope.NativeScope{Organization: org, Project: project.ID, Credential: credential}
			connection := operatortool.Connection{Identity: identity, Resolve: func(context.Context) (operatortool.Authority, error) {
				return operatortool.Authority{Identity: identity, Check: func(ctx context.Context, req operatortool.Requirement) error {
					return records.AuthorizeRecord(ctx, scope, record, req.Scope == apikey.ScopeWrite)
				}}, nil
			}}
			deps := coordinator.ScreenReadDeps{
				ActionContext: func(ctx context.Context, _ conversations.ConversationRecord) (context.Context, error) {
					return operatortool.WithConnection(ctx, connection), nil
				},
				Operator:        func(context.Context) (hubapi.APICredential, error) { return credential, nil },
				AuthorizeRecord: records.AuthorizeRecord,
				RestrictAudience: func(ctx context.Context, row conversations.ConversationRecord, credential hubapi.APICredential) error {
					return records.AuthorizeRecord(ctx, hubscope.NativeScope{Organization: row.OrganizationID, Project: row.ProjectID, Credential: credential}, row, false)
				},
				ReadBudget: func(ctx context.Context, scope hubscope.NativeScope) (any, error) {
					var found billingstore.MonthlyPolicy
					err := read(ctx, scope.Organization, func(r tenantstore.Reader) error {
						var err error
						found, err = r.BillingMonthlyPolicy(ctx, scope.Project)
						return err
					})
					return found, err
				},
			}
			value, err := deps.Read(t.Context(), record, runner.AgentToolCall{Name: "get_project_budget", Arguments: json.RawMessage(`{}`)})
			found, ok := value.(billingstore.MonthlyPolicy)
			if err != nil || !ok || found.Revision != 1 || !found.Policy.Enabled || found.Policy.Mode != budget.HardStop || found.Policy.TotalMicros == nil || *found.Policy.TotalMicros != cap {
				t.Fatalf("read-only authority did not receive its policy: %+v (%v)", value, err)
			}
		})
	}
}
