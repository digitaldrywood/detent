package hubserver

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/localboard"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func localBoardFixture(t *testing.T, kind string) (string, []localboard.Record) {
	t.Helper()
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	updated := created.AddDate(0, 0, 1)
	records := []localboard.Record{
		{Key: localboard.SourceKey("example-board", "default", kind, "z-first"), Kind: kind, SourceID: "z-first", Number: 10, Title: "Example z-first", Body: "Prerequisite body", State: "todo", Author: "source-author", CreatedAt: created, UpdatedAt: updated, Priority: new(2)},
		{Key: localboard.SourceKey("example-board", "default", kind, "a-second"), Kind: kind, SourceID: "a-second", Number: 20, Title: "Example a-second", Body: "Dependent body", State: "Retired lane", Author: "source-author", CreatedAt: created, UpdatedAt: updated, Comments: []localboard.Comment{{ID: "1", Body: "Edited comment", CreatedAt: created, UpdatedAt: updated}}},
	}
	records[1].Dependencies = []string{records[0].Key}
	if kind == "github_local" {
		for i := range records {
			records[i].Repository = "example-org/example-project"
		}
	}
	raw, err := json.Marshal(records)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "board.json")
	if err := os.WriteFile(path, raw, 0400); err != nil {
		t.Fatal(err)
	}
	return path, records
}

type localBoardTestRepository struct {
	*postgres.Repository
	admin *sql.DB
}

func localBoardTarget(t *testing.T, service *Service, name string) nativeFixture {
	t.Helper()
	if service == nil {
		repository, admin := coreImportPostgresWithAdmin(t, 3)
		projectRecordDatabases.Store(repository, admin)
		t.Cleanup(func() { projectRecordDatabases.Delete(repository) })
		service = openLocalPostgresHubTestService(t, repository, Config{SecretKeys: secretTestKeys(t, "1", "1"), RunDetails: &memoryRunDetails{}})
		localBoardDatabaseBudget(t, admin)
		for _, org := range []tracker.OrganizationID{"org_first", "org_first_second"} {
			seedPostgresHubAdmin(t, repository, service, nativeOrganization{ID: org, Name: string(org), Slug: strings.ReplaceAll(string(org), "_", "-")})
			key := bytes.Repeat([]byte{0x5a}, 32)
			envelope, err := service.config.SecretKeys.Seal(key, accessstore.SecretAAD(org, "", accessstore.NativeCursorKeyKind))
			if err != nil {
				t.Fatal(err)
			}
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				if err := w.CreateHubIdentity(t.Context(), accessstore.HubIdentity{OrganizationID: org, ID: newNativeID("hub"), CursorKey: key}); err != nil {
					return err
				}
				return w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: org, Kind: accessstore.NativeCursorKeyKind, Envelope: envelope, UpdatedAt: service.config.now()}, accessstore.SecretAudit{OrganizationID: org, Kind: accessstore.NativeCursorKeyKind, Actor: "fixture", Event: "create", KeyVersion: envelope.Version, RecordedAt: service.config.now()})
			}); err != nil {
				t.Fatal(err)
			}
		}
		service.database.hostedOrganization = "org_first"
		service.localOrganization = "org_first"
		service.config.TenantRepository = localBoardTestRepository{repository, admin}
	}
	organization := tracker.OrganizationID("")
	if name == "second-target" {
		organization = "org_first_second"
	}
	f := newNativeFixture(t, service, organization, name)
	if err := service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		record, err := w.ProjectRecord(t.Context(), f.project.ID)
		if err != nil {
			return err
		}
		record.States = nativeTriageStates(append(nativeFixtureStates(), tracker.NativeState{Name: "Backlog", Transitions: []string{"Todo"}}))
		record.Settings.DuplicateChallenge = false
		_, err = w.UpdateProjectRecord(t.Context(), record)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return f
}

