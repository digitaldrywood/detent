package hubserver

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	hubmigrations "github.com/digitaldrywood/detent/internal/hubserver/migrations"
	"github.com/digitaldrywood/detent/internal/issuecontract"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeVersionRoundTrip(t *testing.T) {
	f := newDefaultNativeFixture(t, Config{})
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: apiCredential{Scope: apiScopeAdmin}}
	for _, kind := range []string{"issue", "comment"} {
		t.Run(kind, func(t *testing.T) {
			issue := f.create(t, "history "+kind)
			related := f.create(t, "dependency "+kind)
			recordID := string(issue.WorkItemID)
			if kind == "comment" {
				recordID = "comment-" + recordID
			}
			const count = 2*nativeVersionSnapshotInterval + 7
			expected := make(map[int64][]byte)
			now := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
			tx, err := f.service.database.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			for revision := int64(1); revision <= count; revision++ {
				if revision == 1 && kind == "issue" {
					var raw []byte
					if err := tx.QueryRowContext(t.Context(), "SELECT record_json FROM collaboration_versions WHERE organization_id=? AND record_id=? AND revision=1", scope.organization, recordID).Scan(&raw); err != nil {
						t.Fatal(err)
					}
					var original tracker.NativeIssue
					if err := json.Unmarshal(raw, &original); err != nil {
						t.Fatal(err)
					}
					original.WebURL = ""
					expected[revision], err = json.Marshal(original)
					if err != nil {
						t.Fatal(err)
					}
					continue
				}
				stamp := now.Add(time.Duration(revision) * time.Second)
				var record any
				data := tracker.CollaborationData{Revision: tracker.Revision(revision)}
				if kind == "issue" {
					value := issue
					value.Revision, value.UpdatedAt = tracker.Revision(revision), stamp
					value.Body = strings.Repeat("Retained body content. ", 256) + fmt.Sprintf("body generation %d <>& 雪", revision/4)
					value.Labels, value.Assignees = []string{fmt.Sprintf("label-%d", revision%3)}, []string{fmt.Sprintf("member-%d", revision%2)}
					value.IssueContract = &issuecontract.State{ConfirmedSections: map[string]string{"acceptance criteria": strconv.FormatInt(revision, 10)}, RecordedAt: stamp}
					if revision%2 == 0 {
						value.Dependencies = []tracker.NativeWorkItemID{related.WorkItemID}
						value.Blockers = []tracker.NativeDependency{{ID: related.WorkItemID}}
						value.Verification = &tracker.NativeVerification{State: tracker.VerificationPending}
					}
					record = value
				} else {
					record = tracker.NativeComment{ID: recordID, OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: issue.WorkItemID, Revision: tracker.Revision(revision), Sequence: 1, Body: fmt.Sprintf("comment revision %d <>& 雪", revision), CreatedAt: now, UpdatedAt: stamp}
					data.CommentID = recordID
				}
				raw, err := json.Marshal(record)
				if err != nil {
					t.Fatal(err)
				}
				expected[revision] = raw
				var versionReads int
				ctx := withRequestTiming(t.Context(), &requestTiming{trace: func(query string, _ []driver.NamedValue) {
					if strings.Contains(query, "SELECT") && strings.Contains(query, "collaboration_versions") {
						versionReads++
					}
				}})
				if err := recordNativeChange(ctx, tx, scope, record, string(issue.WorkItemID), tracker.Revision(revision), "issue.edited", data, stamp); err != nil {
					t.Fatal(err)
				}
				if versionReads > 1 {
					t.Fatalf("revision %d read history %d times", revision, versionReads)
				}
			}
			if err := f.service.database.commit(t.Context(), tx); err != nil {
				t.Fatal(err)
			}
			for revision := int64(1); revision <= count; revision++ {
				raw, err := reconstructNativeVersion(t.Context(), f.service.database.reader, string(scope.organization), string(scope.project), string(issue.WorkItemID), recordID, revision)
				if err != nil {
					t.Fatal(err)
				}
				var expectedStored []byte
				if kind == "issue" && revision == 1 {
					if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT record_json FROM collaboration_versions WHERE organization_id=? AND record_id=? AND revision=1", scope.organization, recordID).Scan(&expectedStored); err != nil {
						t.Fatal(err)
					}
				} else {
					expectedStored = expected[revision]
				}
				if !bytes.Equal(raw, expectedStored) {
					t.Fatalf("revision %d did not reconstruct exact snapshot bytes", revision)
				}
				comment := ""
				if kind == "comment" {
					comment = recordID
				}
				read, err := f.service.readVersion(t.Context(), scope, string(issue.WorkItemID), comment, revision)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(read, expected[revision]) {
					t.Fatalf("revision %d response mismatch\ngot %s\nwant %s", revision, read, expected[revision])
				}
				var encoding string
				if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT encoding FROM collaboration_versions WHERE organization_id=? AND record_id=? AND revision=?", scope.organization, recordID, revision).Scan(&encoding); err != nil {
					t.Fatal(err)
				}
				if (revision == 1 || revision%nativeVersionSnapshotInterval == 0) && encoding != "snapshot" {
					t.Fatalf("revision %d did not store a checkpoint snapshot", revision)
				}
				var stored int
				if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT length(CAST(record_json AS BLOB)) FROM collaboration_versions WHERE organization_id=? AND record_id=? AND revision=?", scope.organization, recordID, revision).Scan(&stored); err != nil {
					t.Fatal(err)
				}
				if stored > len(expectedStored) {
					t.Fatalf("revision %d encoding grew the record", revision)
				}
			}
			if _, err := f.service.readVersion(t.Context(), scope, string(issue.WorkItemID), "", count+1); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("missing revision error=%v", err)
			}
			ref, section := publishTestSection(t, f, string(issue.WorkItemID), "revision", recordID, 1, count, archiveTestTable(t, f.service, "collaboration_versions", "record_id=?", recordID))
			if _, err := f.service.database.db.ExecContext(t.Context(), "DROP TRIGGER IF EXISTS collaboration_versions_no_delete"); err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM collaboration_versions WHERE record_id=?", recordID); err != nil {
				t.Fatal(err)
			}
			fetches := 0
			f.service.config.ArchiveReader = func(_ context.Context, got attachment.ArchiveReference, _ bool) (entryauthority.ArchiveResponse, error) {
				fetches++
				if got != ref {
					t.Fatal("wrong revision archive")
				}
				return entryauthority.ArchiveResponse{Data: section}, nil
			}
			for _, revision := range []int64{1, 2, nativeVersionSnapshotInterval - 1, nativeVersionSnapshotInterval, nativeVersionSnapshotInterval + 1, count} {
				comment := ""
				if kind == "comment" {
					comment = recordID
				}
				ctx := f.service.archiveContext(t.Context(), scope, false)
				before := fetches
				got, err := f.service.readVersion(ctx, scope, string(issue.WorkItemID), comment, revision)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got, expected[revision]) || fetches != before+1 {
					t.Fatalf("archived %s revision %d changed or fetched more than one section", kind, revision)
				}
			}

		})
	}
}

