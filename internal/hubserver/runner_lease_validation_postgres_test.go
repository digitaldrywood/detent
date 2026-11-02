package hubserver

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestValidateRunnerLeaseUsesPostgresTenantRecords(t *testing.T) {
	admin := postgresStartupTemplate.Database(t)
	connection := startupConnection(t, admin, "detent_runtime_v2", "")
	if !filepath.IsAbs(connection.Host) {
		connection.Host = "127.0.0.1"
	}
	cfg := startupConfig(connection)
	cfg.SecretKeys = secretTestKeys(t, "1", "1")
	store, err := openDatabase(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	seedStartupOrganization(t, store.postgres, "org_startup")
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	service, err := Open(t.Context(), cfg)
	if err != nil {
		t.Fatalf("open PostgreSQL Hub: %v", err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Error(err)
		}
	})
	if service.database.db != nil || service.database.reader != nil {
		t.Fatal("PostgreSQL Hub retained a SQLite reader")
	}

	const projectID tracker.ProjectID = "project_example"
	const issueID tracker.NativeWorkItemID = "wi_example"
	const runnerID = "runner_example"
	const machineID tracker.MachineID = "machine_example"
	const tokenID = "credential_runner"
	const leaseID = "lease_example"
	const workerToken = "example_runner_token"
	workerHash := apikey.HashToken(workerToken)
	now := service.config.now().UTC()
	expires := now.Add(time.Hour)
	descriptor := hubTestPolicy()
	descriptor.Requirements.RequiredTags = []string{"example-tag"}
	policyRevision, err := eventstore.NewContent(service.config.RunDetails).PrepareProjectPolicy(t.Context(), "org_startup", descriptor)
	if err != nil {
		t.Fatal(err)
	}
	policyRevision.ProjectID = projectID
	policyRevision.ApprovedBy = "test_hub_admin"
	policyRevision.ApprovedAt = now
	var lease leasestore.Lease
	err = service.config.TenantRepository.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
		if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: "org_startup", ID: projectID, Name: "Example project", Slug: "example-project", Profile: "native"}, States: nativeFixtureStates()}); err != nil {
			return err
		}
		if err := w.ApproveProjectPolicy(t.Context(), policyRevision, ""); err != nil {
			return err
		}
		item, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: "org_startup", ProjectID: projectID, NativeID: issueID, Number: 1, Revision: 1, Title: "Example issue", WorkflowStateName: new("Todo"), CreatedAt: now, UpdatedAt: now, NativeCreatedAt: now, NativeUpdatedAt: now})
		if err != nil {
			return err
		}
		if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: "org_startup", ID: tokenID, Name: "Example runner", Hash: workerHash, Fingerprint: tokenFingerprint(workerHash), Scope: "worker", NativeOnly: true, CreatedAt: now, UpdatedAt: now, ExpiresAt: &expires, ProjectAccess: "selected", Kind: "service"}); err != nil {
			return err
		}
		if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: "org_startup", TokenID: tokenID, ProjectID: projectID}); err != nil {
			return err
		}
		if err := w.CreateRunnerEnrollment(t.Context(), runnerstore.Enrollment{OrganizationID: "org_startup", ID: "enrollment_runner", RunnerID: runnerID, MachineID: machineID, TokenHash: apikey.HashToken("example_enrollment_secret"), Scope: "projects", ProjectIDs: []tracker.ProjectID{projectID}, Operations: []string{runnerauth.Read, runnerauth.Claim}, CreatedAt: now, ExpiresAt: expires, CreatedBy: "test_hub_admin"}); err != nil {
			return err
		}
		if err := w.RedeemRunnerEnrollment(t.Context(), "enrollment_runner", runnerID, machineID, now); err != nil {
			return err
		}
		if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: "org_startup", ID: string(machineID), Hostname: "example-host", Capacity: 1, Version: "example"}); err != nil {
			return err
		}
		if err := w.CreateRunnerIdentity(t.Context(), runnerstore.Identity{OrganizationID: "org_startup", RunnerID: runnerID, MachineID: machineID, TokenID: tokenID, EnrollmentID: "enrollment_runner", Scope: "projects", State: "active", Revision: 1, Tags: []string{"example-tag"}, Operations: []string{runnerauth.Read, runnerauth.Claim}, CapacityLimit: 1, ReportedCapacity: 1, ProjectIDs: []tracker.ProjectID{projectID}, CreatedAt: now, ExpiresAt: expires, LastHeartbeatAt: now}); err != nil {
			return err
		}
		lease, err = w.Claim(t.Context(), leasestore.Claim{IssueID: item.ID, ExpectedRevision: item.Revision, MachineID: string(machineID), SessionID: "session_example", LeaseID: leaseID, TTL: 5 * time.Minute})
		if err != nil {
			return err
		}
		if err := w.AssignRunnerLease(t.Context(), runnerstore.LeaseAssignment{OrganizationID: "org_startup", LeaseID: leaseID, RunnerID: runnerID, AssignedAt: now}); err != nil {
			return err
		}
		return w.BindLeasePolicy(t.Context(), policystore.LeasePolicy{OrganizationID: "org_startup", ProjectID: projectID, LeaseID: leaseID, PolicyID: descriptor.ID, BoundAt: now})
	})
	if err != nil {
		t.Fatalf("seed PostgreSQL lease: %v", err)
	}

	requestBody, err := json.Marshal(tracker.NativeLeaseMutation{FencingToken: tracker.FencingToken(lease.FencingToken)})
	if err != nil {
		t.Fatal(err)
	}
	path := nativeBase + "/leases/:lease/validate"
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, strings.ReplaceAll(path, ":organization", "org_startup"), bytes.NewReader(requestBody))
	request.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	context := echo.New().NewContext(request, recorder)
	context.SetPath(path)
	context.SetParamNames("organization", "project", "lease")
	context.SetParamValues("org_startup", string(projectID), leaseID)
	credential := apiCredential{ID: tokenID, Hash: workerHash, Scope: apiScopeWorker, HostedKeyKind: "service", Runner: runnerauth.Identity{Binding: runnerauth.Binding{RunnerID: runnerID, MachineID: machineID}, OrganizationID: "org_startup", Operations: []string{runnerauth.Read, runnerauth.Claim}}}
	context.Set("hub_api_credential", credential)
	context.Set("native_scope", nativeScope{organization: "org_startup", project: projectID, credential: credential})
	if err := service.validateRunnerLease(context); err != nil {
		t.Fatalf("validate PostgreSQL lease: %v", err)
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("lease validation status = %d, body %s", recorder.Code, recorder.Body.String())
	}
	var validated runnerauth.Runner
	if err := json.Unmarshal(recorder.Body.Bytes(), &validated); err != nil {
		t.Fatal(err)
	}
	if validated.RunnerID != runnerID || validated.MachineID != machineID || validated.OrganizationID != "org_startup" || !strings.Contains(strings.Join(validated.Tags, ","), "example-tag") {
		t.Fatalf("lease validation runner = %+v", validated)
	}

	scope := nativeScope{organization: "org_startup", project: projectID, credential: credential}
	if _, err := service.validateRunnerLeaseTenant(t.Context(), scope, leaseID, tracker.FencingToken(lease.FencingToken+1)); !errors.Is(err, tracker.ErrStaleFencingToken) {
		t.Fatalf("stale lease fence error = %v, want stale fencing token", err)
	}
	wrongCredential := scope
	wrongCredential.credential.Hash = apikey.HashToken("different_runner_token")
	if _, err := service.validateRunnerLeaseTenant(t.Context(), wrongCredential, leaseID, tracker.FencingToken(lease.FencingToken)); err == nil {
		t.Fatal("lease validation accepted a credential with a different hash")
	}

	type validationCase struct {
		name         string
		mutation     string
		mutationArgs []any
		restore      string
		restoreArgs  []any
		wantCode     string
		wantStale    bool
	}
	expiredLeaseStart := service.config.now().UTC().Add(-time.Minute)
	cases := []validationCase{
		{name: "revoked credential", mutation: `UPDATE api_tokens SET revoked_at=$1 WHERE organization_id=$2 AND id=$3`, mutationArgs: []any{now, "org_startup", tokenID}, restore: `UPDATE api_tokens SET revoked_at=NULL WHERE organization_id=$1 AND id=$2`, restoreArgs: []any{"org_startup", tokenID}, wantCode: "unauthorized"},
		{name: "expired credential", mutation: `UPDATE api_tokens SET expires_at=$1 WHERE organization_id=$2 AND id=$3`, mutationArgs: []any{now.Add(-time.Second), "org_startup", tokenID}, restore: `UPDATE api_tokens SET expires_at=$1 WHERE organization_id=$2 AND id=$3`, restoreArgs: []any{expires, "org_startup", tokenID}, wantCode: "unauthorized"},
		{name: "removed runner", mutation: `UPDATE runner_identities SET removed_at=$1 WHERE organization_id=$2 AND runner_id=$3`, mutationArgs: []any{now, "org_startup", runnerID}, restore: `UPDATE runner_identities SET removed_at=NULL WHERE organization_id=$1 AND runner_id=$2`, restoreArgs: []any{"org_startup", runnerID}, wantCode: "not_found"},
		{name: "expired runner", mutation: `UPDATE runner_identities SET expires_at=$1 WHERE organization_id=$2 AND runner_id=$3`, mutationArgs: []any{now.Add(-time.Second), "org_startup", runnerID}, restore: `UPDATE runner_identities SET expires_at=$1 WHERE organization_id=$2 AND runner_id=$3`, restoreArgs: []any{expires, "org_startup", runnerID}, wantCode: "not_found"},
		{name: "disabled runner", mutation: `UPDATE runner_identities SET state='disabled' WHERE organization_id=$1 AND runner_id=$2`, mutationArgs: []any{"org_startup", runnerID}, restore: `UPDATE runner_identities SET state='active' WHERE organization_id=$1 AND runner_id=$2`, restoreArgs: []any{"org_startup", runnerID}, wantCode: "runner_disabled"},
		{name: "claim operation removed", mutation: `UPDATE runner_identities SET operations=ARRAY['read']::text[] WHERE organization_id=$1 AND runner_id=$2`, mutationArgs: []any{"org_startup", runnerID}, restore: `UPDATE runner_identities SET operations=ARRAY['read','claim']::text[] WHERE organization_id=$1 AND runner_id=$2`, restoreArgs: []any{"org_startup", runnerID}, wantCode: "claim_not_permitted"},
		{name: "missing project grant", mutation: `DELETE FROM token_grants WHERE organization_id=$1 AND token_id=$2 AND project_id=$3`, mutationArgs: []any{"org_startup", tokenID, projectID}, restore: `INSERT INTO token_grants(organization_id,token_id,project_id) VALUES($1,$2,$3)`, restoreArgs: []any{"org_startup", tokenID, projectID}, wantCode: "not_found"},
		{name: "selector mismatch", mutation: `UPDATE runner_identities SET tags=ARRAY['other-tag']::text[] WHERE organization_id=$1 AND runner_id=$2`, mutationArgs: []any{"org_startup", runnerID}, restore: `UPDATE runner_identities SET tags=ARRAY['example-tag']::text[] WHERE organization_id=$1 AND runner_id=$2`, restoreArgs: []any{"org_startup", runnerID}, wantCode: "selector_no_match"},
		{name: "released lease", mutation: `UPDATE leases SET released_at=$1 WHERE organization_id=$2 AND lease_id=$3`, mutationArgs: []any{lease.AcquiredAt, "org_startup", leaseID}, restore: `UPDATE leases SET released_at=NULL WHERE organization_id=$1 AND lease_id=$2`, restoreArgs: []any{"org_startup", leaseID}, wantStale: true},
		{name: "expired lease", mutation: `UPDATE leases SET acquired_at=$1,renewed_at=$1,expires_at=$2 WHERE organization_id=$3 AND lease_id=$4`, mutationArgs: []any{expiredLeaseStart, expiredLeaseStart.Add(time.Second), "org_startup", leaseID}, restore: `UPDATE leases SET acquired_at=$1,renewed_at=$2,expires_at=$3 WHERE organization_id=$4 AND lease_id=$5`, restoreArgs: []any{lease.AcquiredAt, lease.RenewedAt, lease.ExpiresAt, "org_startup", leaseID}, wantStale: true},
		{name: "archived issue keeps active lease valid", mutation: `UPDATE issues SET archived=true WHERE organization_id=$1 AND id=$2`, mutationArgs: []any{"org_startup", lease.IssueID}, restore: `UPDATE issues SET archived=false WHERE organization_id=$1 AND id=$2`, restoreArgs: []any{"org_startup", lease.IssueID}},
		{name: "retired project keeps active lease valid", mutation: `UPDATE projects SET deleted_at=$1 WHERE organization_id=$2 AND id=$3`, mutationArgs: []any{now, "org_startup", projectID}, restore: `UPDATE projects SET deleted_at=NULL WHERE organization_id=$1 AND id=$2`, restoreArgs: []any{"org_startup", projectID}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if _, err := admin.ExecContext(t.Context(), test.mutation, test.mutationArgs...); err != nil {
				t.Fatalf("apply authorization fixture: %v", err)
			}
			_, validationErr := service.validateRunnerLeaseTenant(t.Context(), scope, leaseID, tracker.FencingToken(lease.FencingToken))
			if _, err := admin.ExecContext(t.Context(), test.restore, test.restoreArgs...); err != nil {
				t.Fatalf("restore authorization fixture: %v", err)
			}
			if test.wantStale {
				if !errors.Is(validationErr, tracker.ErrStaleFencingToken) {
					t.Fatalf("validation error = %v, want stale fencing token", validationErr)
				}
				return
			}
			if test.wantCode == "" {
				if validationErr != nil {
					t.Fatalf("validation error = %v, want success", validationErr)
				}
				return
			}
			var nativeFailure *nativeError
			if !errors.As(validationErr, &nativeFailure) || nativeFailure.Code != test.wantCode {
				t.Fatalf("validation error = %v, want code %q", validationErr, test.wantCode)
			}
		})
	}
}