func seedLocalBoardIntake(t *testing.T, f nativeFixture, records []localboard.Record) map[string]tracker.NativeIssue {
	t.Helper()
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		record, err := w.ProjectRecord(t.Context(), f.project.ID)
		if err != nil {
			return err
		}
		record.Settings.CheckoutRepository = "example-org/example-project"
		_, err = w.UpdateProjectRecord(t.Context(), record)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	items := map[string]tracker.NativeIssue{}
	for i := len(records) - 1; i >= 0; i-- {
		record := records[i]
		snapshot := linkedSnapshot()
		snapshot.URL = "https://github.com/example-org/example-project/issues/" + strconv.Itoa(record.Number)
		snapshot.Provenance.ExternalID = "I_" + strconv.Itoa(record.Number)
		f.service.config.ImportBackend = linkedTestImporter{snapshot: snapshot}
		response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", f.token, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "intake-" + record.Key}, GitHubIssueURL: snapshot.URL, Body: nativeContractTestBody})
		requireNativeStatus(t, response, http.StatusOK)
		var issue tracker.NativeIssue
		decodeHubResponse(t, response, &issue)
		ref, err := eventstore.NewContent(f.service.runDetailStore()).PrepareLinkedIssueSnapshot(t.Context(), f.project.OrganizationID, snapshot)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
			source, err := w.LinkedSource(t.Context(), f.project.ID, issue.WorkItemID)
			if err != nil {
				return err
			}
			source.SnapshotObjectKey, source.SnapshotSHA256, source.SnapshotBytes, source.SnapshotContentBytes = ref.Key, ref.SHA256, ref.CompressedSize, ref.UncompressedSize
			_, err = w.UpdateLinkedSource(t.Context(), source)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		items[record.SourceID] = issue
	}
	return items
}

