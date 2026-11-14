package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/changerequest"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresPolicyObservationsKeepCurrentReportProvenanceAndApproveByID(t *testing.T) {
	repository, _ := projectRecordPostgresWithAdmin(t, 4)
	details := &memoryRunDetails{}
	service := openPostgresHubTestService(t, repository, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), RunDetails: details})
	organization, err := service.createNativeOrganizationFor(t.Context(), "Example organization")
	if err != nil {
		t.Fatal(err)
	}
	seedPostgresHubAdmin(t, repository, service, organization)
	projectsPath := "/api/v2/organizations/" + string(organization.ID) + "/projects"
	response := performHubAPIRequest(t, service, http.MethodPost, projectsPath, testHubAdminToken, map[string]any{"idempotency_key": "project", "name": "Example project", "grant_access": true, "states": nativeFixtureStates()})
	requireNativeStatus(t, response, http.StatusCreated)
	var project tracker.NativeProject
	decodeHubResponse(t, response, &project)
	if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
		record, err := w.ProjectRecord(t.Context(), project.ID)
		if err != nil {
			return err
		}
		record.Settings.Repository = "example-org/example-project"
		record.Settings.CheckoutRepository = "example-org/example-project"
		_, err = w.UpdateProjectRecord(t.Context(), record)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	now := service.config.now().UTC()
	type runnerCredential struct{ token, runnerID string }
	runners := []runnerCredential{{"example_runner_token_a", "runner_example_a"}, {"example_runner_token_b", "runner_example_b"}}
	for index, runner := range runners {
		tokenID := fmt.Sprintf("credential_example_%c", 'a'+index)
		machineID := tracker.MachineID("machine_example_" + string(rune('a'+index)))
		enrollmentID := fmt.Sprintf("enrollment_example_%c", 'a'+index)
		expires := now.Add(time.Hour)
		err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
			hash := apikey.HashToken(runner.token)
			if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization.ID, ID: tokenID, Name: runner.runnerID, Hash: hash, Fingerprint: tokenFingerprint(hash), Scope: "worker", NativeOnly: true, CreatedAt: now, UpdatedAt: now, ExpiresAt: &expires, ProjectAccess: "selected", Kind: "service"}); err != nil {
				return err
			}
			if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization.ID, TokenID: tokenID, ProjectID: project.ID}); err != nil {
				return err
			}
			if err := w.CreateRunnerEnrollment(t.Context(), runnerstore.Enrollment{OrganizationID: organization.ID, ID: enrollmentID, RunnerID: runner.runnerID, MachineID: machineID, TokenHash: apikey.HashToken("example_enrollment_" + runner.runnerID), Scope: "projects", ProjectIDs: []tracker.ProjectID{project.ID}, Operations: []string{runnerauth.Read, runnerauth.Heartbeat}, CreatedAt: now, ExpiresAt: expires, CreatedBy: "example-admin"}); err != nil {
				return err
			}
			if err := w.RedeemRunnerEnrollment(t.Context(), enrollmentID, runner.runnerID, machineID, now); err != nil {
				return err
			}
			if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: organization.ID, ID: string(machineID), Hostname: runner.runnerID, Capacity: 1, Version: "example"}); err != nil {
				return err
			}
			return w.CreateRunnerIdentity(t.Context(), runnerstore.Identity{OrganizationID: organization.ID, RunnerID: runner.runnerID, MachineID: machineID, TokenID: tokenID, EnrollmentID: enrollmentID, Scope: "projects", State: "active", Operations: []string{runnerauth.Read, runnerauth.Heartbeat}, CapacityLimit: 1, ReportedCapacity: 1, ProjectIDs: []tracker.ProjectID{project.ID}, CreatedAt: now, ExpiresAt: expires, LastHeartbeatAt: now})
		})
		if err != nil {
			t.Fatalf("seed runner %s: %v", runner.runnerID, err)
		}
		if err := repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(reader tenantstore.Reader) error {
			identity, err := reader.RunnerIdentityByCredential(t.Context(), tokenID)
			if err != nil {
				return err
			}
			allowed, err := reader.RunnerProjectAuthorized(t.Context(), identity.RunnerID, project.ID, now)
			if err != nil || !allowed {
				return fmt.Errorf("runner project authorization allowed=%t: %w", allowed, err)
			}
			credential, err := reader.AccessCredential(t.Context(), tokenID)
			if err != nil {
				return err
			}
			return service.authorizeAccessScope(t.Context(), reader, nativeScope{organization: organization.ID, project: project.ID, credential: apiCredential{ID: credential.ID, Hash: credential.Hash, Scope: apiScopeWorker, Runner: runnerauth.Identity{Binding: runnerauth.Binding{RunnerID: identity.RunnerID}, OrganizationID: identity.OrganizationID}}}, true)
		}); err != nil {
			t.Fatalf("authorize seeded runner %s: %v", runner.runnerID, err)
		}
	}
	base := projectsPath + "/" + string(project.ID)
	authoredDescriptor := func(contents, revision string) policy.Descriptor {
		t.Helper()
		workflow, err := config.ParseProjectDefinition(config.ProjectDefinitionSources{
			ConfigPath:   "detent.yaml",
			HasConfig:    true,
			Config:       []byte("schema: 1\ntracker:\n  kind: hub_native\n  repository: example-org/example-project\ngate:\n  run: true\n  required_status_checks: []\n"),
			WorkflowPath: "WORKFLOW.md",
			Workflow:     []byte(contents),
		})
		if err != nil {
			t.Fatal(err)
		}
		descriptor, err := config.ResolvePolicy(workflow)
		if err != nil {
			t.Fatal(err)
		}
		if descriptor.Workflow == nil {
			t.Fatal("authored fixture did not resolve repository workflow metadata")
		}
		workflowDescriptor := *descriptor.Workflow
		workflowDescriptor.Revision = revision
		descriptor.Workflow = &workflowDescriptor
		return descriptor.WithID()
	}
	descriptorA := authoredDescriptor("Implement the assigned issue.\n", strings.Repeat("a", 40))
	descriptorObservationA := authoredDescriptor("Implement the assigned issue.\n", strings.Repeat("b", 40))
	if descriptorA.ID != descriptorObservationA.ID || !sameTenantProjectPolicyDescriptorContent(descriptorA, descriptorObservationA) {
		t.Fatal("same authored policy with new workflow provenance did not match")
	}
	gateMismatch := descriptorObservationA
	gateMismatch.Gates.GitHubPullRequest = !gateMismatch.Gates.GitHubPullRequest
	if sameTenantProjectPolicyDescriptorContent(descriptorA, gateMismatch) {
		t.Fatal("policy descriptor comparison accepted a gate mismatch")
	}
	metadataMismatch := descriptorObservationA
	metadataMismatch.Requirements.RequiredTags = []string{"linux"}
	if sameTenantProjectPolicyDescriptorContent(descriptorA, metadataMismatch) {
		t.Fatal("policy descriptor comparison accepted a requirements mismatch")
	}
	legacyPayload, err := json.MarshalIndent(descriptorA, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	legacyReference, err := details.StoreDetail(t.Context(), string(organization.ID), legacyPayload)
	if err != nil {
		t.Fatal(err)
	}
	legacyRevision := policystore.Revision{
		OrganizationID:      organization.ID,
		ProjectID:           project.ID,
		PolicyID:            descriptorA.ID,
		ApprovedBy:          "runner-report",
		ApprovedAt:          now.Add(-time.Minute),
		GitHubPullRequest:   new(descriptorA.Gates.GitHubPullRequest),
		PayloadObjectKey:    legacyReference.Key,
		PayloadSHA256:       legacyReference.SHA256,
		PayloadBytes:        legacyReference.CompressedSize,
		PayloadContentBytes: legacyReference.UncompressedSize,
	}
	if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(writer tenantstore.Writer) error {
		return writer.ImportProjectPolicyRevision(t.Context(), legacyRevision, false)
	}); err != nil {
		t.Fatal(err)
	}
	descriptorB := authoredDescriptor("Implement a different assigned issue.\n", strings.Repeat("b", 40))
	sourceA := &policy.RepositorySource{Repository: "example-org/example-project", Commit: strings.Repeat("a", 40), DefaultBranch: "main", DefaultBranchHead: strings.Repeat("a", 40)}
	sourceB := &policy.RepositorySource{Repository: "example-org/example-project", Commit: strings.Repeat("b", 40), DefaultBranch: "main", DefaultBranchHead: strings.Repeat("b", 40)}
	report := func(runner runnerCredential, descriptor policy.Descriptor, source *policy.RepositorySource) {
		t.Helper()
		body := policy.Observation{Descriptor: descriptor, Source: source}
		requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, base+"/policy/observed", runner.token, body), http.StatusNoContent)
	}
	read := func() projectPolicyRead {
		t.Helper()
		response := performHubAPIRequest(t, service, http.MethodGet, base+"/policy", testHubAdminToken, nil)
		requireNativeStatus(t, response, http.StatusOK)
		var view projectPolicyRead
		decodeHubResponse(t, response, &view)
		return view
	}
	report(runners[0], descriptorObservationA, sourceA)
	report(runners[0], descriptorObservationA, nil)
	report(runners[1], descriptorB, sourceB)
	view := read()
	if len(view.ObservedPolicies) != 2 || !view.ObservedPolicies[0].Conflict || !view.ObservedPolicies[1].Conflict {
		t.Fatalf("competing current observations = %+v", view.ObservedPolicies)
	}
	var observedA policy.ObservedPolicy
	for _, candidate := range view.ObservedPolicies {
		if candidate.Policy.ID == descriptorA.ID {
			observedA = candidate
		}
	}
	if observedA.Policy.ID == "" || observedA.Source == nil || *observedA.Source != *sourceA || !reflect.DeepEqual(observedA.RunnerIDs, []string{runners[0].runnerID}) {
		t.Fatalf("same-policy report lost source or runner identity: %+v", observedA)
	}
	var runnerAExpiry time.Time
	if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
		identity, err := w.RunnerIdentity(t.Context(), runners[0].runnerID)
		if err != nil {
			return err
		}
		runnerAExpiry = identity.ExpiresAt
		identity.ExpiresAt = now.Add(-time.Second)
		return w.UpdateRunnerIdentity(t.Context(), identity)
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(reader tenantstore.Reader) error {
		current, err := reader.CurrentProjectPolicyObservation(t.Context(), project.ID, descriptorA.ID, now)
		if err != nil {
			return err
		}
		if current {
			t.Fatal("expired runner identity remained eligible through a live worker token")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
		identity, err := w.RunnerIdentity(t.Context(), runners[0].runnerID)
		if err != nil {
			return err
		}
		identity.ExpiresAt = runnerAExpiry
		return w.UpdateRunnerIdentity(t.Context(), identity)
	}); err != nil {
		t.Fatal(err)
	}
	response = performHubAPIRequest(t, service, http.MethodPut, base+"/policy", testHubAdminToken, policy.Change{ObservedPolicyID: descriptorA.ID})
	requireNativeStatus(t, response, http.StatusOK)
	view = read()
	if view.Policy == nil || view.Policy.ID != descriptorA.ID || view.ApprovedBy != "test_hub_admin" || len(view.ObservedPolicies) != 1 || view.ObservedPolicies[0].Policy.ID != descriptorB.ID || !view.ObservedPolicies[0].Conflict {
		t.Fatalf("observed policy approval or conflict state = %+v", view)
	}
	other, err := service.createNativeOrganizationFor(t.Context(), "Other example organization")
	if err != nil {
		t.Fatal(err)
	}
	seedPostgresHubAdmin(t, repository, service, other)
	otherProjects := "/api/v2/organizations/" + string(other.ID) + "/projects"
	otherResponse := performHubAPIRequest(t, service, http.MethodPost, otherProjects, testHubAdminToken, map[string]any{"idempotency_key": "project", "name": "Other example project", "grant_access": true, "states": nativeFixtureStates()})
	requireNativeStatus(t, otherResponse, http.StatusCreated)
	var otherProject tracker.NativeProject
	decodeHubResponse(t, otherResponse, &otherProject)
	otherBase := otherProjects + "/" + string(otherProject.ID)
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPut, otherBase+"/policy", testHubAdminToken, policy.Change{Policy: descriptorA}), http.StatusOK)
	capturedSkew := make(chan context.Context, 1)
	service.echo.GET("/api/v2/organizations/:organization/skew-operator", func(c echo.Context) error {
		capturedSkew <- c.Request().Context()
		return c.NoContent(http.StatusOK)
	}, service.operatorAuthority)
	for _, mode := range []string{"observation", "approval", "observed approval", "receipted approval", "reused approval", "unapproved observation"} {
		t.Run("same-content revision skew/"+mode, func(t *testing.T) {
			for _, target := range []struct {
				organization tracker.OrganizationID
				project      tracker.ProjectID
				base         string
			}{{organization.ID, project.ID, base}, {other.ID, otherProject.ID, otherBase}} {
				if err := repository.WriteTenant(t.Context(), target.organization, corestore.Serving, func(w tenantstore.Writer) error {
					record, err := w.ProjectRecord(t.Context(), target.project)
					if err != nil {
						return err
					}
					record.Core.WorkflowSourceRevision = descriptorObservationA.Workflow.Revision
					_, err = w.UpdateProjectRecord(t.Context(), record)
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			for _, target := range []struct {
				organization tracker.OrganizationID
				project      tracker.ProjectID
				base         string
			}{{organization.ID, project.ID, base}, {other.ID, otherProject.ID, otherBase}} {
				selectPostgresHubTestOrganization(service, target.organization)
				requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodGet, "/api/v2/organizations/"+string(target.organization)+"/skew-operator", testHubAdminToken, nil), http.StatusOK)
				ctx := operatortool.BindConnection(context.WithoutCancel(<-capturedSkew), "skew-"+string(target.organization), "policy fixture")
				if err := (hubProjectExecutor{service}).OpenConnection(ctx); err != nil {
					t.Fatal(err)
				}
				file := func(key string) error {
					raw, err := json.Marshal(map[string]string{"project_id": string(target.project), "request_id": mode + key, "title": "Example policy filing", "description": workItemRecordBody, "type": "bug", "state": "Backlog"})
					if err != nil {
						return err
					}
					result, err := (nativeOperatorExecutor{service}).Execute(ctx, operatortool.Call{Name: operatortool.FileIssue, Arguments: raw})
					if err == nil && !strings.Contains(string(result.Content), `"state":"Backlog"`) {
						t.Fatalf("missing filed Backlog issue: %s", result.Content)
					}
					return err
				}
				var conflict *operatortool.ConflictError
				if err := file("before"); !errors.As(err, &conflict) || conflict.Code != "policy_mismatch" {
					t.Fatalf("skew filing error = %v", err)
				}
				before, err := service.readTenantProjectPolicy(t.Context(), target.organization, target.project)
				if err != nil {
					t.Fatal(err)
				}
				switch {
				case mode == "unapproved observation" && target.organization == organization.ID:
					report(runners[0], descriptorB, sourceB)
					if err := file("unapproved"); !errors.As(err, &conflict) || conflict.Code != "policy_mismatch" {
						t.Fatalf("unapproved content repaired filing: %v", err)
					}
					report(runners[0], descriptorObservationA, sourceB)
				case mode == "reused approval" && target.organization == other.ID:
					reused := authoredDescriptor("Example isolated approval history.\n", strings.Repeat("e", 40))
					requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPut, target.base+"/policy", testHubAdminToken, policy.Change{ExpectedID: descriptorA.ID, Policy: reused}), http.StatusOK)
					requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPut, target.base+"/policy", testHubAdminToken, policy.Change{ExpectedID: reused.ID, Policy: descriptorObservationA}), http.StatusOK)
				case mode == "observation" && target.organization == organization.ID:
					report(runners[0], descriptorObservationA, sourceB)
				case mode == "receipted approval":
					call := projectCall(t, "approve_project_policy", string(target.project), "skew-approval", operatortool.PolicyApprovalInput{ExpectedID: descriptorA.ID, Policy: descriptorObservationA})
					for range 2 {
						if _, err := (hubProjectExecutor{service}).command(ctx, call, projectCommandExecute); err != nil {
							t.Fatal(err)
						}
					}
				default:
					change := policy.Change{ExpectedID: descriptorA.ID, Policy: descriptorObservationA}
					if mode == "observed approval" {
						change = policy.Change{ExpectedID: descriptorA.ID, ObservedPolicyID: descriptorA.ID}
					}
					requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPut, target.base+"/policy", testHubAdminToken, change), http.StatusOK)
				}
				if err := file("after"); err != nil {
					t.Fatalf("same-content reconciliation still refuses Backlog file_issue: %v", err)
				}
				after, err := service.readTenantProjectPolicy(t.Context(), target.organization, target.project)
				if err != nil || !reflect.DeepEqual(before.Policy, after.Policy) || mode != "reused approval" && !reflect.DeepEqual(before, after) {
					t.Fatalf("canonical approval changed: %v", err)
				}
			}
		})
	}
	selectPostgresHubTestOrganization(service, organization.ID)
	if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
		return w.RevokeAccessCredential(t.Context(), "credential_example_a", now)
	}); err != nil {
		t.Fatal(err)
	}
	view = read()
	if len(view.ObservedPolicies) != 1 || view.ObservedPolicies[0].Policy.ID != descriptorB.ID || view.ObservedPolicies[0].Conflict {
		t.Fatalf("revoked reporter remained in current policy conflict: %+v", view.ObservedPolicies)
	}
	response = performHubAPIRequest(t, service, http.MethodPut, base+"/policy", testHubAdminToken, policy.Change{ExpectedID: descriptorA.ID, ObservedPolicyID: descriptorB.ID})
	requireNativeStatus(t, response, http.StatusOK)
	view = read()
	if view.Policy == nil || view.Policy.ID != descriptorB.ID {
		t.Fatalf("second observed policy was not approved: %+v", view.Policy)
	}
	descriptorC := descriptorB
	descriptorC = authoredDescriptor("Implement a third assigned issue.\n", strings.Repeat("c", 40))
	sourceC := &policy.RepositorySource{Repository: "example-org/example-project", Commit: strings.Repeat("c", 40), DefaultBranch: "main", DefaultBranchHead: strings.Repeat("d", 40), DefaultBranchReachable: true}
	report(runners[1], descriptorC, sourceC)
	view = read()
	if view.Policy == nil || view.Policy.ID != descriptorB.ID || len(view.ObservedPolicies) != 1 || view.ObservedPolicies[0].Policy.ID != descriptorC.ID {
		t.Fatalf("runner-reported default-branch reachability approved policy without a human: %+v", view)
	}
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPut, base+"/policy", testHubAdminToken, policy.Change{ExpectedID: descriptorB.ID, ObservedPolicyID: descriptorC.ID}), http.StatusOK)
	view = read()
	if view.Policy == nil || view.Policy.ID != descriptorC.ID || view.ApprovedBy == "repository-default-branch" || strings.HasPrefix(view.ApprovedBy, "runner_") || len(view.History) == 0 || view.History[0].AppliedBy != view.ApprovedBy || view.History[0].Definition == nil || view.History[0].Definition.ID != descriptorC.ID || view.History[0].PreviousDefinition == nil || view.History[0].PreviousDefinition.ID != descriptorB.ID {
		t.Fatalf("human approval of the reported default-branch policy was not applied with history: %+v", view)
	}
	response = performHubAPIRequest(t, service, http.MethodGet, base+"/policy?limit=2", testHubAdminToken, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var historyPage projectPolicyRead
	decodeHubResponse(t, response, &historyPage)
	if len(historyPage.History) != 2 || historyPage.HistoryNext == "" {
		t.Fatalf("first workflow history page = %+v", historyPage.History)
	}
	response = performHubAPIRequest(t, service, http.MethodGet, base+"/policy?limit=2&after="+historyPage.HistoryNext, testHubAdminToken, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var secondHistoryPage projectPolicyRead
	decodeHubResponse(t, response, &secondHistoryPage)
	if len(secondHistoryPage.History) != 1 || secondHistoryPage.HistoryNext != "" {
		t.Fatalf("second workflow history page = %+v", secondHistoryPage.History)
	}
	if err := repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(reader tenantstore.Reader) error {
		record, err := reader.ProjectRecord(t.Context(), project.ID)
		if err != nil {
			return err
		}
		if record.Core.WorkflowSourceRevision != strings.Repeat("c", 40) {
			t.Fatalf("applied project workflow revision = %q", record.Core.WorkflowSourceRevision)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	captured := make(chan context.Context, 1)
	service.echo.GET("/api/v2/organizations/:organization/policy-operator-test", func(c echo.Context) error {
		captured <- c.Request().Context()
		return c.NoContent(http.StatusOK)
	}, service.operatorAuthority)
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodGet, "/api/v2/organizations/"+string(organization.ID)+"/policy-operator-test", testHubAdminToken, nil), http.StatusOK)
	operatorContext := operatortool.BindConnection(context.WithoutCancel(<-captured), "policy-operator", "policy fixture")
	executor := hubProjectExecutor{service}
	if err := executor.OpenConnection(operatorContext); err != nil {
		t.Fatal(err)
	}
	operatorArguments, err := json.Marshal(map[string]string{"project_id": string(project.ID)})
	if err != nil {
		t.Fatal(err)
	}
	operatorRead, err := executor.read(operatorContext, operatortool.Call{Name: "get_project_policy", Arguments: operatorArguments})
	if err != nil {
		t.Fatal(err)
	}
	var operatorPage struct {
		Data projectPolicyRead `json:"data"`
	}
	if err := json.Unmarshal(operatorRead.Content, &operatorPage); err != nil || operatorPage.Data.Policy == nil || operatorPage.Data.Policy.ID != descriptorC.ID || operatorPage.Data.ApprovedBy != view.ApprovedBy {
		t.Fatalf("selected PostgreSQL operator policy read = %+v, %v", operatorPage.Data, err)
	}
	descriptorD := authoredDescriptor("Operator-approved workflow content.\n", strings.Repeat("e", 40))
	sourceD := &policy.RepositorySource{Repository: "example-org/example-project", Commit: strings.Repeat("e", 40), DefaultBranch: "main", DefaultBranchHead: strings.Repeat("f", 40)}
	report(runners[1], descriptorD, sourceD)
	descriptorE := authoredDescriptor("Later administrator workflow content.\n", strings.Repeat("f", 40))
	sourceE := &policy.RepositorySource{Repository: "example-org/example-project", Commit: strings.Repeat("f", 40), DefaultBranch: "main", DefaultBranchHead: strings.Repeat("1", 40)}
	assertPostgresPolicyApprovalLockOrdering(t, service, repository, organization.ID, project.ID, "credential_example_b", runners[1].runnerID, descriptorC.ID, descriptorD, descriptorE, sourceE)
	approvalCall := projectCall(t, "approve_project_policy", string(project.ID), "operator-policy-approval", operatortool.PolicyApprovalInput{ExpectedID: descriptorC.ID, ObservedPolicyID: descriptorD.ID})
	firstApproval, err := executor.command(operatorContext, approvalCall, projectCommandExecute)
	if err != nil {
		t.Fatal(err)
	}
	response = performHubAPIRequest(t, service, http.MethodPut, base+"/policy", testHubAdminToken, policy.Change{ExpectedID: descriptorD.ID, Policy: descriptorE})
	requireNativeStatus(t, response, http.StatusOK)
	assertPostgresPolicyObservationOrdering(t, service, repository, organization.ID, project.ID, "credential_example_b", runners[1].runnerID, descriptorD, sourceD)
	if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
		return w.RevokeAccessCredential(t.Context(), "credential_example_b", now)
	}); err != nil {
		t.Fatal(err)
	}
	executeReplay, err := executor.command(operatorContext, approvalCall, projectCommandExecute)
	if err != nil || string(executeReplay) != string(firstApproval) {
		t.Fatalf("operator execute receipt replay after observation removal = %s, %v; first %s", executeReplay, err, firstApproval)
	}
	replayApproval, err := executor.command(operatorContext, approvalCall, projectCommandReplay)
	if err != nil || string(replayApproval) != string(firstApproval) {
		t.Fatalf("operator policy receipt replay = %s, %v; first %s", replayApproval, err, firstApproval)
	}
	changedApproval := projectCall(t, "approve_project_policy", string(project.ID), "operator-policy-approval", operatortool.PolicyApprovalInput{ExpectedID: descriptorC.ID, ObservedPolicyID: descriptorC.ID})
	if _, err := executor.command(operatorContext, changedApproval, projectCommandExecute); err == nil {
		t.Fatal("same operator request key accepted different policy input")
	}
	revoke := performHubAPIRequest(t, service, http.MethodDelete, base+"/policy", testHubAdminToken, policy.Change{ExpectedID: descriptorE.ID})
	requireNativeStatus(t, revoke, http.StatusGone)
	if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
		return w.RevokeAccessCredential(t.Context(), "test_hub_admin", now)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := executor.command(operatorContext, approvalCall, projectCommandExecute); err == nil {
		t.Fatal("revoked operator credential replayed a policy receipt")
	}
}

