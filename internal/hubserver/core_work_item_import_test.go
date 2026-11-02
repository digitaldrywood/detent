package hubserver

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestCoreWorkItemImportPreservesTypedHistoryAcrossTenantsAndReplays(t *testing.T) {
	repository := projectRecordPostgres(t)
	objects := &memoryRunDetails{}
	orgs := []tracker.OrganizationID{"org_import_first", "org_import_second"}
	for _, org := range orgs {
		project := tracker.ProjectID("prj_" + string(org))
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: "Example organization", Slug: "example-organization", CreatedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}); err != nil {
				return err
			}
			if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: org, ProviderID: "provider_" + string(org)}); err != nil {
				return err
			}
			_, err := w.CreateProjectRecord(t.Context(), projectstore.Record{
				Core:     corestore.Project{OrganizationID: org, ID: project, Name: "Example project", Slug: "example-project", Profile: "native", RequireDependencies: true},
				Settings: projectstore.Settings{OrganizationID: org, ProjectID: project, RepositoryEnabled: true},
				States:   nativeFixtureStates(),
			})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		path := coreWorkItemSourceCopy(t, org, project, false)
		source := CoreWorkItemImportSource{CopyPath: path, OrganizationID: org, ProjectID: project, Objects: objects}
		for range 2 {
			count, err := ImportCoreWorkItems(t.Context(), repository, source)
			if err != nil || count != 1 {
				t.Fatalf("organization %s import count=%d error=%v", org, count, err)
			}
		}
		if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			item, err := r.WorkItem(t.Context(), project, "wi_core_import")
			if err != nil {
				return err
			}
			if item.ID != 41 || item.Revision != 2 || item.EventSequence != 2 || item.Title != "Updated title" || item.WorkflowStateName == nil || *item.WorkflowStateName != "Todo" || item.BodyObjectKey == nil {
				return errors.New("typed work item did not preserve source identity and current fields")
			}
			if *item.BodyObjectKey == "" {
				return errors.New("work item body reference is empty")
			}
			objects.mu.Lock()
			var issueBody string
			if err := json.Unmarshal(objects.data[*item.BodyObjectKey], &issueBody); err != nil {
				objects.mu.Unlock()
				return err
			}
			objects.mu.Unlock()
			if issueBody != coreImportBody() {
				return errors.New("work item payload did not preserve source bytes")
			}
			comments, err := r.Comments(t.Context(), project, item.NativeID, 0, 10)
			if err != nil {
				return err
			}
			if len(comments) != 1 || comments[0].ID != "comment_core_import" || comments[0].Revision != 1 || comments[0].Sequence != 1 {
				return errors.New("typed comment did not preserve source identity and revision")
			}
			objects.mu.Lock()
			var commentBody string
			if err := json.Unmarshal(objects.data[comments[0].BodyObjectKey], &commentBody); err != nil {
				objects.mu.Unlock()
				return err
			}
			objects.mu.Unlock()
			if commentBody != "Example comment" {
				return errors.New("comment payload did not preserve source bytes")
			}
			versions := eventstore.NewContent(objects)
			currentVersion, err := r.CollaborationVersion(t.Context(), string(item.NativeID), 2)
			if err != nil {
				return err
			}
			currentRaw, err := versions.CollaborationVersion(t.Context(), currentVersion)
			if err != nil {
				return err
			}
			var current tracker.NativeIssue
			if err := json.Unmarshal(currentRaw, &current); err != nil || current.Title != "Updated title" || current.Body != coreImportBody() {
				return errors.New("reconstructed current issue version lost source fields")
			}
			events, err := r.Events(t.Context(), project, item.NativeID, 0, 10)
			if err != nil {
				return err
			}
			if len(events) != 2 || events[0].Sequence != 1 || events[1].Sequence != 2 {
				return errors.New("collaboration events did not preserve source sequence")
			}
			eventContent := eventstore.NewContent(objects)
			currentEvent, err := eventContent.Event(t.Context(), events[1])
			if err != nil || currentEvent.Data.Revision != 2 || currentEvent.Type != "issue.edited" {
				return errors.New("collaboration event payload did not preserve source fields")
			}
			facts, err := r.WorkItemFacts(t.Context(), project, item.NativeID)
			if err != nil {
				return err
			}
			if facts.Author != "example-author" || facts.Repository != "example-org/example-project" || len(facts.Assignees) != 1 || facts.Assignees[0] != "example-assignee" {
				return errors.New("claim facts did not preserve source fields")
			}
			labels, err := r.WorkItemLabels(t.Context(), project, item.NativeID, corestore.LabelCursor{}, 10)
			if err != nil {
				return err
			}
			if len(labels.Items) != 1 || labels.Items[0] != "needs-review" {
				return errors.New("typed labels did not preserve source fields")
			}
			queue, err := r.QueueEntry(t.Context(), project, item.NativeID, "native")
			if err != nil {
				return err
			}
			if queue.State != "Todo" || queue.Rank != "010" || queue.Priority == nil || *queue.Priority != 1 {
				return errors.New("queue entry did not preserve source fields")
			}
			return nil
		}); err != nil {
			t.Fatalf("organization %s read imported data: %v", org, err)
		}
	}
}