func TestLocalBoardImportFixtures(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"local_sqlite", "github_local"} {
		t.Run(kind, func(t *testing.T) {
			path, records := localBoardFixture(t, kind)
			if len(records) != 2 || records[0].SourceID != "z-first" || records[1].SourceID != "a-second" {
				t.Fatalf("dependency order or open selection: %+v", records)
			}
			if len(records[1].Comments) != 1 || records[1].Comments[0].Body != "Edited comment" {
				t.Fatalf("comment event replay: %+v", records[1].Comments)
			}
			markers := []string{"<!-- detent:self-improvement proposal_id=import-first -->", "<!-- detent:self-improvement proposal_id=import-second -->"}
			for index := range records {
				records[index].Body += "\n" + strings.Repeat("x", 70<<10) + "\n" + markers[index]
			}
			records[0].Body += "\n\nhttps://figma.com/design/example"
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			f := localBoardTarget(t, nil, "import")
			targets := []nativeFixture{f, localBoardTarget(t, f.service, "second-target")}
			importedBySource := map[tracker.OrganizationID]map[string]tracker.NativeIssue{}
			for _, f := range targets {
				intake := map[string]tracker.NativeIssue{}
				if kind == "github_local" {
					intake = seedLocalBoardIntake(t, f, records)
				}
				if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
					record, err := w.ProjectRecord(t.Context(), f.project.ID)
					if err != nil {
						return err
					}
					record.Settings.RequireVisualAttachments = true
					_, err = w.UpdateProjectRecord(t.Context(), record)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				dryCounts := localBoardCounts(t, f)
				plans := []localboard.Result{}
				for _, record := range records {
					response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/local-board-import", f.token, localboard.Request{Record: record, DryRun: true})
					requireNativeStatus(t, response, http.StatusOK)
					var plan localboard.Result
					decodeHubResponse(t, response, &plan)
					plans = append(plans, plan)
				}
				if !reflect.DeepEqual(dryCounts, localBoardCounts(t, f)) {
					t.Fatal("dry-run wrote native data")
				}
				var count int
				if err := f.service.config.TenantRepository.(localBoardTestRepository).admin.QueryRowContext(t.Context(), "SELECT count(*) FROM local_board_imports WHERE project_id=$1", f.project.ID).Scan(&count); err != nil || count != 0 {
					t.Fatalf("dry-run wrote imports: %d, %v", count, err)
				}
				imported := []tracker.NativeIssue{}
				for i, record := range records {
					response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/local-board-import", f.token, localboard.Request{Record: record})
					requireNativeStatus(t, response, http.StatusOK)
					var result localboard.Result
					decodeHubResponse(t, response, &result)
					plan := plans[i]
					plan.WorkItemID = result.WorkItemID
					if !reflect.DeepEqual(plan, result) {
						t.Fatalf("dry-run %+v != real-run %+v", plan, result)
					}
					var issue tracker.NativeIssue
					decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(result.WorkItemID), f.token, nil), &issue)
					wantState := "Todo"
					if i == 1 {
						wantState = "Backlog"
					}
					if issue.State != wantState || result.UnknownLane != (i == 1) {
						t.Fatalf("lane: %+v, %+v", issue, result)
					}
					if kind == "github_local" {
						if issue.WorkItemID != intake[record.SourceID].WorkItemID || issue.Title != intake[record.SourceID].Title || issue.Body != intake[record.SourceID].Body {
							t.Fatalf("GitHub match overwrote content or created duplicate: %+v", issue)
						}
					} else if issue.Title != record.Title || issue.Body != record.Body || issue.Provenance == nil || issue.Provenance.AuthorID != "source-author" {
						t.Fatalf("local content/provenance: %+v", issue)
					}
					if kind == "local_sqlite" {
						if i == 0 && (issue.Priority == nil || *issue.Priority != 2) || i == 1 && issue.Priority != nil {
							t.Fatalf("source priority lost: %v", issue.Priority)
						}
					}
					imported = append(imported, issue)
					if importedBySource[f.project.OrganizationID] == nil {
						importedBySource[f.project.OrganizationID] = map[string]tracker.NativeIssue{}
					}
					importedBySource[f.project.OrganizationID][record.Key] = issue
				}
				if !reflect.DeepEqual(imported[1].Dependencies, []tracker.NativeWorkItemID{imported[0].WorkItemID}) {
					t.Fatalf("dependencies: %+v", imported[1].Dependencies)
				}
				repository := f.service.config.TenantRepository.(localBoardTestRepository).Repository
				if err := repository.ReadDoctorProposalIdentities(t.Context(), f.project.OrganizationID, corestore.Serving, func(r postgres.DoctorProposalIdentityReader) error {
					for index, marker := range markers {
						result, err := r.LookupDoctorProposalIdentity(t.Context(), postgres.DoctorProposalIdentitySelection{ProjectID: f.project.ID, Identity: postgres.DoctorProposalIdentity{Kind: "marker", Value: marker}})
						if err != nil {
							return err
						}
						want := tracker.NativeWorkItemID("")
						if kind == "local_sqlite" {
							want = imported[index].WorkItemID
						}
						if result.WorkItemID != want {
							t.Fatalf("imported current-body identity = %s, want %s", result.WorkItemID, want)
						}
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				var comments tracker.Page[tracker.NativeComment]
				commentsResponse := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(imported[1].WorkItemID)+"/comments", f.token, nil)
				requireNativeStatus(t, commentsResponse, http.StatusOK)
				decodeHubResponse(t, commentsResponse, &comments)
				found := false
				for _, comment := range comments.Items {
					if comment.Body == "Edited comment" {
						found = true
						if comment.Provenance == nil || comment.Provenance.Provider != kind || comment.Provenance.AuthorID != "detent" || !comment.Provenance.UpdatedAt.Equal(time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)) {
							t.Fatalf("comment provenance: %+v", comment)
						}
					}
					if comment.Body == "Deleted comment" || comment.Body == "Original comment" {
						t.Fatalf("replayed deleted or stale comment: %+v", comment)
					}
				}
				if !found {
					t.Fatal("missing imported comment")
				}
				transition := tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "native-progress"}, ExpectedRevision: imported[0].Revision, State: "In Progress", Reason: "user_requested"}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(imported[0].WorkItemID)+"/workflow", f.token, transition), http.StatusOK)
				counts := localBoardCounts(t, f)
				if kind == "local_sqlite" {
					admin := f.service.config.TenantRepository.(localBoardTestRepository).admin
					if _, err := admin.ExecContext(t.Context(), `DELETE FROM local_board_imports WHERE organization_id=$1 AND project_id=$2`, f.project.OrganizationID, f.project.ID); err != nil {
						t.Fatal(err)
					}
					for _, record := range records {
						before := localBoardCounts(t, f)
						for _, dry := range []bool{true, false} {
							response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/local-board-import", f.token, localboard.Request{Record: record, DryRun: dry})
							requireNativeStatus(t, response, http.StatusOK)
							var result localboard.Result
							decodeHubResponse(t, response, &result)
							if result.Action != "updated" || result.Comments != 0 || result.Dependencies != 0 || result.WorkItemID != importedBySource[f.project.OrganizationID][record.Key].WorkItemID || record.SourceID == "z-first" && result.State != "In Progress" {
								t.Fatalf("existing canonical source was recreated or changed: %+v", result)
							}
							if dry && !reflect.DeepEqual(before, localBoardCounts(t, f)) {
								t.Fatal("source adoption dry-run persisted rows")
							}
						}
					}
					if after := localBoardCounts(t, f); !reflect.DeepEqual(counts, after) {
						t.Fatalf("source adoption changed canonical rows: %v -> %v", counts, after)
					}
				}
				for _, record := range records {
					response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/local-board-import", f.token, localboard.Request{Record: record})
					requireNativeStatus(t, response, http.StatusOK)
					var result localboard.Result
					decodeHubResponse(t, response, &result)
					if result.Action != "unchanged" || result.Comments != 0 || result.Dependencies != 0 {
						t.Fatalf("replay wrote data: %+v", result)
					}
					if record.SourceID == "z-first" && result.State != "In Progress" {
						t.Fatalf("replay reset native lane: %+v", result)
					}
				}
				if after := localBoardCounts(t, f); !reflect.DeepEqual(counts, after) {
					t.Fatalf("second import created rows: %v -> %v", counts, after)
				}
			}
			localBoardReopen(t, f.service)
			for _, target := range targets {
				before := localBoardCounts(t, target)
				for _, record := range records {
					response := performHubAPIRequest(t, target.service, http.MethodPost, target.base+"/local-board-import", target.token, localboard.Request{Record: record})
					requireNativeStatus(t, response, http.StatusOK)
					var result localboard.Result
					decodeHubResponse(t, response, &result)
					expected := importedBySource[target.project.OrganizationID][record.Key]
					if result.Action != "unchanged" || result.WorkItemID != expected.WorkItemID {
						t.Fatalf("reopened source mapping changed: %+v", result)
					}
					response = performHubAPIRequest(t, target.service, http.MethodGet, target.base+"/work-items/"+string(result.WorkItemID), target.token, nil)
					requireNativeStatus(t, response, http.StatusOK)
					var item tracker.NativeIssue
					decodeHubResponse(t, response, &item)
					if item.OrganizationID != target.project.OrganizationID || item.Title != expected.Title || item.Body != expected.Body || !reflect.DeepEqual(item.Dependencies, expected.Dependencies) {
						t.Fatalf("reopened item lost tenant, content or dependencies: %+v", item)
					}
				}
				if after := localBoardCounts(t, target); !reflect.DeepEqual(before, after) {
					t.Fatalf("reopened replay wrote rows: %v -> %v", before, after)
				}
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("source copy changed")
			}
		})
	}
}