func TestSelectedPostgresPolicyApprovalFailsClosedWithoutAccessRepository(t *testing.T) {
	service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), RunDetails: &memoryRunDetails{}})
	service.config.TenantRepository = selectedPostgresWithoutAccess{}
	service.config.AccessRepository = nil
	if err := service.database.db.Close(); err != nil {
		t.Fatal(err)
	}
	scope := nativeScope{organization: "org_example", project: "prj_example", credential: apiCredential{ID: "credential_example", Hash: apikey.HashToken(testHubAdminToken), Scope: apiScopeAdmin}}
	for name, operation := range map[string]func() error{
		"route authorization": func() error { return service.authorizeProjectRecord(t.Context(), scope, true) },
		"mutation authority": func() error {
			_, err := service.beginProjectRecordAuthority(t.Context(), scope)
			return err
		},
		"mutation recheck": func() error { return service.recheckProjectRecordMutation(t.Context(), scope) },
		"policy approval": func() error {
			_, err := service.approveTenantProjectPolicy(t.Context(), scope, scope.credential.ID, policy.Change{Policy: hubTestPolicy()})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := operation()
			var native *nativeError
			if !errors.As(err, &native) || native.status != http.StatusNotFound {
				t.Fatalf("selected PostgreSQL authority error = %v, want not found", err)
			}
		})
	}
}

type selectedPostgresWithoutAccess struct{ tenantstore.Repository }

