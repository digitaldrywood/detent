package hubserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/attachmentstore"
	"github.com/digitaldrywood/detent/internal/hubserver/coreimport"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/githubcontract"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/neutralimport"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/issuecontract"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeVersionRoundTrip(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"issue", "comment"} {
		t.Run(kind, func(t *testing.T) {
			const count = 2*neutralimport.VersionSnapshotInterval + 7
			const organization tracker.OrganizationID = "org_version_history"
			const project tracker.ProjectID = "prj_history"
			const item tracker.NativeWorkItemID = "wi_history"
			const related tracker.NativeWorkItemID = "wi_related"
			recordID := string(item)
			if kind == "comment" {
				recordID = "comment-" + recordID
			}
			now := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
			expected := map[int64][]byte{}
			for revision := int64(1); revision <= count; revision++ {
				stamp := now.Add(time.Duration(revision) * time.Second)
				var record any
				if kind == "issue" {
					value := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: organization, ProjectID: project, WorkItemID: item, Number: 1, Revision: tracker.Revision(revision), Profile: "native"}, Title: "history issue", State: "Todo", Actor: tracker.Actor{Kind: "human", PrincipalID: "version_author"}, CreatedAt: now, UpdatedAt: stamp, LastActivityAt: stamp}
					if revision > 1 {
						value.Body = strings.Repeat("Retained body content. ", 256) + fmt.Sprintf("body generation %d <>& 雪", revision/4)
						value.Labels, value.Assignees = []string{fmt.Sprintf("label-%d", revision%3)}, []string{fmt.Sprintf("member-%d", revision%2)}
						value.IssueContract = &issuecontract.State{ConfirmedSections: map[string]string{"acceptance criteria": strconv.FormatInt(revision, 10)}, RecordedAt: stamp}
						if revision%2 == 0 {
							value.Dependencies = []tracker.NativeWorkItemID{related}
							value.Blockers = []tracker.NativeDependency{{ID: related}}
							value.Verification = &tracker.NativeVerification{State: tracker.VerificationPending}
						}
					}
					record = value
				} else {
					record = tracker.NativeComment{ID: recordID, OrganizationID: organization, ProjectID: project, WorkItemID: item, Revision: tracker.Revision(revision), Sequence: 1, Body: fmt.Sprintf("comment revision %d <>& 雪", revision), CreatedAt: now, UpdatedAt: stamp}
				}
				raw, err := json.Marshal(record)
				if err != nil {
					t.Fatal(err)
				}
				expected[revision] = raw
			}
			for _, archived := range []bool{false, true} {
				t.Run(fmt.Sprintf("archived=%v", archived), func(t *testing.T) {
					repository, admin := projectRecordPostgresWithAdmin(t, 3)
					details := &memoryRunDetails{}
					service := &Service{database: &database{}, secretRewrapReady: make(chan struct{}), config: Config{TenantRepository: repository, AccessRepository: repository, RunDetails: details, Clock: func() time.Time { return now }, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}}
					token := string(organization) + "_reader"
					deniedToken := string(organization) + "_denied"
					if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
						if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: organization, Name: "Version history", Slug: "version-history", CreatedAt: now}); err != nil {
							return err
						}
						if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: organization, ProviderID: "provider_history"}); err != nil {
							return err
						}
						if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: project, Name: "History", Slug: "history", Profile: "native"}, States: nativeFixtureStates()}); err != nil {
							return err
						}
						for index, id := range []tracker.NativeWorkItemID{item, related} {
							revision := int64(1)
							if id == item {
								revision = count
							}
							row, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: organization, ProjectID: project, NativeID: id, Number: int64(index + 1), Revision: revision, Title: "history " + kind, WorkflowStateName: new("Todo"), ActorKind: new("human"), ActorPrincipalID: new("version_author"), CreatedAt: now, UpdatedAt: now, NativeCreatedAt: now, NativeUpdatedAt: now})
							if err != nil {
								return err
							}
							if row.NativeID != id || row.Number != int64(index+1) {
								t.Fatal("version history fixture lost a scoped issue identity")
							}
						}
						for _, key := range []struct {
							id, token, scope string
							native           bool
						}{{"reader", token, "admin", false}, {"denied", deniedToken, "operator", true}} {
							if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: key.id, Name: "Example version reader " + key.id, Hash: apikey.HashToken(key.token), Scope: key.scope, NativeOnly: key.native, Kind: "service", ProjectAccess: "selected", CreatedAt: now, UpdatedAt: now}); err != nil {
								return err
							}
						}
						return w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization, TokenID: "reader", ProjectID: project})
					}); err != nil {
						t.Fatal(err)
					}
					credential, status, err := service.authenticateTenantAPIHash(t.Context(), organization, apikey.HashToken(token), "", "")
					if err != nil {
						t.Fatalf("version reader status=%d error=%v", status, err)
					}
					scope := hubscope.NativeScope{Organization: organization, Project: project, Credential: credential}
					content := eventstore.NewContent(details)
					comment := ""
					if kind == "comment" {
						comment = recordID
					}
					if !archived {
						for revision := int64(1); revision <= count; revision++ {
							version, err := content.PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: organization, ProjectID: project, WorkItemID: item, RecordID: recordID, Revision: revision}, expected[revision])
							if err != nil {
								t.Fatal(err)
							}
							versionReads := 0
							ctx := postgres.WithTransactionTiming(t.Context(), nil, func(_ time.Time, query string) {
								if strings.Contains(query, "SELECT") && strings.Contains(query, "collaboration_versions") {
									versionReads++
								}
							})
							if err := repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error { _, err := w.CreateCollaborationVersion(ctx, version); return err }); err != nil {
								t.Fatal(err)
							}
							if versionReads > 1 {
								t.Fatalf("revision %d read history %d times", revision, versionReads)
							}
						}
						for revision := int64(1); revision <= count; revision++ {
							var version eventstore.CollaborationVersion
							if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
								var err error
								version, err = r.CollaborationVersion(t.Context(), recordID, revision)
								return err
							}); err != nil {
								t.Fatal(err)
							}
							before := details.reads
							raw, err := content.CollaborationVersion(t.Context(), version)
							if err != nil {
								t.Fatal(err)
							}
							if !bytes.Equal(raw, expected[revision]) {
								t.Fatalf("revision %d did not reconstruct exact snapshot bytes", revision)
							}
							if details.reads != before+1 {
								t.Fatalf("revision %d did not read one independent immutable snapshot", revision)
							}
							read, err := service.readVersion(t.Context(), scope, string(item), comment, revision)
							if err != nil {
								t.Fatal(err)
							}
							if !bytes.Equal(read, expected[revision]) {
								t.Fatalf("revision %d response mismatch\ngot %s\nwant %s", revision, read, expected[revision])
							}
							if version.PayloadBytes > int64(len(expected[revision])) {
								t.Fatalf("revision %d encoding grew the record", revision)
							}
						}
					} else {
						table := attachment.ArchiveTable{Name: "collaboration_versions", Columns: []string{"rowid", "organization_id", "project_id", "work_item_id", "record_id", "revision", "record_json", "encoding"}}
						for revision := int64(1); revision <= count; revision++ {
							values := []any{revision, organization, project, item, recordID, revision, string(expected[revision]), "snapshot"}
							row := make([]json.RawMessage, len(values))
							for index, value := range values {
								raw, err := json.Marshal(value)
								if err != nil {
									t.Fatal(err)
								}
								row[index] = raw
							}
							table.Rows = append(table.Rows, row)
						}
						const sectionID = "historical_versions"
						raw, err := json.Marshal(attachment.ArchiveSection{SchemaVersion: 1, OrganizationID: string(organization), ProjectID: string(project), WorkItemID: string(item), SectionID: sectionID, Tables: []attachment.ArchiveTable{table}})
						if err != nil {
							t.Fatal(err)
						}
						ref, section, err := attachment.PrepareArchive(string(organization), 1, bytes.NewReader(raw))
						if err != nil {
							t.Fatal(err)
						}
						const attachmentID = "version_section_archive"
						record := attachmentstore.ArchiveRecord{OrganizationID: organization, ProjectID: project, ID: attachmentID, Uploader: "reader", Reference: ref, CreatedAt: now}
						if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
							if _, err := w.PrepareAttachmentArchive(t.Context(), record); err != nil {
								return err
							}
							if err := w.PublishAttachmentArchive(t.Context(), record, item); err != nil {
								return err
							}
							object, err := w.RegisterArchiveObject(t.Context(), artifactstore.ArchiveObject{OrganizationID: organization, ID: attachmentID, Reference: ref, CreatedAt: now})
							if err != nil {
								return err
							}
							return w.AddArchiveOwner(t.Context(), artifactstore.ArchiveOwner{OrganizationID: organization, ArchiveID: object.ID, Kind: "issue-storage-archive", ID: attachmentID, CreatedAt: now})
						}); err != nil {
							t.Fatal(err)
						}
						if _, err := admin.ExecContext(t.Context(), `INSERT INTO issue_archive_sections (organization_id,project_id,work_item_id,archive_revision,section_id,attachment_id,kind,subject_id,first_sequence,last_sequence,retirement_complete) VALUES ($1,$2,$3,$4,$5,$6,'revision',$7,1,$4,true)`, organization, project, item, count, sectionID, attachmentID, recordID); err != nil {
							t.Fatal(err)
						}
						fetches := 0
						service.config.ArchiveReader = func(_ context.Context, got attachment.ArchiveReference, _ bool) (entryauthority.ArchiveResponse, error) {
							fetches++
							if repository.Stats().AcquiredConns() != 0 {
								t.Fatal("archive read holds a PostgreSQL connection")
							}
							if got != ref {
								t.Fatal("wrong revision archive")
							}
							return entryauthority.ArchiveResponse{Data: section}, nil
						}
						deniedCredential, status, err := service.authenticateTenantAPIHash(t.Context(), organization, apikey.HashToken(deniedToken), "", "")
						if err != nil {
							t.Fatalf("denied version reader status=%d error=%v", status, err)
						}
						denied := scope
						denied.Credential = deniedCredential
						if _, err := service.readVersion(t.Context(), denied, string(item), comment, 1); err == nil || fetches != 0 {
							t.Fatalf("unauthorized historical read error=%v fetches=%d", err, fetches)
						}
						for _, revision := range []int64{1, 2, neutralimport.VersionSnapshotInterval - 1, neutralimport.VersionSnapshotInterval, neutralimport.VersionSnapshotInterval + 1, count} {
							ctx := service.archiveContext(t.Context(), scope, false)
							before := fetches
							got, err := service.readVersion(ctx, scope, string(item), comment, revision)
							if err != nil {
								t.Fatal(err)
							}
							if !bytes.Equal(got, expected[revision]) || fetches != before+1 {
								t.Fatalf("archived %s revision %d changed or fetched more than one section", kind, revision)
							}
						}
						var versions int
						if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM collaboration_versions WHERE organization_id=$1 AND record_id=$2`, organization, recordID).Scan(&versions); err != nil || versions != 0 {
							t.Fatalf("historical read materialized live versions=%d error=%v", versions, err)
						}
					}
					if _, err := service.readVersion(t.Context(), scope, string(item), comment, count+1); !hubapi.IsNativeNotFound(err) {
						t.Fatalf("missing revision error=%v", err)
					}
				})
			}
		})
	}
}

func TestNativeTransitionHistoryBytes(t *testing.T) {
	t.Parallel()
	f := newDefaultNativeFixture(t, Config{})
	issue := f.create(t, "transition history")
	scope := nativeBoardFixtureScope(t, f)
	now := time.Now().UTC()
	f.service.config.Clock = func() time.Time { return now }
	path := f.base + "/work-items/" + string(issue.WorkItemID)
	body := strings.Repeat("Large unchanged issue body. 雪\n", 1024) + issueContractTestSections
	response := performHubAPIRequest(t, f.service, http.MethodPatch, path, f.token, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "history-body"}, ExpectedRevision: issue.Revision, Body: &body})
	requireNativeStatus(t, response, http.StatusOK)
	decodeHubResponse(t, response, &issue)
	snapshots := make(map[tracker.Revision]string)
	for i := range 513 {
		now = now.Add(24 * time.Second)
		state := "In Progress"
		if issue.State == state {
			state = "Todo"
		}
		response := performHubAPIRequest(t, f.service, http.MethodPost, path+"/workflow", f.token, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("history-transition-%d", i)}, ExpectedRevision: issue.Revision, State: state, Reason: "user_requested"})
		requireNativeStatus(t, response, http.StatusOK)
		decodeHubResponse(t, response, &issue)
		raw, err := json.Marshal(issue)
		if err != nil {
			t.Fatal(err)
		}
		snapshots[issue.Revision] = string(raw)
	}
	var fullBytes, storedBytes int64
	content := eventstore.NewContent(f.service.runDetailStore())
	for revision, expected := range snapshots {
		var version eventstore.CollaborationVersion
		if err := f.service.config.TenantRepository.ReadTenant(t.Context(), scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			version, err = r.CollaborationVersion(t.Context(), string(issue.WorkItemID), int64(revision))
			return err
		}); err != nil {
			t.Fatal(err)
		}
		actual, err := content.CollaborationVersion(t.Context(), version)
		if err != nil {
			t.Fatal(err)
		}
		var original tracker.NativeIssue
		if err := json.Unmarshal(actual, &original); err != nil {
			t.Fatal(err)
		}
		original.WebURL = issue.WebURL
		original = hubrecords.RelationshipOmittedIssue(original)
		canonical, err := json.Marshal(original)
		if err != nil {
			t.Fatal(err)
		}
		if string(canonical) != expected {
			t.Fatalf("transition revision %d changed", revision)
		}
		if version.PayloadBytes >= 4096 {
			t.Fatalf("compressed version repeated body: %d bytes", version.PayloadBytes)
		}
		storedBytes += version.PayloadBytes
		fullBytes += int64(len(expected))
	}
	if storedBytes >= fullBytes/8 {
		t.Fatalf("transition history=%d bytes, full snapshots=%d", storedBytes, fullBytes)
	}
	t.Logf("513 transitions: snapshots=%d stored=%d reduction=%.1f%%", fullBytes, storedBytes, 100*(1-float64(storedBytes)/float64(fullBytes)))
}

func TestNativeNoOpRevisions(t *testing.T) {
	t.Parallel()
	f := newDefaultNativeFixture(t, Config{})
	issue := f.create(t, "no-op edits")
	scope := nativeBoardFixtureScope(t, f)
	admin := conversationTestAdmin(t, f.service)
	path := f.base + "/work-items/" + string(issue.WorkItemID)
	states := nativeFixtureStates()
	states[0].Transitions = append(states[0].Transitions, "Todo")
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		project, err := w.ProjectRecord(t.Context(), scope.Project)
		if err != nil {
			return err
		}
		project.States = states
		_, err = w.UpdateProjectRecord(t.Context(), project)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		reason string
		status int
	}{
		{reason: "worker_progress", status: http.StatusUnprocessableEntity},
		{reason: "dependency_ready", status: http.StatusUnprocessableEntity},
		{reason: "user_requested", status: http.StatusOK},
	} {
		t.Run("same state command "+test.reason, func(t *testing.T) {
			var beforeVersions, beforeEvents int
			if err := admin.QueryRowContext(t.Context(), "SELECT (SELECT count(*) FROM collaboration_versions WHERE organization_id=$1 AND work_item_id=$2),(SELECT count(*) FROM collaboration_events WHERE organization_id=$1 AND work_item_id=$2)", scope.Organization, issue.WorkItemID).Scan(&beforeVersions, &beforeEvents); err != nil {
				t.Fatal(err)
			}
			for index := range 2 {
				response := performHubAPIRequest(t, f.service, http.MethodPost, path+"/workflow", f.token, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("same-state-%s-%d", test.reason, index)}, ExpectedRevision: issue.Revision, State: issue.State, Reason: test.reason})
				requireNativeStatus(t, response, test.status)
				var result tracker.NativeIssue
				if test.status == http.StatusOK {
					decodeHubResponse(t, response, &result)
				} else {
					result = readWorkItem(t, f, issue.WorkItemID, "")
				}
				if result.Revision != issue.Revision || !result.UpdatedAt.Equal(issue.UpdatedAt) {
					t.Fatalf("same-state command changed revision or timestamp: %+v", result)
				}
			}
			var versions, events int
			if err := admin.QueryRowContext(t.Context(), "SELECT (SELECT count(*) FROM collaboration_versions WHERE organization_id=$1 AND work_item_id=$2),(SELECT count(*) FROM collaboration_events WHERE organization_id=$1 AND work_item_id=$2)", scope.Organization, issue.WorkItemID).Scan(&versions, &events); err != nil {
				t.Fatal(err)
			}
			if versions != beforeVersions || events != beforeEvents {
				t.Fatalf("same-state command wrote history: versions=%d/%d events=%d/%d", versions, beforeVersions, events, beforeEvents)
			}
		})
	}
	var beforeVersions, beforeEvents int
	if err := admin.QueryRowContext(t.Context(), "SELECT (SELECT count(*) FROM collaboration_versions WHERE organization_id=$1 AND work_item_id=$2),(SELECT count(*) FROM collaboration_events WHERE organization_id=$1 AND work_item_id=$2)", scope.Organization, issue.WorkItemID).Scan(&beforeVersions, &beforeEvents); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"title", "body", "labels", "assignees", "priority"} {
		request := map[string]any{"expected_revision": fmt.Sprint(issue.Revision), "idempotency_key": "identical-" + field, field: map[string]any{"title": issue.Title, "body": issue.Body, "labels": issue.Labels, "assignees": issue.Assignees, "priority": issue.Priority}[field]}
		before, beforeVersion, err := f.service.readWorkItemRecord(t.Context(), scope, string(issue.WorkItemID))
		if err != nil {
			t.Fatal(err)
		}
		var updates int
		ctx := postgres.WithTransactionTiming(t.Context(), nil, func(_ time.Time, statement string) {
			if strings.Contains(strings.ToUpper(statement), "UPDATE ISSUES") {
				updates++
			}
		})
		raw, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPatch, path, bytes.NewReader(raw)).WithContext(ctx)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+f.token)
		response := httptest.NewRecorder()
		f.service.echo.ServeHTTP(response, req)
		requireNativeStatus(t, response, http.StatusOK)
		var result tracker.NativeIssue
		decodeHubResponse(t, response, &result)
		after, afterVersion, err := f.service.readWorkItemRecord(t.Context(), scope, string(issue.WorkItemID))
		if err != nil {
			t.Fatal(err)
		}
		var versions, events int
		if err := admin.QueryRowContext(t.Context(), "SELECT (SELECT count(*) FROM collaboration_versions WHERE organization_id=$1 AND work_item_id=$2),(SELECT count(*) FROM collaboration_events WHERE organization_id=$1 AND work_item_id=$2)", scope.Organization, issue.WorkItemID).Scan(&versions, &events); err != nil {
			t.Fatal(err)
		}
		if result.Revision != issue.Revision || !result.UpdatedAt.Equal(issue.UpdatedAt) || !reflect.DeepEqual(before, after) || !reflect.DeepEqual(beforeVersion, afterVersion) || versions != beforeVersions || events != beforeEvents || updates != 0 {
			t.Fatalf("identical %s changed record or history: versions %d/%d events %d/%d updates %d", field, versions, beforeVersions, events, beforeEvents, updates)
		}
	}
	changed := "real title change"
	var realUpdates int
	ctx := postgres.WithTransactionTiming(t.Context(), nil, func(_ time.Time, statement string) {
		if strings.Contains(strings.ToUpper(statement), "UPDATE ISSUES") {
			realUpdates++
		}
	})
	raw, err := json.Marshal(tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "real-edit"}, ExpectedRevision: issue.Revision, Title: &changed})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPatch, path, bytes.NewReader(raw)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+f.token)
	response := httptest.NewRecorder()
	f.service.echo.ServeHTTP(response, req)
	requireNativeStatus(t, response, http.StatusOK)
	var result tracker.NativeIssue
	decodeHubResponse(t, response, &result)
	if result.Revision != issue.Revision+1 || realUpdates == 0 {
		t.Fatalf("real edit revision=%d want=%d issue updates=%d", result.Revision, issue.Revision+1, realUpdates)
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPatch, path, f.token, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "stale-identical"}, ExpectedRevision: issue.Revision, Title: &changed}), http.StatusConflict)
	response = performHubAPIRequest(t, f.service, http.MethodPost, path+"/comments", f.token, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "no-op-comment"}, Body: "comment content"})
	requireNativeStatus(t, response, http.StatusOK)
	var comment tracker.NativeComment
	decodeHubResponse(t, response, &comment)
	for index, body := range []string{comment.Body, "changed comment", "changed comment"} {
		response = performHubAPIRequest(t, f.service, http.MethodPatch, path+"/comments/"+comment.ID, f.token, tracker.UpdateComment{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("comment-edit-%d", index)}, ExpectedRevision: comment.Revision, Body: body})
		requireNativeStatus(t, response, http.StatusOK)
		var edited tracker.NativeComment
		decodeHubResponse(t, response, &edited)
		want := comment.Revision
		if body != comment.Body {
			want++
		}
		if edited.Revision != want {
			t.Fatalf("comment revision=%d want %d", edited.Revision, want)
		}
		comment = edited
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPatch, path+"/comments/"+comment.ID, f.token, tracker.UpdateComment{Mutation: tracker.Mutation{IdempotencyKey: "stale-comment"}, ExpectedRevision: 1, Body: comment.Body}), http.StatusConflict)
	var versions, events int
	if err := admin.QueryRowContext(t.Context(), "SELECT (SELECT count(*) FROM collaboration_versions WHERE organization_id=$1 AND work_item_id=$2),(SELECT count(*) FROM collaboration_events WHERE organization_id=$1 AND work_item_id=$2)", scope.Organization, issue.WorkItemID).Scan(&versions, &events); err != nil {
		t.Fatal(err)
	}
	if versions != beforeVersions+3 || events != beforeEvents+3 {
		t.Fatalf("expected one real issue edit, comment creation and comment edit: versions=%d/%d events=%d/%d", versions, beforeVersions, events, beforeEvents)
	}
	response = performHubAPIRequest(t, f.service, http.MethodPost, path+"/workflow", f.token, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "real-transition"}, ExpectedRevision: result.Revision, State: "In Progress", Reason: "user_requested"})
	requireNativeStatus(t, response, http.StatusOK)
	var transitioned tracker.NativeIssue
	decodeHubResponse(t, response, &transitioned)
	if transitioned.Revision != result.Revision+1 || transitioned.State != "In Progress" {
		t.Fatalf("real transition did not advance once: %+v", transitioned)
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/workflow", f.token, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "stale-transition"}, ExpectedRevision: result.Revision, State: "In Progress", Reason: "user_requested"}), http.StatusConflict)
	if err := admin.QueryRowContext(t.Context(), "SELECT (SELECT count(*) FROM collaboration_versions WHERE organization_id=$1 AND work_item_id=$2),(SELECT count(*) FROM collaboration_events WHERE organization_id=$1 AND work_item_id=$2)", scope.Organization, issue.WorkItemID).Scan(&versions, &events); err != nil {
		t.Fatal(err)
	}
	if versions != beforeVersions+4 || events != beforeEvents+4 {
		t.Fatalf("real transition wrote extra history: versions=%d/%d events=%d/%d", versions, beforeVersions, events, beforeEvents)
	}
	t.Run("operator hold response and replay", func(t *testing.T) {
		now := time.Now().UTC().Truncate(time.Microsecond).Add(time.Hour)
		states[1].Transitions = append(states[1].Transitions, "Blocked")
		states = append(states, tracker.NativeState{Name: "Blocked", Transitions: []string{"Blocked", "Todo"}})
		if err := f.service.config.TenantRepository.WriteTenant(t.Context(), scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
			project, err := w.ProjectRecord(t.Context(), scope.Project)
			if err != nil {
				return err
			}
			project.States = states
			_, err = w.UpdateProjectRecord(t.Context(), project)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		clock := f.service.config.Clock
		f.service.config.Clock = func() time.Time { return now }
		t.Cleanup(func() { f.service.config.Clock = clock })
		request := tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "enter-blocked"}, ExpectedRevision: transitioned.Revision, State: "Blocked", Reason: "user_requested"}
		response := performHubAPIRequest(t, f.service, http.MethodPost, path+"/workflow", f.token, request)
		requireNativeStatus(t, response, http.StatusOK)
		var blocked tracker.NativeIssue
		decodeHubResponse(t, response, &blocked)
		now = now.Add(time.Minute)
		request.IdempotencyKey = "retain-operator-hold"
		request.ExpectedRevision = blocked.Revision
		for _, operation := range []string{"hold", "replay"} {
			response := performHubAPIRequest(t, f.service, http.MethodPost, path+"/workflow", f.token, request)
			requireNativeStatus(t, response, http.StatusOK)
			var result tracker.NativeIssue
			decodeHubResponse(t, response, &result)
			current := readWorkItem(t, f, issue.WorkItemID, "")
			if result.Revision != blocked.Revision || !result.UpdatedAt.Equal(blocked.UpdatedAt) || !result.LastActivityAt.Equal(now) || !result.LastActivityAt.Equal(current.LastActivityAt) {
				t.Fatalf("%s returned stale operator hold: revision=%d/%d activity=%s/%s", operation, result.Revision, blocked.Revision, result.LastActivityAt, current.LastActivityAt)
			}
		}
		var holdVersions, holdEvents int
		if err := admin.QueryRowContext(t.Context(), "SELECT (SELECT count(*) FROM collaboration_versions WHERE organization_id=$1 AND work_item_id=$2),(SELECT count(*) FROM collaboration_events WHERE organization_id=$1 AND work_item_id=$2)", scope.Organization, issue.WorkItemID).Scan(&holdVersions, &holdEvents); err != nil {
			t.Fatal(err)
		}
		if holdVersions != versions+1 || holdEvents != events+2 {
			t.Fatalf("operator hold or replay wrote extra history: versions=%d/%d events=%d/%d", holdVersions, versions, holdEvents, events)
		}
	})
}

func TestCollaborationDeltaMigration(t *testing.T) {
	t.Parallel()
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("verification_failure_%t", fail), func(t *testing.T) {
			f := newDefaultNativeFixture(t, Config{})
			org, project := f.project.OrganizationID, f.project.ID
			admin := conversationTestAdmin(t, f.service)
			const revisions = 2*neutralimport.VersionSnapshotInterval + 3
			source := coreWorkItemNeutralFixture(t, org, project, false)
			source.Records["collaboration_versions"] = nil
			var creation []neutralimport.Row
			for _, row := range source.Records["collaboration_events"] {
				var kind string
				if err := row.Decode(map[string]any{"type": &kind}); err != nil {
					t.Fatal(err)
				}
				if kind == "issue.created" {
					creation = append(creation, row)
				}
			}
			source.Records["collaboration_events"] = creation
			original := map[string]map[int64][]byte{}
			owners, urls := map[string]string{}, map[string]string{}
			var rawBytes, versionCount int64
			for index, name := range []string{"first", "second"} {
				suffix := ""
				if index > 0 {
					suffix = "_second"
				}
				item, commentID := "wi_core_import"+suffix, "comment_core_import"+suffix
				owners[item], owners[commentID] = item, item
				original[item], original[commentID] = map[int64][]byte{}, map[int64][]byte{}
				previous := map[string][]byte{}
				previousEncoding := map[string]string{}
				urls[item] = fmt.Sprintf("https://github.com/example-org/example-project/issues/%d", 1+index*2)
				for revision := int64(1); revision <= revisions; revision++ {
					issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: project, WorkItemID: tracker.NativeWorkItemID(item), Number: 1 + index*2, Revision: tracker.Revision(revision), Profile: "native"}, Title: "migration " + name, Body: coreImportBody(), State: "Todo", Labels: []string{"needs-review"}, Assignees: []string{"example-assignee"}, Actor: tracker.Actor{Kind: "human", PrincipalID: "example-owner"}, CreatedAt: parseTestTime(t, "2026-10-01T00:00:00Z"), UpdatedAt: parseTestTime(t, "2026-10-02T00:00:00Z"), LastActivityAt: parseTestTime(t, "2026-10-02T00:00:00Z")}
					if revision > 1 {
						issue.Body = strings.Repeat("Large retained body. 雪\n", 100) + "\n## Migration context\n" + urls[item]
					}
					if revision%2 == 0 {
						issue.State = "In Progress"
					}
					comment := tracker.NativeComment{ID: commentID, OrganizationID: org, ProjectID: project, WorkItemID: issue.WorkItemID, Revision: tracker.Revision(revision), Sequence: 1, Body: fmt.Sprintf("legacy comment %d 雪", revision), Actor: tracker.Actor{Kind: "human", PrincipalID: "example-commenter"}, CreatedAt: issue.CreatedAt, UpdatedAt: issue.UpdatedAt}
					for _, record := range []struct {
						id    string
						value any
					}{{item, issue}, {commentID, comment}} {
						raw := historyTestJSON(t, record.value)
						if record.id == commentID && revision == 2 {
							raw = append(append([]byte("  "), raw...), '\n')
						}
						original[record.id][revision] = raw
						rawBytes, versionCount = rawBytes+int64(len(raw)), versionCount+1
						encoded, encoding, err := neutralimport.EncodeVersion(previous[record.id], previousEncoding[record.id], raw, revision)
						if err != nil {
							t.Fatal(err)
						}
						if (revision == 1 || revision%neutralimport.VersionSnapshotInterval == 0) && encoding != "snapshot" || record.id == item && revision > 2 && revision%neutralimport.VersionSnapshotInterval != 0 && encoding != "patch" {
							t.Fatalf("source %s revision %d lost checkpoint/delta encoding: %s", record.id, revision, encoding)
						}
						previous[record.id], previousEncoding[record.id] = encoded, encoding
						source.Add("collaboration_versions", map[string]any{"organization_id": org, "project_id": project, "work_item_id": item, "record_id": record.id, "revision": revision, "encoding": encoding, "record_json": string(encoded)})
					}
					if revision == revisions {
						source.Set("issues", map[string]string{"native_id": item}, map[string]any{"title": issue.Title, "body": issue.Body, "revision": revisions, "event_sequence": 1})
						source.Set("native_comments", map[string]string{"id": commentID}, map[string]any{"revision": revisions, "body": comment.Body, "updated_at": hubapi.FormatHubTime(comment.UpdatedAt)})
					}
				}
			}
			input := coreimport.CoreWorkItemImportSource{BundlePath: source.Write(), OrganizationID: org, ProjectID: project, Objects: f.service.runDetailStore()}
			manifest, err := os.ReadFile(filepath.Join(input.BundlePath, "manifest.json"))
			if err != nil {
				t.Fatal(err)
			}
			type accounting struct{ records, versions, bytes, charged int64 }
			readAccounting := func() accounting {
				t.Helper()
				var value accounting
				if err := admin.QueryRowContext(t.Context(), "SELECT (SELECT value FROM hosted_usage_counters WHERE organization_id=$1 AND name='history_records'),count(*),COALESCE(sum(payload_bytes),0) FROM collaboration_versions WHERE organization_id=$1 AND project_id=$2", org, project).Scan(&value.records, &value.versions, &value.bytes); err != nil {
					t.Fatal(err)
				}
				if err := f.service.config.TenantRepository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
					var err error
					value.charged, err = r.CollaborationBytes(t.Context())
					return err
				}); err != nil {
					t.Fatal(err)
				}
				return value
			}
			before := readAccounting()
			assertEmpty := func() {
				t.Helper()
				var rows int
				if err := admin.QueryRowContext(t.Context(), "SELECT (SELECT count(*) FROM issues WHERE organization_id=$1 AND project_id=$2)+(SELECT count(*) FROM native_comments WHERE organization_id=$1 AND project_id=$2)+(SELECT count(*) FROM collaboration_versions WHERE organization_id=$1 AND project_id=$2)+(SELECT count(*) FROM collaboration_events WHERE organization_id=$1 AND project_id=$2)", org, project).Scan(&rows); err != nil || rows != 0 {
					t.Fatalf("failed conversion retained rows=%d error=%v", rows, err)
				}
				if after := readAccounting(); after != before {
					t.Fatalf("failed conversion changed accounting: before=%+v after=%+v", before, after)
				}
			}
			if fail {
				assertEmpty()
				if _, err := admin.ExecContext(t.Context(), fmt.Sprintf(`CREATE FUNCTION reject_test_collaboration_import() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.record_id='wi_core_import' AND NEW.revision=%d THEN RAISE EXCEPTION 'injected final version failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_test_collaboration_import BEFORE INSERT ON collaboration_versions FOR EACH ROW EXECUTE FUNCTION reject_test_collaboration_import()`, revisions)); err != nil {
					t.Fatal(err)
				}
				_, err = coreimport.ImportCoreWorkItems(t.Context(), f.service.database.postgres, input)
				var databaseError interface {
					error
					SQLState() string
				}
				if !errors.As(err, &databaseError) || databaseError.SQLState() != "P0001" || !strings.Contains(databaseError.Error(), "injected final version failure") {
					t.Fatalf("final persistence failure=%v", err)
				}
				assertEmpty()
				if _, err := admin.ExecContext(t.Context(), "DROP TRIGGER reject_test_collaboration_import ON collaboration_versions; DROP FUNCTION reject_test_collaboration_import()"); err != nil {
					t.Fatal(err)
				}
			}
			if count, err := coreimport.ImportCoreWorkItems(t.Context(), f.service.database.postgres, input); err != nil || count != 2 {
				t.Fatalf("history conversion count=%d error=%v", count, err)
			}
			content := eventstore.NewContent(f.service.runDetailStore())
			var payloadBytes int64
			for record, revisions := range original {
				for revision, expected := range revisions {
					var version eventstore.CollaborationVersion
					if err := f.service.config.TenantRepository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
						var err error
						version, err = r.CollaborationVersion(t.Context(), record, revision)
						return err
					}); err != nil {
						t.Fatal(err)
					}
					raw, err := content.CollaborationVersion(t.Context(), version)
					if err != nil || !bytes.Equal(raw, expected) {
						t.Fatalf("migration changed %s revision %d: %v", owners[record], revision, err)
					}
					payloadBytes += version.PayloadBytes
					if record == owners[record] && revision > 1 {
						var issue tracker.NativeIssue
						if err := json.Unmarshal(raw, &issue); err != nil || githubcontract.MigrationSourceURL(issue.Body) != urls[record] {
							t.Fatalf("migration changed imported URLs: %v", err)
						}
					}
				}
			}
			after := readAccounting()
			if after.records-before.records != versionCount+2 || after.versions-before.versions != versionCount || after.bytes-before.bytes != payloadBytes || payloadBytes >= rawBytes {
				t.Fatalf("accounting before=%+v after=%+v payload=%d raw=%d records=%d", before, after, payloadBytes, rawBytes, versionCount)
			}
			if count, err := coreimport.ImportCoreWorkItems(t.Context(), f.service.database.postgres, input); err != nil || count != 2 {
				t.Fatalf("source replay count=%d error=%v", count, err)
			}
			if replay := readAccounting(); replay != after {
				t.Fatalf("source replay changed accounting: before=%+v after=%+v", after, replay)
			}
			retainedManifest, err := os.ReadFile(filepath.Join(input.BundlePath, "manifest.json"))
			if err != nil || !bytes.Equal(manifest, retainedManifest) {
				t.Fatalf("conversion changed verified source manifest: %v", err)
			}
			for _, statement := range []string{"UPDATE collaboration_versions SET payload_object_key=payload_object_key WHERE organization_id=$1 AND project_id=$2", "DELETE FROM collaboration_versions WHERE organization_id=$1 AND project_id=$2"} {
				_, err := admin.ExecContext(t.Context(), statement, org, project)
				var state interface{ SQLState() string }
				if !errors.As(err, &state) || state.SQLState() != "23514" {
					t.Fatalf("history mutation result=%v", err)
				}
			}
		})
	}
}
