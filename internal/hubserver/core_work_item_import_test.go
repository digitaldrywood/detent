package hubserver

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/storagecatalog"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestCoreWorkItemImportPreservesTypedHistoryAcrossTenantsAndReplays(t *testing.T) {
	var native *pgxpool.Config
	repository, _ := coreImportPostgresWithAdmin(t, 3, func(config *pgxpool.Config) { native = config.Copy() })
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
		fixture := coreWorkItemNeutralFixture(t, org, project, false)
		event := fixture.records["collaboration_events"][1]
		var actor, recorded string
		if err := event.Decode(map[string]any{"actor_json": &actor, "recorded_at": &recorded}); err != nil {
			t.Fatal(err)
		}
		runData, err := json.Marshal(tracker.CollaborationData{Revision: 2, Run: &tracker.NativeRunData{AttemptID: "attempt_report_import", RunID: "run_report_import", Sequence: 1, Outcome: "failed", TerminalFailure: &tracker.NativeTerminalFailure{Error: "hub request timed out"}}})
		if err != nil {
			t.Fatal(err)
		}
		fixture.add("collaboration_events", map[string]any{"id": "event_report_import", "sequence": 3, "type": "run.finished", "schema_version": 1, "actor_json": actor, "data_json": string(runData), "event_revision": 2, "recorded_at": recorded, "project_id": project, "work_item_id": "wi_core_import"})
		fixture.set("issues", map[string]string{"native_id": "wi_core_import"}, map[string]any{"event_sequence": 3})
		path := fixture.write()

		source := CoreWorkItemImportSource{BundlePath: path, OrganizationID: org, ProjectID: project, Objects: objects}
		for range 2 {
			count, err := ImportCoreWorkItems(t.Context(), repository, source)
			if err != nil || count != 2 {
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
		t.Run(string(org)+" nullable report revision", func(t *testing.T) {
			nullableFixture := fixture.clone(t)
			nullableFixture.set("collaboration_events", map[string]string{"type": "run.finished"}, map[string]any{"event_revision": nil})
			nullableSource := source
			nullableSource.BundlePath = nullableFixture.write()

			parity, err := ImportCoreReportEvents(t.Context(), repository, nullableSource)
			if err != nil || parity.Events != 1 || parity.ProjectionSHA256 != projectionDigest {
				t.Fatalf("nullable source report fidelity: %+v error %v", parity, err)
			}
		})

		if err := repository.Close(); err != nil {
			t.Fatal(err)
		}
		pool, err := pgxpool.NewWithConfig(t.Context(), native.Copy())
		if err != nil {
			t.Fatal(err)
		}
		repository, err = postgres.NewRepository(t.Context(), pool, postgres.PoolConfig{MaxConnections: 3, ServingConnections: 2, BackgroundConnections: 1})
		if err != nil {
			pool.Close()
			t.Fatal(err)
		}
		reopened := repository
		t.Cleanup(func() {
			if err := reopened.Close(); err != nil {
				t.Error(err)
			}
		})

		if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			item, err := r.WorkItem(t.Context(), project, "wi_core_import")
			if err != nil {
				return err
			}
			if item.ID != 41 || item.Revision != 2 || item.EventSequence != 3 || item.Title != "Updated title" || item.WorkflowStateName == nil || *item.WorkflowStateName != "Todo" || item.BodyObjectKey == nil {
				return errors.New("typed work item did not preserve source identity and current fields")
			}
			second, err := r.WorkItem(t.Context(), project, "wi_core_import_second")
			if err != nil {
				return err
			}
			secondComments, err := r.Comments(t.Context(), project, second.NativeID, 0, 10)
			if err != nil {
				return err
			}
			for _, key := range []eventstore.CollaborationVersionKey{{RecordID: string(second.NativeID), Revision: 1}, {RecordID: string(second.NativeID), Revision: 2}, {RecordID: "comment_core_import_second", Revision: 1}} {
				if _, err := r.CollaborationVersion(t.Context(), key.RecordID, key.Revision); err != nil {
					return err
				}
			}
			if second.ID != 43 || second.Number != 3 || second.Revision != 2 || second.EventSequence != 2 || len(secondComments) != 1 || secondComments[0].ID != "comment_core_import_second" {
				return errors.New("second work item or its history was not preserved after reopen")
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
	repository := coreImportPostgres(t)
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
	source := CoreWorkItemImportSource{BundlePath: original, OrganizationID: org, ProjectID: project, Objects: objects}
	if count, err := ImportCoreWorkItems(t.Context(), repository, source); err != nil || count != 2 {
		t.Fatalf("initial import count=%d error=%v", count, err)
	}

	changedFixture := coreWorkItemNeutralFixture(t, org, project, false)
	changedFixture.records["issues"] = nil
	source.BundlePath = changedFixture.write()

	if _, err := ImportCoreWorkItems(t.Context(), repository, source); !errors.Is(err, mutation.ErrConflict) {
		t.Fatalf("changed empty copy error=%v", err)
	}
}

func TestCoreIssueDependencyImportWaitsForCrossProjectIssuesAndReplays(t *testing.T) {
	repository, admin := coreImportPostgresWithAdmin(t, 3)
	objects := &memoryRunDetails{}
	for _, org := range []tracker.OrganizationID{"org_import_dependency", "org_import_dependency_other"} {
		t.Run(string(org), func(t *testing.T) {
			dependentProject := tracker.ProjectID("prj_import_dependency_a")
			blockerProject := tracker.ProjectID("prj_import_dependency_b")
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: "Example organization", Slug: string(org), CreatedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}); err != nil {
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
			if count, err := ImportCoreWorkItems(t.Context(), repository, CoreWorkItemImportSource{BundlePath: copyPath, OrganizationID: org, ProjectID: dependentProject, Objects: objects}); err != nil || count != 2 {
				t.Fatalf("dependent issue import count=%d error=%v", count, err)
			}
			dependencySource := CoreDependencyImportSource{BundlePath: copyPath, OrganizationID: org, Objects: objects}
			if _, err := ImportCoreIssueDependencies(t.Context(), repository, dependencySource); !errors.Is(err, corestore.ErrNotFound) {
				t.Fatalf("dependency import before blocker exists error=%v", err)
			}
			if count, err := ImportCoreWorkItems(t.Context(), repository, CoreWorkItemImportSource{BundlePath: copyPath, OrganizationID: org, ProjectID: blockerProject, Objects: objects}); err != nil || count != 1 {
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

		})
	}
}

func TestCoreWorkItemImportRollsBackMalformedQueue(t *testing.T) {
	repository := coreImportPostgres(t)
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
	source := CoreWorkItemImportSource{BundlePath: coreWorkItemSourceCopy(t, org, project, true), OrganizationID: org, ProjectID: project, Objects: objects}
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
	return coreWorkItemNeutralFixture(t, org, project, malformedQueue).write()
}
func coreWorkItemNeutralFixture(t *testing.T, org tracker.OrganizationID, project tracker.ProjectID, malformedQueue bool) *coreNeutralFixture {
	t.Helper()
	return coreWorkItemNeutralFixtureBody(t, org, project, malformedQueue, coreImportBody())
}
func coreWorkItemNeutralFixtureBody(t *testing.T, org tracker.OrganizationID, project tracker.ProjectID, malformedQueue bool, body string) *coreNeutralFixture {
	t.Helper()
	fixture := newCoreNeutralFixture(t, org, "issues", "workflow_states", "repositories", "queue_entries", "native_comments", "collaboration_versions", "collaboration_events", "issue_dependencies", "change_content_objects", "attachments")
	const created = "2026-10-01T00:00:00Z"
	const updated = "2026-10-02T00:00:00Z"
	fixture.add("workflow_states", map[string]any{"id": 1, "project_id": project, "detent_state": "Todo"})
	fixture.add("repositories", map[string]any{"id": 1, "github_owner": "example-org", "github_name": "example-project"})
	actor := tracker.Actor{Kind: "human", PrincipalID: "example-owner"}
	actorJSON, err := json.Marshal(actor)
	if err != nil {
		t.Fatal(err)
	}
	for index, suffix := range []string{"", "_second"} {
		base := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: project, WorkItemID: tracker.NativeWorkItemID("wi_core_import" + suffix), Number: 1 + index*2, Revision: 1, Profile: "native"}, Title: "Initial title", Body: body, State: "Todo", Labels: []string{"needs-review"}, Assignees: []string{"example-assignee"}, Actor: actor, CreatedAt: parseTestTime(t, created), UpdatedAt: parseTestTime(t, created), LastActivityAt: parseTestTime(t, created)}
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
			fixture.add("collaboration_versions", map[string]any{"organization_id": org, "project_id": project, "work_item_id": "wi_core_import" + suffix, "record_id": "wi_core_import" + suffix, "revision": version.revision, "encoding": version.encoding, "record_json": string(version.raw)})
		}
		commentActor, _ := json.Marshal(tracker.Actor{Kind: "human", PrincipalID: "example-commenter"})
		comment := tracker.NativeComment{ID: "comment_core_import" + suffix, OrganizationID: org, ProjectID: project, WorkItemID: tracker.NativeWorkItemID("wi_core_import" + suffix), Revision: 1, Sequence: 1, Body: "Example comment", Actor: tracker.Actor{Kind: "human", PrincipalID: "example-commenter"}, CreatedAt: parseTestTime(t, created), UpdatedAt: parseTestTime(t, created)}
		commentRaw, err := json.Marshal(comment)
		if err != nil {
			t.Fatal(err)
		}
		fixture.add("native_comments", map[string]any{"id": comment.ID, "revision": 1, "sequence": 1, "body": "Example comment", "actor_json": string(commentActor), "edited_by_json": nil, "provenance_json": nil, "source_key": nil, "deleted": 0, "created_at": created, "updated_at": created, "organization_id": org, "project_id": project, "work_item_id": "wi_core_import" + suffix})
		fixture.add("collaboration_versions", map[string]any{"organization_id": org, "project_id": project, "work_item_id": "wi_core_import" + suffix, "record_id": comment.ID, "revision": 1, "encoding": "snapshot", "record_json": string(commentRaw)})
		queueState := "Todo"
		if malformedQueue {
			queueState = ""
		}
		fixture.add("queue_entries", map[string]any{"id": 41 + index*2, "issue_id": 41 + index*2, "scope": "native", "state": queueState, "rank": "010", "priority_override": 1, "created_at": created, "updated_at": updated})
		for sequence, name := range []string{"issue.created", "issue.edited"} {
			data, _ := json.Marshal(tracker.CollaborationData{Revision: tracker.Revision(sequence + 1)})
			fixture.add("collaboration_events", map[string]any{"id": "event_core_import_" + string(rune('1'+sequence)) + suffix, "sequence": sequence + 1, "type": name, "schema_version": 1, "actor_json": string(actorJSON), "data_json": string(data), "event_revision": sequence + 1, "recorded_at": []string{created, updated}[sequence], "organization_id": org, "project_id": project, "work_item_id": "wi_core_import" + suffix})
		}
		fixture.add("issues", map[string]any{"id": 41 + index*2, "native_id": "wi_core_import" + suffix, "project_id": project, "number": 1 + index*2, "revision": 2, "event_sequence": 2, "native_source_key": nil, "title": "Updated title", "body": body, "issue_contract_json": nil, "report_context_json": nil, "actor_json": string(actorJSON), "provenance_json": nil, "archived": 0, "created_at": created, "updated_at": updated, "native_created_at": created, "native_updated_at": updated, "last_activity_at": updated, "workflow_state_id": 1, "labels_json": "[\"needs-review\"]", "assignees_json": "[\"example-assignee\"]", "author_login": "example-author", "repository_id": 1, "dispatch_wait_since": nil, "organization_id": org})
	}
	return fixture
}

