package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/gate"
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
			review := tracker.ChangeReview{ID: "review", VersionID: versionID, Decision: "changes_requested", Actor: issue.Actor, CreatedAt: now, SecurityAudit: &tracker.SecurityAuditReview{SessionID: run.ID, Run: run, VersionID: versionID}}
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
			}
			got := exchanges.Load()
			detail, err := service.nativeClaimCurrentChangeDetail(t.Context(), scope, scan.plans[0], now)
			if err != nil || !reflect.DeepEqual(detail, expected) {
				t.Fatalf("snapshot changed validated detail: %v", err)
			}
			t.Logf("tenant=%s candidates=%d statements=%d exchanges=%d", org, index+1, statements, got)
			if baselineExchanges == 0 {
				baselineExchanges = got
				baselineStatements = statements
			}
			if got > 25 || got != baselineExchanges || statements != baselineStatements {
				t.Errorf("snapshot cost grew: statements=%d exchanges=%d; baseline=%d/%d", statements, got, baselineStatements, baselineExchanges)
			}
		}
		input := changeTestInput()
		for index := range 50 {
			id := int64(1000 + index)
			if index == 49 {
				id = 501
			}
			versionID := fmt.Sprintf("version_%03d", index)
			run := securityaudit.Run{ID: id, ProjectID: "example", Repository: input.Repository, BaseSHA: input.BaseSHA, HeadSHA: input.HeadSHA}
			review := tracker.ChangeReview{ID: "legacy-run", VersionID: versionID, Decision: "changes_requested", CreatedAt: now, SecurityAudit: &tracker.SecurityAuditReview{SessionID: id, VersionID: versionID, Run: run}}
			raw, err := json.Marshal(review)
			if err != nil {
				t.Fatal(err)
			}
			evidence, err := content.PrepareEvidence(t.Context(), eventstore.Evidence{OrganizationID: org, ChangeID: fmt.Sprintf("change_%03d", index), Kind: "review"}, raw)
			if err != nil {
				t.Fatal(err)
			}
			evidence.ReviewKind = ""
			evidence.ReviewSessionKnown = false
			evidence.ReviewSessionID = 0
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error { _, err := w.AppendEvidence(t.Context(), evidence); return err }); err != nil {
				t.Fatal(err)
			}
		}

		disposition := securityaudit.Disposition{AuditRunID: 501, FindingID: "foreign", Status: securityaudit.DispositionFalsePositive, ServiceIdentity: securityaudit.ServiceIdentity("example"), RecordedAt: now}
		raw, _ := json.Marshal(disposition)
		ref, err := details.StoreDetail(t.Context(), string(org), raw)
		if err != nil {
			t.Fatal(err)
		}
		request := tracker.NativeClaim{PolicyID: descriptor.ID, WorkItemID: "wi_049", WorkflowStates: []string{"Merging"}}
		scalar, err := service.readNativeClaimPlanRows(t.Context(), scope, request, now, nil, nil, nativeClaimPlan{})
		if err != nil {
			t.Fatal(err)
		}
		scalar.changeFacts = nil
		expected, err := service.nativeClaimCurrentChangeDetail(t.Context(), scope, scalar, now)
		if err != nil {
			t.Fatalf("scalar legacy audit without dispositions: %v", err)
		}
		actual, err := service.readNativeClaimPlan(t.Context(), scope, request, now)
		if err != nil {
			t.Errorf("batch legacy audit without dispositions tenant=%s: %v", org, err)
		} else {
			detail, err := service.nativeClaimCurrentChangeDetail(t.Context(), scope, actual, now)
			if err != nil || !reflect.DeepEqual(detail, expected) {
				t.Errorf("legacy audit detail changed: %v", err)
			}
		}
		var legacyExchanges, legacyStatements int64
		for _, size := range []int{2, 50} {
			pageRequest := tracker.NativeClaim{PolicyID: descriptor.ID, WorkflowStates: []string{"Merging"}}
			for index := 50 - size; index < 50; index++ {
				pageRequest.ProviderCandidates = append(pageRequest.ProviderCandidates, tracker.NativeCapacityCandidate{WorkItemID: tracker.NativeWorkItemID(fmt.Sprintf("wi_%03d", index))})
			}
			var statements int64
			ctx := postgres.WithTransactionTiming(t.Context(), nil, func(time.Time, string) { statements++ })
			exchanges.Store(0)
			scan, err := service.readNativeClaimCandidates(ctx, scope, pageRequest, now)
			if err != nil {
				t.Fatal(err)
			}
			if len(scan.plans) != size {
				t.Fatalf("legacy candidates=%d want %d", len(scan.plans), size)
			}
			for _, plan := range scan.plans {
				if _, err := service.hydrateNativeClaimPlan(ctx, scope, pageRequest, now, plan, &descriptor); err != nil {
					t.Errorf("legacy page hydration tenant=%s size=%d: %v", org, size, err)
				}
			}
			got := exchanges.Load()
			t.Logf("legacy tenant=%s candidates=%d statements=%d exchanges=%d", org, size, statements, got)
			if size == 2 {
				legacyExchanges, legacyStatements = got, statements
			} else if got != legacyExchanges || statements != legacyStatements {
				t.Errorf("legacy snapshot cost grew: %d/%d versus %d/%d", statements, got, legacyStatements, legacyExchanges)
			}
		}
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			_, err := w.(eventstore.AuditDispositionWriter).CreateAuditDisposition(t.Context(), eventstore.AuditDisposition{Organization: org, Project: "example", ChangeID: "change_000", ReviewSequence: 1, ActorID: "test_hub_admin", RequestKey: "foreign-run", RequestHash: "example", Reference: ref, Disposition: disposition})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := service.nativeClaimCurrentChangeDetail(t.Context(), scope, scalar, now); !errors.Is(err, corestore.ErrRevisionConflict) {
			t.Errorf("scalar legacy foreign disposition: %v", err)
		}
		if _, err := service.readNativeClaimPlan(t.Context(), scope, request, now); !errors.Is(err, corestore.ErrRevisionConflict) {
			t.Fatalf("foreign audit disposition outside candidate page: %v", err)
		}

		for index, tc := range []struct {
			name       string
			reviewKind string
			corrupt    func(*eventstore.Evidence)
		}{
			{"kind", "implementation", func(row *eventstore.Evidence) { row.ReviewKind = "human" }},
			{"landing", "security_audit", func(row *eventstore.Evidence) { row.ReviewLanding = true }},
			{"session_id", "validator", func(row *eventstore.Evidence) { row.ReviewSessionID++ }},
			{"session_machine", "implementation", func(row *eventstore.Evidence) { row.ReviewSessionMachine = "machine_other" }},
		} {
			t.Run(string(org)+"/typed_"+tc.name, func(t *testing.T) {
				changeID, versionID := fmt.Sprintf("change_%03d", index), fmt.Sprintf("version_%03d", index)
				sessionID := int64(800 + index)
				review := tracker.ChangeReview{ID: "provenance_" + tc.name, VersionID: versionID, Decision: "approved", CreatedAt: now, SessionMachineID: "machine_expected"}
				switch tc.reviewKind {
				case "implementation":
					review.ImplementationReview = &gate.ValidatorResult{SessionID: sessionID, VersionID: versionID}
				case "validator":
					review.Validator = &gate.ValidatorResult{SessionID: sessionID, VersionID: versionID}
				case "security_audit":
					review.SecurityAudit = &tracker.SecurityAuditReview{SessionID: sessionID, VersionID: versionID, Run: securityaudit.Run{ID: sessionID, ProjectID: "example"}}
				}
				raw, err := json.Marshal(review)
				if err != nil {
					t.Fatal(err)
				}
				evidence, err := content.PrepareEvidence(t.Context(), eventstore.Evidence{OrganizationID: org, ChangeID: changeID, Kind: "review"}, raw)
				if err != nil {
					t.Fatal(err)
				}
				tc.corrupt(&evidence)
				if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error { _, err := w.AppendEvidence(t.Context(), evidence); return err }); err != nil {
					t.Fatal(err)
				}
				request := tracker.NativeClaim{PolicyID: descriptor.ID, WorkItemID: tracker.NativeWorkItemID(fmt.Sprintf("wi_%03d", index)), WorkflowStates: []string{"Merging"}}
				plan, err := service.readNativeClaimPlanRows(t.Context(), scope, request, now, nil, nil, nativeClaimPlan{})
				if err != nil {
					t.Fatal(err)
				}
				scalar := plan
				scalar.changeFacts = nil
				if _, err := service.hydrateNativeClaimPlan(t.Context(), scope, request, now, scalar, &descriptor); !errors.Is(err, artifact.ErrIntegrity) {
					t.Errorf("scalar accepted typed %s mismatch: %v", tc.name, err)
				}
				if _, err := service.hydrateNativeClaimPlan(t.Context(), scope, request, now, plan, &descriptor); !errors.Is(err, artifact.ErrIntegrity) {
					t.Errorf("batch accepted typed %s mismatch: %v", tc.name, err)
				}
			})
		}

	}
}