func localBoardCounts(t *testing.T, f nativeFixture) []int {
	t.Helper()
	counts := []int{}
	for _, query := range []string{"SELECT count(*) FROM issues", "SELECT count(*) FROM native_comments", "SELECT count(*) FROM local_board_imports", "SELECT count(*) FROM native_commands", "SELECT count(*) FROM issue_relationships", "SELECT (SELECT count(*) FROM collaboration_versions)+(SELECT count(*) FROM collaboration_events)+(SELECT count(*) FROM native_attempt_events)", "SELECT count(*) FROM collaboration_events", "SELECT count(*) FROM search_documents", "SELECT count(*) FROM archive_objects"} {
		var count int
		if err := f.service.config.TenantRepository.(localBoardTestRepository).admin.QueryRowContext(t.Context(), query).Scan(&count); err != nil {
			t.Fatal(err)
		}
		counts = append(counts, count)
	}
	return counts
}

func TestLocalBoardImportRefusals(t *testing.T) {
	t.Parallel()
	_, records := localBoardFixture(t, "local_sqlite")
	f := localBoardTarget(t, nil, "refusals")
	for _, test := range []struct {
		name, token string
		record      localboard.Record
		status      int
	}{
		{"worker", localBoardWorker(t, f), records[0], http.StatusForbidden},
		{"missing grant", newNativeFixture(t, f.service, "", "no-grant").token, records[0], http.StatusForbidden},
		{"missing prerequisite", f.token, records[1], http.StatusUnprocessableEntity},
		{"invalid content", f.token, func() localboard.Record {
			r := records[0]
			r.Title = strings.Repeat("title-secret", 100)
			r.Body = "body-secret"
			return r
		}(), http.StatusUnprocessableEntity},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := localBoardCounts(t, f)
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/local-board-import", test.token, localboard.Request{Record: test.record})
			requireNativeStatus(t, response, test.status)
			if strings.Contains(response.Body.String(), "title-secret") || strings.Contains(response.Body.String(), "body-secret") {
				t.Fatal("error exposed issue content")
			}
			if after := localBoardCounts(t, f); !reflect.DeepEqual(before, after) {
				t.Fatal("refused import wrote data")
			}
		})
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/local-board-import", f.token, localboard.Request{Record: records[0]}), http.StatusOK)
	changed := records[0]
	changed.Body += " changed"
	before := localBoardCounts(t, f)
	for _, dry := range []bool{true, false} {
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/local-board-import", f.token, localboard.Request{Record: changed, DryRun: dry}), http.StatusUnprocessableEntity)
	}
	if after := localBoardCounts(t, f); !reflect.DeepEqual(before, after) {
		t.Fatalf("changed pinned source wrote rows: %v -> %v", before, after)
	}

}

