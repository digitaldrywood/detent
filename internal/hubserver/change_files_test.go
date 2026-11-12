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

func TestBoundReviewDecisions(t *testing.T) {
	t.Parallel()
	service := newPostgresNativeService(t, Config{})
	organization := tracker.OrganizationID(artifact.NewID("org"))
	seedPostgresHubAdmin(t, service.config.TenantRepository, service, nativeOrganization{ID: organization, Name: string(organization), Slug: strings.ReplaceAll(string(organization), "_", "-")})
	f := newChangeFixture(t, service, organization)
	first := publishReviewVersion(t, f, "first", "")
	bundle := seedReviewBundle(t, f, first, time.Now().Add(time.Hour))
	path := f.path + "/versions/" + first.ID + "/reviews"
	for _, tt := range []struct {
		name string
		edit func(*tracker.ReviewChange)
		want int
	}{
		{"valid", func(*tracker.ReviewChange) {}, 200},
		{"wrong head", func(r *tracker.ReviewChange) { r.Bundle.HeadSHA = strings.Repeat("c", 40) }, 422},
		{"wrong manifest", func(r *tracker.ReviewChange) { r.Bundle.SHA256 = strings.Repeat("d", 64) }, 422},
		{"wrong artifact", func(r *tracker.ReviewChange) { r.Bundle.ArtifactID = artifact.NewID("artifact") }, 404},
		{"wrong revision", func(r *tracker.ReviewChange) { r.Bundle.Revision++ }, 404},
		{"wrong expected version", func(r *tracker.ReviewChange) { r.ExpectedVersionID = artifact.NewID("version") }, 409},
		{"invalid decision", func(r *tracker.ReviewChange) { r.Decision = "merge" }, 422},
	} {
		t.Run(tt.name, func(t *testing.T) {
			copy := bundle
			r := tracker.ReviewChange{Mutation: tracker.Mutation{IdempotencyKey: tt.name}, Decision: "approved", ExpectedVersionID: first.ID, Bundle: &copy}
			tt.edit(&r)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, f.token, r), tt.want)
		})
	}
	worker := f.worker(t, "review-worker")
	workerReview := tracker.ReviewChange{Mutation: tracker.Mutation{IdempotencyKey: "worker-review"}, Decision: "approved", ExpectedVersionID: first.ID, Bundle: &bundle}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, worker, workerReview), 403)
	second := publishReviewVersion(t, f, "second", first.ID)
	for _, tt := range []struct {
		key  string
		want int
	}{{"valid", 200}, {"stale-tab", 409}} {
		r := tracker.ReviewChange{Mutation: tracker.Mutation{IdempotencyKey: tt.key}, Decision: "approved", ExpectedVersionID: first.ID, Bundle: &bundle}
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, f.token, r), tt.want)
	}
	r := tracker.ReviewChange{Mutation: tracker.Mutation{IdempotencyKey: "rebind"}, Decision: "approved", ExpectedVersionID: second.ID, Bundle: &bundle}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.path+"/versions/"+second.ID+"/reviews", f.token, r), 422)
	if detail := f.detail(t); detail.Summary.NativeReview != "stale" || len(detail.Reviews) != 1 {
		t.Fatalf("stale approval transferred: %#v", detail.Summary)
	}
}