func TestPostgresProjectPolicyApprovalRejectsDemotedHostedAdmin(t *testing.T) {
	repository := projectRecordPostgres(t)
	service := openPostgresHubTestService(t, repository, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), RunDetails: &memoryRunDetails{}})
	organization, err := service.createNativeOrganizationFor(t.Context(), "Example organization")
	if err != nil {
		t.Fatal(err)
	}
	seedPostgresHubAdmin(t, repository, service, organization)
	response := performHubAPIRequest(t, service, http.MethodPost, "/api/v2/organizations/"+string(organization.ID)+"/projects", testHubAdminToken, map[string]any{"idempotency_key": "project", "name": "Example project", "grant_access": true, "states": nativeFixtureStates()})
	requireNativeStatus(t, response, http.StatusCreated)
	var project tracker.NativeProject
	decodeHubResponse(t, response, &project)
	now := service.config.now().UTC()
	const userID, memberID, credentialID, sessionHash = "user_example", "membership_example", "credential_example_owner", "session_example_owner"
	credentialHash := apikey.HashToken("example_owner_token")
	if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization.ID, ID: credentialID, Name: "Example owner", Hash: credentialHash, Fingerprint: tokenFingerprint(credentialHash), Scope: "operator", NativeOnly: true, CreatedAt: now, UpdatedAt: now, UserID: userID, MembershipID: memberID, KeyScope: "admin", ProjectAccess: "selected", Kind: "personal"}); err != nil {
			return err
		}
		if err := w.CreateAccessMember(t.Context(), accessstore.Member{OrganizationID: organization.ID, UserID: userID, Email: "owner@example.test", MembershipID: memberID, Role: "owner", Active: true, PrincipalID: credentialID, CreatedAt: now, UpdatedAt: now}); err != nil {
			return err
		}
		if err := w.CreateAccessSession(t.Context(), accessstore.Session{OrganizationID: organization.ID, Hash: sessionHash, Email: "owner@example.test", Subject: userID, ProviderOrganizationID: "provider_example", SessionID: "session_example", IdentityCreatedAt: now, IdentityExpiresAt: now.Add(time.Hour), CreatedAt: now, ExpiresAt: now.Add(time.Hour)}); err != nil {
			return err
		}
		if err := w.CreateAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: organization.ID, UserID: userID, ProjectID: project.ID, CanWrite: true}); err != nil {
			return err
		}
		return w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization.ID, TokenID: credentialID, ProjectID: project.ID})
	}); err != nil {
		t.Fatal(err)
	}
	service.config.Hosted = &HostedConfig{OrganizationID: string(organization.ID)}
	identity := &auth.HostedIdentity{Subject: userID, OrganizationID: "provider_example", SessionID: "session_example", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	actor := apiCredential{ID: credentialID, Hash: credentialHash, Scope: apiScopeOperator, NativeOnly: true, Hosted: identity, SessionHash: sessionHash, HostedRole: "owner", HostedMembership: memberID, HostedKeyScope: apikey.ScopeAdmin}
	scope := nativeScope{organization: organization.ID, project: project.ID, credential: actor, requireHostedAdmin: true}
	if err := repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(r tenantstore.Reader) error {
		return service.authorizeAccessScope(t.Context(), r, scope, true)
	}); err != nil {
		t.Fatalf("authorize current owner before demotion: %v", err)
	}
	if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
		member, err := w.AccessMember(t.Context(), userID)
		if err != nil {
			return err
		}
		member.Role = "member"
		member.UpdatedAt = now.Add(time.Second)
		return w.UpdateAccessMember(t.Context(), member)
	}); err != nil {
		t.Fatal(err)
	}
	memberScope := scope
	memberScope.requireHostedAdmin = false
	if err := repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(r tenantstore.Reader) error {
		return service.authorizeAccessScope(t.Context(), r, memberScope, true)
	}); err != nil {
		t.Fatalf("demoted member lost retained project write grant: %v", err)
	}
	_, err = service.approveTenantProjectPolicy(t.Context(), scope, credentialID, policy.Change{Policy: hubTestPolicy()})
	if !errors.Is(err, accessstore.ErrDenied) {
		t.Fatalf("approval after admin demotion error = %v, want access denial", err)
	}
}

type postgresPolicyObservationGateKey struct{}

type postgresPolicyObservationGate struct {
	reached chan struct{}
	release chan struct{}
}

type policyLockWriteKindKey struct{}

type policyApprovalLockGate struct {
	approvalChecked chan struct{}
	approvalRelease chan struct{}
	replacementLock chan struct{}
	approvalLocked  atomic.Bool
	replacementHeld atomic.Bool
	lockedAtCheck   atomic.Bool
	recordReadLock  atomic.Bool
}

type policyApprovalLockRepository struct {
	tenantstore.Repository
	gate *policyApprovalLockGate
}

func (r policyApprovalLockRepository) WriteTenant(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, write func(tenantstore.Writer) error) error {
	kind, _ := ctx.Value(policyLockWriteKindKey{}).(string)
	err := r.Repository.WriteTenant(ctx, organization, workload, func(writer tenantstore.Writer) error {
		return write(policyApprovalLockWriter{Writer: writer, kind: kind, gate: r.gate})
	})
	return err
}

type policyApprovalLockWriter struct {
	tenantstore.Writer
	kind string
	gate *policyApprovalLockGate
}

func (w policyApprovalLockWriter) LockWorkItemProject(ctx context.Context, project tracker.ProjectID) error {
	if w.kind == "replacement" {
		select {
		case <-w.gate.replacementLock:
		default:
			close(w.gate.replacementLock)
		}
	}
	if err := w.Writer.LockWorkItemProject(ctx, project); err != nil {
		return err
	}
	switch w.kind {
	case "approval":
		w.gate.approvalLocked.Store(true)
	case "replacement":
		w.gate.replacementHeld.Store(true)
	}
	return nil
}

func (w policyApprovalLockWriter) CurrentProjectPolicyObservation(ctx context.Context, project tracker.ProjectID, policyID string, now time.Time) (bool, error) {
	current, err := w.Writer.CurrentProjectPolicyObservation(ctx, project, policyID, now)
	if w.kind == "approval" {
		w.gate.lockedAtCheck.Store(w.gate.approvalLocked.Load())
		close(w.gate.approvalChecked)
		select {
		case <-w.gate.approvalRelease:
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	return current, err
}

func (w policyApprovalLockWriter) ProjectRecord(ctx context.Context, project tracker.ProjectID) (projectstore.Record, error) {
	if w.kind == "replacement" {
		w.gate.recordReadLock.Store(w.gate.replacementHeld.Load())
	}
	return w.Writer.ProjectRecord(ctx, project)
}

func assertPostgresPolicyApprovalLockOrdering(t *testing.T, service *Service, repository tenantstore.Repository, organization tracker.OrganizationID, project tracker.ProjectID, credentialID, runnerID, expectedID string, approved, replacement policy.Descriptor, replacementSource *policy.RepositorySource) {
	t.Helper()
	var credential apiCredential
	if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(reader tenantstore.Reader) error {
		token, err := reader.AccessCredential(t.Context(), credentialID)
		if err != nil {
			return err
		}
		identity, err := reader.RunnerIdentityByCredential(t.Context(), credentialID)
		if err != nil {
			return err
		}
		credential = apiCredential{ID: token.ID, Hash: token.Hash, Scope: apiScopeWorker, Runner: runnerauth.Identity{Binding: runnerauth.Binding{RunnerID: identity.RunnerID}, OrganizationID: identity.OrganizationID}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	gate := &policyApprovalLockGate{
		approvalChecked: make(chan struct{}),
		approvalRelease: make(chan struct{}),
		replacementLock: make(chan struct{}),
	}
	originalRepository := service.config.TenantRepository
	service.config.TenantRepository = policyApprovalLockRepository{Repository: repository, gate: gate}
	var releaseOnce sync.Once
	releaseApproval := func() { releaseOnce.Do(func() { close(gate.approvalRelease) }) }
	var approvalStarted, replacementStarted, approvalJoined, replacementJoined bool
	approvalDone := make(chan error, 1)
	replacementDone := make(chan error, 1)
	t.Cleanup(func() {
		releaseApproval()
		if approvalStarted && !approvalJoined {
			<-approvalDone
		}
		if replacementStarted && !replacementJoined {
			<-replacementDone
		}
		service.config.TenantRepository = originalRepository
	})
	approvalCtx := context.WithValue(t.Context(), policyLockWriteKindKey{}, "approval")
	approvalStarted = true
	go func() {
		_, err := service.approveTenantProjectPolicy(approvalCtx, nativeScope{organization: organization, project: project, credential: apiCredential{ID: "test_hub_admin", Hash: apikey.HashToken(testHubAdminToken), Scope: apiScopeAdmin}}, "test_hub_admin", policy.Change{ExpectedID: expectedID, ObservedPolicyID: approved.ID})
		approvalDone <- err
	}()
	select {
	case <-gate.approvalChecked:
	case err := <-approvalDone:
		approvalJoined = true
		t.Fatalf("approval did not reach the observation lock barrier: %v", err)
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	}
	replacementCtx := context.WithValue(t.Context(), policyLockWriteKindKey{}, "replacement")
	replacementStarted = true
	go func() {
		replacementDone <- service.observeTenantProjectPolicy(replacementCtx, nativeScope{organization: organization, project: project, credential: credential}, credential, runnerID, policy.Observation{Descriptor: replacement, Source: replacementSource})
	}()
	select {
	case <-gate.replacementLock:
	case err := <-replacementDone:
		replacementJoined = true
		t.Fatalf("replacement report did not reach the project lock: %v", err)
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	}
	if !gate.lockedAtCheck.Load() {
		select {
		case err := <-replacementDone:
			replacementJoined = true
			if err != nil {
				t.Fatalf("replacement report before approval: %v", err)
			}
		case <-t.Context().Done():
			t.Fatal(t.Context().Err())
		}
	}
	releaseApproval()
	if err := <-approvalDone; err != nil {
		approvalJoined = true
		t.Fatalf("approve current report: %v", err)
	}
	approvalJoined = true
	if err := <-replacementDone; err != nil {
		replacementJoined = true
		t.Fatalf("replace report after approval: %v", err)
	}
	replacementJoined = true
	service.config.TenantRepository = originalRepository
	if !gate.lockedAtCheck.Load() {
		t.Fatal("approval validated the report before locking the project")
	}
	if !gate.recordReadLock.Load() {
		t.Fatal("default-branch observation read project settings before locking the project")
	}
	if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(reader tenantstore.Reader) error {
		current, err := reader.CurrentProjectPolicyObservation(t.Context(), project, replacement.ID, service.config.now())
		if err != nil {
			return err
		}
		if !current {
			t.Fatal("replacement report did not become current after approval committed")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

type postgresPolicyObservationRepository struct {
	tenantstore.Repository
	gate *postgresPolicyObservationGate
}

func (r postgresPolicyObservationRepository) WriteTenant(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, write func(tenantstore.Writer) error) error {
	if blocked, _ := ctx.Value(postgresPolicyObservationGateKey{}).(bool); blocked {
		close(r.gate.reached)
		select {
		case <-r.gate.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return r.Repository.WriteTenant(ctx, organization, workload, write)
}

func assertPostgresPolicyObservationOrdering(t *testing.T, service *Service, repository tenantstore.Repository, organization tracker.OrganizationID, project tracker.ProjectID, credentialID, runnerID string, descriptor policy.Descriptor, source *policy.RepositorySource) {
	t.Helper()
	var credential apiCredential
	if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(reader tenantstore.Reader) error {
		row, err := reader.AccessCredential(t.Context(), credentialID)
		if err != nil {
			return err
		}
		identity, err := reader.RunnerIdentityByCredential(t.Context(), credentialID)
		if err != nil {
			return err
		}
		credential = apiCredential{ID: row.ID, Hash: row.Hash, Scope: apiScopeWorker, Runner: runnerauth.Identity{Binding: runnerauth.Binding{RunnerID: identity.RunnerID}, OrganizationID: identity.OrganizationID}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	gate := &postgresPolicyObservationGate{reached: make(chan struct{}), release: make(chan struct{})}
	originalRepository := service.config.TenantRepository
	originalClock := service.config.now
	service.config.TenantRepository = postgresPolicyObservationRepository{Repository: repository, gate: gate}
	var releaseOnce sync.Once
	releaseOlder := func() { releaseOnce.Do(func() { close(gate.release) }) }
	olderJoined := false
	olderStarted := false
	olderDone := make(chan error, 1)
	t.Cleanup(func() {
		releaseOlder()
		if olderStarted && !olderJoined {
			<-olderDone
		}
		service.config.TenantRepository = originalRepository
		service.config.now = originalClock
	})
	olderAt := originalClock().UTC().Truncate(time.Microsecond).Add(time.Second)
	newerAt := olderAt.Add(time.Second)
	service.config.now = func() time.Time { return olderAt }
	olderContext := context.WithValue(t.Context(), postgresPolicyObservationGateKey{}, true)
	olderStarted = true
	go func() {
		olderSource := *source
		olderSource.DefaultBranchHead = strings.Repeat("a", 40)
		olderDone <- service.observeTenantProjectPolicy(olderContext, nativeScope{organization: organization, project: project, credential: credential}, credential, runnerID, policy.Observation{Descriptor: descriptor, Source: &olderSource})
	}()
	select {
	case <-gate.reached:
	case err := <-olderDone:
		olderJoined = true
		t.Fatalf("older observation did not reach the write barrier: %v", err)
	}
	service.config.now = func() time.Time { return newerAt }
	newerSource := *source
	newerSource.DefaultBranchHead = strings.Repeat("b", 40)
	if err := service.observeTenantProjectPolicy(t.Context(), nativeScope{organization: organization, project: project, credential: credential}, credential, runnerID, policy.Observation{Descriptor: descriptor, Source: &newerSource}); err != nil {
		t.Fatalf("write newer observation: %v", err)
	}
	releaseOlder()
	if err := <-olderDone; err != nil {
		olderJoined = true
		t.Fatalf("finish older observation: %v", err)
	}
	olderJoined = true
	service.config.now = originalClock
	if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(reader tenantstore.Reader) error {
		rows, _, err := reader.ProjectPolicyObservations(t.Context(), project, "", descriptor.ID, originalClock(), 10)
		if err != nil {
			return err
		}
		for _, row := range rows {
			if row.RunnerID == runnerID {
				if !row.ObservedAt.Equal(newerAt) || row.Source == nil || row.Source.DefaultBranchHead != newerSource.DefaultBranchHead {
					t.Fatalf("older report replaced newer report: %+v", row.Observation)
				}
				return nil
			}
		}
		return fmt.Errorf("newer runner observation is missing")
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCompatibilityPolicyUpgradeAndRevocation(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"legacy", "approved", "revoked"} {
		t.Run(state, func(t *testing.T) {
			service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db")})
			_, issueID := seedProjection(t, service.database.db)
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, "/api/v1/machines/register", testHubAdminToken, map[string]any{"id": "machine_abc", "hostname": "runner", "capacity": 1, "version": "test"}), http.StatusOK)
			lease, err := service.database.Claim(t.Context(), tracker.ClaimRequest{WorkItemID: tracker.WorkItemID(issueID), MachineID: "machine_abc", SessionID: "session", TTL: time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			if state != "legacy" {
				descriptor := hubTestPolicy()
				approveHubTestPolicy(t, service, "/api/v1/repositories/digitaldrywood/detent/policy", descriptor)
				if _, err := service.database.db.ExecContext(t.Context(), "INSERT INTO lease_policies (lease_id, scope, policy_id) VALUES (?, ?, ?)", lease.ID, "repository:digitaldrywood/detent", descriptor.ID); err != nil {
					t.Fatal(err)
				}
				if state == "revoked" {
					requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodDelete, "/api/v1/repositories/digitaldrywood/detent/policy", testHubAdminToken, map[string]string{"expected_policy_id": descriptor.ID}), http.StatusGone)
				}
			}
			response := performHubAPIRequest(t, service, http.MethodGet, "/api/v1/repositories/digitaldrywood/detent/policy", testHubAdminToken, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var current struct {
				Policy *policy.Descriptor `json:"policy"`
			}
			decodeHubResponse(t, response, &current)
			if state != "legacy" {
				if current.Policy == nil || !reflect.DeepEqual(*current.Policy, hubTestPolicy()) {
					t.Fatalf("approved repository policy: %s", response.Body)
				}
			} else if current.Policy != nil || !strings.Contains(response.Body.String(), `"policy":null`) {
				t.Fatalf("absent repository policy: %s", response.Body)
			}
			wantRenew, wantEvent := http.StatusOK, http.StatusCreated
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, "/api/v1/leases/"+string(lease.ID)+"/renew", testHubAdminToken, map[string]any{"fencing_token": lease.FencingToken, "ttl_seconds": 90}), wantRenew)
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, fmt.Sprintf("/api/v1/work-items/%d/events", issueID), testHubAdminToken, map[string]any{"fencing_token": lease.FencingToken, "kind": "progress", "payload": map[string]string{"step": "plan"}}), wantEvent)
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, "/api/v1/leases/"+string(lease.ID)+"/release", testHubAdminToken, map[string]any{"fencing_token": lease.FencingToken, "reason": "cancelled"}), http.StatusNoContent)
		})
	}
}

func hubTestPolicy() policy.Descriptor {
	return policy.Descriptor{
		SourceRevision: strings.Repeat("a", 40), SourceDigest: policy.Digest([]byte("source")), ConfigDigest: policy.Digest([]byte("config")),
		Gates: policy.Gates{Kind: "command", PlanReview: "human", PlanStopDigest: policy.Digest([]byte("Plan Review")), AutomatedReview: "optional", MergeMethod: "squash"},
	}.WithID()
}

func approveHubTestPolicy(t *testing.T, service *Service, path string, descriptor policy.Descriptor) {
	t.Helper()
	current := performHubAPIRequest(t, service, http.MethodGet, path, testHubAdminToken, nil)
	var approval policy.Approval
	if current.Code == http.StatusOK {
		decodeHubResponse(t, current, &approval)
	}
	response := performHubAPIRequest(t, service, http.MethodPut, path, testHubAdminToken, policy.Change{ExpectedID: approval.Policy.ID, Policy: descriptor})
	requireNativeStatus(t, response, http.StatusOK)
}

func TestProjectPolicyAuthorizationAndAtomicClaims(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t, nil, "", "policy")
	worker := f.worker(t, "worker")
	issue := f.create(t, "work")
	descriptor := hubTestPolicy()
	descriptor.Workflow = &policy.Workflow{Source: "detent.yaml", States: append(nativeFixtureStates(), tracker.NativeState{Name: "Rework", Dispatchable: true, Transitions: []string{"In Progress", "Done"}})}
	descriptor = descriptor.WithID()
	claim := tracker.NativeClaim{WorkItemID: issue.WorkItemID, MachineID: "machine_abc", SessionID: "session", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/register", worker, map[string]any{"id": claim.MachineID, "hostname": "runner", "capacity": 2, "version": "test"}), http.StatusOK)
	runner := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
	runner.enroll(t)
	for _, token := range []string{worker, f.token, runner.redemption.Credential} {
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/policy", token, policy.Change{Policy: descriptor}), http.StatusForbidden)
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", worker, claim), http.StatusUnprocessableEntity)
	approveHubTestPolicy(t, f.service, f.base+"/policy", descriptor)
	readExecution := func() policy.Approval {
		t.Helper()
		response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/policy?execution_configuration=1", worker, nil)
		requireNativeStatus(t, response, http.StatusOK)
		var approval policy.Approval
		decodeHubResponse(t, response, &approval)
		if approval.ExecutionConfiguration == nil || approval.ExecutionConfiguration.PolicyID != approval.Policy.ID {
			t.Fatal("policy read lost execution configuration")
		}
		return approval
	}
	initialExecution := readExecution()
	running, err := config.ApplyExecutionConfiguration(config.Workflow{Config: config.Default()}, initialExecution.Policy, initialExecution.ExecutionConfiguration)
	if err != nil {
		t.Fatal(err)
	}
	initialValues := string(initialExecution.ExecutionConfiguration.Values)
	for _, field := range []string{"states", "workflow_markdown"} {
		request := map[string]any{"idempotency_key": "repository-override-" + field, "expected_revision": "2", "intake": "disabled", "projection": "disabled"}
		if field == "states" {
			request[field] = nativeFixtureStates()
		} else {
			request[field] = "---\ntracker:\n  kind: hub_native\n---\nWork\n"
		}
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/integration", testHubAdminToken, request), http.StatusUnprocessableEntity)
	}
	for _, test := range []struct{ name, id string }{{"missing", ""}, {"unknown", "policy_" + strings.Repeat("b", 64)}} {
		t.Run(test.name, func(t *testing.T) {
			claim.PolicyID = test.id
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", worker, claim), http.StatusUnprocessableEntity)
			var leases int
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM leases").Scan(&leases); err != nil || leases != 0 {
				t.Fatalf("rejected claim allocated a lease: %d %v", leases, err)
			}
		})
	}
	claim.PolicyID = descriptor.ID
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", worker, claim)
	requireNativeStatus(t, response, http.StatusOK)
	var lease tracker.NativeLease
	decodeHubResponse(t, response, &lease)
	if lease.PolicyID != descriptor.ID {
		t.Fatalf("lease lost policy: %#v", lease)
	}
	event := nativeStartedEvent(lease)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/events", worker, event), http.StatusOK)
	changed := descriptor
	changed.Gates.AutoPromote = true
	changed.Workflow = &policy.Workflow{Source: "detent.yaml", States: append(append([]tracker.NativeState(nil), descriptor.Workflow.States...), tracker.NativeState{Name: "QA"})}
	changed = changed.WithID()
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/policy", testHubAdminToken, policy.Change{ExpectedID: descriptor.ID, Policy: changed}), http.StatusOK)
	pinned, err := f.service.database.leasePolicyID(t.Context(), lease.ID)
	if err != nil || pinned != descriptor.ID {
		t.Fatalf("approval changed existing lease policy: %s %v", pinned, err)
	}
	updatedExecution := readExecution()
	if updatedExecution.ExecutionConfiguration.PolicyID != changed.ID || running.Config.Policy.ID != pinned || string(initialExecution.ExecutionConfiguration.Values) != initialValues {
		t.Fatal("policy approval replaced a running attempt's execution configuration")
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/renew", worker, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, TTLSeconds: 90}), http.StatusOK)
	reported := descriptor
	reported.SourceDigest = policy.Digest([]byte("reported configuration"))
	reported = reported.WithID()
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/policy/observed", worker, policy.Observation{Descriptor: reported}), http.StatusNoContent)
	claim.PolicyID = reported.ID
	newIssue := f.create(t, "new policy work")
	claim.WorkItemID, claim.SessionID = newIssue.WorkItemID, "new-policy-session"
	response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", worker, claim)
	requireNativeStatus(t, response, http.StatusOK)
	var newLease tracker.NativeLease
	decodeHubResponse(t, response, &newLease)
	if newLease.PolicyID != reported.ID || newLease.ID == lease.ID {
		t.Fatalf("new claim lost reported configuration: %+v", newLease)
	}
	event.Type, event.Data.Sequence, event.Data.Handoff = "run.checkpointed", 2, nativeTestCheckpoint()
	event.IdempotencyKey, event.Data.PolicyID = "forged", changed.ID
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/events", worker, event), http.StatusConflict)
	event.Data.PolicyID, event.IdempotencyKey = descriptor.ID, "pinned"
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/events", worker, event), http.StatusOK)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodDelete, f.base+"/policy", worker, map[string]string{"expected_policy_id": changed.ID}), http.StatusForbidden)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodDelete, f.base+"/policy", testHubAdminToken, map[string]string{"expected_policy_id": changed.ID}), http.StatusGone)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/renew", worker, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, TTLSeconds: 90}), http.StatusOK)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(newLease.ID)+"/release", worker, tracker.NativeLeaseMutation{FencingToken: newLease.FencingToken, Reason: "cancelled"}), http.StatusNoContent)
	claim.WorkItemID = f.create(t, "reported configuration without approval").WorkItemID
	claim.SessionID = "reported-without-approval"
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", worker, claim), http.StatusOK)
	event.Data.PolicyID = descriptor.ID
	event.IdempotencyKey = "revoked"
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/events", worker, event), http.StatusOK)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/release", worker, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "cancelled"}), http.StatusNoContent)
}