func TestNativeTransitionHistoryBytes(t *testing.T) {
	f := newDefaultNativeFixture(t, Config{})
	issue := f.create(t, "transition history")
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: apiCredential{Scope: apiScopeAdmin}}
	now := time.Now().UTC()
	tx, err := f.service.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	issue, _, err = readNativeIssue(t.Context(), tx, scope, string(issue.WorkItemID))
	if err != nil {
		t.Fatal(err)
	}
	issue.Body = strings.Repeat("Large unchanged issue body. 雪\n", 1024) + issueContractTestSections
	issue, err = persistNativeIssue(t.Context(), tx, scope, issue, "issue.edited", tracker.CollaborationData{Fields: []string{"body"}}, now)
	if err != nil {
		t.Fatal(err)
	}
	snapshots := make(map[tracker.Revision]string)
	for i := range 513 {
		from := issue.State
		issue.State = "In Progress"
		if from == issue.State {
			issue.State = "Todo"
		}
		issue, err = persistNativeIssue(t.Context(), tx, scope, issue, "workflow.transitioned", tracker.CollaborationData{FromState: from, ToState: issue.State}, now.Add(time.Duration(i+1)*24*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		snapshots[issue.Revision], err = marshalNative(issue)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := f.service.database.commit(t.Context(), tx); err != nil {
		t.Fatal(err)
	}
	var fullBytes, storedBytes int
	for revision, expected := range snapshots {
		actual, err := reconstructNativeVersion(t.Context(), f.service.database.reader, string(scope.organization), string(scope.project), string(issue.WorkItemID), string(issue.WorkItemID), int64(revision))
		if err != nil {
			t.Fatal(err)
		}
		if string(actual) != expected {
			t.Fatalf("transition revision %d changed", revision)
		}
		var size int
		var encoding string
		if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT length(CAST(record_json AS BLOB)),encoding FROM collaboration_versions WHERE organization_id=? AND record_id=? AND revision=?", scope.organization, issue.WorkItemID, revision).Scan(&size, &encoding); err != nil {
			t.Fatal(err)
		}
		if encoding == "patch" && size >= 4096 {
			t.Fatalf("patch repeated body: %d bytes", size)
		}
		storedBytes += size
		fullBytes += len(expected)
	}
	if storedBytes >= fullBytes/8 {
		t.Fatalf("transition history=%d bytes, full snapshots=%d", storedBytes, fullBytes)
	}
	t.Logf("513 transitions: snapshots=%d stored=%d reduction=%.1f%%", fullBytes, storedBytes, 100*(1-float64(storedBytes)/float64(fullBytes)))
}

func TestNativeNoOpRevisions(t *testing.T) {
	f := newDefaultNativeFixture(t, Config{})
	issue := f.create(t, "no-op edits")
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: apiCredential{Scope: apiScopeAdmin}}
	tests := []struct {
		name, event string
		data        tracker.CollaborationData
	}{
		{"identical edit", "issue.edited", tracker.CollaborationData{Fields: []string{"title", "body", "type", "labels", "assignees", "priority"}}},
		{"same release reference", "issue.edited", tracker.CollaborationData{Change: &tracker.NativeChangeReference{ChangeID: "change-example", VersionID: "version-example"}}},
		{"same state", "workflow.transitioned", tracker.CollaborationData{FromState: "Todo", ToState: "Todo"}},
		{"same related issues", "issue.edited", tracker.CollaborationData{Fields: []string{"related"}}},
		{"same dependencies", "dependency.changed", tracker.CollaborationData{Operation: "remove"}},
		{"same source intake", "github.imported", tracker.CollaborationData{Operation: "source_intake"}},
		{"same verification", "issue.edited", tracker.CollaborationData{Fields: []string{"verification"}}},
		{"same archive", "issue.edited", tracker.CollaborationData{Fields: []string{"archived"}}},
		{"same preferences", "issue.edited", tracker.CollaborationData{Fields: []string{"body"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tx, err := f.service.database.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			current, _, err := readNativeIssue(t.Context(), tx, scope, string(issue.WorkItemID))
			if err != nil {
				t.Fatal(err)
			}
			var beforeVersions, beforeEvents int
			if err := tx.QueryRowContext(t.Context(), "SELECT (SELECT count(*) FROM collaboration_versions),(SELECT count(*) FROM collaboration_events)").Scan(&beforeVersions, &beforeEvents); err != nil {
				t.Fatal(err)
			}
			var updates int
			ctx := withRequestTiming(t.Context(), &requestTiming{trace: func(query string, _ []driver.NamedValue) {
				if strings.HasPrefix(query, "UPDATE issues") {
					updates++
				}
			}})
			result, err := persistNativeIssue(ctx, tx, scope, current, test.event, test.data, time.Now().Add(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			var versions, events int
			if err := tx.QueryRowContext(t.Context(), "SELECT (SELECT count(*) FROM collaboration_versions),(SELECT count(*) FROM collaboration_events)").Scan(&versions, &events); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(result, current) || versions != beforeVersions || events != beforeEvents || updates != 0 {
				t.Fatalf("no-op changed record or history: versions %d/%d events %d/%d updates %d", versions, beforeVersions, events, beforeEvents, updates)
			}
		})
	}
	path := f.base + "/work-items/" + string(issue.WorkItemID)
	states := nativeFixtureStates()
	states[0].Transitions = append(states[0].Transitions, "Todo")
	rawStates, err := json.Marshal(states)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET states_json=? WHERE id=?", string(rawStates), f.project.ID); err != nil {
		t.Fatal(err)
	}
	for _, reason := range []string{"worker_progress", "dependency_ready", "user_requested"} {
		t.Run("same state command "+reason, func(t *testing.T) {
			var beforeVersions, beforeEvents int
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT (SELECT count(*) FROM collaboration_versions WHERE work_item_id=?),(SELECT count(*) FROM collaboration_events WHERE work_item_id=?)", issue.WorkItemID, issue.WorkItemID).Scan(&beforeVersions, &beforeEvents); err != nil {
				t.Fatal(err)
			}
			for index := range 2 {
				response := performHubAPIRequest(t, f.service, http.MethodPost, path+"/workflow", f.token, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("same-state-%s-%d", reason, index)}, ExpectedRevision: issue.Revision, State: issue.State, Reason: reason})
				requireNativeStatus(t, response, http.StatusOK)
				var result tracker.NativeIssue
				decodeHubResponse(t, response, &result)
				if result.Revision != issue.Revision || !result.UpdatedAt.Equal(issue.UpdatedAt) {
					t.Fatalf("same-state command changed revision or timestamp: %+v", result)
				}
			}
			var versions, events int
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT (SELECT count(*) FROM collaboration_versions WHERE work_item_id=?),(SELECT count(*) FROM collaboration_events WHERE work_item_id=?)", issue.WorkItemID, issue.WorkItemID).Scan(&versions, &events); err != nil {
				t.Fatal(err)
			}
			if versions != beforeVersions || events != beforeEvents {
				t.Fatalf("same-state command wrote history: versions=%d/%d events=%d/%d", versions, beforeVersions, events, beforeEvents)
			}
		})
	}
	var beforeVersions, beforeEvents int
	if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT (SELECT count(*) FROM collaboration_versions WHERE work_item_id=?),(SELECT count(*) FROM collaboration_events WHERE work_item_id=?)", issue.WorkItemID, issue.WorkItemID).Scan(&beforeVersions, &beforeEvents); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"title", "body", "labels", "assignees", "priority"} {
		request := map[string]any{"expected_revision": fmt.Sprint(issue.Revision), "idempotency_key": "identical-" + field, field: map[string]any{"title": issue.Title, "body": issue.Body, "labels": issue.Labels, "assignees": issue.Assignees, "priority": issue.Priority}[field]}
		response := performHubAPIRequest(t, f.service, http.MethodPatch, path, f.token, request)
		requireNativeStatus(t, response, http.StatusOK)
		var result tracker.NativeIssue
		decodeHubResponse(t, response, &result)
		if result.Revision != issue.Revision {
			t.Fatalf("identical %s bumped revision", field)
		}
	}
	changed := "real title change"
	response := performHubAPIRequest(t, f.service, http.MethodPatch, path, f.token, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "real-edit"}, ExpectedRevision: issue.Revision, Title: &changed})
	requireNativeStatus(t, response, http.StatusOK)
	var result tracker.NativeIssue
	decodeHubResponse(t, response, &result)
	if result.Revision != issue.Revision+1 {
		t.Fatal("real edit did not advance once")
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
	if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT (SELECT count(*) FROM collaboration_versions WHERE work_item_id=?),(SELECT count(*) FROM collaboration_events WHERE work_item_id=?)", issue.WorkItemID, issue.WorkItemID).Scan(&versions, &events); err != nil {
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
	if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT (SELECT count(*) FROM collaboration_versions WHERE work_item_id=?),(SELECT count(*) FROM collaboration_events WHERE work_item_id=?)", issue.WorkItemID, issue.WorkItemID).Scan(&versions, &events); err != nil {
		t.Fatal(err)
	}
	if versions != beforeVersions+4 || events != beforeEvents+4 {
		t.Fatalf("real transition wrote extra history: versions=%d/%d events=%d/%d", versions, beforeVersions, events, beforeEvents)
	}
	t.Run("operator hold response and replay", func(t *testing.T) {
		now := time.Now().UTC().Add(time.Hour)
		states[1].Transitions = append(states[1].Transitions, "Blocked")
		states = append(states, tracker.NativeState{Name: "Blocked", Transitions: []string{"Blocked", "Todo"}})
		tx, err := f.service.database.db.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if err := applyNativeProjectStates(t.Context(), tx, scope, states, now); err != nil {
			t.Fatal(err)
		}
		if err := f.service.database.commit(t.Context(), tx); err != nil {
			t.Fatal(err)
		}
		clock := f.service.database.now
		f.service.database.now = func() time.Time { return now }
		t.Cleanup(func() { f.service.database.now = clock })
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
		if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT (SELECT count(*) FROM collaboration_versions WHERE work_item_id=?),(SELECT count(*) FROM collaboration_events WHERE work_item_id=?)", issue.WorkItemID, issue.WorkItemID).Scan(&holdVersions, &holdEvents); err != nil {
			t.Fatal(err)
		}
		if holdVersions != versions+1 || holdEvents != events+2 {
			t.Fatalf("operator hold or replay wrote extra history: versions=%d/%d events=%d/%d", holdVersions, versions, holdEvents, events)
		}
	})
}

