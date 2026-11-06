package hubserver

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/explain"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresRuntimeEvidenceReadsSelectedFencedAttemptWithoutLegacyDatabase(t *testing.T) {
	repository := projectRecordPostgres(t)
	clock := &leaseTestClock{value: time.Now().UTC()}
	service := openPostgresHubTestService(t, repository, Config{
		DatabasePath: filepath.Join(t.TempDir(), "hub.db"), RunDetails: &memoryRunDetails{}, Clock: clock.Now,
		NativeRunEvents: tenantNativeRunEventParticipant{}, SecretKeys: secretTestKeys(t, "1", "1"),
	})
	service.config.NativeRunEvents = tenantNativeRunEventParticipant{service: service}
	organization, err := service.createNativeOrganizationFor(t.Context(), "Example organization")
	if err != nil {
		t.Fatal(err)
	}
	seedPostgresHubAdmin(t, repository, service, organization)
	projectPath := "/api/v2/organizations/" + string(organization.ID) + "/projects"
	projectResponse := performHubAPIRequest(t, service, http.MethodPost, projectPath, testHubAdminToken, map[string]any{
		"idempotency_key": "runtime-project", "name": "Example project", "states": nativeFixtureStates(),
	})
	requireNativeStatus(t, projectResponse, http.StatusOK)
	var project tracker.NativeProject
	decodeHubResponse(t, projectResponse, &project)
	descriptor := hubTestPolicy()
	policyRevision, err := eventstore.NewContent(service.config.RunDetails).PrepareProjectPolicy(t.Context(), organization.ID, descriptor)
	if err != nil {
		t.Fatal(err)
	}
	policyRevision.ProjectID = project.ID
	policyRevision.ApprovedBy = "test_hub_admin"
	policyRevision.ApprovedAt = clock.Now().UTC()
	if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
		return w.ApproveProjectPolicy(t.Context(), policyRevision, "")
	}); err != nil {
		t.Fatal(err)
	}
	itemPath := projectPath + "/" + string(project.ID) + "/work-items"
	issueResponse := performHubAPIRequest(t, service, http.MethodPost, itemPath, testHubAdminToken, tracker.CreateIssue{
		Mutation: tracker.Mutation{IdempotencyKey: "runtime-item"}, Title: "Example issue", Body: workItemRecordBody, State: "Todo",
	})
	requireNativeStatus(t, issueResponse, http.StatusOK)
	var issue tracker.NativeIssue
	decodeHubResponse(t, issueResponse, &issue)
	otherResponse := performHubAPIRequest(t, service, http.MethodPost, itemPath, testHubAdminToken, tracker.CreateIssue{
		Mutation: tracker.Mutation{IdempotencyKey: "runtime-other-item"}, Title: "Other issue", Body: workItemRecordBody, State: "Todo",
	})
	requireNativeStatus(t, otherResponse, http.StatusOK)
	var other tracker.NativeIssue
	decodeHubResponse(t, otherResponse, &other)

	startedAt := clock.Now().UTC()
	const runnerID = "runner_runtime_example"
	const machineID = "machine_runtime_example"
	const credentialID = "credential_runtime_example"
	const enrollmentID = "enrollment_runtime_example"
	const token = "example_runtime_runner_token"
	tokenHash := apikey.HashToken(token)
	expires := startedAt.Add(time.Hour)
	if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization.ID, ID: credentialID, Name: "Example runner", Hash: tokenHash, Fingerprint: tokenFingerprint(tokenHash), Scope: "worker", NativeOnly: true, CreatedAt: startedAt, UpdatedAt: startedAt, ExpiresAt: &expires, ProjectAccess: "selected", Kind: "service"}); err != nil {
			return err
		}
		if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization.ID, TokenID: credentialID, ProjectID: project.ID}); err != nil {
			return err
		}
		if err := w.CreateRunnerEnrollment(t.Context(), runnerstore.Enrollment{OrganizationID: organization.ID, ID: enrollmentID, RunnerID: runnerID, MachineID: machineID, TokenHash: apikey.HashToken("example_runtime_enrollment"), Scope: "projects", ProjectIDs: []tracker.ProjectID{project.ID}, Operations: []string{runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events}, CreatedAt: startedAt, ExpiresAt: expires, CreatedBy: "example-admin"}); err != nil {
			return err
		}
		if err := w.RedeemRunnerEnrollment(t.Context(), enrollmentID, runnerID, machineID, startedAt); err != nil {
			return err
		}
		if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: organization.ID, ID: machineID, Hostname: "example-host", Capacity: 1, Version: "example"}); err != nil {
			return err
		}
		if err := w.CreateRunnerIdentity(t.Context(), runnerstore.Identity{OrganizationID: organization.ID, RunnerID: runnerID, MachineID: machineID, TokenID: credentialID, EnrollmentID: enrollmentID, Scope: "projects", State: "active", Operations: []string{runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events}, DisplayName: "Example runner", CapacityLimit: 1, ReportedCapacity: 1, ProjectIDs: []tracker.ProjectID{project.ID}, Availability: runnerstore.Availability{Timezone: "UTC", Windows: []string{"Mon-Sun 00:00-24:00"}}, CreatedAt: startedAt, ExpiresAt: expires, LastHeartbeatAt: startedAt}); err != nil {
			return err
		}
		return w.UpdateRunnerCapacityConfig(t.Context(), runnerID, runnerauth.CapacityConfig{Revision: strings.Repeat("c", 64), LocalLimit: 1, ClientLimit: 1, RuntimeLimit: 1, Manageable: true, ObservedAt: startedAt})
	}); err != nil {
		t.Fatal(err)
	}
	scope := nativeScope{organization: organization.ID, project: project.ID, credential: apiCredential{ID: credentialID, Hash: tokenHash, Scope: apiScopeWorker, Runner: runnerauth.Identity{Binding: runnerauth.Binding{RunnerID: runnerID, MachineID: machineID}, OrganizationID: organization.ID}}}
	request := tracker.NativeClaim{PolicyID: descriptor.ID, WorkItemID: issue.WorkItemID, MachineID: machineID, SessionID: "runtime-session", TTLSeconds: 300, ProtocolMajor: tracker.NativeProtocolMajor, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability}}
	lease, err := service.claimNativeIssueTenant(t.Context(), scope, request, 5*time.Minute)
	if err != nil {
		t.Fatalf("claim issue: %v", err)
	}
	clock.Advance(time.Second)
	publicLease := tracker.NativeLease{ID: tracker.LeaseID(lease.LeaseID), WorkItemID: issue.WorkItemID, MachineID: tracker.MachineID(lease.MachineID), SessionID: lease.SessionID, FencingToken: tracker.FencingToken(lease.FencingToken), PolicyID: descriptor.ID}
	started := nativeStartedEvent(publicLease)
	eventPath := projectPath + "/" + string(project.ID) + "/work-items/" + string(issue.WorkItemID) + "/events"
	started.IdempotencyKey = "runtime-start"
	response := performHubAPIRequest(t, service, http.MethodPost, eventPath, token, started)
	requireNativeStatus(t, response, http.StatusOK)
	var claimedLease leasestore.Lease
	if err := repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		claimedLease, err = r.Lease(t.Context(), lease.LeaseID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	clock.Advance(claimedLease.RenewedAt.Sub(clock.Now()) + time.Second)
	if claimedLease.ReleasedAt != nil || clock.Now().Before(claimedLease.RenewedAt) || !clock.Now().Before(claimedLease.ExpiresAt) {
		t.Fatalf("fixture did not retain a current lease: now=%s renewed=%s expires=%s released=%v", clock.Now(), claimedLease.RenewedAt, claimedLease.ExpiresAt, claimedLease.ReleasedAt)
	}

	legacyDB, legacyReader := service.database.db, service.database.reader
	legacyAuthReader, legacyClaimReader := service.database.authReader, service.database.claimReader
	service.database.db, service.database.reader, service.database.authReader, service.database.claimReader = nil, nil, nil, nil
	t.Cleanup(func() {
		service.database.db, service.database.reader = legacyDB, legacyReader
		service.database.authReader, service.database.claimReader = legacyAuthReader, legacyClaimReader
	})
	runtimePath := itemPath + "/" + string(issue.WorkItemID) + "/runtime?native_attempt_id=" + started.Data.AttemptID
	response = performHubAPIRequest(t, service, http.MethodGet, runtimePath, token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var evidence tracker.NativeRuntimeEvidence
	decodeHubResponse(t, response, &evidence)
	if evidence.Issue.OrganizationID != organization.ID || evidence.Issue.ProjectID != project.ID || evidence.Issue.WorkItemID != issue.WorkItemID || evidence.Issue.Body != "" {
		t.Fatalf("runtime issue reference is not scoped or redacted: %#v", evidence.Issue)
	}
	if evidence.Attempt == nil || evidence.Attempt.AttemptID != started.Data.AttemptID || evidence.Attempt.LeaseID != tracker.LeaseID(lease.LeaseID) || evidence.Attempt.FencingToken != tracker.FencingToken(lease.FencingToken) || evidence.Attempt.PolicyID != descriptor.ID || !evidence.Attempt.Current {
		t.Fatalf("runtime attempt lost its lease or policy binding: %#v", evidence.Attempt)
	}
	if evidence.CurrentLease == nil || evidence.CurrentLease.ID != tracker.LeaseID(lease.LeaseID) || evidence.CurrentLease.FencingToken != tracker.FencingToken(lease.FencingToken) || evidence.CurrentLease.PolicyID != descriptor.ID {
		t.Fatalf("runtime current lease differs from the claim: %#v", evidence.CurrentLease)
	}
	admin, _, err := service.authenticateTenantAPIHash(t.Context(), organization.ID, apikey.HashToken(testHubAdminToken), "", "")
	if err != nil {
		t.Fatal(err)
	}
	operatorContext := operatortool.WithConnection(t.Context(), operatortool.Connection{Identity: operatorIdentity(admin, string(organization.ID)), Resolve: func(ctx context.Context) (operatortool.Authority, error) {
		current, _, err := service.authenticateTenantAPIHash(ctx, organization.ID, apikey.HashToken(testHubAdminToken), "", "")
		if err != nil {
			return operatortool.Authority{}, err
		}
		return service.operatorCurrentAuthority(ctx, current, string(organization.ID))
	}})
	reads := operatorWorkReads{service: service, scope: nativeScope{organization: organization.ID, project: project.ID, credential: admin}}
	t.Run("work_config", func(t *testing.T) {
		defer func() {
			if value := recover(); value != nil {
				t.Fatalf("configuration used legacy storage: %v", value)
			}
		}()
		result, err := reads.ReadWork(operatorContext, operatortool.WorkConfig, operatortool.WorkReadRequest{ProjectID: string(project.ID)})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(result.Content), string(project.ID)) || !strings.Contains(string(result.Content), "reporter_templates") {
			t.Fatalf("configuration omitted authorized project: %s", result.Content)
		}
		if _, err := reads.ReadWork(operatorContext, operatortool.WorkConfig, operatortool.WorkReadRequest{ProjectID: "project_missing"}); err == nil {
			t.Fatal("configuration accepted missing project")
		}
	})
	t.Run("explain_without_attempt", func(t *testing.T) {
		defer func() {
			if value := recover(); value != nil {
				t.Fatalf("explanation used legacy storage: %v", value)
			}
		}()
		result, err := reads.Explain(operatorContext, explain.Query{ProjectID: string(project.ID), Reference: string(other.WorkItemID)})
		if err != nil || result.Identity.IssueID != string(other.WorkItemID) {
			t.Fatalf("explanation lost authorized issue: %#v error=%v", result.Identity, err)
		}
	})
	for _, target := range []tracker.NativeIssue{issue, other} {
		explanation, failures, partial, err := service.readOperatorExplanation(t.Context(), scope, target)
		if err != nil {
			t.Fatal(err)
		}
		if explanation.Issue.WorkItemID != target.WorkItemID || explanation.Selection != "latest" || len(failures) != 0 || partial {
			t.Fatalf("unexpected explanation history: %#v failures=%+v partial=%v", explanation, failures, partial)
		}
		if target.WorkItemID == issue.WorkItemID {
			if explanation.Attempt == nil || explanation.Attempt.AttemptID != started.Data.AttemptID {
				t.Fatalf("explanation did not select the latest attempt: %#v", explanation.Attempt)
			}
		} else if explanation.Attempt != nil || explanation.Scheduling.Outcome != "unknown" || explanation.Scheduling.At.IsZero() {
			t.Fatalf("unattempted issue lost explicit unknown evidence: %#v", explanation)
		}
	}
	finished := started
	finished.IdempotencyKey, finished.Type = "runtime-finished", "run.finished"
	finished.Data.Sequence, finished.Data.Outcome = 2, "failed"
	finished.Data.TerminalFailure = &tracker.NativeTerminalFailure{Error: "example provider failure", Stage: "implement", ObservedAt: clock.Now()}
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, eventPath, token, finished), http.StatusOK)
	_, failures, partial, err := service.readOperatorExplanation(t.Context(), scope, issue)
	if err != nil {
		t.Fatal(err)
	}
	signature := latestNativeFailureSignature(failures, partial)
	if signature == nil || signature.Signature != "example provider failure" || signature.Count != 1 || signature.Partial {
		t.Fatalf("recorded failure signature lost: %#v", signature)
	}
	wrongItemPath := itemPath + "/" + string(other.WorkItemID) + "/runtime?native_attempt_id=" + started.Data.AttemptID
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodGet, wrongItemPath, token, nil), http.StatusNotFound)

	clock.Advance(6 * time.Minute)
	response = performHubAPIRequest(t, service, http.MethodGet, runtimePath, token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	evidence = tracker.NativeRuntimeEvidence{}
	decodeHubResponse(t, response, &evidence)
	if evidence.Attempt == nil || evidence.Attempt.Current || evidence.CurrentLease != nil {
		t.Fatalf("expired lease remained current in runtime evidence: attempt=%#v lease=%#v", evidence.Attempt, evidence.CurrentLease)
	}
	snapshots, _, _, err := service.readOperatorAttemptSnapshots(t.Context(), scope, string(issue.WorkItemID))
	if err != nil || len(snapshots) != 1 {
		t.Fatalf("load attempt hook: %v snapshots=%d", err, len(snapshots))
	}
	changed := false
	service.config.RunDetails = &changeReadInterceptionStore{DetailStore: service.config.RunDetails, key: snapshots[0].attempt.PayloadObjectKey, afterRead: func(ctx context.Context) error {
		changed = true
		return repository.WriteTenant(ctx, organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
			return w.DeleteAccessTokenGrant(ctx, credentialID, project.ID)
		})
	}}
	if _, _, _, err := service.readOperatorExplanation(t.Context(), scope, issue); !changed || err == nil {
		t.Fatalf("explanation accepted revoked project grant: hook=%v error=%v", changed, err)
	}

}