func TestConfigurationChangesPreserveLandingWork(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name          string
		stale         int
		target        string
		refusal       string
		status        int
		barrier       bool
		activeLanding bool
		mutation      string
	}{
		{name: "zero", target: "Rework", status: http.StatusOK},
		{name: "one", stale: 1, target: "Rework", status: http.StatusOK},
		{name: "several", stale: 3, target: "Rework", status: http.StatusOK},
		{name: "fallback", stale: 2, target: "In Progress", status: http.StatusOK},
		{name: "rolling barrier", stale: 2, target: "Rework", status: http.StatusOK, barrier: true},
		{name: "active landing lease", stale: 1, target: "Rework", status: http.StatusOK, activeLanding: true},
		{name: "no refusal lane needed", stale: 2, target: "Done", status: http.StatusOK},
		{name: "unauthorized", stale: 2, target: "Rework", refusal: "unauthorized", status: http.StatusForbidden},
		{name: "stale approval", stale: 2, target: "Rework", refusal: "expected", status: http.StatusConflict},
		{name: "configuration does not transition issues", stale: 2, target: "Rework", refusal: "write", status: http.StatusOK},
		{name: "review policy replacement", stale: 3, target: "Rework", mutation: "review", status: http.StatusOK},
		{name: "review policy unauthorized", stale: 2, target: "Rework", mutation: "review", refusal: "unauthorized", status: http.StatusForbidden},
		{name: "review policy transition rollback", stale: 2, target: "Rework", mutation: "review", refusal: "write", status: http.StatusInternalServerError},
		{name: "repository policy revocation", stale: 3, target: "Rework", mutation: "revoke", status: http.StatusGone},
		{name: "repository policy revocation unauthorized", stale: 2, target: "Rework", mutation: "revoke", refusal: "unauthorized", status: http.StatusForbidden},
		{name: "repository revocation does not transition issues", stale: 2, target: "Rework", mutation: "revoke", refusal: "write", status: http.StatusGone},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newNativeFixture(t, nil, "", "policy-landing")
			states := HostedProjectStates()
			states = append(states, tracker.NativeState{Name: "Rework", Dispatchable: true, Transitions: []string{"In Progress", "Merging", "Done"}})
			for i := range states {
				if states[i].Name == "Merging" {
					states[i].Transitions = []string{test.target}
				}
			}
			previous := hubTestPolicy()
			previous.Workflow = &policy.Workflow{Source: "detent.yaml", States: states}
			previous = previous.WithID()
			next := previous
			next.Gates.AutoPromote = true
			if test.barrier {
				next.Gates.LandingMode = "rolling_barrier"
				next.Gates.LandingCommandDigest = policy.Digest([]byte("make check-barrier"))
			}
			next = next.WithID()
			controlPolicy := next
			if test.mutation != "" {
				controlPolicy = previous
			}
			approveHubTestPolicy(t, f.service, f.base+"/policy", controlPolicy)
			nextRules := defaultChangeReviewPolicy(previous)
			nextRules.RequireReview = true
			nextRules.ID = changerequest.PolicyID(nextRules)
			if test.mutation == "review" {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/change-review-policy", testHubAdminToken, tracker.ApproveChangeReviewPolicy{Mutation: tracker.Mutation{IdempotencyKey: "future-review"}, Policy: nextRules}), http.StatusOK)
			}
			createChange := func(name, state string, descriptor policy.Descriptor) changeFixture {
				t.Helper()
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", f.token, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: name}, Title: name, State: state})
				requireNativeStatus(t, response, http.StatusOK)
				var issue tracker.NativeIssue
				decodeHubResponse(t, response, &issue)
				path := f.base + "/work-items/" + string(issue.WorkItemID) + "/changes"
				response = performHubAPIRequest(t, f.service, http.MethodPost, path, f.token, tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "change"}, Title: name})
				requireNativeStatus(t, response, http.StatusOK)
				var change tracker.ChangeRequest
				decodeHubResponse(t, response, &change)
				path += "/" + change.ID
				input := changeTestInput()
				input.PolicyID = descriptor.ID
				bundle := changeTestSource(t, &input)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/versions", f.token, tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "publish"}, ChangeVersionInput: input, SourceBundle: bundle}), http.StatusOK)
				return changeFixture{nativeFixture: f, issue: issue, change: change, path: path}
			}
			current := createChange("current policy", "In Progress", controlPolicy)
			if test.mutation == "review" {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/change-review-policy", testHubAdminToken, tracker.ApproveChangeReviewPolicy{Mutation: tracker.Mutation{IdempotencyKey: "old-review"}, ExpectedID: nextRules.ID, Policy: defaultChangeReviewPolicy(previous)}), http.StatusOK)
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/policy", testHubAdminToken, policy.Change{ExpectedID: controlPolicy.ID, Policy: previous}), http.StatusOK)
			moveToMerging := func(change changeFixture) {
				t.Helper()
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(change.issue.WorkItemID)+"/workflow", f.token, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "merging"}, ExpectedRevision: change.issue.Revision, State: "Merging", Reason: "user_requested"}), http.StatusOK)
			}
			moveToMerging(current)
			changes := []changeFixture{current}
			for i := range test.stale {
				change := createChange(fmt.Sprintf("stale %d", i), "In Progress", previous)
				moveToMerging(change)
				changes = append(changes, change)
			}
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
			var heldAttempts []tracker.NativeAttempt
			for _, hold := range []struct {
				name        string
				disposition tracker.NativeDisposition
			}{
				{name: "human hold", disposition: tracker.NativeDisposition{Status: "blocked", HumanAction: true}},
				{name: "permission hold", disposition: tracker.NativeDisposition{Status: "blocked", ReasonCode: "permission_wait"}},
			} {
				held := createChange(hold.name, "In Progress", previous)
				worker := f.worker(t, hold.name)
				lease := claimNativeAttempt(t, f, worker, hold.name, hold.name, held.issue.WorkItemID, previous.ID)
				event := nativeStartedEvent(lease)
				path := f.base + "/work-items/" + string(held.issue.WorkItemID)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, event), http.StatusOK)
				event.Type, event.IdempotencyKey, event.Data.Sequence = "run.finished", "finished", 2
				event.Data.Outcome, event.Data.Disposition = "succeeded", &hold.disposition
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, event), http.StatusOK)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/release", worker, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "completed"}), http.StatusNoContent)
				issue, _, err := readNativeIssue(f.service.runDetailContext(t.Context()), f.service.database.db, scope, string(held.issue.WorkItemID))
				if err != nil {
					t.Fatal(err)
				}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/workflow", f.token, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "hold"}, ExpectedRevision: issue.Revision, State: "Blocked", Reason: "user_requested"}), http.StatusOK)
				attempt, err := readNativeAttempt(f.service.runDetailContext(t.Context()), f.service.database.db, scope, string(held.issue.WorkItemID), event.Data.AttemptID, f.service.config.now())
				if err != nil {
					t.Fatal(err)
				}
				heldAttempts = append(heldAttempts, attempt)
				changes = append(changes, held)
			}
			var active tracker.NativeIssue
			if test.activeLanding {
				active = changes[1].issue
			} else {
				active = f.create(t, "active execution")
			}
			worker := f.worker(t, "active-worker")
			lease := claimNativeAttempt(t, f, worker, "active-machine", "active-session", active.WorkItemID, previous.ID)
			before := make([]tracker.ChangeDetail, len(changes))
			issuesBefore := make([]tracker.NativeIssue, len(changes))
			for i, change := range changes {
				before[i] = change.detail(t)
				issue, _, err := readNativeIssue(f.service.runDetailContext(t.Context()), f.service.database.db, scope, string(change.issue.WorkItemID))
				if err != nil {
					t.Fatal(err)
				}
				issuesBefore[i] = issue
			}
			token, expected := testHubAdminToken, previous.ID
			switch test.refusal {
			case "unauthorized":
				token = worker
			case "expected":
				expected = "policy_stale"
			case "write":
				statement := fmt.Sprintf("CREATE TRIGGER refuse_later_transition BEFORE UPDATE OF workflow_state_id ON issues WHEN OLD.native_id = '%s' BEGIN SELECT RAISE(ABORT, 'fixture transition failure'); END", changes[2].issue.WorkItemID)
				if _, err := f.service.database.db.ExecContext(f.service.runDetailContext(t.Context()), statement); err != nil {
					t.Fatal(err)
				}
			}
			drainTestNativeBoard(t, f.service)
			var sequenceBefore, floorBefore int64
			if err := f.service.database.reader.QueryRowContext(f.service.runDetailContext(t.Context()), "SELECT sequence,replay_floor FROM native_board_sequences WHERE organization_id=? AND project_id=?", scope.organization, scope.project).Scan(&sequenceBefore, &floorBefore); err != nil {
				t.Fatal(err)
			}
			succeeded := test.status >= 200 && test.status < 300
			switch test.mutation {
			case "review":
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/change-review-policy", token, tracker.ApproveChangeReviewPolicy{Mutation: tracker.Mutation{IdempotencyKey: "replace-review"}, ExpectedID: defaultChangeReviewPolicy(previous).ID, Policy: nextRules}), test.status)
			case "revoke":
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodDelete, f.base+"/policy", token, map[string]string{"expected_policy_id": expected}), test.status)
			default:
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/policy", token, policy.Change{ExpectedID: expected, Policy: next}), test.status)
			}
			if !succeeded {
				var sequenceAfter, floorAfter int64
				if err := f.service.database.reader.QueryRowContext(f.service.runDetailContext(t.Context()), "SELECT sequence,replay_floor FROM native_board_sequences WHERE organization_id=? AND project_id=?", scope.organization, scope.project).Scan(&sequenceAfter, &floorAfter); err != nil {
					t.Fatal(err)
				}
				if sequenceAfter != sequenceBefore || floorAfter != floorBefore {
					t.Fatalf("failed approval reset board replay: (%d,%d) -> (%d,%d)", sequenceBefore, floorBefore, sequenceAfter, floorAfter)
				}
			}
			wantPolicy := previous.ID
			if succeeded && test.mutation == "" {
				wantPolicy = next.ID
			}
			approval, _, err := readProjectPolicy(f.service.runDetailContext(t.Context()), f.service.database.db, string(scope.organization)+"/"+string(scope.project))
			if err != nil || approval.Policy.ID != wantPolicy {
				t.Fatalf("configuration = %+v, error = %v, want %s", approval, err, wantPolicy)
			}

			if test.mutation == "review" {
				rules, err := readChangePolicy(f.service.runDetailContext(t.Context()), f.service.database.db, scope)
				wantRules := defaultChangeReviewPolicy(previous).ID
				if succeeded {
					wantRules = nextRules.ID
				}
				if err != nil || rules.ID != wantRules {
					t.Fatalf("review policy = %+v, error = %v, want %s", rules, err, wantRules)
				}
			}
			if test.barrier {
				tx, err := f.service.database.db.BeginTx(f.service.runDetailContext(t.Context()), nil)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback()
				var id tracker.WorkItemID
				if err := tx.QueryRowContext(f.service.runDetailContext(t.Context()), "SELECT id FROM issues WHERE native_id = ?", current.issue.WorkItemID).Scan(&id); err != nil {
					t.Fatal(err)
				}
				ready, evaluated, _, err := nativeLandingCandidateReady(f.service.runDetailContext(t.Context()), tx, &scope, id, lease.MachineID, 0, f.service.config.now())
				if err != nil || ready || !evaluated {
					t.Fatalf("stale landing eligibility = %t, evaluated = %t, error = %v", ready, evaluated, err)
				}
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
			}
			for i, change := range changes {
				issue, _, err := readNativeIssue(f.service.runDetailContext(t.Context()), f.service.database.db, scope, string(change.issue.WorkItemID))
				if err != nil {
					t.Fatal(err)
				}
				want := "Merging"
				if i > test.stale {
					want = "Blocked"
				} else if succeeded && test.mutation == "review" && i > 0 {
					want = test.target
				}
				if issue.State != want {
					t.Errorf("%s state = %s, want %s", issue.Title, issue.State, want)
				}
				if (test.mutation != "review" || i == 0 || i > test.stale || !succeeded) && !reflect.DeepEqual(issue, issuesBefore[i]) {
					t.Errorf("%s approval changed unrelated or held work", issue.Title)
				}
				if i > test.stale {
					prior := heldAttempts[i-test.stale-1]
					attempt, err := readNativeAttempt(f.service.runDetailContext(t.Context()), f.service.database.db, scope, string(issue.WorkItemID), prior.AttemptID, f.service.config.now())
					if err != nil || !reflect.DeepEqual(attempt.Disposition, prior.Disposition) {
						t.Errorf("%s hold changed: %+v, error = %v", issue.Title, attempt.Disposition, err)
					}
				}
				after := change.detail(t)
				if !reflect.DeepEqual(after.Change, before[i].Change) || !reflect.DeepEqual(after.Versions, before[i].Versions) {
					t.Errorf("%s immutable change or version changed", issue.Title)
				}
				var transitions int
				if err := f.service.database.db.QueryRowContext(f.service.runDetailContext(t.Context()), "SELECT count(*) FROM collaboration_events WHERE work_item_id = ? AND type = 'workflow.transitioned' AND json_extract(data_json, '$.reason') = 'stale_policy'", issue.WorkItemID).Scan(&transitions); err != nil {
					t.Fatal(err)
				}
				wantTransitions := 0
				if i <= test.stale && succeeded && test.mutation == "review" && i > 0 {
					wantTransitions = 1
				}
				if transitions != wantTransitions {
					t.Errorf("%s stale_policy transitions = %d, want %d", issue.Title, transitions, wantTransitions)
				}
			}
			pinned, err := f.service.database.leasePolicyID(f.service.runDetailContext(t.Context()), lease.ID)
			if err != nil || pinned != previous.ID {
				t.Fatalf("active lease policy = %s, error = %v", pinned, err)
			}
			wantRenew := http.StatusOK
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/renew", worker, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, TTLSeconds: 90}), wantRenew)
		})
	}
}