func coreDependencySourceCopy(t *testing.T, org tracker.OrganizationID, dependentProject, blockerProject tracker.ProjectID) string {
	t.Helper()
	fixture := coreWorkItemNeutralFixture(t, org, dependentProject, false)
	actor := tracker.Actor{Kind: "human", PrincipalID: "example-owner"}
	actorJSON, err := json.Marshal(actor)
	if err != nil {
		t.Fatal(err)
	}
	const created = "2026-10-01T00:00:00Z"
	const updated = "2026-10-02T00:00:00Z"
	fixture.add("issues", map[string]any{"id": 42, "native_id": "wi_core_blocker", "project_id": blockerProject, "number": 2, "revision": 1, "event_sequence": 1, "native_source_key": nil, "title": "Blocker title", "body": "Blocker body", "issue_contract_json": nil, "report_context_json": nil, "actor_json": string(actorJSON), "provenance_json": nil, "archived": 0, "created_at": created, "updated_at": updated, "native_created_at": created, "native_updated_at": updated, "last_activity_at": updated, "workflow_state_id": 1, "labels_json": "[]", "assignees_json": "[]", "author_login": "example-author", "repository_id": 1, "dispatch_wait_since": nil, "organization_id": org})
	blocker := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: blockerProject, WorkItemID: "wi_core_blocker", Number: 2, Revision: 1, Profile: "native"}, Title: "Blocker title", Body: "Blocker body", State: "Todo", Actor: actor, CreatedAt: parseTestTime(t, created), UpdatedAt: parseTestTime(t, updated), LastActivityAt: parseTestTime(t, updated)}
	blockerRaw, err := json.Marshal(blocker)
	if err != nil {
		t.Fatal(err)
	}
	fixture.add("collaboration_versions", map[string]any{"organization_id": org, "project_id": blockerProject, "work_item_id": blocker.WorkItemID, "record_id": blocker.WorkItemID, "revision": blocker.Revision, "encoding": "snapshot", "record_json": string(blockerRaw)})
	fixture.add("issue_dependencies", map[string]any{"blocker_issue_id": 42, "dependent_issue_id": 41, "provenance": "github", "source_version": "source-v17", "created_at": "2026-10-02T03:04:05.123456789Z", "updated_at": "2026-10-03T04:05:06.987654321Z"})
	return fixture.write()
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