func TestCoreWorkItemImportRejectsChangedCopyWhenAllIssuesWereDeleted(t *testing.T) {
	repository := projectRecordPostgres(t)
	objects := &memoryRunDetails{}
	org := tracker.OrganizationID("org_import_empty_changed")
	project := tracker.ProjectID("prj_import_empty_changed")
	if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: "Example organization", Slug: "example-empty-changed", CreatedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}); err != nil {
			return err
		}
		if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: org, ProviderID: "provider_" + string(org)}); err != nil {
			return err
		}
		_, err := w.CreateProjectRecord(t.Context(), projectstore.Record{
			Core:     corestore.Project{OrganizationID: org, ID: project, Name: "Example project", Slug: "example-empty-changed", Profile: "native", RequireDependencies: true},
			Settings: projectstore.Settings{OrganizationID: org, ProjectID: project},
			States:   nativeFixtureStates(),
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	original := coreWorkItemSourceCopy(t, org, project, false)
	source := CoreWorkItemImportSource{CopyPath: original, OrganizationID: org, ProjectID: project, Objects: objects}
	if count, err := ImportCoreWorkItems(t.Context(), repository, source); err != nil || count != 1 {
		t.Fatalf("initial import count=%d error=%v", count, err)
	}

	changedPath := filepath.Join(t.TempDir(), "empty-source.db")
	contents, err := os.ReadFile(original)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(changedPath, contents, 0600); err != nil {
		t.Fatal(err)
	}
	changed, err := sql.Open("sqlite", changedPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := changed.ExecContext(t.Context(), `DELETE FROM issues`); err != nil {
		_ = changed.Close()
		t.Fatal(err)
	}
	if err := changed.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(changedPath, 0400); err != nil {
		t.Fatal(err)
	}
	source.CopyPath = changedPath
	if _, err := ImportCoreWorkItems(t.Context(), repository, source); !errors.Is(err, mutation.ErrConflict) {
		t.Fatalf("changed empty copy error=%v", err)
	}
}

func TestCoreWorkItemImportRollsBackMalformedQueue(t *testing.T) {
	repository := projectRecordPostgres(t)
	objects := &memoryRunDetails{}
	org := tracker.OrganizationID("org_import_malformed")
	project := tracker.ProjectID("prj_import_malformed")
	if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: "Example organization", Slug: "example-malformed", CreatedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}); err != nil {
			return err
		}
		if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: org, ProviderID: "provider_" + string(org)}); err != nil {
			return err
		}
		_, err := w.CreateProjectRecord(t.Context(), projectstore.Record{
			Core:     corestore.Project{OrganizationID: org, ID: project, Name: "Example project", Slug: "example-malformed", Profile: "native", RequireDependencies: true},
			Settings: projectstore.Settings{OrganizationID: org, ProjectID: project},
			States:   nativeFixtureStates(),
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	source := CoreWorkItemImportSource{CopyPath: coreWorkItemSourceCopy(t, org, project, true), OrganizationID: org, ProjectID: project, Objects: objects}
	if _, err := ImportCoreWorkItems(t.Context(), repository, source); err == nil {
		t.Fatal("malformed queue completed import")
	}
	if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
		if _, err := r.WorkItem(t.Context(), project, "wi_core_import"); !errors.Is(err, corestore.ErrNotFound) {
			return errors.New("failed import retained the issue row")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func coreWorkItemSourceCopy(t *testing.T, org tracker.OrganizationID, project tracker.ProjectID, malformedQueue bool) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "source.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, statement := range []string{
		`CREATE TABLE issues(id INTEGER,native_id TEXT,project_id TEXT,number INTEGER,revision INTEGER,event_sequence INTEGER,native_source_key TEXT,title TEXT,body TEXT,issue_contract_json TEXT,report_context_json TEXT,actor_json TEXT,provenance_json TEXT,archived INTEGER,created_at TEXT,updated_at TEXT,native_created_at TEXT,native_updated_at TEXT,last_activity_at TEXT,workflow_state_id INTEGER,labels_json TEXT,assignees_json TEXT,author_login TEXT,repository_id INTEGER,dispatch_wait_since TEXT,organization_id TEXT)`,
		`CREATE TABLE workflow_states(id INTEGER,detent_state TEXT)`,
		`CREATE TABLE repositories(id INTEGER,github_owner TEXT,github_name TEXT)`,
		`CREATE TABLE queue_entries(id INTEGER,issue_id INTEGER,scope TEXT,state TEXT,rank TEXT,priority_override INTEGER,created_at TEXT,updated_at TEXT)`,
		`CREATE TABLE native_comments(id TEXT,revision INTEGER,sequence INTEGER,body TEXT,actor_json TEXT,edited_by_json TEXT,provenance_json TEXT,source_key TEXT,deleted INTEGER,created_at TEXT,updated_at TEXT,organization_id TEXT,project_id TEXT,work_item_id TEXT)`,
		`CREATE TABLE collaboration_versions(organization_id TEXT,project_id TEXT,work_item_id TEXT,record_id TEXT,revision INTEGER,encoding TEXT,record_json TEXT)`,
		`CREATE TABLE collaboration_events(id TEXT,sequence INTEGER,type TEXT,schema_version INTEGER,actor_json TEXT,data_json TEXT,event_revision INTEGER,recorded_at TEXT,organization_id TEXT,project_id TEXT,work_item_id TEXT)`,
	} {
		if _, err := db.ExecContext(t.Context(), statement); err != nil {
			t.Fatal(err)
		}
	}
	const created = "2026-10-01T00:00:00Z"
	const updated = "2026-10-02T00:00:00Z"
	if _, err := db.ExecContext(t.Context(), `INSERT INTO workflow_states VALUES(1,'Todo')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO repositories VALUES(1,'example-org','example-project')`); err != nil {
		t.Fatal(err)
	}
	actor := tracker.Actor{Kind: "human", PrincipalID: "example-owner"}
	actorJSON, err := json.Marshal(actor)
	if err != nil {
		t.Fatal(err)
	}
	base := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: project, WorkItemID: "wi_core_import", Number: 1, Revision: 1, Profile: "native"}, Title: "Initial title", Body: coreImportBody(), State: "Todo", Labels: []string{"needs-review"}, Assignees: []string{"example-assignee"}, Actor: actor, CreatedAt: parseTestTime(t, created), UpdatedAt: parseTestTime(t, created), LastActivityAt: parseTestTime(t, created)}
	baseRaw, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	baseKeys, baseFields, err := nativeVersionObject(baseRaw)
	if err != nil {
		t.Fatal(err)
	}
	baseRaw, err = nativeVersionObjectBytes(baseKeys, baseFields)
	if err != nil {
		t.Fatal(err)
	}
	current := base
	current.Revision = 2
	current.Title = "Updated title"
	current.UpdatedAt = parseTestTime(t, updated)
	current.LastActivityAt = parseTestTime(t, updated)
	currentRaw, err := json.Marshal(current)
	if err != nil {
		t.Fatal(err)
	}
	currentKeys, currentFields, err := nativeVersionObject(currentRaw)
	if err != nil {
		t.Fatal(err)
	}
	currentRaw, err = nativeVersionObjectBytes(currentKeys, currentFields)
	if err != nil {
		t.Fatal(err)
	}
	patch, encoding, err := encodeNativeVersion(baseRaw, "snapshot", currentRaw, 2)
	if err != nil || encoding != "patch" {
		t.Fatalf("encode source history encoding=%q error=%v", encoding, err)
	}
	for _, version := range []struct {
		revision int
		encoding string
		raw      []byte
	}{{1, "snapshot", baseRaw}, {2, encoding, patch}} {
		if _, err := db.ExecContext(t.Context(), `INSERT INTO collaboration_versions VALUES(?,?,?,?,?,?,?)`, org, project, "wi_core_import", "wi_core_import", version.revision, version.encoding, string(version.raw)); err != nil {
			t.Fatal(err)
		}
	}
	commentActor, _ := json.Marshal(tracker.Actor{Kind: "human", PrincipalID: "example-commenter"})
	comment := tracker.NativeComment{ID: "comment_core_import", OrganizationID: org, ProjectID: project, WorkItemID: "wi_core_import", Revision: 1, Sequence: 1, Body: "Example comment", Actor: tracker.Actor{Kind: "human", PrincipalID: "example-commenter"}, CreatedAt: parseTestTime(t, created), UpdatedAt: parseTestTime(t, created)}
	commentRaw, err := json.Marshal(comment)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO native_comments VALUES(?,1,1,'Example comment',?,NULL,NULL,NULL,0,?,?,?, ?,?)`, comment.ID, string(commentActor), created, created, org, project, "wi_core_import"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO collaboration_versions VALUES(?,?,?,?,?,?,?)`, org, project, "wi_core_import", comment.ID, 1, "snapshot", string(commentRaw)); err != nil {
		t.Fatal(err)
	}
	queueState := "Todo"
	if malformedQueue {
		queueState = ""
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO queue_entries VALUES(41,41,'native',?,'010',1,?,?)`, queueState, created, updated); err != nil {
		t.Fatal(err)
	}
	for sequence, name := range []string{"issue.created", "issue.edited"} {
		data, _ := json.Marshal(tracker.CollaborationData{Revision: tracker.Revision(sequence + 1)})
		if _, err := db.ExecContext(t.Context(), `INSERT INTO collaboration_events VALUES(?,?,?,?,?,?,?,?,?,?,?)`, "event_core_import_"+string(rune('1'+sequence)), sequence+1, name, 1, string(actorJSON), string(data), sequence+1, []string{created, updated}[sequence], org, project, "wi_core_import"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO issues VALUES(41,'wi_core_import',?,1,2,2,NULL,'Updated title',?,NULL,NULL,?,NULL,0,?,?,?, ?,?,1,'["needs-review"]','["example-assignee"]','example-author',1,NULL,?)`, project, coreImportBody(), string(actorJSON), created, updated, created, updated, updated, org); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0400); err != nil {
		t.Fatal(err)
	}
	return path
}

func coreImportBody() string {
	return "Example issue body " + strings.Repeat("x", 8192)
}

func parseTestTime(t *testing.T, raw string) time.Time {
	t.Helper()
	value, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		t.Fatal(err)
	}
	return value
}
