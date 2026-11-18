package coreimport

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/coreimport/coreimporttest"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/neutralimport"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/neutralimport/neutralimporttest"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/storagecatalog"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestCoreWorkItemImportPreservesTypedHistoryAcrossTenantsAndReplays(t *testing.T) {
	t.Parallel()
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
				States:   hubtest.States(),
			})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		fixture := coreWorkItemNeutralFixture(t, org, project, false)
		fixture.Set("issues", map[string]string{"native_id": "wi_core_import_second"}, map[string]any{"issue_contract_json": "{}"})
		fixture.Set("repositories", map[string]string{"id": "1"}, map[string]any{"github_node_id": "R_original"})
		fixture.Set("issues", map[string]string{"id": "41"}, map[string]any{"github_node_id": "I_original"})
		event := fixture.Records["collaboration_events"][1]
		var actor, recorded string
		if err := event.Decode(map[string]any{"actor_json": &actor, "recorded_at": &recorded}); err != nil {
			t.Fatal(err)
		}
		runData, err := json.Marshal(tracker.CollaborationData{Revision: 2, Run: &tracker.NativeRunData{AttemptID: "attempt_report_import", RunID: "run_report_import", Sequence: 1, Outcome: "failed", TerminalFailure: &tracker.NativeTerminalFailure{Error: "hub request timed out"}}})
		if err != nil {
			t.Fatal(err)
		}
		fixture.Add("collaboration_events", map[string]any{"id": "event_report_import", "sequence": 3, "type": "run.finished", "schema_version": 1, "actor_json": actor, "data_json": string(runData), "event_revision": 2, "recorded_at": recorded, "project_id": project, "work_item_id": "wi_core_import"})
		fixture.Set("issues", map[string]string{"native_id": "wi_core_import"}, map[string]any{"event_sequence": 3})
		path := fixture.Write()

		source := CoreWorkItemImportSource{BundlePath: path, OrganizationID: org, ProjectID: project, Objects: objects}
		for range 2 {
			count, err := ImportCoreWorkItems(t.Context(), repository, source)
			if err != nil || count != 2 {
				t.Fatalf("organization %s import count=%d error=%v", org, count, err)
			}
		}
		var projectionDigest string
		for range 2 {
			parity, err := importCoreReportEvents(t.Context(), repository, source)
			if err != nil || parity.Events != 1 || parity.AttemptObservations != 1 || parity.Reworks != 0 || parity.SourceSHA256 == "" || parity.ProjectionSHA256 == "" {
				t.Fatalf("source report parity = %+v error %v", parity, err)
			}
			if projectionDigest != "" && projectionDigest != parity.ProjectionSHA256 {
				t.Fatal("source report projection changed across idempotent import")
			}
			projectionDigest = parity.ProjectionSHA256
		}
		t.Run(string(org)+" direct-object report detail", func(t *testing.T) {
			direct := fixture.Clone(t)
			var data tracker.CollaborationData
			if err := json.Unmarshal(runData, &data); err != nil {
				t.Fatal(err)
			}
			full, err := json.Marshal(data.Run)
			if err != nil {
				t.Fatal(err)
			}
			ref, err := objects.StoreDetail(t.Context(), string(org), full)
			if err != nil {
				t.Fatal(err)
			}
			direct.Add("change_content_objects", map[string]any{"object_key": ref.Key, "object_sha256": ref.SHA256, "object_bytes": ref.CompressedSize, "content_bytes": ref.UncompressedSize})
			detail := hubrecords.NewRunDetailReference(ref, string(org), hubrecords.RunDetailEntry(*data.Run))
			detail.ObjectKey = ref.Key
			stored, err := json.Marshal(hubrecords.StoredRun{NativeRunData: hubrecords.CompactRun(*data.Run), Detail: detail})
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(runData, &fields); err != nil {
				t.Fatal(err)
			}
			fields["run"] = stored
			body, err := json.Marshal(fields)
			if err != nil {
				t.Fatal(err)
			}
			direct.Set("collaboration_events", map[string]string{"id": "event_report_import"}, map[string]any{"data_json": string(body)})
			directSource := source
			directSource.BundlePath = direct.Write()
			for range 2 {
				parity, err := importCoreReportEvents(t.Context(), repository, directSource)
				if err != nil || parity.Events != 1 || parity.AttemptObservations != 1 || parity.ProjectionSHA256 != projectionDigest {
					t.Fatalf("direct-object report changed retained projection: %+v error=%v", parity, err)
				}
			}
		})

		t.Run(string(org)+" nullable report revision", func(t *testing.T) {
			nullableFixture := fixture.Clone(t)
			nullableFixture.Set("collaboration_events", map[string]string{"type": "run.finished"}, map[string]any{"event_revision": nil})
			nullableSource := source
			nullableSource.BundlePath = nullableFixture.Write()

			parity, err := importCoreReportEvents(t.Context(), repository, nullableSource)
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
			if item.ID != 41 || item.Revision != 2 || item.EventSequence != 3 || item.Title != "Updated title" || item.WorkflowStateName == nil || *item.WorkflowStateName != "Todo" || item.BodyObjectKey == nil || item.ActorKind == nil || *item.ActorKind != "human" || item.ActorPrincipalID == nil || *item.ActorPrincipalID != "example-owner" {
				return errors.New("typed work item did not preserve source identity and current fields")
			}
			if item.ProvenanceProvider == nil || *item.ProvenanceProvider != "github" || item.ProvenanceExternalID == nil || *item.ProvenanceExternalID != "I_original" {
				return errors.New("original GitHub node provenance was not preserved")
			}
			receipt, err := r.Receipt(t.Context(), "production-import", "core-work-item-source", "41")
			if err != nil {
				return err
			}
			raw, err := eventstore.NewContent(objects).Receipt(t.Context(), receipt)
			if err != nil {
				return err
			}
			var preserved struct {
				Mapping CoreWorkItemIdentityMapping
			}
			if err := json.Unmarshal(raw, &preserved); err != nil {
				return err
			}
			if preserved.Mapping.SourceIssueID != 41 || preserved.Mapping.SourceRepositoryID != 1 || preserved.Mapping.SourceRepositoryNodeID != "R_original" || preserved.Mapping.SourceGitHubNodeID != "I_original" || preserved.Mapping.TargetWorkItemID != "wi_core_import" || preserved.Mapping.TargetProjectID != project || preserved.Mapping.OrganizationID != org {
				return fmt.Errorf("original numeric/provenance mapping changed: %+v", preserved.Mapping)
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
			if issueBody != coreimporttest.WorkItemBody() {
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
			if err := json.Unmarshal(currentRaw, &current); err != nil || current.Title != "Updated title" || current.Body != coreimporttest.WorkItemBody() {
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
	for index, test := range []struct {
		name                      string
		fields                    map[string]any
		missingRun                bool
		nullableRevision          bool
		payloadRevision           tracker.Revision
		wantRevision              tracker.Revision
		from, to, reason, attempt string
		fence                     tracker.FencingToken
		run                       bool
		wantErr                   error
		edited                    []string
	}{
		{edited: []string{"title"}, payloadRevision: 2, wantRevision: 7, name: "unknown readiness", fields: map[string]any{"page_projection_ready": nil, "from_state": "Backlog", "to_state": "Cancelled", "reason": "typed reason", "run_present": false, "run_attempt_id": "typed-attempt", "run_fencing_token": 9}, from: "Todo", to: "Done", reason: "opaque reason", attempt: "opaque-attempt", fence: 3, run: true},
		{edited: []string{"title"}, payloadRevision: 2, wantRevision: 7, name: "false readiness", fields: map[string]any{"page_projection_ready": false, "from_state": "Backlog", "to_state": "Cancelled", "reason": "typed reason", "run_present": true, "run_attempt_id": "typed-attempt", "run_fencing_token": 9}, from: "Todo", to: "Done", reason: "opaque reason", attempt: "opaque-attempt", fence: 3, run: true},
		{edited: []string{"title"}, payloadRevision: 2, wantRevision: 7, name: "known corrections", fields: map[string]any{"page_projection_ready": true, "from_state": "Backlog", "to_state": "Cancelled", "reason": "typed reason", "run_present": true, "run_attempt_id": "typed-attempt", "run_fencing_token": 9}, from: "Backlog", to: "Cancelled", reason: "typed reason", attempt: "typed-attempt", fence: 9, run: true},
		{edited: []string{"title"}, payloadRevision: 2, wantRevision: 7, name: "known empty facts", fields: map[string]any{"page_projection_ready": true, "from_state": "", "to_state": "", "reason": "", "run_present": true, "run_attempt_id": "", "run_fencing_token": 0}, run: true},
		{edited: []string{"title"}, payloadRevision: 2, wantRevision: 7, name: "unknown facts", fields: map[string]any{"page_projection_ready": true, "from_state": nil, "to_state": "Cancelled", "reason": nil, "run_present": nil, "run_attempt_id": "typed-attempt", "run_fencing_token": 9}, from: "Todo", to: "Cancelled", reason: "opaque reason", attempt: "opaque-attempt", fence: 3, run: true},
		{edited: []string{"title"}, payloadRevision: 2, wantRevision: 7, name: "known run with null fields", fields: map[string]any{"page_projection_ready": true, "run_present": true, "run_attempt_id": nil, "run_fencing_token": nil}, from: "Todo", to: "Done", reason: "opaque reason", attempt: "opaque-attempt", fence: 3, run: true},
		{edited: []string{"title"}, payloadRevision: 2, wantRevision: 7, name: "explicit no run", fields: map[string]any{"page_projection_ready": true, "run_present": false, "run_attempt_id": "typed-attempt", "run_fencing_token": 9}, from: "Todo", to: "Done", reason: "opaque reason", wantErr: eventstore.ErrInvalidRecord},
		{edited: []string{"title"}, payloadRevision: 2, wantRevision: 7, name: "ready run missing payload", fields: map[string]any{"page_projection_ready": true, "run_present": true, "run_attempt_id": "typed-attempt", "run_fencing_token": 9}, missingRun: true, wantErr: eventstore.ErrInvalidRecord},
		{edited: []string{"title"}, payloadRevision: 2, wantRevision: 7, name: "unknown run missing payload", fields: map[string]any{"page_projection_ready": nil, "run_present": true, "run_attempt_id": "typed-attempt", "run_fencing_token": 9}, missingRun: true, from: "Todo", to: "Done", reason: "opaque reason"},
		{edited: []string{"title"}, payloadRevision: 2, wantRevision: 7, name: "known absent run", fields: map[string]any{"page_projection_ready": true, "run_present": false, "run_attempt_id": "", "run_fencing_token": 0}, missingRun: true, from: "Todo", to: "Done", reason: "opaque reason"},
		{edited: []string{"title"}, nullableRevision: true, payloadRevision: 0, wantRevision: 0, name: "ready nullable zero revision", fields: map[string]any{"page_projection_ready": true, "run_present": true}, from: "Todo", to: "Done", reason: "opaque reason", attempt: "opaque-attempt", fence: 3, run: true},
		{edited: []string{"title"}, nullableRevision: true, payloadRevision: 2, wantRevision: 2, name: "unknown nullable nonzero revision", fields: map[string]any{"page_projection_ready": nil}, from: "Todo", to: "Done", reason: "opaque reason", attempt: "opaque-attempt", fence: 3, run: true},
		{edited: []string{"title"}, payloadRevision: 2, wantRevision: 7, name: "invalid readiness", fields: map[string]any{"page_projection_ready": 2}, wantErr: neutralimport.ErrInvalid},

		{edited: []string{"body"}, payloadRevision: 2, wantRevision: 7, name: "known edit correction", fields: map[string]any{"page_projection_ready": true, "edited_title": false, "edited_body": true}, from: "Todo", to: "Done", reason: "opaque reason", attempt: "opaque-attempt", fence: 3, run: true},
		{edited: []string{}, payloadRevision: 2, wantRevision: 7, name: "known empty edits", fields: map[string]any{"page_projection_ready": true, "edited_title": false, "edited_body": false}, from: "Todo", to: "Done", reason: "opaque reason", attempt: "opaque-attempt", fence: 3, run: true},
		{edited: []string{"title"}, payloadRevision: 2, wantRevision: 7, name: "unknown edits", fields: map[string]any{"page_projection_ready": true, "edited_title": nil, "edited_body": nil}, from: "Todo", to: "Done", reason: "opaque reason", attempt: "opaque-attempt", fence: 3, run: true},
		{edited: []string{"title"}, payloadRevision: 2, wantRevision: 7, name: "false readiness ignores edit correction", fields: map[string]any{"page_projection_ready": false, "edited_title": false, "edited_body": true}, from: "Todo", to: "Done", reason: "opaque reason", attempt: "opaque-attempt", fence: 3, run: true},
		{edited: []string{"title"}, payloadRevision: 2, wantRevision: 7, name: "unknown readiness ignores edit correction", fields: map[string]any{"page_projection_ready": nil, "edited_title": false, "edited_body": true}, from: "Todo", to: "Done", reason: "opaque reason", attempt: "opaque-attempt", fence: 3, run: true},
		{payloadRevision: 2, wantRevision: 7, name: "invalid edit correction", fields: map[string]any{"page_projection_ready": true, "edited_title": 2}, wantErr: neutralimport.ErrInvalid},
	} {
		t.Run("historical page facts/"+test.name, func(t *testing.T) {
			organization := tracker.OrganizationID(fmt.Sprintf("org_event_facts_%02d", index))
			project := tracker.ProjectID("prj_event_facts")
			if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: organization, Name: "Example organization", Slug: string(organization), CreatedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}); err != nil {
					return err
				}
				if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: organization, ProviderID: "provider_" + string(organization)}); err != nil {
					return err
				}
				_, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: project, Name: "Example project", Profile: "native"}, Settings: projectstore.Settings{OrganizationID: organization, ProjectID: project, RepositoryEnabled: true}, States: hubtest.States()})
				return err
			}); err != nil {
				t.Fatal(err)
			}
			fixture := coreWorkItemNeutralFixture(t, organization, project, false)
			for _, suffix := range []string{"", "_second"} {
				item := "wi_core_import" + suffix
				data := tracker.CollaborationData{Revision: test.payloadRevision, FromState: "Todo", ToState: "Done", Reason: "opaque reason", Fields: []string{"title"}, Run: &tracker.NativeRunData{RunID: "run_payload", AttemptID: "opaque-attempt", FencingToken: 3, Sequence: 4}}
				if test.missingRun {
					data.Run = nil
				}
				raw, err := json.Marshal(data)
				if err != nil {
					t.Fatal(err)
				}
				actor, err := json.Marshal(tracker.Actor{Kind: "human", PrincipalID: "example-owner"})
				if err != nil {
					t.Fatal(err)
				}
				fields := map[string]any{"id": "event_page_facts" + suffix, "organization_id": organization, "project_id": project, "work_item_id": item, "sequence": 3, "type": "workflow.transitioned", "schema_version": 1, "actor_json": string(actor), "data_json": string(raw), "event_revision": 7, "recorded_at": "2026-10-02T01:00:00Z"}
				if test.nullableRevision {
					fields["event_revision"] = nil
				}
				for name, value := range test.fields {
					fields[name] = value
				}
				fixture.Add("collaboration_events", fields)
				fixture.Set("issues", map[string]string{"native_id": item}, map[string]any{"event_sequence": 3})
			}
			source := CoreWorkItemImportSource{BundlePath: fixture.Write(), OrganizationID: organization, ProjectID: project, Objects: objects}
			count, err := ImportCoreWorkItems(t.Context(), repository, source)
			if test.wantErr != nil {
				if !errors.Is(err, test.wantErr) || count != 0 {
					t.Fatalf("invalid historical page facts imported=%d error=%v want=%v", count, err, test.wantErr)
				}
				if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
					rows, err := r.WorkItems(t.Context(), project, 0, 10)
					if err == nil && len(rows) != 0 {
						t.Fatalf("invalid event imported partial issues: %+v", rows)
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
				return
			}
			if err != nil || count != 2 {
				t.Fatalf("historical page facts imported=%d error=%v", count, err)
			}
			if count, err := ImportCoreWorkItems(t.Context(), repository, source); err != nil || count != 2 {
				t.Fatalf("normalized source replay imported=%d error=%v", count, err)
			}
			if err := repository.ReadRecords(t.Context(), organization, corestore.Serving, func(r eventstore.Reader) error {
				for _, suffix := range []string{"", "_second"} {
					item := tracker.NativeWorkItemID("wi_core_import" + suffix)
					events, err := r.Events(t.Context(), project, item, 2, 2)
					if err != nil {
						return err
					}
					if len(events) != 1 || events[0].ID != "event_page_facts"+suffix || events[0].WorkItemID != item || events[0].OrganizationID != organization || events[0].EventRevision != int64(test.wantRevision) || events[0].FromState != test.from || events[0].ToState != test.to {
						t.Fatalf("normalized typed event=%+v", events)
					}
					row := events[0]
					raw, err := objects.ReadDetail(t.Context(), string(organization), eventstore.Reference(row.PayloadObjectKey, row.PayloadSHA256, row.PayloadBytes, row.PayloadContentBytes))
					if err != nil {
						return err
					}
					var value tracker.CollaborationEvent
					if err := json.Unmarshal(raw, &value); err != nil {
						return err
					}
					data := value.Data
					if data.Revision != test.wantRevision || data.FromState != test.from || data.ToState != test.to || data.Reason != test.reason || !slices.Equal(data.Fields, test.edited) {
						t.Fatalf("canonical detail did not normalize verified page facts: %+v", data)
					}
					if test.run {
						if data.Run == nil || data.Run.RunID != "run_payload" || data.Run.Sequence != 4 || data.Run.AttemptID != test.attempt || data.Run.FencingToken != test.fence || row.RunID != "run_payload" || row.AttemptID != test.attempt {
							t.Fatalf("normalized run detail and typed identity diverged: row=%+v run=%+v", row, data.Run)
						}
					} else if data.Run != nil || row.RunID != "" || row.AttemptID != "" {
						t.Fatalf("absent historical run was invented: row=%+v run=%+v", row, data.Run)
					}
					if _, err := eventstore.NewContent(objects).Event(t.Context(), row); err != nil {
						return fmt.Errorf("read normalized event detail: %w", err)
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}

}

func TestCoreWorkItemImportRejectsChangedCopyWhenAllIssuesWereDeleted(t *testing.T) {
	t.Parallel()
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
			States:   hubtest.States(),
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
	changedFixture.Records["issues"] = nil
	source.BundlePath = changedFixture.Write()

	if _, err := ImportCoreWorkItems(t.Context(), repository, source); !errors.Is(err, mutation.ErrConflict) {
		t.Fatalf("changed empty copy error=%v", err)
	}
}

func TestCoreIssueDependencyImportWaitsForCrossProjectIssuesAndReplays(t *testing.T) {
	t.Parallel()
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
						States:   hubtest.States(),
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
	t.Parallel()
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
			States:   hubtest.States(),
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
	return coreWorkItemNeutralFixture(t, org, project, malformedQueue).Write()
}
func coreWorkItemNeutralFixture(t *testing.T, org tracker.OrganizationID, project tracker.ProjectID, malformedQueue bool) *neutralimporttest.Fixture {
	t.Helper()
	return coreimporttest.WorkItems(t, repositoryFS, org, project, malformedQueue, coreimporttest.WorkItemBody())
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
	fixture.Add("issues", map[string]any{"id": 42, "native_id": "wi_core_blocker", "project_id": blockerProject, "number": 2, "revision": 1, "event_sequence": 1, "native_source_key": nil, "title": "Blocker title", "body": "Blocker body", "issue_contract_json": nil, "report_context_json": nil, "actor_json": string(actorJSON), "provenance_json": nil, "archived": 0, "created_at": created, "updated_at": updated, "native_created_at": created, "native_updated_at": updated, "last_activity_at": updated, "workflow_state_id": 1, "labels_json": "[]", "assignees_json": "[]", "author_login": "example-author", "repository_id": 1, "dispatch_wait_since": nil, "organization_id": org})
	blocker := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: blockerProject, WorkItemID: "wi_core_blocker", Number: 2, Revision: 1, Profile: "native"}, Title: "Blocker title", Body: "Blocker body", State: "Todo", Actor: actor, CreatedAt: parseTestTime(t, created), UpdatedAt: parseTestTime(t, updated), LastActivityAt: parseTestTime(t, updated)}
	blockerRaw, err := json.Marshal(blocker)
	if err != nil {
		t.Fatal(err)
	}
	fixture.Add("collaboration_versions", map[string]any{"organization_id": org, "project_id": blockerProject, "work_item_id": blocker.WorkItemID, "record_id": blocker.WorkItemID, "revision": blocker.Revision, "encoding": "snapshot", "record_json": string(blockerRaw)})
	fixture.Add("issue_dependencies", map[string]any{"blocker_issue_id": 42, "dependent_issue_id": 41, "provenance": "github", "source_version": "source-v17", "created_at": "2026-10-02T03:04:05.123456789Z", "updated_at": "2026-10-03T04:05:06.987654321Z"})
	return fixture.Write()
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
	t.Parallel()
	repository, admin := coreImportPostgresWithAdmin(t, 3)
	for _, family := range []string{"work_items", "dependencies", "report_events", "linked_sources"} {
		t.Run(family, func(t *testing.T) {
			fixture := coreWorkItemNeutralFixture(t, "org_work_item_schema", "prj_example", false)
			tableName := "issues"
			if family == "report_events" {
				tableName = "collaboration_events"
			}
			if family == "linked_sources" {
				tableName = "linked_issue_sources"
				linked := newCoreNeutralFixture(t, fixture.Organization, tableName)
				fixture.Tables[tableName] = linked.Tables[tableName]
			}
			table := fixture.Tables[tableName]
			table.Columns["unaccounted"] = fmt.Sprintf(`[%d,"unaccounted","TEXT",1,null,0,0]`, len(table.Columns))
			for _, row := range fixture.Records[table.Name] {
				row.Values["unaccounted"] = storagecatalog.Cell{Encoding: "utf8", Data: []byte("must survive migration")}
			}
			fixture.Tables[table.Name] = table
			objects := &memoryRunDetails{}
			path := fixture.Write()
			var err error
			switch family {
			case "work_items", "linked_sources":
				_, err = ImportCoreWorkItems(t.Context(), repository, CoreWorkItemImportSource{BundlePath: path, OrganizationID: fixture.Organization, ProjectID: "prj_example", Objects: objects})
			case "dependencies":
				_, err = ImportCoreIssueDependencies(t.Context(), repository, CoreDependencyImportSource{BundlePath: path, OrganizationID: fixture.Organization, Objects: objects})
			case "report_events":
				_, err = importCoreReportEvents(t.Context(), repository, CoreWorkItemImportSource{BundlePath: path, OrganizationID: fixture.Organization, ProjectID: "prj_example", Objects: objects})
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
	t.Parallel()
	for _, family := range []string{"work_items", "dependencies", "report_events"} {
		for _, bundle := range []string{"", "unused-neutral-bundle"} {
			var err error
			switch family {
			case "work_items":
				_, err = ImportCoreWorkItems(t.Context(), nil, CoreWorkItemImportSource{CopyPath: "preserved-copy", BundlePath: bundle})
			case "dependencies":
				_, err = ImportCoreIssueDependencies(t.Context(), nil, CoreDependencyImportSource{CopyPath: "preserved-copy", BundlePath: bundle})
			case "report_events":
				_, err = importCoreReportEvents(t.Context(), nil, CoreWorkItemImportSource{CopyPath: "preserved-copy", BundlePath: bundle})
			}
			if !errors.Is(err, neutralimport.ErrInvalid) {
				t.Fatalf("%s legacy selector error=%v", family, err)
			}
		}
	}
}

func TestCoreWorkItemImportPreservesVerifiedTerminalStreaks(t *testing.T) {
	t.Parallel()
	repository := coreImportPostgres(t)
	for index, test := range []struct {
		name, state string
		terminal    any
		clock       any
		valid       bool
		restore     string
	}{
		{"verified boundary", "Done", 1, "2026-10-02T00:00:00Z", true, ""},
		{"unknown terminal clock", "Done", 1, nil, true, ""},
		{"unknown workflow terminal flag", "Done", nil, nil, true, ""},
		{"clock before creation", "Done", 1, "2026-09-30T00:00:00Z", false, ""},
		{"clock after update", "Done", 1, "2026-10-03T00:00:00Z", false, ""},
		{"nonterminal clock", "Todo", 0, "2026-10-02T00:00:00Z", false, ""},
		{"malformed clock", "Done", 1, "invalid timestamp", false, ""},
		{"verified restore", "Done", 1, "2026-10-02T00:00:00Z", true, "2026-10-02T00:00:00Z"},
		{"unknown restore floor", "Done", 1, "2026-10-01T00:00:00Z", true, ""},
		{"restore after source update", "Done", 1, "2026-10-02T00:00:00Z", false, "2026-10-03T00:00:00Z"},
	} {
		t.Run(test.name, func(t *testing.T) {
			org := tracker.OrganizationID("org_terminal_import_" + strconv.Itoa(index))
			project := tracker.ProjectID("prj_terminal_import")
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
				if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: "Example", Slug: "example-terminal-" + strconv.Itoa(index), CreatedAt: at}); err != nil {
					return err
				}
				if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: org, ProviderID: "provider_" + string(org)}); err != nil {
					return err
				}
				_, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: project, Name: "Example", Profile: "native"}, States: hubtest.States()})
				return err
			}); err != nil {
				t.Fatal(err)
			}
			fixture := coreWorkItemNeutralFixture(t, org, project, false)
			fixture.Set("workflow_states", map[string]string{"id": "1"}, map[string]any{"detent_state": test.state, "terminal": test.terminal})
			for _, suffix := range []string{"", "_second"} {
				id := "wi_core_import" + suffix
				fixture.Set("issues", map[string]string{"native_id": id}, map[string]any{"terminal_entered_at": test.clock})
				if test.restore != "" {
					fixture.Set("collaboration_events", map[string]string{"work_item_id": id, "sequence": "2"}, map[string]any{"archive_operation": "restore", "recorded_at": test.restore})
				}
				sourceIssueID := int64(41)
				if suffix != "" {
					sourceIssueID = 43
				}
				fixture.Set("queue_entries", map[string]string{"issue_id": strconv.FormatInt(sourceIssueID, 10)}, map[string]any{"state": test.state})
				var base tracker.NativeIssue
				for _, row := range fixture.Records["collaboration_versions"] {
					var record, raw string
					var revision int64
					if err := row.Decode(map[string]any{"record_id": &record, "revision": &revision, "record_json": &raw}); err != nil {
						t.Fatal(err)
					}
					if record == id && revision == 1 {
						if err := json.Unmarshal([]byte(raw), &base); err != nil {
							t.Fatal(err)
						}
						break
					}
				}
				if base.WorkItemID == "" {
					t.Fatal("source issue snapshot missing")
				}
				base.Revision = 2
				base.Title = "Updated title"
				base.State = test.state
				base.UpdatedAt = parseTestTime(t, "2026-10-02T00:00:00Z")
				base.LastActivityAt = base.UpdatedAt
				raw, err := json.Marshal(base)
				if err != nil {
					t.Fatal(err)
				}
				fixture.Set("collaboration_versions", map[string]string{"record_id": id, "revision": "2"}, map[string]any{"encoding": "snapshot", "record_json": string(raw)})
			}
			path := fixture.Write()
			readCopy := func() map[string][]byte {
				t.Helper()
				files, err := os.ReadDir(path)
				if err != nil {
					t.Fatal(err)
				}
				result := map[string][]byte{}
				for _, file := range files {
					if file.IsDir() {
						t.Fatal("unexpected directory in neutral source fixture")
					}
					data, err := os.ReadFile(filepath.Join(path, file.Name()))
					if err != nil {
						t.Fatal(err)
					}
					result[file.Name()] = data
				}
				return result
			}
			before := readCopy()
			source := CoreWorkItemImportSource{BundlePath: path, OrganizationID: org, ProjectID: project, Objects: &memoryRunDetails{}}
			count, err := ImportCoreWorkItems(t.Context(), repository, source)
			if test.valid {
				if err != nil || count != 2 {
					t.Fatalf("terminal import count=%d error=%v", count, err)
				}
			} else if err == nil {
				t.Fatal("unverified terminal clock imported")
			}
			if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
				for _, suffix := range []string{"", "_second"} {
					item, err := r.WorkItem(t.Context(), project, tracker.NativeWorkItemID("wi_core_import"+suffix))
					if !test.valid {
						if !errors.Is(err, corestore.ErrNotFound) {
							t.Fatalf("invalid source left issue: %v", err)
						}
						continue
					}
					if err != nil {
						return err
					}
					if item.ArchiveRestoreKnown != (test.restore != "") {
						t.Fatal("verified restore provenance was invented or lost")
					}
					if test.restore == "" {
						if item.ArchiveRestoredAt != nil {
							t.Fatal("import invented a historical restore timestamp")
						}
					} else if item.ArchiveRestoredAt == nil || !item.ArchiveRestoredAt.Equal(parseTestTime(t, test.restore)) {
						t.Fatalf("verified restore=%v", item.ArchiveRestoredAt)
					}
					if test.name == "unknown restore floor" {
						project, err := r.ProjectRecord(t.Context(), item.ProjectID)
						if err != nil {
							return err
						}
						owner := r.(corestore.WorkItemRetentionReader)
						_, eligible, err := owner.WorkItemRetentionPolicy(t.Context(), item, project.Settings.Revision, item.NativeUpdatedAt.Add(30*24*time.Hour))
						if err != nil {
							return err
						}
						if eligible {
							t.Fatal("unknown source restore archived before the conservative source-update floor")
						}
						_, eligible, err = owner.WorkItemRetentionPolicy(t.Context(), item, project.Settings.Revision, item.NativeUpdatedAt.Add(30*24*time.Hour+time.Second))
						if err != nil {
							return err
						}
						if !eligible {
							t.Fatal("verified source update floor never became eligible")
						}
					}
					if test.clock == nil {
						if item.TerminalEnteredAt != nil {
							t.Fatal("import invented a historical terminal clock")
						}
					} else if item.TerminalEnteredAt == nil || !item.TerminalEnteredAt.Equal(parseTestTime(t, test.clock.(string))) {
						t.Fatalf("verified clock=%v", item.TerminalEnteredAt)
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			after := readCopy()
			if len(before) != len(after) {
				t.Fatal("terminal import changed source copy files")
			}
			for file, raw := range before {
				if !bytes.Equal(raw, after[file]) {
					t.Fatal("terminal import changed its historical source copy")
				}
			}
		})
	}
}