func TestCoreWorkItemImportsRejectUnsupportedSchemaBeforePin(t *testing.T) {
	repository, admin := coreImportPostgresWithAdmin(t, 3)
	for _, family := range []string{"work_items", "dependencies", "report_events"} {
		t.Run(family, func(t *testing.T) {
			fixture := coreWorkItemNeutralFixture(t, "org_work_item_schema", "prj_example", false)
			tableName := "issues"
			if family == "report_events" {
				tableName = "collaboration_events"
			}
			table := fixture.tables[tableName]
			table.Columns["unaccounted"] = fmt.Sprintf(`[%d,"unaccounted","TEXT",1,null,0,0]`, len(table.Columns))
			for _, row := range fixture.records[table.Name] {
				row.Values["unaccounted"] = storagecatalog.Cell{Encoding: "utf8", Data: []byte("must survive migration")}
			}
			fixture.tables[table.Name] = table
			objects := &memoryRunDetails{}
			path := fixture.write()
			var err error
			switch family {
			case "work_items":
				_, err = ImportCoreWorkItems(t.Context(), repository, CoreWorkItemImportSource{BundlePath: path, OrganizationID: fixture.organization, ProjectID: "prj_example", Objects: objects})
			case "dependencies":
				_, err = ImportCoreIssueDependencies(t.Context(), repository, CoreDependencyImportSource{BundlePath: path, OrganizationID: fixture.organization, Objects: objects})
			case "report_events":
				_, err = ImportCoreReportEvents(t.Context(), repository, CoreWorkItemImportSource{BundlePath: path, OrganizationID: fixture.organization, ProjectID: "prj_example", Objects: objects})
			}
			if !errors.Is(err, storagecatalog.ErrUnmapped) {
				t.Fatalf("schema drift error=%v", err)
			}
			var count int
			if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM neutral_import_sources").Scan(&count); err != nil || count != 0 {
				t.Fatalf("source pinned before schema validation: count=%d error=%v", count, err)
			}
		})
	}
}

func TestCoreWorkItemImportsRefuseLegacyCopy(t *testing.T) {
	for _, family := range []string{"work_items", "dependencies", "report_events"} {
		for _, bundle := range []string{"", "unused-neutral-bundle"} {
			var err error
			switch family {
			case "work_items":
				_, err = ImportCoreWorkItems(t.Context(), nil, CoreWorkItemImportSource{CopyPath: "preserved-copy", BundlePath: bundle})
			case "dependencies":
				_, err = ImportCoreIssueDependencies(t.Context(), nil, CoreDependencyImportSource{CopyPath: "preserved-copy", BundlePath: bundle})
			case "report_events":
				_, err = ImportCoreReportEvents(t.Context(), nil, CoreWorkItemImportSource{CopyPath: "preserved-copy", BundlePath: bundle})
			}
			if !errors.Is(err, postgres.ErrNeutralImport) {
				t.Fatalf("%s legacy selector error=%v", family, err)
			}
		}
	}
}