func TestViewedFilesVersionActorAndReplay(t *testing.T) {
	t.Parallel()
	service := newPostgresNativeService(t, Config{})
	organization := tracker.OrganizationID(artifact.NewID("org"))
	seedPostgresHubAdmin(t, service.config.TenantRepository, service, nativeOrganization{ID: organization, Name: string(organization), Slug: strings.ReplaceAll(string(organization), "_", "-")})
	f := newChangeFixture(t, service, organization)
	version := publishReviewVersion(t, f, "first", "")
	bundle := seedReviewBundle(t, f, version, time.Now().Add(time.Hour))
	path := f.path + "/versions/" + version.ID + "/viewed-files"
	r := tracker.ViewChangeFile{Mutation: tracker.Mutation{IdempotencyKey: "view"}, Bundle: bundle, FileSHA256: strings.Repeat("f", 64), Viewed: true}
	for _, tt := range []struct {
		name string
		edit func(*tracker.ViewChangeFile)
		want int
	}{
		{"view", func(*tracker.ViewChangeFile) {}, 200},
		{"retry", func(*tracker.ViewChangeFile) {}, 200},
		{"changed replay", func(r *tracker.ViewChangeFile) { r.Viewed = false }, 409},
		{"raw filename", func(r *tracker.ViewChangeFile) { r.IdempotencyKey = "path"; r.FileSHA256 = "private.go" }, 422},
		{"wrong bundle", func(r *tracker.ViewChangeFile) {
			r.IdempotencyKey = "bundle"
			r.Bundle.SHA256 = strings.Repeat("e", 64)
		}, 422},
	} {
		t.Run(tt.name, func(t *testing.T) {
			copy := r
			tt.edit(&copy)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, f.token, copy), tt.want)
		})
	}
	if err := f.service.Close(); err != nil {
		t.Fatal(err)
	}
	f.service = reopenPostgresNativeService(t, f.service)
	response := performHubAPIRequest(t, f.service, http.MethodGet, path, f.token, nil)
	requireNativeStatus(t, response, 200)
	var files []tracker.ChangeViewedFile
	decodeHubResponse(t, response, &files)
	if len(files) != 1 || !files[0].Viewed || files[0].ManifestSHA256 != bundle.SHA256 {
		t.Fatalf("viewed state lost: %#v", files)
	}
	second := publishReviewVersion(t, f, "second", version.ID)
	response = performHubAPIRequest(t, f.service, http.MethodGet, f.path+"/versions/"+second.ID+"/viewed-files", f.token, nil)
	requireNativeStatus(t, response, 200)
	decodeHubResponse(t, response, &files)
	if len(files) != 0 {
		t.Fatal("viewed state transferred to new version")
	}
	other := newNativeFixture(t, f.service, f.project.OrganizationID, "other-reviewer")
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, path, other.token, nil), 404)
	otherCredential, _, err := f.service.authenticateTenantAPIHash(t.Context(), other.project.OrganizationID, apikey.HashToken(other.token), "", "")
	if err != nil {
		t.Fatal(err)
	}
	otherPrincipal := otherCredential.ID
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, "/api/v2/tokens/"+otherPrincipal+"/grants", testHubAdminToken, map[string]string{"organization_id": string(f.project.OrganizationID), "project_id": string(f.project.ID)}), 204)
	response = performHubAPIRequest(t, f.service, http.MethodGet, path, other.token, nil)
	requireNativeStatus(t, response, 200)
	decodeHubResponse(t, response, &files)
	if len(files) != 0 {
		t.Fatal("viewed state transferred to another project member")
	}
	expired := seedReviewBundle(t, f, second, time.Now().Add(-time.Hour))
	r.Bundle = expired
	r.IdempotencyKey = "expired"
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.path+"/versions/"+second.ID+"/viewed-files", f.token, r), 422)
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
		lease, err = w.Claim(t.Context(), leasestore.Claim{IssueID: item.ID, ExpectedRevision: item.Revision, MachineID: string(credential.Runner.MachineID), SessionID: newNativeID("session"), LeaseID: newNativeID("lease"), TTL: time.Hour})
		if err != nil {
			return err
		}
		if err := w.AssignRunnerLease(t.Context(), runnerstore.LeaseAssignment{OrganizationID: f.project.OrganizationID, LeaseID: lease.LeaseID, RunnerID: credential.Runner.RunnerID, AssignedAt: f.service.config.now()}); err != nil {
			return err
		}
		return w.BindLeasePolicy(t.Context(), policystore.LeasePolicy{OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, LeaseID: tracker.LeaseID(lease.LeaseID), PolicyID: input.PolicyID, BoundAt: f.service.config.now()})
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
	event, err := content.PrepareEvent(t.Context(), f.project.OrganizationID, historyTestJSON(t, tracker.CollaborationEvent{ID: newNativeID("evt"), OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, AggregateID: f.issue.WorkItemID, Type: "run.finished", SchemaVersion: 1, Actor: tracker.Actor{Kind: "runner", PrincipalID: credential.ID}, RecordedAt: f.service.config.now(), Data: tracker.CollaborationData{Run: &data}}))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		if _, err := w.RecordAttempt(t.Context(), leasestore.AttemptEvent{ID: input.AttemptID, Fence: leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}, RunID: input.RunID, Sequence: 1, Status: "succeeded", StartedAt: f.service.config.now(), PayloadObjectKey: payload.Key, PayloadReference: payload}); err != nil {
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