func TestProjectPolicyIsolationAndRestart(t *testing.T) {
	t.Parallel()
	cfg := Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db")}
	service := openTestService(t, cfg)
	a := newNativeFixture(t, service, "", "human")
	b := newNativeFixture(t, service, a.project.OrganizationID, "automatic")
	human := hubTestPolicy()
	human.Gates.Kind, human.Gates.AutomatedReview = "human_review", ""
	human.Workflow = &policy.Workflow{Source: "WORKFLOW.md", States: append(a.project.States, tracker.NativeState{Name: "Plan Review"})}
	human = human.WithID()
	automatic := hubTestPolicy()
	automatic.Gates.AutoPromote, automatic.Gates.MergeMethod = true, "rebase"
	automatic.Workflow = &policy.Workflow{Source: "detent.yaml", States: append(b.project.States, tracker.NativeState{Name: "Repair", Dispatchable: true})}
	automatic = automatic.WithID()
	approveHubTestPolicy(t, service, a.base+"/policy", human)
	approveHubTestPolicy(t, service, b.base+"/policy", automatic)
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodGet, b.base+"/policy", a.token, nil), http.StatusNotFound)
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	service = openTestService(t, cfg)
	for _, test := range []struct {
		name, path, token string
		want              policy.Descriptor
	}{
		{"human", a.base, a.token, human}, {"automatic", b.base, b.token, automatic},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := performHubAPIRequest(t, service, http.MethodGet, test.path+"/policy", test.token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var approval policy.Approval
			decodeHubResponse(t, response, &approval)
			if err := approval.Policy.Match(test.want); err != nil {
				t.Fatal(err)
			}
			if approval.ApprovedBy == "" || approval.ApprovedAt == "" {
				t.Fatalf("provenance missing: %#v", approval)
			}
			response = performHubAPIRequest(t, service, http.MethodGet, test.path, test.token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var project tracker.NativeProject
			decodeHubResponse(t, response, &project)
			if !reflect.DeepEqual(project.States, test.want.Workflow.States) {
				t.Fatalf("reopened project workflow = %#v", project.States)
			}
		})
	}
}

func TestProjectPolicyConcurrentApproval(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t, nil, "", "concurrent-policy")
	initial := hubTestPolicy()
	approveHubTestPolicy(t, f.service, f.base+"/policy", initial)
	var wait sync.WaitGroup
	statuses := make(chan int, 2)
	for _, method := range []string{"merge", "rebase"} {
		wait.Go(func() {
			proposal := initial
			proposal.Gates.MergeMethod = method
			proposal = proposal.WithID()
			statuses <- performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/policy", testHubAdminToken, policy.Change{ExpectedID: initial.ID, Policy: proposal}).Code
		})
	}
	wait.Wait()
	close(statuses)
	winners := 0
	for status := range statuses {
		if status == http.StatusOK {
			winners++
		} else if status != http.StatusConflict {
			t.Fatalf("unexpected status %d", status)
		}
	}
	if winners != 1 {
		t.Fatalf("approval winners = %d, want 1", winners)
	}
}

func TestPolicyRequirementsNeverEnrollOrGrantCapabilities(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name         string
		requirements policy.Requirements
	}{
		{"privileged tag", policy.Requirements{RequiredTags: []string{"production"}}},
		{"runner selector", policy.Requirements{RunnerID: "runner_privileged"}},
		{"machine selector", policy.Requirements{MachineID: "machine_other"}},
		{"forged matching machine", policy.Requirements{MachineID: "machine_abc"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newNativeFixture(t, nil, "", "requirements")
			worker := f.worker(t, "worker")
			f.create(t, "work")
			descriptor := hubTestPolicy()
			descriptor.Requirements = test.requirements
			descriptor = descriptor.WithID()
			approveHubTestPolicy(t, f.service, f.base+"/policy", descriptor)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/register", worker, map[string]any{"id": "machine_abc", "hostname": "runner", "capacity": 1, "version": "test"}), http.StatusOK)
			claim := tracker.NativeClaim{PolicyID: descriptor.ID, MachineID: "machine_abc", SessionID: "session", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}}
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", worker, claim)
			requireNativeStatus(t, response, http.StatusConflict)
			if !strings.Contains(response.Body.String(), "selector_no_match") {
				t.Fatal(response.Body.String())
			}
			var count int
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM machines").Scan(&count); err != nil || count != 1 {
				t.Fatalf("requirements enrolled a machine: %d %v", count, err)
			}
		})
	}
}