func TestLocalBoardImportHostedAuthorityAndAllowances(t *testing.T) {
	t.Parallel()
	_, records := localBoardFixture(t, "local_sqlite")
	admin := postgresStartupTemplate.Database(t)
	for _, test := range []struct {
		name, role, scope string
		limits            map[string]int64
		status            int
	}{
		{"operator", "owner", "write", nil, http.StatusOK},
		{"viewer", "viewer", "read", nil, http.StatusForbidden},
		{"reporter", "reporter", "write", nil, http.StatusForbidden},
		{"storage allowance", "owner", "write", map[string]int64{"collaboration_bytes": 1}, http.StatusTooManyRequests},
		{"issue allowance", "owner", "write", map[string]int64{"unarchived_issues": 0}, http.StatusTooManyRequests},
		{"terminal issue allowance", "owner", "write", map[string]int64{"unarchived_issues": 0}, http.StatusOK},
		{"content exceeds storage", "owner", "write", map[string]int64{"collaboration_bytes": int64(len(records[0].Title) + len(records[0].Body) - 1)}, http.StatusTooManyRequests},
		{"history allowance", "owner", "write", map[string]int64{"history_records": 0}, http.StatusTooManyRequests},
		{"history includes event", "owner", "write", map[string]int64{"history_records": 1}, http.StatusTooManyRequests},
		{"mutation allowance", "owner", "write", map[string]int64{"api_mutations": 1}, http.StatusTooManyRequests},
		{"event allowance", "owner", "write", map[string]int64{"ingested_events": 0}, http.StatusTooManyRequests},
	} {
		t.Run(test.name, func(t *testing.T) {
			organization := tracker.OrganizationID("org_allowance_" + strings.ReplaceAll(test.name, " ", "_"))
			f := localBoardHostedFixture(t, admin, organization)
			repository := f.service.config.TenantRepository.(localBoardTestRepository).Repository
			localBoardHostedPlans(t, f.service, test.limits)
			if test.name == "mutation allowance" {
				if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
					now := f.service.config.now()
					plans := f.service.database.plans()
					window := time.Unix(now.Unix()/plans.WindowSeconds*plans.WindowSeconds, 0).UTC()
					_, err := w.ApplyBillingUsage(t.Context(), billingstore.UsageReceipt{OrganizationID: organization, ID: "existing-api-mutation", WindowStart: window, Metric: "api_mutations", Amount: 1}, 1)
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			user := f.user(t, test.role, test.role, test.role+"@example.test", "", "")
			if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				member, err := w.AccessMember(t.Context(), user.identity.Subject)
				if err != nil {
					return err
				}
				if err := w.SetAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: organization, UserID: member.UserID, ProjectID: f.project, CanWrite: test.scope == "write"}); err != nil {
					return err
				}
				return w.SetAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization, TokenID: member.PrincipalID, ProjectID: f.project})
			}); err != nil {
				t.Fatal(err)
			}
			record := records[0]
			if test.name == "terminal issue allowance" {
				record.State = "Done"
			}
			before := localBoardCounts(t, nativeFixture{service: f.service})
			usageBefore, err := localBoardHostedUsage(t, f.service)
			if err != nil {
				t.Fatal(err)
			}
			dry := f.request(t, user, http.MethodPost, f.base+"/local-board-import", localboard.Request{Record: record, DryRun: true})
			requireNativeStatus(t, dry, test.status)
			if !reflect.DeepEqual(before, localBoardCounts(t, nativeFixture{service: f.service})) {
				t.Fatal("dry-run persisted rows")
			}
			usageAfter, err := localBoardHostedUsage(t, f.service)
			if err != nil {
				t.Fatal(err)
			}
			for _, metric := range []string{"api_mutations", "ingested_events", "collaboration_bytes", "history_records", "events_total", "unarchived_issues"} {
				if usageBefore[metric] != usageAfter[metric] {
					t.Fatalf("dry-run changed %s: before=%d after=%d", metric, usageBefore[metric], usageAfter[metric])
				}
			}
			response := f.request(t, user, http.MethodPost, f.base+"/local-board-import", localboard.Request{Record: record})
			requireNativeStatus(t, response, test.status)
			if test.status != http.StatusOK && !reflect.DeepEqual(before, localBoardCounts(t, nativeFixture{service: f.service})) {
				t.Fatal("rejected import persisted rows")
			}
			if test.name == "operator" {
				localBoardHostedPlans(t, f.service, map[string]int64{"unarchived_issues": 0, "collaboration_bytes": 0, "history_records": 0, "api_mutations": 0, "ingested_events": 0})
				before := localBoardCounts(t, nativeFixture{service: f.service})
				for _, dryRun := range []bool{true, false} {
					response := f.request(t, user, http.MethodPost, f.base+"/local-board-import", localboard.Request{Record: record, DryRun: dryRun})
					requireNativeStatus(t, response, http.StatusOK)
					var result localboard.Result
					decodeHubResponse(t, response, &result)
					if result.Action != "unchanged" {
						t.Fatalf("replay at allowance limit: %+v", result)
					}
				}
				if !reflect.DeepEqual(before, localBoardCounts(t, nativeFixture{service: f.service})) {
					t.Fatal("replay at allowance limit wrote rows")
				}
			}
		})
	}
}

