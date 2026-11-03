package hubserver

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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
		originalPath := coreWorkItemSourceCopy(t, org, project, false)
		contents, err := os.ReadFile(originalPath)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "report-source.db")
		if err := os.WriteFile(path, contents, 0600); err != nil {
			t.Fatal(err)
		}
		copyDB, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		var actor, recorded string
		if err := copyDB.QueryRowContext(t.Context(), `SELECT actor_json,recorded_at FROM collaboration_events WHERE sequence=2`).Scan(&actor, &recorded); err != nil {
			t.Fatal(err)
		}
		runData, err := json.Marshal(tracker.CollaborationData{Revision: 2, Run: &tracker.NativeRunData{AttemptID: "attempt_report_import", RunID: "run_report_import", Sequence: 1, Outcome: "failed", TerminalFailure: &tracker.NativeTerminalFailure{Error: "hub request timed out"}}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := copyDB.ExecContext(t.Context(), `INSERT INTO collaboration_events VALUES(?,?,?,?,?,?,?,?,?,?,?)`, "event_report_import", 3, "run.finished", 1, actor, string(runData), 2, recorded, org, project, "wi_core_import"); err != nil {
			t.Fatal(err)
		}
		if _, err := copyDB.ExecContext(t.Context(), `UPDATE issues SET event_sequence=3`); err != nil {
			t.Fatal(err)
		}
		if err := copyDB.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0444); err != nil {
			t.Fatal(err)
		}
		source := CoreWorkItemImportSource{CopyPath: path, OrganizationID: org, ProjectID: project, Objects: objects}
		for range 2 {
			count, err := ImportCoreWorkItems(t.Context(), repository, source)
			if err != nil || count != 1 {
				t.Fatalf("organization %s import count=%d error=%v", org, count, err)
			}
		}
		var projectionDigest string
		for range 2 {
			parity, err := ImportCoreReportEvents(t.Context(), repository, source)
			if err != nil || parity.Events != 1 || parity.AttemptObservations != 1 || parity.Reworks != 0 || parity.SourceSHA256 == "" || parity.ProjectionSHA256 == "" {
				t.Fatalf("source report parity = %+v error %v", parity, err)
			}
			if projectionDigest != "" && projectionDigest != parity.ProjectionSHA256 {
				t.Fatal("source report projection changed across idempotent import")
			}
			projectionDigest = parity.ProjectionSHA256
		}
		if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			item, err := r.WorkItem(t.Context(), project, "wi_core_import")
			if err != nil {
				return err
			}
			if item.ID != 41 || item.Revision != 2 || item.EventSequence != 3 || item.Title != "Updated title" || item.WorkflowStateName == nil || *item.WorkflowStateName != "Todo" || item.BodyObjectKey == nil {
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
			if len(events) != 3 || events[0].Sequence != 1 || events[1].Sequence != 2 || events[2].Sequence != 3 {
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

func TestCoreIssueDependencyImportWaitsForCrossProjectIssuesAndReplays(t *testing.T) {
	repository, admin := projectRecordPostgresWithAdmin(t, 2)
	objects := &memoryRunDetails{}
	org := tracker.OrganizationID("org_import_dependency")
	dependentProject := tracker.ProjectID("prj_import_dependency_a")
	blockerProject := tracker.ProjectID("prj_import_dependency_b")
	if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: "Example organization", Slug: "example-dependency", CreatedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}); err != nil {
			return fmt.Errorf("create access organization: %w", err)
		}
		if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: org, ProviderID: "provider_" + string(org)}); err != nil {
			return fmt.Errorf("create access tenant: %w", err)
		}
		for index, project := range []tracker.ProjectID{dependentProject, blockerProject} {
			slug := "example-dependent"
			if index == 1 {
				slug = "example-blocker"
			}
			_, err := w.CreateProjectRecord(t.Context(), projectstore.Record{
				Core:     corestore.Project{OrganizationID: org, ID: project, Name: "Example project " + string(project), Slug: slug, Profile: "native", RequireDependencies: true},
				Settings: projectstore.Settings{OrganizationID: org, ProjectID: project},
				States:   nativeFixtureStates(),
			})
			if err != nil {
				return fmt.Errorf("create project %s: %w", project, err)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	copyPath := coreDependencySourceCopy(t, org, dependentProject, blockerProject)
	if count, err := ImportCoreWorkItems(t.Context(), repository, CoreWorkItemImportSource{CopyPath: copyPath, OrganizationID: org, ProjectID: dependentProject, Objects: objects}); err != nil || count != 1 {
		t.Fatalf("dependent issue import count=%d error=%v", count, err)
	}
	dependencySource := CoreDependencyImportSource{CopyPath: copyPath, OrganizationID: org, Objects: objects}
	if _, err := ImportCoreIssueDependencies(t.Context(), repository, dependencySource); !errors.Is(err, corestore.ErrNotFound) {
		t.Fatalf("dependency import before blocker exists error=%v", err)
	}
	if count, err := ImportCoreWorkItems(t.Context(), repository, CoreWorkItemImportSource{CopyPath: copyPath, OrganizationID: org, ProjectID: blockerProject, Objects: objects}); err != nil || count != 1 {
		t.Fatalf("blocker issue import count=%d error=%v", count, err)
	}
	for range 2 {
		if count, err := ImportCoreIssueDependencies(t.Context(), repository, dependencySource); err != nil || count != 1 {
			t.Fatalf("dependency import count=%d error=%v", count, err)
		}
	}
	if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
		page, err := r.Relationships(t.Context(), dependentProject, "wi_core_import", corestore.RelationshipCursor{}, 10)
		if err != nil {
			return err
		}
		if len(page.Items) != 1 || page.Items[0].Type != "dependency" || page.Items[0].Direction != "blocked_by" || page.Items[0].RelatedProjectID != blockerProject || page.Items[0].RelatedWorkItemID != "wi_core_blocker" || !page.Items[0].CreatedAt.Equal(parseTestTime(t, "2026-10-02T03:04:05.123456Z")) {
			return errors.New("cross-project dependency did not preserve its typed relationship and creation time")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var provenance string
	var sourceVersion sql.NullString
	var createdAt, updatedAt time.Time
	err := admin.QueryRowContext(t.Context(), `SELECT provenance,source_version,created_at,updated_at FROM issue_dependencies WHERE organization_id=$1 AND dependent_project_id=$2 AND dependent_issue_id=41 AND blocker_project_id=$3 AND blocker_issue_id=42`, org, dependentProject, blockerProject).Scan(&provenance, &sourceVersion, &createdAt, &updatedAt)
	if err != nil || provenance != "github" || !sourceVersion.Valid || sourceVersion.String != "source-v17" || !createdAt.Equal(parseTestTime(t, "2026-10-02T03:04:05.123456Z")) || !updatedAt.Equal(parseTestTime(t, "2026-10-03T04:05:06.987654Z")) {
		t.Fatalf("dependency metadata provenance=%q source_version=%v created=%s updated=%s error=%v", provenance, sourceVersion, createdAt, updatedAt, err)
	}
	var receipt eventstore.Receipt
	if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		receipt, err = r.Receipt(t.Context(), "production-import", "core-dependency", "41:42")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	request, err := eventstore.NewContent(objects).Receipt(t.Context(), receipt)
	if err != nil {
		t.Fatal(err)
	}
	var sourceTimes struct {
		CreatedAt string `json:"created_at"`
		UpdatedAt string `json:"updated_at"`
	}
	if err := json.Unmarshal(request, &sourceTimes); err != nil || sourceTimes.CreatedAt != "2026-10-02T03:04:05.123456789Z" || sourceTimes.UpdatedAt != "2026-10-03T04:05:06.987654321Z" {
		t.Fatalf("dependency receipt did not retain source timestamps: %+v error=%v", sourceTimes, err)
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

func coreDependencySourceCopy(t *testing.T, org tracker.OrganizationID, dependentProject, blockerProject tracker.ProjectID) string {
	t.Helper()
	path := coreWorkItemSourceCopy(t, org, dependentProject, false)
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	actor := tracker.Actor{Kind: "human", PrincipalID: "example-owner"}
	actorJSON, err := json.Marshal(actor)
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	const created = "2026-10-01T00:00:00Z"
	const updated = "2026-10-02T00:00:00Z"
	if _, err := db.ExecContext(t.Context(), `INSERT INTO issues(id,native_id,project_id,number,revision,event_sequence,native_source_key,title,body,issue_contract_json,report_context_json,actor_json,provenance_json,archived,created_at,updated_at,native_created_at,native_updated_at,last_activity_at,workflow_state_id,labels_json,assignees_json,author_login,repository_id,dispatch_wait_since,organization_id)
VALUES(42,'wi_core_blocker',?,2,1,1,NULL,'Blocker title','Blocker body',NULL,NULL,?,NULL,0,?,?,?,?,?,1,'[]','[]','example-author',1,NULL,?)`, blockerProject, string(actorJSON), created, updated, created, updated, updated, org); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	blocker := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: blockerProject, WorkItemID: "wi_core_blocker", Number: 2, Revision: 1, Profile: "native"}, Title: "Blocker title", Body: "Blocker body", State: "Todo", Actor: actor, CreatedAt: parseTestTime(t, created), UpdatedAt: parseTestTime(t, updated), LastActivityAt: parseTestTime(t, updated)}
	blockerRaw, err := json.Marshal(blocker)
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO collaboration_versions VALUES(?,?,?,?,?,?,?)`, org, blockerProject, blocker.WorkItemID, blocker.WorkItemID, blocker.Revision, "snapshot", string(blockerRaw)); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `CREATE TABLE issue_dependencies(blocker_issue_id INTEGER,dependent_issue_id INTEGER,provenance TEXT,source_version TEXT,created_at TEXT,updated_at TEXT)`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO issue_dependencies VALUES(42,41,'github','source-v17','2026-10-02T03:04:05.123456789Z','2026-10-03T04:05:06.987654321Z')`); err != nil {
		_ = db.Close()
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