func TestCollaborationDeltaMigration(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("verification_failure_%t", fail), func(t *testing.T) {
			f := newDefaultNativeFixture(t, Config{})
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: apiCredential{Scope: apiScopeAdmin}}
			original := map[string]map[int64]string{}
			owners := map[string]string{}
			for _, name := range []string{"first", "second"} {
				issue := f.create(t, "migration "+name)
				original[string(issue.WorkItemID)] = map[int64]string{}
				owners[string(issue.WorkItemID)] = string(issue.WorkItemID)
				for revision := int64(2); revision <= 2*nativeVersionSnapshotInterval+3; revision++ {
					issue.Revision = tracker.Revision(revision)
					issue.Body = strings.Repeat("Large retained body. 雪\n", 100) + fmt.Sprintf("\n## Migration context\nhttps://github.com/example-org/example-project/issues/%d", issue.Number)
					issue.State = "Todo"
					if revision%2 == 0 {
						issue.State = "In Progress"
					}
					raw, err := marshalNative(issue)
					if err != nil {
						t.Fatal(err)
					}
					original[string(issue.WorkItemID)][revision] = raw
					if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO collaboration_versions(organization_id,project_id,work_item_id,record_id,revision,record_json) VALUES(?,?,?,?,?,?)", scope.organization, scope.project, issue.WorkItemID, issue.WorkItemID, revision, raw); err != nil {
						t.Fatal(err)
					}
				}
				commentID := "legacy-comment-" + string(issue.WorkItemID)
				owners[commentID] = string(issue.WorkItemID)
				original[commentID] = map[int64]string{}
				for revision := int64(1); revision <= 2*nativeVersionSnapshotInterval+3; revision++ {
					comment := tracker.NativeComment{ID: commentID, OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: issue.WorkItemID, Revision: tracker.Revision(revision), Body: fmt.Sprintf("legacy comment %d 雪", revision)}
					raw, err := marshalNative(comment)
					if err != nil {
						t.Fatal(err)
					}
					if revision == 2 {
						raw = "  " + raw + "\n"
					}
					original[commentID][revision] = raw
					if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO collaboration_versions(organization_id,project_id,work_item_id,record_id,revision,record_json) VALUES(?,?,?,?,?,?)", scope.organization, scope.project, issue.WorkItemID, commentID, revision, raw); err != nil {
						t.Fatal(err)
					}
				}
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), "ALTER TABLE collaboration_versions DROP COLUMN encoding"); err != nil {
				t.Fatal(err)
			}
			var recordsBefore, bytesBefore int64
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT (SELECT value FROM hosted_usage_counters WHERE name='history_records'),(SELECT value FROM hosted_usage_counters WHERE name='collaboration_bytes')").Scan(&recordsBefore, &bytesBefore); err != nil {
				t.Fatal(err)
			}
			tx, err := f.service.database.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			urlsBefore := map[string][]string{}
			for record, item := range owners {
				if record != item {
					continue
				}
				urlsBefore[item], err = importedGitHubIssueURLs(t.Context(), tx, string(scope.project), item, "")
				if err != nil {
					t.Fatal(err)
				}
			}
			if fail {
				err = hubmigrations.CollaborationDeltas(t.Context(), tx, encodeNativeVersion, func(context.Context, *sql.Tx, string, string, string, string, int64) ([]byte, error) {
					return []byte("{}"), nil
				}, recountCollaborationBytes)
				if err == nil || !strings.Contains(err.Error(), "changed a revision") {
					t.Fatalf("corrupt conversion error=%v", err)
				}
				if err := tx.Rollback(); err != nil {
					t.Fatal(err)
				}
				var columns int
				if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM pragma_table_info('collaboration_versions') WHERE name='encoding'").Scan(&columns); err != nil || columns != 0 {
					t.Fatalf("failed migration retained column: %d %v", columns, err)
				}
			} else {
				if err := migrateCollaborationDeltas(t.Context(), tx); err != nil {
					t.Fatal(err)
				}
				var columnType string
				var required int
				if err := tx.QueryRowContext(t.Context(), "SELECT type, [notnull] FROM pragma_table_info('collaboration_versions') WHERE name='encoding'").Scan(&columnType, &required); err != nil || columnType != "TEXT" || required != 1 {
					t.Fatalf("encoding column type=%s required=%d: %v", columnType, required, err)
				}
				for record, revisions := range original {
					item := owners[record]
					for revision, expected := range revisions {
						raw, err := reconstructNativeVersion(t.Context(), tx, string(scope.organization), string(scope.project), item, record, revision)
						if err != nil || string(raw) != expected {
							t.Fatalf("migration changed %s revision %d: %v", item, revision, err)
						}
					}
					if record != item {
						continue
					}
					after, err := importedGitHubIssueURLs(t.Context(), tx, string(scope.project), item, "")
					if err != nil || !reflect.DeepEqual(after, urlsBefore[item]) {
						t.Fatalf("migration changed imported URLs: %v", err)
					}
				}
				if err := f.service.database.commit(t.Context(), tx); err != nil {
					t.Fatal(err)
				}
				var recordsAfter, bytesAfter, recount int64
				if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT (SELECT value FROM hosted_usage_counters WHERE name='history_records'),(SELECT value FROM hosted_usage_counters WHERE name='collaboration_bytes'),("+collaborationBytesRecount+")").Scan(&recordsAfter, &bytesAfter, &recount); err != nil {
					t.Fatal(err)
				}
				if recordsAfter != recordsBefore || bytesAfter != recount || bytesAfter >= bytesBefore {
					t.Fatalf("accounting records=%d/%d bytes=%d/%d recount=%d", recordsAfter, recordsBefore, bytesAfter, bytesBefore, recount)
				}
				t.Logf("migration fixture: %d -> %d collaboration bytes, %.1f%% reduction", bytesBefore, bytesAfter, 100*(1-float64(bytesAfter)/float64(bytesBefore)))
			}
			for _, statement := range []string{"UPDATE collaboration_versions SET record_json=record_json", "DELETE FROM collaboration_versions"} {
				if _, err := f.service.database.db.ExecContext(t.Context(), statement); err == nil || !strings.Contains(err.Error(), "collaboration history is append-only") {
					t.Fatalf("history mutation result=%v", err)
				}
			}
		})
	}
}