func TestLocalBoardGitHubMatchRefusals(t *testing.T) {
	t.Parallel()
	_, records := localBoardFixture(t, "github_local")
	for _, test := range []string{"missing intake", "repository mismatch", "native lane changed", "terminal native item"} {
		t.Run(test, func(t *testing.T) {
			f := localBoardTarget(t, nil, test)
			intake := seedLocalBoardIntake(t, f, records)
			record := records[0]
			switch test {
			case "missing intake":
				record.Number = 999
			case "repository mismatch":
				record.Repository = "other-org/other-project"
			case "native lane changed", "terminal native item":
				issue := intake[record.SourceID]
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/workflow", f.token, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "move-native"}, ExpectedRevision: issue.Revision, State: "Todo", Reason: "user_requested"})
				requireNativeStatus(t, response, http.StatusOK)
				decodeHubResponse(t, response, &issue)
				if test == "terminal native item" {
					response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/workflow", f.token, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "complete-native"}, ExpectedRevision: issue.Revision, State: "Done", Reason: "user_requested"})
					requireNativeStatus(t, response, http.StatusOK)
				}
			}
			before := localBoardCounts(t, f)
			for _, dry := range []bool{true, false} {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/local-board-import", f.token, localboard.Request{Record: record, DryRun: dry}), http.StatusUnprocessableEntity)
			}
			if after := localBoardCounts(t, f); !reflect.DeepEqual(before, after) {
				t.Fatal("unmatched or protected GitHub item changed")
			}
		})
	}
}