func TestPostgresOperatorReadsRejectRoleChanges(t *testing.T) {
	for _, tool := range []string{operatortool.WorkConfig, operatortool.ExplainItem} {
		t.Run(tool, func(t *testing.T) {
			repository, f, owner, _ := newExportPostgresFixture(t)
			response := f.request(t, owner, http.MethodPost, f.base+"/work-items", tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "explain-role"}, Title: "Example issue", Body: workItemRecordBody, State: "Todo"})
			requireNativeStatus(t, response, http.StatusOK)
			var issue tracker.NativeIssue
			decodeHubResponse(t, response, &issue)
			scope := nativeScope{organization: "org_security", project: f.project, credential: apiCredential{ID: owner.identity.Subject, Scope: apiScopeOperator, Hosted: owner.identity.Hosted, SessionHash: apikey.HashToken(owner.token), HostedRole: "owner"}}
			if err := repository.ReadTenant(t.Context(), scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
				member, err := r.AccessMember(t.Context(), owner.identity.Subject)
				if err != nil {
					return err
				}
				principal, err := r.AccessCredential(t.Context(), member.PrincipalID)
				scope.credential.ID, scope.credential.Hash = principal.ID, principal.Hash
				scope.credential.HostedPrincipal, scope.credential.HostedMembership = principal.ID, member.MembershipID
				return err
			}); err != nil {
				t.Fatal(err)
			}
			identity := operatorIdentity(scope.credential, "org_security")
			ctx := operatortool.WithConnection(t.Context(), operatortool.Connection{Identity: identity, Resolve: func(context.Context) (operatortool.Authority, error) {
				return operatortool.Authority{Identity: identity, Check: func(ctx context.Context, req operatortool.Requirement) error {
					return repository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error { return f.service.authorizeAccessScope(ctx, r, scope, false) })
				}}, nil
			}})
			changed := false
			details := f.service.config.RunDetails.(*memoryRunDetails)
			details.beforeRead = func(ctx context.Context) error {
				details.beforeRead = nil
				changed = true
				return repository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
					member, err := w.AccessMember(ctx, owner.identity.Subject)
					if err != nil {
						return err
					}
					member.Role = "reporter"
					return w.UpdateAccessMember(ctx, member)
				})
			}
			t.Cleanup(func() { details.beforeRead = nil })
			reads := operatorWorkReads{service: f.service, scope: scope}
			var err error
			if tool == operatortool.WorkConfig {
				_, err = reads.ReadWork(ctx, tool, operatortool.WorkReadRequest{ProjectID: string(f.project), Limit: 10})
			} else {
				_, err = reads.Explain(ctx, explain.Query{ProjectID: string(f.project), Reference: string(issue.WorkItemID)})
			}
			if !changed || err == nil {
				t.Fatalf("explanation accepted downgraded role: hook=%v error=%v", changed, err)
			}
		})
	}
}