// TestProjectPolicyApprovalSeedsChangeReviewPolicy checks what approving a
// repository policy means for a native project's review expectation: a project
// with none gets the default, which asks for a person only under a
// human-review gate; the default follows the descriptor when it is approved
// again; an administrator's policy that pins checks is left to go stale; and a
// descriptor the default cannot satisfy seeds nothing.
func TestProjectPolicyApprovalSeedsChangeReviewPolicy(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t, nil, "", "seeded")
	rulesPath := f.base + "/change-review-policy"
	read := func(t *testing.T, status int) tracker.ChangeReviewPolicy {
		t.Helper()
		response := performHubAPIRequest(t, f.service, http.MethodGet, rulesPath, f.token, nil)
		requireNativeStatus(t, response, status)
		var rules tracker.ChangeReviewPolicy
		if status == http.StatusOK {
			decodeHubResponse(t, response, &rules)
		}
		return rules
	}
	read(t, http.StatusOK)
	first := hubTestPolicy()
	approveHubTestPolicy(t, f.service, f.base+"/policy", first)
	seeded := read(t, http.StatusOK)
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		stored, err := r.ReviewPolicy(t.Context(), f.project.ID)
		if err != nil {
			return err
		}
		if stored.Revision != 1 || stored.PolicyRevisionID != first.ID {
			t.Fatalf("seeded review revision=%+v", stored)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if first.Gates.Kind == "human_review" {
		t.Fatal("the fixture descriptor must not ask for human review")
	}
	if seeded.PolicyID != first.ID || seeded.RequireReview || len(seeded.RequiredChecks) != 0 || seeded.ID != changerequest.PolicyID(seeded) {
		t.Fatalf("seeded review policy = %#v", seeded)
	}
	second := first
	second.ConfigDigest = policy.Digest([]byte("second"))
	second = second.WithID()
	response := performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/policy", testHubAdminToken, policy.Change{ExpectedID: first.ID, Policy: second})
	requireNativeStatus(t, response, http.StatusOK)
	if followed := read(t, http.StatusOK); followed.PolicyID != second.ID || followed.RequireReview || len(followed.RequiredChecks) != 0 {
		t.Fatalf("default review policy did not follow the descriptor: %#v", followed)
	}
	var principal string
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		credential, err := r.CredentialByHash(t.Context(), apikey.HashToken(f.token))
		principal = credential.ID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	custom := tracker.ChangeReviewPolicy{PolicyID: second.ID, RequireReview: true, RequiredChecks: []tracker.ChangeCheckSpec{{Name: "test", PrincipalID: principal, WorkflowID: "ci.yml", WorkflowSHA256: policy.Digest([]byte("trusted CI")), Source: "independent", MaxAgeSeconds: 3600}}}
	response = performHubAPIRequest(t, f.service, http.MethodPut, rulesPath, testHubAdminToken, tracker.ApproveChangeReviewPolicy{Mutation: tracker.Mutation{IdempotencyKey: "custom"}, ExpectedID: read(t, http.StatusOK).ID, Policy: custom})
	requireNativeStatus(t, response, http.StatusOK)
	third := second
	third.ConfigDigest = policy.Digest([]byte("third"))
	third = third.WithID()
	response = performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/policy", testHubAdminToken, policy.Change{ExpectedID: second.ID, Policy: third})
	requireNativeStatus(t, response, http.StatusOK)
	if kept := read(t, http.StatusOK); kept.PolicyID != second.ID || len(kept.RequiredChecks) != 1 {
		t.Fatalf("an administrator's review policy was rewritten: %#v", kept)
	}

	checked := newNativeFixture(t, nil, "", "checked")
	descriptor := hubTestPolicy()
	descriptor.Gates.RequiredChecks = 1
	descriptor = descriptor.WithID()
	approveHubTestPolicy(t, checked.service, checked.base+"/policy", descriptor)
	if err := checked.service.config.TenantRepository.ReadTenant(t.Context(), checked.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		_, err := r.ReviewPolicy(t.Context(), checked.project.ID)
		if !errors.Is(err, corestore.ErrNotFound) {
			t.Fatalf("unsatisfiable default seeded review: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	reviewed := newNativeFixture(t, nil, "", "reviewed")
	gated := hubTestPolicy()
	gated.Gates.Kind, gated.Gates.AutomatedReview = "human_review", ""
	gated.Gates.HumanReview = true
	gated = gated.WithID()
	approveHubTestPolicy(t, reviewed.service, reviewed.base+"/policy", gated)
	response = performHubAPIRequest(t, reviewed.service, http.MethodGet, reviewed.base+"/change-review-policy", reviewed.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var asked tracker.ChangeReviewPolicy
	decodeHubResponse(t, response, &asked)
	if asked.PolicyID != gated.ID || !asked.RequireReview {
		t.Fatalf("a human-review gate seeded %#v, want a review requirement", asked)
	}
	for _, humanReview := range []bool{false, true} {
		t.Run(fmt.Sprintf("validator human review=%t", humanReview), func(t *testing.T) {
			f := newNativeFixture(t, nil, "", "validator")
			descriptor := hubTestPolicy()
			descriptor.Gates.Validator, descriptor.Gates.HumanReview = true, humanReview
			descriptor = descriptor.WithID()
			approveHubTestPolicy(t, f.service, f.base+"/policy", descriptor)
			response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/change-review-policy", f.token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var rules tracker.ChangeReviewPolicy
			decodeHubResponse(t, response, &rules)
			if rules.PolicyID != descriptor.ID || rules.RequireReview != humanReview || len(rules.RequiredChecks) != 0 {
				t.Fatalf("validator default policy = %+v", rules)
			}
		})
	}

}

func seedNativeFixturePolicy(t *testing.T, s *Service, scope string) {
	t.Helper()
	seedNativeFixturePolicyContext(t.Context(), t, s, scope)
}

func seedNativeFixturePolicyContext(ctx context.Context, t *testing.T, s *Service, scope string) {
	t.Helper()
	organization, project, ok := strings.Cut(scope, "/")
	if !ok || organization == "" || project == "" {
		t.Fatalf("invalid native policy scope %q", scope)
	}
	org, projectID := tracker.OrganizationID(organization), tracker.ProjectID(project)
	err := s.config.TenantRepository.ReadTenant(ctx, org, corestore.Serving, func(r tenantstore.Reader) error {
		_, err := r.ProjectPolicy(ctx, projectID)
		return err
	})
	if err == nil {
		return
	}
	if !errors.Is(err, corestore.ErrNotFound) {
		t.Fatal(err)
	}
	revision, err := eventstore.NewContent(s.runDetailStore()).PrepareProjectPolicy(ctx, org, hubTestPolicy())
	if err != nil {
		t.Fatal(err)
	}
	revision.ProjectID = projectID
	revision.ApprovedBy = "fixture"
	revision.ApprovedAt = s.config.now()
	if err := s.config.TenantRepository.WriteTenant(ctx, org, corestore.Serving, func(w tenantstore.Writer) error {
		return w.ApproveProjectPolicy(ctx, revision, "")
	}); err != nil {
		t.Fatal(err)
	}
}

func clearNativeFixturePolicy(t *testing.T, f nativeFixture) {
	t.Helper()
	scope := string(f.project.OrganizationID) + "/" + string(f.project.ID)
	if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM project_policies WHERE scope=?", scope); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM policy_revisions WHERE scope=? AND approved_by='fixture'", scope); err != nil {
		t.Fatal(err)
	}
}

func TestApprovedPolicyClaimExposure(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{config.BillingModeMetered, config.BillingModeSubscription} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			f := newDefaultNativeFixture(t, Config{})
			cfg := config.Default()
			cfg.Tracker.Kind = config.TrackerHubNative
			cfg.Budget.BillingMode = mode
			cfg.Plan.Enabled = false
			cfg.Gate.SecurityAudit.Enabled = false
			cfg.Gate.Validator.Enabled = true
			cfg.Runners = config.Runners{Profile: "approved", Profiles: map[string]policy.Requirements{"approved": {RequiredTags: []string{"linux"}}}}
			descriptor, err := config.ResolvePolicy(config.Workflow{Config: cfg})
			if err != nil {
				t.Fatal(err)
			}
			var behavior map[string]json.RawMessage
			if err := json.Unmarshal(descriptor.Configuration.Behavior, &behavior); err != nil {
				t.Fatal(err)
			}
			behavior["RetiredBehavior"] = json.RawMessage(`{"Enabled":true}`)
			delete(behavior, "Gate")
			descriptor.Configuration.Behavior, err = json.Marshal(behavior)
			if err != nil {
				t.Fatal(err)
			}
			descriptor.SourceRevision = strings.Repeat("b", 64)
			descriptor = descriptor.WithID()
			raw, err := json.Marshal(descriptor)
			if err != nil {
				t.Fatal(err)
			}
			db := f.service.database.db
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
			policyScope := string(scope.organization) + "/" + string(scope.project)
			if _, err := db.ExecContext(t.Context(), "INSERT INTO policy_revisions (scope,policy_id,metadata_json,approved_by,approved_at) VALUES (?,?,?,'fixture',?)", policyScope, descriptor.ID, string(raw), formatHubTime(f.service.config.now())); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(t.Context(), "UPDATE project_policies SET policy_id=? WHERE scope=?", descriptor.ID, policyScope); err != nil {
				t.Fatal(err)
			}
			r := placementFixtureRunner(t, f, "local-runner", "local", false, 1, "gpt-6.1-sol")
			exposure, err := monthlyClaimExposure(t.Context(), db, scope, r.binding.MachineID, false)
			if err != nil || exposure.SpriteInfrastructure || exposure.RunnerAPI != (mode == config.BillingModeMetered) {
				t.Fatalf("claim exposure = %+v, error = %v", exposure, err)
			}
			contract, err := nativeIssueContractConfig(t.Context(), db, scope)
			if err != nil || contract.Budget.EffectiveBillingMode() != mode || contract.Plan.Enabled || contract.Gate.SecurityAudit.Enabled != descriptor.Gates.SecurityAudit || contract.Gate.Validator.Enabled != descriptor.Gates.Validator {
				t.Fatalf("issue contract configuration = %+v, error = %v", contract, err)
			}
			snapshot, err := readPlacementSnapshotWithCapacity(t.Context(), db, scope, f.service.config.now(), nil, newPlacementCapacity(), make(map[string]bool))
			if err != nil || snapshot.resolver == nil || !reflect.DeepEqual(snapshot.Requirements, policy.Requirements{RequiredTags: []string{"linux"}}) || strings.Contains(strings.ToLower(snapshot.Reason), "approval") {
				t.Fatalf("placement = %+v, error = %v", snapshot, err)
			}
			approved, found, err := readProjectPolicy(t.Context(), db, policyScope)
			if err != nil || !found || !reflect.DeepEqual(approved.Policy, descriptor) {
				t.Fatalf("stored policy changed: %+v, error = %v", approved, err)
			}
		})
	}
}

func TestNativePolicySubmissionConsistency(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		change func(*policy.Descriptor)
	}{
		{"gates", func(d *policy.Descriptor) { d.Gates.SecurityAudit = !d.Gates.SecurityAudit }},
		{"validator", func(d *policy.Descriptor) { d.Gates.Validator = !d.Gates.Validator }},
		{"planning", func(d *policy.Descriptor) { d.Gates.PlanEnabled = !d.Gates.PlanEnabled }},
		{"requirements", func(d *policy.Descriptor) { d.Requirements.RequiredTags = []string{"privileged"} }},
		{"profile", func(d *policy.Descriptor) { d.Profile = "privileged" }},
		{"revision", func(d *policy.Descriptor) { d.SourceRevision = strings.Repeat("b", 64) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newDefaultNativeFixture(t, Config{})
			path := string(f.project.OrganizationID) + "/" + string(f.project.ID)
			before, found, err := readProjectPolicy(t.Context(), f.service.database.db, path)
			if err != nil || !found {
				t.Fatal(err)
			}
			cfg := config.Default()
			cfg.Tracker.Kind = config.TrackerHubNative
			descriptor, err := config.ResolvePolicy(config.Workflow{Config: cfg})
			if err != nil {
				t.Fatal(err)
			}
			test.change(&descriptor)
			descriptor = descriptor.WithID()
			if err := descriptor.Validate(); err != nil {
				t.Fatal(err)
			}
			for _, submission := range []struct {
				method, path string
				body         any
			}{
				{http.MethodPut, f.base + "/policy", policy.Change{ExpectedID: before.Policy.ID, Policy: descriptor}},
				{http.MethodPost, f.base + "/policy/observed", policy.Observation{Descriptor: descriptor}},
			} {
				response := performHubAPIRequest(t, f.service, submission.method, submission.path, testHubAdminToken, submission.body)
				requireNativeStatus(t, response, http.StatusUnprocessableEntity)
				if !strings.Contains(response.Body.String(), "policy_mismatch") {
					t.Fatalf("submission refused for unrelated reason: %s", response.Body.String())
				}
			}
			after, found, err := readProjectPolicy(t.Context(), f.service.database.db, path)
			if err != nil || !found || !reflect.DeepEqual(before, after) {
				t.Fatalf("inconsistent submission changed approval: %+v, error = %v", after, err)
			}
			var observations int
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM project_observed_policies WHERE scope=?", path).Scan(&observations); err != nil {
				t.Fatal(err)
			}
			if observations != 0 {
				t.Fatalf("inconsistent observations stored: %d", observations)
			}
		})
	}
}