func localBoardHostedPlans(t *testing.T, service *Service, limits map[string]int64) {
	t.Helper()
	plans := *service.database.plans()
	plans.Plans = slices.Clone(plans.Plans)
	var base HostedPlan
	for _, plan := range plans.Plans {
		if plan.PlanReference == plans.Base {
			base = plan
		}
	}
	base.ID = newNativeID("plan")
	base.Allowances = maps.Clone(base.Allowances)
	maps.Copy(base.Allowances, limits)
	plans.Plans = append(plans.Plans, base)
	plans.Base = base.PlanReference
	cfg := *service.config.Hosted
	cfg.Plans = &plans
	if err := service.database.configureHostedPlans(t.Context(), &cfg); err != nil {
		t.Fatal(err)
	}
	if err := service.config.TenantRepository.WriteTenant(t.Context(), tracker.OrganizationID(cfg.OrganizationID), corestore.Serving, func(w tenantstore.Writer) error {
		current, err := w.BillingAssignment(t.Context())
		if err != nil {
			return err
		}
		current.Base = billingstore.PlanReference{ID: plans.Base.ID, Version: plans.Base.Version}
		return w.SetBillingAssignment(t.Context(), current, current.Revision)
	}); err != nil {
		t.Fatal(err)
	}
}

func localBoardHostedUsage(t *testing.T, service *Service) (map[string]int64, error) {
	t.Helper()
	result := map[string]int64{}
	admin := service.config.TenantRepository.(localBoardTestRepository).admin
	for key, query := range map[string]string{
		"collaboration_bytes": "SELECT COALESCE(sum(value),0) FROM hosted_usage_counters WHERE name='collaboration_bytes'",
		"history_records":     "SELECT (SELECT count(*) FROM collaboration_versions)+(SELECT count(*) FROM collaboration_events)+(SELECT count(*) FROM native_attempt_events)",
		"events_total":        "SELECT count(*) FROM collaboration_events",
		"unarchived_issues":   "SELECT count(*) FROM issues WHERE NOT archived",
	} {
		var count int64
		if err := admin.QueryRowContext(t.Context(), query).Scan(&count); err != nil {
			return nil, err
		}
		result[key] = count
	}
	organization := service.accessOrganization()
	now := service.config.now()
	plans := service.database.plans()
	window := time.Unix(now.Unix()/plans.WindowSeconds*plans.WindowSeconds, 0).UTC()
	if err := service.config.TenantRepository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
		for _, metric := range []string{"api_mutations", "ingested_events"} {
			value, err := r.BillingUsage(t.Context(), window, metric)
			if errors.Is(err, corestore.ErrNotFound) {
				value, err = 0, nil
			}
			if err != nil {
				return err
			}
			result[metric] = value
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return result, nil
}

func localBoardWorker(t *testing.T, f nativeFixture) string {
	t.Helper()
	token := newNativeID("worker")
	now := f.service.config.now()
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: f.project.OrganizationID, ID: token, Name: "Example worker", Hash: apikey.HashToken(token), Scope: string(apiScopeWorker), NativeOnly: true, Kind: "service", ProjectAccess: "selected", CreatedAt: now, UpdatedAt: now}); err != nil {
			return err
		}
		return w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: f.project.OrganizationID, TokenID: token, ProjectID: f.project.ID})
	}); err != nil {
		t.Fatal(err)
	}
	return token
}

