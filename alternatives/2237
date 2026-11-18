package coreimport_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/coreimport"
	"github.com/digitaldrywood/detent/internal/hubserver/coreimport/coreimporttest"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/githubcontract"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/neutralimport"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestCollaborationDeltaMigration(t *testing.T) {
	t.Parallel()
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("verification_failure_%t", fail), func(t *testing.T) {
			f := hubfixture.NewDefaultNative(t, hubfixture.Config{})
			org, project := f.Project.OrganizationID, f.Project.ID
			admin := f.Hub.Admin
			const revisions = 2*neutralimport.VersionSnapshotInterval + 3
			source := coreimporttest.WorkItems(t, os.DirFS("../../../.."), org, project, false, coreimporttest.WorkItemBody())
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
					issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: project, WorkItemID: tracker.NativeWorkItemID(item), Number: 1 + index*2, Revision: tracker.Revision(revision), Profile: "native"}, Title: "migration " + name, Body: coreimporttest.WorkItemBody(), State: "Todo", Labels: []string{"needs-review"}, Assignees: []string{"example-assignee"}, Actor: tracker.Actor{Kind: "human", PrincipalID: "example-owner"}, CreatedAt: parseTestTime(t, "2026-10-01T00:00:00Z"), UpdatedAt: parseTestTime(t, "2026-10-02T00:00:00Z"), LastActivityAt: parseTestTime(t, "2026-10-02T00:00:00Z")}
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
			input := coreimport.CoreWorkItemImportSource{BundlePath: source.Write(), OrganizationID: org, ProjectID: project, Objects: f.Hub.Config.RunDetails}
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
				if err := f.Hub.Repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
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
				_, err = coreimport.ImportCoreWorkItems(t.Context(), f.Hub.Repository, input)
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
			if count, err := coreimport.ImportCoreWorkItems(t.Context(), f.Hub.Repository, input); err != nil || count != 2 {
				t.Fatalf("history conversion count=%d error=%v", count, err)
			}
			content := eventstore.NewContent(f.Hub.Config.RunDetails)
			var payloadBytes int64
			for record, revisions := range original {
				for revision, expected := range revisions {
					var version eventstore.CollaborationVersion
					if err := f.Hub.Repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
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
			if count, err := coreimport.ImportCoreWorkItems(t.Context(), f.Hub.Repository, input); err != nil || count != 2 {
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
