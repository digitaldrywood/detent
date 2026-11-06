package hubserver

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
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

func TestTenantDependencyReasonMutationReplaysAndFencesWithoutSQLite(t *testing.T) {
	repository := projectRecordPostgres(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	details := &memoryRunDetails{}
	service := &Service{echo: echo.New(), secretRewrapReady: make(chan struct{}), database: &database{}, config: Config{TenantRepository: repository, AccessRepository: repository, RunDetails: details, now: func() time.Time { return now }, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Hosted: &HostedConfig{OrganizationID: "org_first"}}}
	service.registerNativeRoutes(service.echo)
	leases := map[tracker.OrganizationID]leasestore.Lease{}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		versions := map[tracker.NativeWorkItemID]eventstore.CollaborationVersion{}
		for index := 1; index <= 2; index++ {
			id := tracker.NativeWorkItemID(fmt.Sprintf("item_%d", index))
			issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: "project", WorkItemID: id, Number: index, Revision: 1, Profile: "native"}, Title: "Example " + string(id), State: "Todo", Actor: tracker.Actor{Kind: "human", PrincipalID: "example-owner"}, CreatedAt: now, UpdatedAt: now}
			if org == "org_second" && index == 1 {
				issue.BlockedReason = tracker.DependencyReason{Text: "Retained dependency reason.", Basis: dependencyBasis([]string{"item_2"}), Revision: 3}
			}
			raw, err := json.Marshal(issue)
			if err != nil {
				t.Fatal(err)
			}
			version, err := eventstore.NewContent(details).PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: org, ProjectID: "project", WorkItemID: id, RecordID: string(id), Revision: 1}, raw)
			if err != nil {
				t.Fatal(err)
			}
			versions[id] = version
		}
		descriptor := hubTestPolicy()
		descriptor.Requirements.RequiredTags = []string{"example-tag"}
		revision, err := eventstore.NewContent(details).PrepareProjectPolicy(t.Context(), org, descriptor)
		if err != nil {
			t.Fatal(err)
		}
		revision.ProjectID = "project"
		revision.ApprovedAt = now
		revision.ApprovedBy = "example-owner"
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: string(org), Slug: strings.ReplaceAll(string(org), "_", "-"), Local: true, CreatedAt: now}); err != nil {
				return err
			}
			if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: "project", Name: "Example project", Slug: "example-project", Profile: "native"}, States: nativeFixtureStates()}); err != nil {
				return err
			}
			if err := w.ApproveProjectPolicy(t.Context(), revision, ""); err != nil {
				return err
			}
			if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: org, ID: "runner", Name: "Example runner", Hash: apikey.HashToken(string(org) + "_token"), Scope: "worker", NativeOnly: true, Kind: "service", ProjectAccess: "selected", CreatedAt: now, UpdatedAt: now}); err != nil {
				return err
			}
			if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: org, TokenID: "runner", ProjectID: "project"}); err != nil {
				return err
			}
			if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: org, ID: "machine", Hostname: "example", Capacity: 2, Version: "example"}); err != nil {
				return err
			}
			operations := []string{runnerauth.Read, runnerauth.Claim, runnerauth.Collaborate}
			if err := w.CreateRunnerEnrollment(t.Context(), runnerstore.Enrollment{OrganizationID: org, ID: "enrollment", RunnerID: "runner", MachineID: "machine", TokenHash: apikey.HashToken(string(org) + "_enrollment"), Scope: "projects", Operations: operations, ProjectIDs: []tracker.ProjectID{"project"}, CreatedAt: now, ExpiresAt: now.Add(time.Hour), CreatedBy: "example-owner"}); err != nil {
				return err
			}
			if err := w.CreateRunnerIdentity(t.Context(), runnerstore.Identity{OrganizationID: org, RunnerID: "runner", MachineID: "machine", TokenID: "runner", EnrollmentID: "enrollment", Scope: "projects", State: "active", Tags: []string{"example-tag"}, Operations: operations, CapacityLimit: 2, ReportedCapacity: 2, CreatedAt: now, ExpiresAt: now.Add(time.Hour), LastHeartbeatAt: now}); err != nil {
				return err
			}
			var anchor corestore.WorkItem
			for index := 1; index <= 2; index++ {
				id := tracker.NativeWorkItemID(fmt.Sprintf("item_%d", index))
				state, kind, principal := "Todo", "human", "example-owner"
				row, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: "project", NativeID: id, Number: int64(index), Title: "Example " + string(id), WorkflowStateName: &state, ActorKind: &kind, ActorPrincipalID: &principal})
				if err != nil {
					return err
				}
				if _, err := w.CreateCollaborationVersion(t.Context(), versions[id]); err != nil {
					return err
				}
				if index == 1 {
					anchor = row
				}
			}
			if err := w.SetRelationship(t.Context(), corestore.Relationship{OrganizationID: org, ProjectID: "project", WorkItemID: "item_1", RelatedProjectID: "project", RelatedWorkItemID: "item_2", Type: "dependency"}, "add", now); err != nil {
				return err
			}
			lease, err := w.Claim(t.Context(), leasestore.Claim{IssueID: anchor.ID, ExpectedRevision: 1, MachineID: "machine", SessionID: "session", LeaseID: "lease_" + string(org), TTL: time.Hour})
			if err != nil {
				return err
			}
			leases[org] = lease
			if err := w.AssignRunnerLease(t.Context(), runnerstore.LeaseAssignment{OrganizationID: org, LeaseID: lease.LeaseID, RunnerID: "runner", AssignedAt: now}); err != nil {
				return err
			}
			return w.BindLeasePolicy(t.Context(), policystore.LeasePolicy{OrganizationID: org, ProjectID: "project", LeaseID: tracker.LeaseID(lease.LeaseID), PolicyID: descriptor.ID, BoundAt: now})
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, lease := range leases {
		if lease.RenewedAt.After(now) {
			now = lease.RenewedAt
		}
	}
	now = now.Add(time.Second)
	path := "/api/v2/organizations/org_first/projects/project/work-items/item_1/dependency-reasons"
	request := tracker.UpdateDependencyReason{Mutation: tracker.Mutation{IdempotencyKey: "reason-current", LeaseID: tracker.LeaseID(leases["org_first"].LeaseID), FencingToken: tracker.FencingToken(leases["org_first"].FencingToken)}, WorkItemID: "item_1", Kind: "blocked", Text: "Waits for the shared foundation.", Basis: dependencyBasis([]string{"item_2"}), ExpectedRevision: 0}
	setTags := func(tags []string) {
		t.Helper()
		if err := repository.WriteTenant(t.Context(), "org_first", corestore.Serving, func(w tenantstore.Writer) error {
			identity, err := w.RunnerIdentity(t.Context(), "runner")
			if err != nil {
				return err
			}
			identity.Tags = tags
			return w.UpdateRunnerRouting(t.Context(), identity.Revision, identity)
		}); err != nil {
			t.Fatal(err)
		}
	}
	setTags([]string{"other-tag"})
	mismatch := request
	mismatch.IdempotencyKey = "policy mismatch"
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path, "org_first_token", mismatch), http.StatusConflict)
	setTags([]string{"example-tag"})
	for _, test := range []struct {
		name   string
		mutate func(*tracker.UpdateDependencyReason)
		status int
	}{{name: "stale basis", mutate: func(r *tracker.UpdateDependencyReason) { r.Basis = "outdated" }, status: http.StatusConflict}, {name: "stale revision", mutate: func(r *tracker.UpdateDependencyReason) { r.ExpectedRevision = 1 }, status: http.StatusConflict}, {name: "stale fence", mutate: func(r *tracker.UpdateDependencyReason) { r.FencingToken++ }, status: http.StatusConflict}, {name: "current", mutate: func(*tracker.UpdateDependencyReason) {}, status: http.StatusOK}} {
		t.Run(test.name, func(t *testing.T) {
			r := request
			r.IdempotencyKey = test.name
			test.mutate(&r)
			response := performHubAPIRequest(t, service, http.MethodPost, path, "org_first_token", r)
			requireNativeStatus(t, response, test.status)
		})
	}
	response := performHubAPIRequest(t, service, http.MethodPost, path, "org_first_token", request)
	requireNativeStatus(t, response, http.StatusConflict)
	request.IdempotencyKey = "current"
	replay := performHubAPIRequest(t, service, http.MethodPost, path, "org_first_token", request)
	requireNativeStatus(t, replay, http.StatusOK)
	var reason tracker.DependencyReason
	decodeHubResponse(t, replay, &reason)
	if reason.Revision != 1 || reason.Text != request.Text || reason.Basis != request.Basis {
		t.Fatalf("persisted reason response = %+v", reason)
	}
	read := performHubAPIRequest(t, service, http.MethodGet, strings.TrimSuffix(path, "/dependency-reasons"), "org_first_token", nil)
	requireNativeStatus(t, read, http.StatusOK)
	var issue tracker.NativeIssue
	decodeHubResponse(t, read, &issue)

	if issue.Revision != 1 || issue.State != "Todo" || issue.BlockedReason.Text != request.Text || issue.BlockedReason.Revision != 1 {
		t.Fatalf("dependency reason changed issue authority or was not read: %+v", issue)
	}
	if err := repository.ReadTenant(t.Context(), "org_second", corestore.Serving, func(r tenantstore.Reader) error {
		records, err := r.(corestore.DependencyReasonReader).DependencyReasonRecords(t.Context(), "project", []tracker.NativeWorkItemID{"item_1", "item_2"})
		if len(records) != 0 {
			t.Errorf("reason leaked between tenants: %+v", records)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	neighbor := request
	neighbor.IdempotencyKey = "neighbor"
	neighbor.WorkItemID = "item_2"
	neighbor.Kind = "unblocks"
	neighbor.Text = "Enables the dependent feature."
	neighbor.Basis = dependencyBasis([]string{"item_1"})
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path, "org_first_token", neighbor), http.StatusOK)
	neighborRead := performHubAPIRequest(t, service, http.MethodGet, strings.TrimSuffix(path, "item_1/dependency-reasons")+"item_2", "org_first_token", nil)
	requireNativeStatus(t, neighborRead, http.StatusOK)
	var neighborIssue tracker.NativeIssue
	decodeHubResponse(t, neighborRead, &neighborIssue)
	if neighborIssue.UnblocksReason.Text != neighbor.Text || neighborIssue.UnblocksReason.Revision != 1 {
		t.Fatalf("neighbor reason row is unusable: %+v", neighborIssue.UnblocksReason)
	}

	var workers sync.WaitGroup
	statuses := make(chan int, 2)
	for index := range 2 {
		workers.Go(func() {
			r := request
			r.IdempotencyKey = fmt.Sprintf("concurrent_%d", index)
			r.ExpectedRevision = 1
			statuses <- performHubAPIRequest(t, service, http.MethodPost, path, "org_first_token", r).Code
		})
	}
	workers.Wait()
	close(statuses)
	codes := []int{}
	for status := range statuses {
		codes = append(codes, status)
	}
	slices.Sort(codes)
	if !slices.Equal(codes, []int{http.StatusOK, http.StatusConflict}) {
		t.Fatalf("concurrent reason CAS statuses = %v", codes)
	}
	if err := repository.WriteTenant(t.Context(), "org_first", corestore.Serving, func(w tenantstore.Writer) error {
		_, err := w.Release(t.Context(), leasestore.Fence{LeaseID: leases["org_first"].LeaseID, FencingToken: leases["org_first"].FencingToken}, "completed")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path, "org_first_token", request), http.StatusOK)
	r := request
	r.IdempotencyKey = "released lease"
	r.ExpectedRevision = 2
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path, "org_first_token", r), http.StatusConflict)
	if err := repository.WriteTenant(t.Context(), "org_first", corestore.Serving, func(w tenantstore.Writer) error {
		return w.SetRelationship(t.Context(), corestore.Relationship{OrganizationID: "org_first", ProjectID: "project", WorkItemID: "item_1", RelatedProjectID: "project", RelatedWorkItemID: "item_2", Type: "dependency"}, "remove", now)
	}); err != nil {
		t.Fatal(err)
	}
	staleRead := performHubAPIRequest(t, service, http.MethodGet, strings.TrimSuffix(path, "/dependency-reasons"), "org_first_token", nil)
	requireNativeStatus(t, staleRead, http.StatusOK)
	decodeHubResponse(t, staleRead, &issue)
	if issue.BlockedReason.Text != "" || issue.BlockedReason.Basis == issue.BlockedReason.CurrentBasis || issue.Revision != 1 {
		t.Fatalf("removed graph still exposes the old reason: %+v", issue.BlockedReason)
	}
	service.config.Hosted.OrganizationID = "org_second"
	legacy := request
	legacy.IdempotencyKey = "retain legacy baseline"
	legacy.LeaseID = tracker.LeaseID(leases["org_second"].LeaseID)
	legacy.FencingToken = tracker.FencingToken(leases["org_second"].FencingToken)
	legacy.ExpectedRevision = 3
	legacyPath := strings.Replace(path, "org_first", "org_second", 1)
	legacyResult := performHubAPIRequest(t, service, http.MethodPost, legacyPath, "org_second_token", legacy)
	requireNativeStatus(t, legacyResult, http.StatusOK)
	decodeHubResponse(t, legacyResult, &reason)
	if reason.Revision != 4 {
		t.Fatalf("legacy reason revision reset: %+v", reason)
	}

}
