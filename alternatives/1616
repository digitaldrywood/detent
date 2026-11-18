package hubserver

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func seedReviewBundle(t *testing.T, f changeFixture, version tracker.ChangeVersion, expires time.Time) tracker.ChangeReviewBundle {
	t.Helper()
	ref := artifact.Reference{Scope: artifact.Scope{OrganizationID: string(f.project.OrganizationID), ProjectID: string(f.project.ID), WorkItemID: string(f.issue.WorkItemID), VersionID: version.ID, RunID: version.RunID, AttemptID: version.AttemptID}, ArtifactID: artifact.NewID("artifact"), ManifestID: artifact.NewID("manifest"), ServiceID: artifact.NewID("service"), Revision: 1, SHA256: version.Code.SHA256, Kind: "diff", State: "complete", Availability: "available", ExpiresAt: expires}
	ref.SchemaVersion, ref.ObservedAt = 1, expires.Add(-time.Hour)
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.BindArtifactService(t.Context(), f.project.ID, artifact.Binding{ServiceID: ref.ServiceID, Origin: "https://artifacts.example.test", Mode: "customer", PublisherTokenID: version.Checks[0].PrincipalID}); err != nil {
			return err
		}
		_, err := w.AppendArtifactReference(t.Context(), ref)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return tracker.ChangeReviewBundle{ArtifactID: ref.ArtifactID, Revision: ref.Revision, SHA256: ref.SHA256, HeadSHA: version.HeadSHA}
}

func publishReviewVersion(t *testing.T, f changeFixture, key, expected string) tracker.ChangeVersion {
	t.Helper()
	token := f.worker(t, "bundle-publisher-"+key)
	credential, _, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(token), "", "")
	if err != nil {
		t.Fatal(err)
	}
	input := changeTestInput()
	if expected != "" {
		input.HeadSHA = strings.Repeat("c", 40)
	}
	input.RunID, input.AttemptID = artifact.NewID("run"), artifact.NewID("attempt")
	var lease leasestore.Lease
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		item, err := w.WorkItem(t.Context(), f.project.ID, f.issue.WorkItemID)
		if err != nil {
			return err
		}
		lease, err = w.Claim(t.Context(), leasestore.Claim{IssueID: item.ID, ExpectedRevision: item.Revision, MachineID: string(credential.Runner.MachineID), SessionID: hubapi.NewID("session"), LeaseID: hubapi.NewID("lease"), TTL: time.Hour})
		if err != nil {
			return err
		}
		if err := w.AssignRunnerLease(t.Context(), runnerstore.LeaseAssignment{OrganizationID: f.project.OrganizationID, LeaseID: lease.LeaseID, RunnerID: credential.Runner.RunnerID, AssignedAt: f.service.config.Clock()}); err != nil {
			return err
		}
		return w.BindLeasePolicy(t.Context(), policystore.LeasePolicy{OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, LeaseID: tracker.LeaseID(lease.LeaseID), PolicyID: input.PolicyID, BoundAt: f.service.config.Clock()})
	}); err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()
		if err := f.service.config.TenantRepository.WriteTenant(ctx, f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
			_, err := w.Release(ctx, leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}, "completed")
			return err
		}); err != nil {
			t.Error(err)
		}
	}()
	data := tracker.NativeRunData{AttemptID: input.AttemptID, RunID: input.RunID, LeaseID: tracker.LeaseID(lease.LeaseID), FencingToken: tracker.FencingToken(lease.FencingToken), Sequence: 1, Outcome: "succeeded", PolicyID: input.PolicyID, MachineID: tracker.MachineID(lease.MachineID), SessionID: lease.SessionID, RunnerID: credential.Runner.RunnerID, Identity: &tracker.NativeExecutionIdentity{Role: "code", Backend: "codex", Model: "example-model"}}
	content := eventstore.NewContent(f.service.runDetailStore())
	payload, err := content.PreparePayload(t.Context(), f.project.OrganizationID, historyTestJSON(t, data))
	if err != nil {
		t.Fatal(err)
	}
	event, err := content.PrepareEvent(t.Context(), f.project.OrganizationID, historyTestJSON(t, tracker.CollaborationEvent{ID: hubapi.NewID("evt"), OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, AggregateID: f.issue.WorkItemID, Type: "run.finished", SchemaVersion: 1, Actor: tracker.Actor{Kind: "runner", PrincipalID: credential.ID}, RecordedAt: f.service.config.Clock(), Data: tracker.CollaborationData{Run: &data}}))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		if _, err := w.RecordAttempt(t.Context(), leasestore.AttemptEvent{ID: input.AttemptID, Fence: leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}, RunID: input.RunID, Sequence: 1, Status: "succeeded", StartedAt: f.service.config.Clock(), PayloadObjectKey: payload.Key, PayloadReference: payload}); err != nil {
			return err
		}
		_, err := w.AppendEvent(t.Context(), event)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	request := tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: key, LeaseID: tracker.LeaseID(lease.LeaseID), FencingToken: tracker.FencingToken(lease.FencingToken)}, ExpectedVersionID: expected, ChangeVersionInput: input}
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.path+"/versions", token, request)
	requireNativeStatus(t, response, http.StatusOK)
	var version tracker.ChangeVersion
	decodeHubResponse(t, response, &version)
	return version
}