func TestProjectPolicyCache(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		count      int
		otherScope bool
		otherDB    bool
		mutate     bool
	}{
		{name: "repeated authored policy decodes once", count: 1},
		{name: "two policies in two projects stay distinct", count: 2, otherScope: true},
		{name: "same policy in two projects stays scoped", count: 1, otherScope: true},
		{name: "same policy in two databases stays scoped", count: 1, otherDB: true},
		{name: "returned maps and slices are isolated", count: 1, mutate: true},
		{name: "oldest entry evicted at fixed bound", count: projectPolicyCacheLimit + 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			workflow, err := config.ParseProjectDefinition(config.ProjectDefinitionSources{
				ConfigPath: "detent.yaml", HasConfig: true,
				Config:       []byte("schema: 1\ntracker:\n  kind: hub_native\n  repository: example-org/example-project\ngate:\n  run: true\n  required_status_checks: []\n"),
				WorkflowPath: "WORKFLOW.md", Workflow: []byte("Implement the assigned issue.\n"),
			})
			if err != nil {
				t.Fatal(err)
			}
			descriptor, err := config.ResolvePolicy(workflow)
			if err != nil {
				t.Fatal(err)
			}
			decodes := 0
			decode := func(raw string) (*config.ResolvedNativePolicy, error) {
				decodes++
				return decodeResolvedProjectPolicy(raw)
			}
			db := &database{policies: projectPolicyCache{decode: decode}}
			ctx := db.policyContext(t.Context())
			scope := "organization/project"
			read := func(ctx context.Context, scope string, descriptor policy.Descriptor) policy.Descriptor {
				t.Helper()
				raw, err := json.Marshal(descriptor)
				if err != nil {
					t.Fatal(err)
				}
				approval, err := decodeProjectPolicy(ctx, scope, descriptor.ID, string(raw), policy.Approval{})
				if err != nil {
					t.Fatal(err)
				}
				return approval.Policy
			}
			first := read(ctx, scope, descriptor)
			applied, err := config.ApplyNativePolicy(config.Workflow{Config: config.Default()}, first)
			if err != nil {
				t.Fatal(err)
			}
			cached, err := projectPolicyWorkflow(ctx, scope, first)
			if err != nil || !reflect.DeepEqual(cached, applied) {
				t.Fatalf("cached authored workflow differs from application: %v", err)
			}
			if test.mutate {
				first.Authored.Files["detent.yaml"] = "changed"
				first.Configuration.Behavior[0] = '!'
				first.Workflow.States[0].Transitions = []string{"changed"}
				cached.Authored.Files["WORKFLOW.md"] = "changed"
				cached.DefinitionSources.Config[0] = '!'
				cached.Config.Tracker.ActiveStates[0] = "changed"
				cached.Config.Tracker.PriorityMap.Map["changed"] = 1
				*cached.Config.Worker.AllowLocalBinding = true
				cached.Config = cached.Config.WithWorkerDefaults(config.WorkerDefaults{AllowLocalBinding: new(false)})
				if !cached.Config.Worker.EffectiveAllowLocalBinding() {
					t.Fatal("copied workflow lost its project worker setting")
				}
			}
			for i := 1; i < test.count; i++ {
				if i == projectPolicyCacheLimit {
					read(ctx, scope, descriptor)
				}
				other := hubTestPolicy()
				other.SourceRevision = fmt.Sprintf("%040x", i)
				other = other.WithID()
				otherScope := scope
				if test.otherScope {
					otherScope = "organization/other-project"
				}
				if got := read(ctx, otherScope, other); !reflect.DeepEqual(got, other) {
					t.Fatal("policy identities mixed")
				}
			}
			if test.otherScope && test.count == 1 {
				read(ctx, "organization/other-project", descriptor)
			}
			if test.otherDB {
				other := &database{policies: projectPolicyCache{decode: decode}}
				read(other.policyContext(t.Context()), scope, descriptor)
			}
			if test.count > projectPolicyCacheLimit {
				if len(db.policies.entries) != projectPolicyCacheLimit {
					t.Fatalf("cache entries = %d", len(db.policies.entries))
				}
				if _, found := db.policies.entries[projectPolicyCacheKey{scope: scope, id: descriptor.ID}]; found {
					t.Fatal("oldest policy retained past bound")
				}
			}
			if got := read(ctx, scope, descriptor); !reflect.DeepEqual(got, descriptor) {
				t.Fatal("cached descriptor changed")
			}
			again, err := projectPolicyWorkflow(ctx, scope, descriptor)
			if err != nil || !reflect.DeepEqual(again, applied) {
				t.Fatalf("cached workflow changed after read: %v", err)
			}
			want := test.count
			if test.otherDB || test.otherScope && test.count == 1 || test.count > projectPolicyCacheLimit {
				want++
			}
			if decodes != want {
				t.Fatalf("policy decodes = %d, want %d", decodes, want)
			}
		})
	}
}

func TestProjectPolicyCacheConcurrentReads(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		otherKey string
		fail     bool
		panic    bool
		evict    bool
	}{
		{name: "same key shares one decode and independent results"},
		{name: "unrelated warm hit completes during decode", otherKey: "warm"},
		{name: "unrelated cold miss completes during decode", otherKey: "cold"},
		{name: "failed decode reaches waiters and later retries", fail: true},
		{name: "panicked decode releases waiters and later retries", panic: true},
		{name: "eviction during decode preserves cache entries", evict: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				descriptor := hubTestPolicy()
				descriptor.Requirements.RequiredTags = []string{"example"}
				descriptor = descriptor.WithID()
				raw, err := json.Marshal(descriptor)
				if err != nil {
					t.Fatal(err)
				}
				other := hubTestPolicy()
				otherRaw, err := json.Marshal(other)
				if err != nil {
					t.Fatal(err)
				}
				started, release := make(chan struct{}), make(chan struct{})
				resume := sync.OnceFunc(func() { close(release) })
				var readers sync.WaitGroup
				db := &database{}
				t.Cleanup(func() {
					resume()
					synctest.Wait()
					if t.Failed() {
						db.policies.mu.Lock()
						for key, pending := range db.policies.inflight {
							select {
							case <-pending.done:
							default:
								pending.err = errors.New("test cleanup released pending decode")
								close(pending.done)
							}
							delete(db.policies.inflight, key)
						}
						db.policies.mu.Unlock()
					}
					readers.Wait()
				})
				var decodes atomic.Int32
				decodeErr := errors.New("example decode failure")
				const panicValue = "example private policy input"
				db.policies.decode = func(input string) (*config.ResolvedNativePolicy, error) {
					if input == string(raw) && decodes.Add(1) == 1 {
						close(started)
						<-release
						if test.panic {
							panic(panicValue)
						}
						if test.fail {
							return nil, decodeErr
						}
					}
					return decodeResolvedProjectPolicy(input)
				}
				ctx := db.policyContext(t.Context())
				scope := "organization/project"
				if test.otherKey == "warm" || test.evict {
					if _, err := decodeProjectPolicy(ctx, scope, other.ID, string(otherRaw), policy.Approval{}); err != nil {
						t.Fatal(err)
					}
				}
				type readResult struct {
					approval   policy.Approval
					err        error
					panicValue any
				}
				const count = 8
				results := make(chan readResult, count)
				read := func() {
					defer readers.Done()
					defer func() {
						if value := recover(); value != nil {
							results <- readResult{panicValue: value}
						}
					}()
					approval, err := decodeProjectPolicy(ctx, scope, descriptor.ID, string(raw), policy.Approval{})
					results <- readResult{approval: approval, err: err}
				}
				readers.Add(1)
				go read()
				<-started
				if !db.policies.mu.TryLock() {
					t.Fatal("shared cache mutex held during decode")
				}
				db.policies.mu.Unlock()
				for range count - 1 {
					readers.Add(1)
					go read()
				}
				if test.otherKey != "" {
					otherResult := make(chan readResult, 1)
					readers.Add(1)
					go func() {
						defer readers.Done()
						approval, err := decodeProjectPolicy(ctx, scope, other.ID, string(otherRaw), policy.Approval{})
						otherResult <- readResult{approval: approval, err: err}
					}()
					synctest.Wait()
					select {
					case result := <-otherResult:
						if result.err != nil || !reflect.DeepEqual(result.approval.Policy, other) {
							t.Fatalf("unrelated read = %+v, %v", result.approval, result.err)
						}
					default:
						t.Fatal("unrelated read blocked on paused decode")
					}
				} else {
					synctest.Wait()
				}
				if got := decodes.Load(); got != 1 {
					t.Fatalf("paused decodes = %d, want 1", got)
				}
				if test.evict {
					for i := range projectPolicyCacheLimit {
						if _, err := decodeProjectPolicy(ctx, fmt.Sprintf("replacement/%d", i), other.ID, string(otherRaw), policy.Approval{}); err != nil {
							t.Fatal(err)
						}
					}
					if _, ok := db.policies.entries[projectPolicyCacheKey{scope: scope, id: other.ID}]; ok {
						t.Fatal("warm entry survived eviction during decode")
					}
				}
				resume()
				synctest.Wait()
				values := make([]policy.Descriptor, 0, count)
				panics := 0
				for range count {
					var result readResult
					select {
					case result = <-results:
					default:
						t.Fatal("reader remained blocked after decode completed")
					}
					if result.panicValue != nil {
						if !test.panic || result.panicValue != panicValue {
							t.Fatalf("unexpected decoder panic: %v", result.panicValue)
						}
						panics++
					} else if test.panic {
						if result.err == nil || result.err.Error() != "decode project policy panicked" || !reflect.DeepEqual(result.approval, policy.Approval{}) {
							t.Fatalf("panic waiter = %+v, %v", result.approval, result.err)
						}
					} else if test.fail {
						if !errors.Is(result.err, decodeErr) {
							t.Fatalf("shared decode error = %v, want %v", result.err, decodeErr)
						}
					} else {
						if result.err != nil || !reflect.DeepEqual(result.approval.Policy, descriptor) {
							t.Fatalf("concurrent read = %+v, %v", result.approval, result.err)
						}
						values = append(values, result.approval.Policy)
					}
				}
				readers.Wait()
				if test.panic && panics != 1 {
					t.Fatalf("decoder panics = %d, want 1", panics)
				}
				if !test.fail && !test.panic {
					values[0].Requirements.RequiredTags[0] = "changed"
					for _, value := range values[1:] {
						if !reflect.DeepEqual(value, descriptor) {
							t.Fatal("concurrent results share mutable values")
						}
					}
				}
				if test.evict {
					if len(db.policies.entries) != projectPolicyCacheLimit || len(db.policies.inflight) != 0 {
						t.Fatal("completed decode violated cache bound or retained pending entry")
					}
					if _, ok := db.policies.entries[projectPolicyCacheKey{scope: scope, id: other.ID}]; ok {
						t.Fatal("completed decode restored an evicted entry")
					}
					if _, ok := db.policies.entries[projectPolicyCacheKey{scope: "replacement/0", id: other.ID}]; ok {
						t.Fatal("completed decode did not evict the oldest replacement")
					}
					for i := 1; i < projectPolicyCacheLimit; i++ {
						if _, ok := db.policies.entries[projectPolicyCacheKey{scope: fmt.Sprintf("replacement/%d", i), id: other.ID}]; !ok {
							t.Fatal("completed decode evicted a newer replacement")
						}
					}
					seen := make(map[projectPolicyCacheKey]bool)
					for _, key := range db.policies.order {
						if _, ok := db.policies.entries[key]; !ok || seen[key] {
							t.Fatal("eviction left stale or duplicate cache ordering")
						}
						seen[key] = true
					}
					if len(seen) != len(db.policies.entries) {
						t.Fatal("completed decode published an orphaned cache entry")
					}
				}
				for range 2 {
					approval, err := decodeProjectPolicy(ctx, scope, descriptor.ID, string(raw), policy.Approval{})
					if err != nil || !reflect.DeepEqual(approval.Policy, descriptor) {
						t.Fatalf("later read = %+v, %v", approval, err)
					}
				}
				want := int32(1)
				if test.fail || test.panic {
					want++
				}
				if got := decodes.Load(); got != want {
					t.Fatalf("decodes = %d, want %d", got, want)
				}
			})
		})
	}
}

func TestProjectPolicyReadsWithoutApproval(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		approved bool
		large    bool
		warm     bool
	}{
		{name: "no approval"},
		{name: "approved", approved: true},
		{name: "cached approval metadata", approved: true, warm: true},
		{name: "cached revoked approval", warm: true},
		{name: "no approval with large observation", large: true},
		{name: "approved with large observation", approved: true, large: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			approved := test.approved
			t.Parallel()
			f := newNativeFixture(t, nil, "", "policy-read")
			scope := string(f.project.OrganizationID) + "/" + string(f.project.ID)
			if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM project_policies WHERE scope=?", scope); err != nil {
				t.Fatal(err)
			}
			descriptor := hubTestPolicy()
			ctx := f.service.database.policyContext(t.Context())
			if approved || test.warm {
				approveHubTestPolicy(t, f.service, f.base+"/policy", descriptor)
			}
			if test.warm {
				if _, err := requireProjectPolicy(ctx, f.service.database.reader, scope); err != nil {
					t.Fatal(err)
				}
				if approved {
					if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE policy_revisions SET approved_by='updated-approver', approved_at='2026-10-27T12:00:00Z' WHERE scope=? AND policy_id=?", scope, descriptor.ID); err != nil {
						t.Fatal(err)
					}
					current, err := requireProjectPolicy(ctx, f.service.database.reader, scope)
					if err != nil || current.ApprovedBy != "updated-approver" || current.ApprovedAt != "2026-10-27T12:00:00Z" {
						t.Fatalf("cached approval metadata: %+v, %v", current, err)
					}
				} else if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM project_policies WHERE scope=?", scope); err != nil {
					t.Fatal(err)
				}
			}
			candidate := descriptor
			candidate.Gates.AutoPromote = true
			if test.large {
				workflow, err := config.ParseProjectDefinition(config.ProjectDefinitionSources{
					ConfigPath: "detent.yaml", HasConfig: true,
					Config:       []byte("schema: 1\ntracker:\n  kind: hub_native\n  repository: example-org/example-project\ngate:\n  run: true\n  required_status_checks: []\n"),
					WorkflowPath: "WORKFLOW.md", Workflow: []byte(strings.Repeat("Implement the assigned issue.\n", operatortool.MaxResultBytes/28)),
				})
				if err != nil {
					t.Fatal(err)
				}
				candidate, err = config.ResolvePolicy(workflow)
				if err != nil {
					t.Fatal(err)
				}
			}
			candidate = candidate.WithID()
			worker := f.worker(t, "policy-reader")
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/policy/observed", worker, policy.Observation{Descriptor: candidate}), http.StatusNoContent)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/register", worker, map[string]any{"id": "policy_read_machine", "hostname": "runner", "capacity": 1, "version": "test"}), http.StatusOK)
			_, err := monthlyClaimExposure(ctx, f.service.database.reader, nativeScope{organization: f.project.OrganizationID, project: f.project.ID}, "policy_read_machine", false)
			if approved && err != nil {
				t.Fatal(err)
			}
			if !approved {
				var refusal *nativeError
				if !errors.As(err, &refusal) || refusal.Code != "policy_not_approved" {
					t.Fatalf("unapproved claim exposure: %v", err)
				}
			}
			response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/policy", f.token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var rest struct {
				Policy     *policy.Descriptor      `json:"policy"`
				ApprovedBy string                  `json:"approved_by"`
				ApprovedAt string                  `json:"approved_at"`
				Observed   []policy.ObservedPolicy `json:"observed_policies"`
				History    []policy.WorkflowApply  `json:"history"`
			}
			decodeHubResponse(t, response, &rest)
			if approved {
				if rest.Policy == nil || !reflect.DeepEqual(*rest.Policy, descriptor) || rest.ApprovedBy == "" || rest.ApprovedAt == "" {
					t.Fatalf("approved payload changed: %s", response.Body)
				}
			} else if rest.Policy != nil || !strings.Contains(response.Body.String(), `"policy":null`) || rest.ApprovedBy != "" || rest.ApprovedAt != "" || len(rest.History) != 0 {
				t.Fatalf("missing approval: %s", response.Body)
			}
			if len(rest.Observed) != 1 || !reflect.DeepEqual(rest.Observed[0].Policy, candidate) {
				t.Fatalf("observed candidates: %s", response.Body)
			}
			captured := make(chan context.Context, 1)
			f.service.echo.GET("/api/v2/organizations/:organization/policy-read-test", func(c echo.Context) error { captured <- c.Request().Context(); return c.NoContent(http.StatusOK) }, f.service.operatorAuthority)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, "/api/v2/organizations/"+string(f.project.OrganizationID)+"/policy-read-test", testHubAdminToken, nil), http.StatusOK)
			ctx = operatortool.BindConnection(<-captured, "policy-read", "policy fixture")
			executor := hubProjectExecutor{f.service}
			if err := executor.OpenConnection(ctx); err != nil {
				t.Fatal(err)
			}
			for _, transport := range []string{"stdio", "http"} {
				call := hostedContextProtocol(t, f.service, ctx, transport)
				reply := call("tools/call", "get_project_policy", map[string]any{"project_id": string(f.project.ID)})
				var result struct {
					IsError    bool `json:"isError"`
					Structured struct {
						Data json.RawMessage `json:"data"`
					} `json:"structuredContent"`
				}
				if err := json.Unmarshal(reply.Result, &result); err != nil {
					t.Fatal(err)
				}
				if test.large {
					var data projectPolicyRead
					if result.IsError || json.Unmarshal(result.Structured.Data, &data) != nil || !data.ObservedPolicyDetailsOmitted || len(data.ObservedPolicies) != 1 || data.ObservedPolicies[0].Policy.ID != candidate.ID || len(result.Structured.Data) > operatortool.MaxResultBytes {
						t.Fatalf("%s large policy read failed: %s", transport, reply.Result)
					}
					if !reflect.DeepEqual(data.Policy, rest.Policy) || !reflect.DeepEqual(data.History, rest.History) {
						t.Fatalf("%s large observation changed approval or history", transport)
					}
					continue
				}
				var data any
				var expected any
				if result.IsError || json.Unmarshal(result.Structured.Data, &data) != nil || json.Unmarshal(response.Body.Bytes(), &expected) != nil || !reflect.DeepEqual(data, expected) {
					t.Fatalf("%s policy differs from REST: %s; REST %s", transport, reply.Result, response.Body)
				}
			}
		})
	}
}

