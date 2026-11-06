package hubserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestClaimMergingHydrationHasFixedSnapshotCost(t *testing.T) {
	admin := postgresStartupTemplate.Database(t)
	connection := startupConnection(t, admin, "detent_runtime_v2", "")
	config := postgresStartupTemplate.NativeConfig(t, admin, connection.User, connection.Password)
	config.MaxConns = 3
	var exchanges atomic.Int64
	dial := config.ConnConfig.DialFunc
	config.ConnConfig.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := dial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return &nativeLeaseWireConn{Conn: conn, exchanges: &exchanges, startup: true}, nil
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	repository, err := postgres.NewRepository(t.Context(), pool, postgres.PoolConfig{MaxConnections: 3, ServingConnections: 2, BackgroundConnections: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repository.Close(); err != nil {
			t.Error(err)
		}
	})
	now := time.Now().UTC().Truncate(time.Microsecond)
	details := &memoryRunDetails{}
	service := &Service{database: &database{postgres: repository}, config: Config{TenantRepository: repository, AccessRepository: repository, RunDetails: details, SecretKeys: secretTestKeys(t, "1", "1"), now: func() time.Time { return now }}}
	content := eventstore.NewContent(details)
	var baselineExchanges, baselineStatements int64
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		service.localOrganization = string(org)
		seedPostgresHubAdmin(t, repository, service, nativeOrganization{ID: org, Name: string(org), Slug: string(org)})
		descriptor := hubTestPolicy()
		policy, err := content.PrepareProjectPolicy(t.Context(), org, descriptor)
		if err != nil {
			t.Fatal(err)
		}
		policy.ProjectID = "example"
		policy.ApprovedBy = "test_hub_admin"
		policy.ApprovedAt = now
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: "example", Name: "Example", Profile: "native"}, States: append(nativeFixtureStates(), tracker.NativeState{Name: "Merging", Dispatchable: true})}); err != nil {
				return err
			}
			return w.ApproveProjectPolicy(t.Context(), policy, "")
		}); err != nil {
			t.Fatal(err)
		}
		scope := nativeScope{organization: org, project: "example", credential: apiCredential{ID: "test_hub_admin", Hash: apikey.HashToken(testHubAdminToken), Scope: apiScopeAdmin}}
		for index := range 50 {
			id := tracker.NativeWorkItemID(fmt.Sprintf("wi_%03d", index))
			changeID := fmt.Sprintf("change_%03d", index)
			versionID := fmt.Sprintf("version_%03d", index)
			issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: "example", WorkItemID: id, Number: index + 1, Revision: 1}, Title: "Example", State: "Merging", Actor: tracker.Actor{Kind: "human", PrincipalID: "test_hub_admin"}, CreatedAt: now, UpdatedAt: now}
			raw, _ := json.Marshal(issue)
			collaboration, err := content.PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: org, ProjectID: "example", WorkItemID: id, RecordID: string(id), Revision: 1}, raw)
			if err != nil {
				t.Fatal(err)
			}
			change := tracker.ChangeRequest{ID: changeID, OrganizationID: org, ProjectID: "example", WorkItemID: id, LinkedIssues: []tracker.NativeWorkItemID{id}, Title: "Example", Revision: 1, CurrentVersion: versionID, CreatedAt: now, UpdatedAt: now}
			raw, _ = json.Marshal(change)
			changeRow, err := content.PrepareChange(t.Context(), org, raw)
			if err != nil {
				t.Fatal(err)
			}
			input := changeTestInput()
			input.PolicyID = descriptor.ID
			version := tracker.ChangeVersion{ID: versionID, ChangeID: changeID, Number: 1, ChangeVersionInput: input, Policy: descriptor, Actor: issue.Actor, CreatedAt: now}
			raw, _ = json.Marshal(version)
			versionRow, err := content.PrepareVersion(t.Context(), org, raw)
			if err != nil {
				t.Fatal(err)
			}
			run := securityaudit.Run{ID: int64(index + 1), ProjectID: "example", Repository: input.Repository, BaseSHA: input.BaseSHA, HeadSHA: input.HeadSHA, Findings: []securityaudit.Finding{{ID: "finding", Body: "Example", Certainty: securityaudit.CertaintyConfirmed}}}
			review := tracker.ChangeReview{ID: "review", VersionID: versionID, Decision: "changes_requested", Actor: issue.Actor, CreatedAt: now, SecurityAudit: &tracker.SecurityAuditReview{Run: run, VersionID: versionID}}
			raw, _ = json.Marshal(review)
			evidence, err := content.PrepareEvidence(t.Context(), eventstore.Evidence{OrganizationID: org, ChangeID: changeID, VersionID: &versionID, Kind: "review", CreatedAt: now}, raw)
			if err != nil {
				t.Fatal(err)
			}
			disposition := securityaudit.Disposition{AuditRunID: run.ID, FindingID: "finding", Status: securityaudit.DispositionFalsePositive, ServiceIdentity: securityaudit.ServiceIdentity("example"), RecordedAt: now, Evidence: "Example source evidence"}
			raw, _ = json.Marshal(disposition)
			ref, err := details.StoreDetail(t.Context(), string(org), raw)
			if err != nil {
				t.Fatal(err)
			}
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: "example", NativeID: id, Number: int64(index + 1), Title: issue.Title, Revision: 1, ActorKind: &issue.Actor.Kind, ActorPrincipalID: &issue.Actor.PrincipalID, WorkflowStateName: &issue.State}); err != nil {
					return fmt.Errorf("CreateWorkItem: %w", err)
				}
				if err := w.PutWorkItemFacts(t.Context(), claimstore.WorkItemFacts{OrganizationID: org, ProjectID: "example", WorkItemID: id}); err != nil {
					return err
				}
				if err := w.PutQueueEntry(t.Context(), claimstore.QueueEntry{OrganizationID: org, ProjectID: "example", WorkItemID: id, Scope: "example", State: "Merging", CreatedAt: now, UpdatedAt: now}); err != nil {
					return err
				}
				if _, err := w.CreateCollaborationVersion(t.Context(), collaboration); err != nil {
					return fmt.Errorf("CreateCollaborationVersion: %w", err)
				}
				if _, err := w.CreateChange(t.Context(), changeRow); err != nil {
					return fmt.Errorf("CreateChange: %w", err)
				}
				if _, err := w.CreateVersion(t.Context(), versionRow); err != nil {
					return fmt.Errorf("CreateVersion: %w", err)
				}
				if err := w.LinkIssue(t.Context(), eventstore.Link{OrganizationID: org, ProjectID: "example", WorkItemID: id, ChangeID: changeID, CreatedAt: now}); err != nil {
					return fmt.Errorf("LinkIssue: %w", err)
				}
				evidence, err = w.AppendEvidence(t.Context(), evidence)
				if err != nil {
					return fmt.Errorf("append evidence: %w", err)
				}
				_, err = w.(eventstore.AuditDispositionWriter).CreateAuditDisposition(t.Context(), eventstore.AuditDisposition{Organization: org, Project: "example", ChangeID: changeID, ReviewSequence: evidence.Sequence, ActorID: "test_hub_admin", RequestKey: changeID, RequestHash: "example", Reference: ref, Disposition: disposition})
				if err != nil {
					return fmt.Errorf("disposition: %w", err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if index != 1 && index != 49 {
				continue
			}
			request := tracker.NativeClaim{PolicyID: descriptor.ID, WorkflowStates: []string{"Merging"}}
			scan, err := service.readNativeClaimCandidates(t.Context(), scope, request, now)
			if err != nil {
				t.Fatal(err)
			}
			if len(scan.plans) != index+1 {
				t.Fatalf("candidates=%d want %d", len(scan.plans), index+1)
			}
			legacy := scan.plans[0]
			legacy.changeFacts = nil
			expected, err := service.nativeClaimCurrentChangeDetail(t.Context(), scope, legacy, now)
			if err != nil {
				t.Fatal(err)
			}
			var statements int64
			ctx := postgres.WithTransactionTiming(t.Context(), nil, func(time.Time, string) { statements++ })
			exchanges.Store(0)
			scan, err = service.readNativeClaimCandidates(ctx, scope, request, now)
			if err != nil {
				t.Fatal(err)
			}
			for _, plan := range scan.plans {
				hydrated, err := service.hydrateNativeClaimPlan(ctx, scope, request, now, plan, &descriptor)
				if err != nil {
					t.Fatal(err)
				}
				if hydrated.changeRecord.OrganizationID != org {
					t.Fatal("cross tenant change")
				}
				detail, err := service.nativeClaimCurrentChangeDetail(ctx, scope, plan, now)
				if err != nil {
					t.Fatal(err)
				}
				if plan.candidate.WorkItemID == scan.plans[0].candidate.WorkItemID && !reflect.DeepEqual(detail, expected) {
					t.Fatal("snapshot changed validated detail")
				}
			}
			got := exchanges.Load()
			t.Logf("tenant=%s candidates=%d statements=%d exchanges=%d", org, index+1, statements, got)
			if baselineExchanges == 0 {
				baselineExchanges = got
				baselineStatements = statements
			}
			if got > 25 || got != baselineExchanges || statements != baselineStatements {
				t.Errorf("snapshot cost grew: statements=%d exchanges=%d; baseline=%d/%d", statements, got, baselineStatements, baselineExchanges)
			}
		}
	}
}