func TestCoreWorkItemPreparationRejectsCorruptHistory(t *testing.T) {
	t.Parallel()
	org, project := tracker.OrganizationID("org_import_corrupt"), tracker.ProjectID("prj_import_corrupt")
	fixture := coreimporttest.WorkItems(t, repositoryFS, org, project, false, coreimporttest.WorkItemBody())
	fixture.Set("collaboration_versions", map[string]string{"record_id": "wi_core_import", "revision": "2"}, map[string]any{"encoding": "snapshot", "record_json": "{}"})
	source := CoreWorkItemImportSource{BundlePath: fixture.Write(), OrganizationID: org, ProjectID: project, Objects: &memoryRunDetails{}}
	err := neutralimport.Read(t.Context(), source.BundlePath, org, func(bundle *neutralimport.Bundle) error {
		reader := neutralimport.NewIndex(bundle)
		rows, err := readCoreWorkItemImportPage(t.Context(), reader, source, 0)
		if err != nil {
			return err
		}
		if len(rows) != 2 {
			t.Fatalf("corrupt source items=%d want2", len(rows))
		}
		_, err = prepareCoreWorkItemImport(t.Context(), reader, eventstore.NewContent(source.Objects), source, bundle.Identity().SourceSHA256, rows[0])
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "current issue row does not match immutable history") {
		t.Fatalf("corrupt conversion error=%v", err)
	}
}

func TestCoreImportEventsKeepHistoricalRunSequence(t *testing.T) {
	t.Parallel()
	org, project := tracker.OrganizationID("org_import_events"), tracker.ProjectID("prj_import_events")
	item := corestore.WorkItem{OrganizationID: org, ProjectID: project, NativeID: "wi_import_events"}
	run := tracker.NativeRunData{AttemptID: "attempt_import_events", LeaseID: "lease_import_events", FencingToken: 1, RunID: "run_import_events"}
	actor, err := json.Marshal(tracker.Actor{Kind: "worker", PrincipalID: "example-runner"})
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(tracker.CollaborationData{Run: &run})
	if err != nil {
		t.Fatal(err)
	}
	fixture := newCoreNeutralFixture(t, org, "collaboration_events")
	fixture.Add("collaboration_events", map[string]any{"organization_id": org, "project_id": project, "work_item_id": item.NativeID, "id": "event_import_events", "sequence": 0, "type": "run.started", "schema_version": 1, "actor_json": string(actor), "data_json": string(data), "recorded_at": "2026-10-01T00:00:00Z"})
	source := CoreWorkItemImportSource{BundlePath: fixture.Write(), OrganizationID: org, ProjectID: project, Objects: &memoryRunDetails{}}
	if err := neutralimport.Read(t.Context(), source.BundlePath, org, func(bundle *neutralimport.Bundle) error {
		record := postgres.CoreWorkItemImport{Item: item}
		if err := readCoreImportEvents(t.Context(), neutralimport.NewIndex(bundle), eventstore.NewContent(source.Objects), source, item, &record, make(map[string][]byte)); err != nil {
			return err
		}
		if len(record.Events) != 1 {
			t.Fatal("verified historical fixture lost its event")
		}
		value, err := eventstore.NewContent(source.Objects).Event(t.Context(), record.Events[0])
		if err != nil {
			return err
		}
		if value.Data.Run == nil || value.Data.Run.Sequence != 0 || value.Data.Run.AttemptID != run.AttemptID {
			t.Fatal("historical event acquired runtime sequence authority")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