func TestPostgresObservedPolicyPreservesHubTriageAndOccupiedWorkflow(t *testing.T) {
	repository := projectRecordPostgres(t)
	service := openPostgresHubTestService(t, repository, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), RunDetails: &memoryRunDetails{}})
	descriptor := func(instructions, revision string, legacy bool) policy.Descriptor {
		t.Helper()
		configuration := "schema: 1\ntracker:\n  kind: hub_native\n  repository: example-org/example-project\n  lanes:\n    - {name: Backlog, role: holding}\n    - {name: Todo, role: active}\n    - {name: In Progress, role: active}\n    - {name: Done, role: terminal}\n    - {name: Blocked, role: holding}\n"
		if legacy {
			configuration += "    - {name: Legacy, role: holding}\n"
		}
		configuration += "gate:\n  run: true\n  required_status_checks: []\nserver:\n  kanban:\n    allowed_transitions:\n      Todo: [In Progress, Done]\n      In Progress: [Todo, Done]\n      Done: [Todo]\n"
		if legacy {
			configuration += "      Legacy: [Todo]\n"
		}
		definition, err := config.ParseProjectDefinition(config.ProjectDefinitionSources{ConfigPath: "detent.yaml", HasConfig: true, Config: []byte(configuration), WorkflowPath: "WORKFLOW.md", Workflow: []byte(instructions)})
		if err != nil {
			t.Fatal(err)
		}
		result, err := config.ResolvePolicy(definition)
		if err != nil {
			t.Fatal(err)
		}
		result.Workflow.Revision = revision
		return result.WithID()
	}
	original := descriptor("Implement the assigned issue.\n", strings.Repeat("a", 40), true)
	candidate := descriptor("Implement the changed assigned issue.\n", strings.Repeat("b", 40), false)
	for orgIndex := range 2 {
		organization, err := service.createNativeOrganizationFor(t.Context(), fmt.Sprintf("Example organization %d", orgIndex))
		if err != nil {
			t.Fatal(err)
		}
		seedPostgresHubAdmin(t, repository, service, organization)
		for _, test := range []struct {
			name, occupied string
			archived       bool
		}{
			{name: "empty triage"}, {name: "occupied triage", occupied: "Triage"},
			{name: "occupied removed lane", occupied: "Legacy"}, {name: "archived removed lane", occupied: "Legacy", archived: true},
		} {
			t.Run(fmt.Sprintf("organization_%d/%s", orgIndex, test.name), func(t *testing.T) {
				path := "/api/v2/organizations/" + string(organization.ID) + "/projects"
				response := performHubAPIRequest(t, service, http.MethodPost, path, testHubAdminToken, map[string]any{"idempotency_key": test.name, "name": test.name, "grant_access": true, "states": original.Workflow.States})
				requireNativeStatus(t, response, http.StatusCreated)
				var project tracker.NativeProject
				decodeHubResponse(t, response, &project)
				base := path + "/" + string(project.ID)
				f := nativeFixture{service: service, project: project, base: base, token: testHubAdminToken}
				runner := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat)
				runner.enroll(t)
				response = performHubAPIRequest(t, service, http.MethodPut, base+"/policy", testHubAdminToken, policy.Change{Policy: original})
				requireNativeStatus(t, response, http.StatusOK)
				var before projectstore.Record
				err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
					record, err := w.ProjectRecord(t.Context(), project.ID)
					if err != nil {
						return err
					}
					record.Settings.Repository = "example-org/example-project"
					record.Settings.CheckoutRepository = "example-org/example-project"
					record.States = nativeTriageStates(original.Workflow.States)
					before, err = w.UpdateProjectRecord(t.Context(), record)
					if err != nil {
						return err
					}
					if test.occupied != "" {
						_, err = w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: organization.ID, ProjectID: project.ID, NativeID: tracker.NativeWorkItemID("item_" + string(project.ID)), Number: 1, Title: "Example item", WorkflowStateName: new(test.occupied), Archived: test.archived})
					}
					return err
				})
				if err != nil {
					t.Fatal(err)
				}
				source := &policy.RepositorySource{Repository: "example-org/example-project", Commit: candidate.Workflow.Revision, DefaultBranch: "develop", DefaultBranchHead: candidate.Workflow.Revision, DefaultBranchReachable: true}
				for _, observed := range []policy.Descriptor{candidate, candidate} {
					response = performHubAPIRequest(t, service, http.MethodPost, base+"/policy/observed", runner.redemption.Credential, policy.Observation{Descriptor: observed, Source: source})
					requireNativeStatus(t, response, http.StatusNoContent)
					err = repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(r tenantstore.Reader) error {
						after, err := r.ProjectRecord(t.Context(), project.ID)
						if err != nil {
							return err
						}
						if !reflect.DeepEqual(before, after) {
							t.Errorf("runner observation changed project metadata, states or transitions: before=%+v after=%+v", before, after)
						}
						return nil
					})
					if err != nil {
						t.Fatal(err)
					}
					response = performHubAPIRequest(t, service, http.MethodGet, base+"/policy", testHubAdminToken, nil)
					requireNativeStatus(t, response, http.StatusOK)
					var pending projectPolicyRead
					decodeHubResponse(t, response, &pending)
					if pending.Policy == nil || pending.Policy.ID != original.ID || len(pending.ObservedPolicies) != 1 || pending.ObservedPolicies[0].Policy.ID != candidate.ID {
						t.Fatalf("runner observation approved policy without a human: %+v", pending)
					}
				}
				response = performHubAPIRequest(t, service, http.MethodPut, base+"/policy", testHubAdminToken, policy.Change{ExpectedID: original.ID, ObservedPolicyID: candidate.ID})
				if test.occupied == "Legacy" {
					requireNativeError(t, response, http.StatusUnprocessableEntity, "invalid_request")
				} else {
					requireNativeStatus(t, response, http.StatusOK)
				}
				{
					err = repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(r tenantstore.Reader) error {
						after, err := r.ProjectRecord(t.Context(), project.ID)
						if err != nil {
							return err
						}
						review, err := r.ReviewPolicy(t.Context(), project.ID)
						if err != nil {
							return err
						}
						expectedPolicy, expectedRevision := candidate.ID, int64(2)
						if test.occupied == "Legacy" {
							expectedPolicy, expectedRevision = original.ID, 1
						}
						if review.PolicyRevisionID != expectedPolicy || review.Revision != expectedRevision {
							t.Errorf("observed review policy=%+v, want policy=%q revision=%d", review, expectedPolicy, expectedRevision)
						}
						if test.occupied == "Legacy" {
							if !reflect.DeepEqual(before, after) {
								t.Errorf("conflict changed project metadata, states or transitions: before=%+v after=%+v", before, after)
							}
						} else {
							if !reflect.DeepEqual(after.States, nativeTriageStates(candidate.Workflow.States)) {
								t.Errorf("Hub Triage was not preserved: %+v", after.States)
							}
							if after.Core.WorkflowSourceRevision != candidate.Workflow.Revision {
								t.Errorf("workflow revision = %q", after.Core.WorkflowSourceRevision)
							}
							if err := validateNativeWorkflowPolicy(after, candidate, true); err != nil {
								t.Errorf("normalized workflow refused: %v", err)
							}
							after.States[0].Dispatchable = true
							if err := validateNativeWorkflowPolicy(after, candidate, true); err == nil {
								t.Error("altered Hub Triage authorization accepted")
							}
						}
						return nil
					})
					if err != nil {
						t.Fatal(err)
					}
					response = performHubAPIRequest(t, service, http.MethodGet, base+"/policy", testHubAdminToken, nil)
					requireNativeStatus(t, response, http.StatusOK)
					var view projectPolicyRead
					decodeHubResponse(t, response, &view)
					expectedID := candidate.ID
					if test.occupied == "Legacy" {
						expectedID = original.ID
						if len(view.ObservedPolicies) != 1 || view.ObservedPolicies[0].Policy.ID != candidate.ID {
							t.Errorf("conflicting observation was not retained: %+v", view.ObservedPolicies)
						}
					}
					if view.Policy == nil || view.Policy.ID != expectedID {
						t.Errorf("approved policy = %+v, want %s", view.Policy, expectedID)
					}
				}
			})
		}
	}
}

func TestNativeWorkflowPolicyCanonicalStates(t *testing.T) {
	for _, test := range []struct {
		name     string
		change   func(*projectstore.Record)
		conflict bool
	}{
		{name: "lane and transition order"},
		{name: "triage dispatch permission", change: func(p *projectstore.Record) { p.States[len(p.States)-1].Dispatchable = true }, conflict: true},
		{name: "triage terminal flag", change: func(p *projectstore.Record) { p.States[len(p.States)-1].Terminal = true }, conflict: true},
		{name: "triage operator permission", change: func(p *projectstore.Record) { p.States[len(p.States)-1].OperatorOnly = true }, conflict: true},
		{name: "repository lane permission", change: func(p *projectstore.Record) { p.States[0].Terminal = false }, conflict: true},
		{name: "removed transition", change: func(p *projectstore.Record) { p.States[0].Transitions = nil }, conflict: true},
		{name: "missing triage", change: func(p *projectstore.Record) { p.States = p.States[:len(p.States)-1] }, conflict: true},
		{name: "workflow revision", change: func(p *projectstore.Record) { p.Core.WorkflowSourceRevision = "stale" }, conflict: true},
		{name: "workflow source", change: func(p *projectstore.Record) { p.Core.WorkflowSource = "other.yaml" }, conflict: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			descriptor := hubTestPolicy()
			descriptor.Workflow = &policy.Workflow{Source: "example-workflow.yaml", Revision: strings.Repeat("a", 40), States: nativeFixtureStates()}
			descriptor = descriptor.WithID()
			project := projectstore.Record{Core: corestore.Project{WorkflowSource: descriptor.Workflow.Source, WorkflowSourceRevision: descriptor.Workflow.Revision}, States: nativeTriageStates(descriptor.Workflow.States)}
			slices.Reverse(project.States)
			for i := range project.States {
				project.States[i].Transitions = slices.Clone(project.States[i].Transitions)
				slices.Reverse(project.States[i].Transitions)
			}
			if test.change != nil {
				test.change(&project)
			}
			before, err := json.Marshal(project)
			if err != nil {
				t.Fatal(err)
			}
			err = validateNativeWorkflowPolicy(project, descriptor, true)
			if (err != nil) != test.conflict {
				t.Fatalf("workflow validation = %v, want conflict %t", err, test.conflict)
			}
			after, err := json.Marshal(project)
			if err != nil {
				t.Fatal(err)
			}
			if string(before) != string(after) {
				t.Fatal("comparison mutated stored workflow")
			}
		})
	}
}

func TestPolicyScopeRequiresTypedRepository(t *testing.T) {
	for _, organization := range []string{"", "org_example"} {
		t.Run(organization, func(t *testing.T) {
			service := &Service{}
			c := echo.New().NewContext(nil, nil)
			c.SetRequest((&http.Request{}).WithContext(t.Context()))
			c.SetParamNames("organization", "project", "owner", "repo")
			c.SetParamValues(organization, "prj_example", "example-org", "example-project")
			if _, err := service.policyScope(c); err == nil {
				t.Fatal("policy scope accepted missing typed repository")
			}
		})
	}
}

func (s *Service) approveTestTenantPolicy(ctx context.Context, path, actor string, change policy.Change) (policy.Approval, error) {
	organization, project, ok := strings.Cut(path, "/")
	if !ok {
		return policy.Approval{}, nativeNotFound()
	}
	credential, _, err := s.authenticateTenantAPIHash(ctx, tracker.OrganizationID(organization), apikey.HashToken(testHubAdminToken), "", "")
	if err != nil {
		return policy.Approval{}, err
	}
	scope := nativeScope{organization: tracker.OrganizationID(organization), project: tracker.ProjectID(project), credential: credential, requireHostedAdmin: true}
	return s.approveTenantProjectPolicy(ctx, scope, actor, change)
}
