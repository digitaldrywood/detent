package hubserver

import (
	"bytes"
	"context"
	"fmt"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"net/http"
	"net/url"
	"strconv"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type commentPageCountingRepository struct {
	tenantstore.Repository
	singles, batches int
}

func (r *commentPageCountingRepository) ReadTenant(ctx context.Context, org tracker.OrganizationID, workload corestore.Workload, read func(tenantstore.Reader) error) error {
	return r.Repository.ReadTenant(ctx, org, workload, func(reader tenantstore.Reader) error {
		return read(commentPageCountingReader{Reader: reader, repository: r})
	})
}

type commentPageCountingReader struct {
	tenantstore.Reader
	repository *commentPageCountingRepository
}

func (r commentPageCountingReader) CollaborationVersion(ctx context.Context, id string, revision int64) (eventstore.CollaborationVersion, error) {
	r.repository.singles++
	return r.Reader.CollaborationVersion(ctx, id, revision)
}

func (r commentPageCountingReader) CollaborationVersions(ctx context.Context, keys []eventstore.CollaborationVersionKey) ([]eventstore.CollaborationVersion, error) {
	r.repository.batches++
	versions, err := r.Reader.(eventstore.CollaborationVersionsReader).CollaborationVersions(ctx, keys)
	for left, right := 0, len(versions)-1; left < right; left, right = left+1, right-1 {
		versions[left], versions[right] = versions[right], versions[left]
	}
	return versions, err
}

func TestCommentPageUsesSetBasedVersions(t *testing.T) {
	repository := projectRecordPostgres(t)
	service := openLocalPostgresHubTestService(t, repository, Config{RunDetails: &memoryRunDetails{}})
	for _, name := range []string{"First organization", "Second organization"} {
		org, err := service.createNativeOrganizationFor(t.Context(), name)
		if err != nil {
			t.Fatal(err)
		}
		seedPostgresHubAdmin(t, repository, service, org)
		service.config.SecretKeys = secretTestKeys(t, "1", "1")
		envelope, err := service.config.SecretKeys.Seal(bytes.Repeat([]byte{1}, 32), accessstore.SecretAAD(org.ID, "", accessstore.NativeCursorKeyKind))
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(t.Context(), org.ID, corestore.Serving, func(w tenantstore.Writer) error {
			return w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: org.ID, Kind: accessstore.NativeCursorKeyKind, Envelope: envelope, UpdatedAt: service.config.now()}, accessstore.SecretAudit{OrganizationID: org.ID, Kind: accessstore.NativeCursorKeyKind, Actor: "migration", Event: "import", KeyVersion: envelope.Version, RecordedAt: service.config.now()})
		}); err != nil {
			t.Fatal(err)
		}
		projectPath := "/api/v2/organizations/" + string(org.ID) + "/projects"
		response := performHubAPIRequest(t, service, http.MethodPost, projectPath, testHubAdminToken, map[string]any{"idempotency_key": "project", "name": "Example", "states": nativeFixtureStates()})
		requireNativeStatus(t, response, http.StatusOK)
		var project tracker.NativeProject
		decodeHubResponse(t, response, &project)
		if err := repository.WriteTenant(t.Context(), org.ID, corestore.Serving, func(w tenantstore.Writer) error {
			record, err := w.ProjectRecord(t.Context(), project.ID)
			if err != nil {
				return err
			}
			record.Settings.DuplicateChallenge = false
			_, err = w.UpdateProjectRecord(t.Context(), record)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		path := projectPath + "/" + string(project.ID) + "/work-items"
		response = performHubAPIRequest(t, service, http.MethodPost, path, testHubAdminToken, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "issue"}, Title: "Example", Body: workItemRecordBody})
		requireNativeStatus(t, response, http.StatusOK)
		var issue tracker.NativeIssue
		decodeHubResponse(t, response, &issue)
		path += "/" + string(issue.WorkItemID) + "/comments"
		for index := range 50 {
			response = performHubAPIRequest(t, service, http.MethodPost, path, testHubAdminToken, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("comment-%d", index)}, Body: fmt.Sprintf("Comment %d", index)})
			requireNativeStatus(t, response, http.StatusOK)
		}
		counted := &commentPageCountingRepository{Repository: repository}
		service.config.TenantRepository = counted
		for _, limit := range []int{1, 2, 50} {
			for _, direction := range []string{"oldest", "newest"} {
				counted.singles, counted.batches = 0, 0
				page, err := service.readCommentRecords(t.Context(), nativeScope{organization: org.ID, project: project.ID}, string(issue.WorkItemID), url.Values{"limit": {strconv.Itoa(limit)}, "direction": {direction}})
				if err != nil {
					t.Fatal(err)
				}
				if counted.singles != 0 || counted.batches != 1 {
					t.Errorf("%s limit=%d direction=%s: single versions=%d batches=%d, want 0/1", name, limit, direction, counted.singles, counted.batches)
				}
				if len(page.Items) != limit || (page.NextCursor != "") != (limit < 50) {
					t.Fatalf("page len=%d next=%q", len(page.Items), page.NextCursor)
				}
				for index, comment := range page.Items {
					want := index
					if direction == "newest" {
						want = 49 - index
					}
					if comment.Body != fmt.Sprintf("Comment %d", want) {
						t.Fatalf("body=%q want comment %d", comment.Body, want)
					}
				}
			}
		}
		service.config.TenantRepository = repository
	}
}