func TestLocalBoardImportRollsBackAtSourceMappingWrite(t *testing.T) {
	f := localBoardTarget(t, nil, "rollback")
	_, records := localBoardFixture(t, "local_sqlite")
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/local-board-import", f.token, localboard.Request{Record: records[0]}), http.StatusOK)
	admin := f.service.config.TenantRepository.(localBoardTestRepository).admin
	if _, err := admin.ExecContext(t.Context(), `CREATE FUNCTION refuse_local_board_mapping() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN RAISE EXCEPTION 'mapping write refused'; END$$; CREATE TRIGGER refuse_local_board_mapping BEFORE INSERT ON local_board_imports FOR EACH ROW EXECUTE FUNCTION refuse_local_board_mapping()`); err != nil {
		t.Fatal(err)
	}
	before := localBoardCounts(t, f)
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/local-board-import", f.token, localboard.Request{Record: records[1]})
	requireNativeStatus(t, response, http.StatusInternalServerError)
	if after := localBoardCounts(t, f); !reflect.DeepEqual(before, after) {
		t.Fatalf("failed final mapping left partial item, comment, dependency, version, event, search or archive rows: %v -> %v", before, after)
	}
}

func localBoardReopen(t *testing.T, service *Service) {
	t.Helper()
	previous := service.config.TenantRepository.(localBoardTestRepository)
	if err := previous.Close(); err != nil {
		t.Fatal(err)
	}
	connection := startupConnection(t, previous.admin, "detent_runtime_v2", "")
	if _, err := previous.admin.ExecContext(t.Context(), "ALTER DATABASE "+pgx.Identifier{connection.Database}.Sanitize()+" CONNECTION LIMIT 4"); err != nil {
		t.Fatal(err)
	}
	cfg := postgresStartupTemplate.NativeConfig(t, previous.admin, connection.User, connection.Password)
	cfg.MaxConns = 3
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := postgres.NewRepository(t.Context(), pool, postgres.PoolConfig{MaxConnections: 3, ServingConnections: 2, BackgroundConnections: 1})
	if err != nil {
		pool.Close()
		t.Fatal(err)
	}
	service.database.postgres = repository
	service.database.billingRepository = repository
	service.config.TenantRepository = localBoardTestRepository{repository, previous.admin}
	service.config.AccessRepository = repository
	service.config.BillingRepository = repository
	service.requestMetrics.repository = repository
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Error(err)
		}
	})
}

func localBoardHostedFixture(t *testing.T, admin *sql.DB, organization tracker.OrganizationID) hostedSecurityFixture {
	t.Helper()
	connection := startupConnection(t, admin, "detent_runtime_v2", "")
	native := postgresStartupTemplate.NativeConfig(t, admin, connection.User, connection.Password)
	native.MaxConns = 3
	pool, err := pgxpool.NewWithConfig(t.Context(), native)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	repository, err := postgres.NewRepository(t.Context(), pool, postgres.PoolConfig{MaxConnections: 3, ServingConnections: 2, BackgroundConnections: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repository.Close(); err != nil {
			t.Error(err)
		}
	})
	projectRecordDatabases.Store(repository, admin)
	t.Cleanup(func() { projectRecordDatabases.Delete(repository) })
	f, _ := newChangePolicyPostgresTenantFixture(t, repository, organization, &memoryRunDetails{})
	service := f.service
	service.workerCancel()
	<-service.workerDone
	<-service.secretRewrapDone
	if err := service.database.postgres.Close(); err != nil {
		t.Fatal(err)
	}
	service.database.postgres = repository
	service.database.billingRepository = repository
	service.config.TenantRepository = localBoardTestRepository{repository, admin}
	service.config.AccessRepository = repository
	service.config.BillingRepository = repository
	service.requestMetrics.repository = repository
	localBoardDatabaseBudget(t, admin)
	return f
}

func localBoardDatabaseBudget(t *testing.T, admin *sql.DB) {
	t.Helper()
	var name string
	if err := admin.QueryRowContext(t.Context(), "SELECT current_database()").Scan(&name); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(t.Context(), "ALTER DATABASE "+pgx.Identifier{name}.Sanitize()+" CONNECTION LIMIT 4"); err != nil {
		t.Fatal(err)
	}
}
