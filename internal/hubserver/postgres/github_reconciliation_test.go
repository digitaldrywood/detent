package postgres_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestGitHubReconciliationRetainsFencedTenantCursors(t *testing.T) {
	admin := pgstoretest.Template.Database(t)
	repository, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	now := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	organizations := []tracker.OrganizationID{"first", "second"}
	outcomes := make(map[tracker.OrganizationID]projectstore.GitHubReconciliation)
	for index, org := range organizations {
		failedAt := now.Add(time.Duration(index+1) * time.Second)
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: string(org), Slug: string(org), CreatedAt: now}); err != nil {
				return err
			}
			for _, project := range []tracker.ProjectID{"one", "two"} {
				record, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: project, Name: string(project), Profile: "native"}, States: []tracker.NativeState{{Name: "Todo"}}})
				if err != nil {
					return err
				}
				record.Settings.Repository = "example-org/" + string(project)
				if _, err := w.UpdateProjectRecord(t.Context(), record); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		for _, project := range []tracker.ProjectID{"one", "two"} {
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				owner := w.(projectstore.GitHubReconciliationWriter)
				row, err := owner.NextGitHubReconciliation(t.Context(), now.Add(-time.Second))
				if err != nil {
					return err
				}
				if row.ProjectID != project || row.Revision != 0 || row.NodeID != "" || row.Repository != "example-org/"+string(project) {
					t.Fatalf("uninitialized binding=%+v", row)
				}
				if row.LastSuccessfulSweepAt != nil || row.LastFailureAt != nil {
					t.Fatalf("uninitialized binding reported outcomes: %+v", row)
				}
				row.NodeID, row.NextAttemptAt = "R_"+string(project), now
				return owner.SaveGitHubReconciliation(t.Context(), row)
			}); err != nil {
				t.Fatal(err)
			}
		}
		for _, project := range []tracker.ProjectID{"one", "two"} {
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				owner := w.(projectstore.GitHubReconciliationWriter)
				row, err := owner.NextGitHubReconciliation(t.Context(), now)
				if err != nil {
					return err
				}
				if row.ProjectID != project || row.Revision != 1 || row.AfterWorkItem != "" {
					t.Fatalf("cursor=%+v", row)
				}
				row.AfterWorkItem = "wi_after"
				row.Attempts = 2
				row.LastFailureAt = &failedAt
				row.NextAttemptAt = now.Add(time.Minute)
				if err := owner.SaveGitHubReconciliation(t.Context(), row); err != nil {
					return err
				}
				row.LastSuccessfulSweepAt = &failedAt
				if err := owner.SaveGitHubReconciliation(t.Context(), row); !errors.Is(err, corestore.ErrRevisionConflict) {
					t.Fatalf("stale cursor update=%v", err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
		if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			owner := r.(projectstore.GitHubReconciliationReader)
			if _, err := owner.NextGitHubReconciliation(t.Context(), now); !errors.Is(err, corestore.ErrNotFound) {
				t.Fatalf("future retry selected=%v", err)
			}
			row, err := owner.NextGitHubReconciliation(t.Context(), now.Add(time.Minute))
			if err != nil {
				return err
			}
			if row.AfterWorkItem != "wi_after" || row.Attempts != 2 || row.Revision != 2 {
				t.Fatalf("recovered=%+v", row)
			}
			if row.LastSuccessfulSweepAt != nil || row.LastFailureAt == nil || !row.LastFailureAt.Equal(failedAt) {
				t.Fatalf("recovered failure or stale sweep outcome: %+v", row)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		t.Run(string(org)+"/outcomes", func(t *testing.T) {
			completeAt := failedAt.Add(time.Minute)
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				owner := w.(projectstore.GitHubReconciliationWriter)
				row, err := owner.GitHubReconciliation(t.Context(), "one")
				if err != nil {
					return err
				}
				row.AfterWorkItem, row.Attempts, row.NextAttemptAt = "", 0, completeAt.Add(time.Hour)
				row.LastSuccessfulSweepAt = &completeAt
				wrongSource := row
				wrongSource.NodeID = "R_other"
				if err := owner.SaveGitHubReconciliation(t.Context(), wrongSource); !errors.Is(err, corestore.ErrRevisionConflict) {
					t.Fatalf("different node wrote a sweep outcome: %v", err)
				}
				return owner.SaveGitHubReconciliation(t.Context(), row)
			}); err != nil {
				t.Fatal(err)
			}
			read := func(project tracker.ProjectID) projectstore.GitHubReconciliation {
				t.Helper()
				var row projectstore.GitHubReconciliation
				if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
					var err error
					row, err = r.(projectstore.GitHubReconciliationReader).GitHubReconciliation(t.Context(), project)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				return row
			}
			committed := read("one")
			if committed.LastSuccessfulSweepAt == nil || !committed.LastSuccessfulSweepAt.Equal(completeAt) || committed.LastFailureAt == nil || !committed.LastFailureAt.Equal(failedAt) || committed.AfterWorkItem != "" || committed.Attempts != 0 {
				t.Fatalf("successful sweep lost outcome or history: %+v", committed)
			}
			outcomes[org] = committed
			rollback := errors.New("rollback reconciliation")
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				row := committed
				failure := completeAt.Add(time.Second)
				row.LastFailureAt = &failure
				if err := w.(projectstore.GitHubReconciliationWriter).SaveGitHubReconciliation(t.Context(), row); err != nil {
					return err
				}
				return rollback
			}); !errors.Is(err, rollback) {
				t.Fatalf("rollback outcome write: %v", err)
			}
			if row := read("one"); !reflect.DeepEqual(row, committed) {
				t.Fatalf("rolled-back outcome is visible: %+v", row)
			}

			old := read("two")
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				record, err := w.ProjectRecord(t.Context(), "two")
				if err != nil {
					return err
				}
				record.Settings.CheckoutRepository = "Example-Org/Checkout"
				_, err = w.UpdateProjectRecord(t.Context(), record)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				owner := w.(projectstore.GitHubReconciliationWriter)
				if _, err := owner.GitHubReconciliation(t.Context(), "two"); !errors.Is(err, corestore.ErrNotFound) {
					t.Fatalf("old binding outcome visible: %v", err)
				}
				if err := owner.SaveGitHubReconciliation(t.Context(), old); !errors.Is(err, corestore.ErrRevisionConflict) {
					t.Fatalf("old binding wrote outcome: %v", err)
				}
				row, err := owner.NextGitHubReconciliation(t.Context(), completeAt)
				if err != nil {
					return err
				}
				if row.ProjectID != "two" || row.Repository != "example-org/checkout" || row.Revision != old.Revision || row.NodeID != "" || row.AfterWorkItem != "" || row.Attempts != 0 || row.LastFailureAt != nil || row.LastSuccessfulSweepAt != nil {
					t.Fatalf("replacement inherited the previous binding: %+v", row)
				}
				row.NodeID = "R_checkout"
				return owner.SaveGitHubReconciliation(t.Context(), row)
			}); err != nil {
				t.Fatal(err)
			}
			if row := read("two"); row.Repository != "example-org/checkout" || row.NodeID != "R_checkout" || row.Revision != old.Revision+1 || row.LastFailureAt != nil || row.LastSuccessfulSweepAt != nil {
				t.Fatalf("wrong replacement binding outcome: %+v", row)
			}
		})
	}
	for _, org := range organizations {
		if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			row, err := r.(projectstore.GitHubReconciliationReader).GitHubReconciliation(t.Context(), "one")
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(row, outcomes[org]) {
				t.Fatalf("organization %s observed a different outcome: %+v", org, row)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("webhook_policy_landing", func(t *testing.T) {
		for _, org := range []tracker.OrganizationID{"org_webhook_first", "org_webhook_second"} {
			content := eventstore.NewContent(pgstoretest.NewHistoryObjects())
			write := func(run func(tenantstore.Writer) error) {
				t.Helper()
				if err := repository.WriteTenant(t.Context(), org, corestore.Serving, run); err != nil {
					t.Fatal(err)
				}
			}
			write(func(w tenantstore.Writer) error {
				if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: string(org), Slug: string(org), CreatedAt: now}); err != nil {
					return err
				}
				for _, project := range []tracker.ProjectID{"one", "two"} {
					row, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: project, Name: string(project), Profile: "native"}, States: []tracker.NativeState{{Name: "Todo"}}})
					if err != nil {
						return err
					}
					row.Settings.Repository = "example-org/example-project"
					if _, err := w.UpdateProjectRecord(t.Context(), row); err != nil {
						return err
					}
				}
				return nil
			})
			for _, project := range []tracker.ProjectID{"one", "two"} {
				observation := projectstore.GitHubPullRequestObservation{Repository: "example-org/example-project", Number: 18, HeadSHA: strings.Repeat("a", 40), SourceUpdatedAt: now, ObservedAt: now, State: "merged", RepositoryDefaultBranch: new("develop"), MergeSHA: new(strings.Repeat("b", 40)), BaseRef: new("develop"), MergedAt: new(now), MergedBy: new("example-" + string(project))}
				prepareSource := func(value projectstore.GitHubPullRequestObservation, delivery string) projectstore.GitHubWebhook {
					t.Helper()
					raw := pgstoretest.HistoryJSON(t, map[string]any{"action": "closed", "repository": map[string]any{"full_name": value.Repository, "default_branch": value.RepositoryDefaultBranch}, "pull_request": map[string]any{"node_id": "PR_" + string(project), "number": value.Number, "title": "Workflow source", "state": "closed", "draft": value.Draft, "html_url": "https://github.com/example-org/example-project/pull/18", "created_at": now.Add(-2 * time.Hour), "updated_at": value.SourceUpdatedAt, "merged_at": value.MergedAt, "merge_commit_sha": value.MergeSHA, "merged_by": map[string]any{"login": value.MergedBy}, "head": map[string]any{"ref": "workflow-source", "sha": value.HeadSHA}, "base": map[string]any{"ref": value.BaseRef, "sha": strings.Repeat("a", 40)}}})
					ref, err := content.PrepareReceipt(t.Context(), eventstore.Receipt{OrganizationID: org}, raw)
					if err != nil {
						t.Fatal(err)
					}
					source := projectstore.GitHubWebhook{DeliveryID: delivery, Repository: value.Repository, EventType: "pull_request", Action: "closed", Digest: policy.Digest(raw), Payload: ref, ReceivedAt: now, ExpiresAt: now.Add(time.Hour), NextAttemptAt: now, Status: "pending"}
					write(func(w tenantstore.Writer) error {
						created, err := w.(projectstore.GitHubIntakeWriter).CreateGitHubWebhook(t.Context(), source)
						if err == nil && !created {
							t.Fatal("source delivery already exists")
						}
						return err
					})
					return source
				}
				source := prepareSource(observation, "landing_"+string(project))
				for range 2 {
					write(func(w tenantstore.Writer) error {
						return w.(projectstore.GitHubIntakeWriter).RecordGitHubPullRequestSource(t.Context(), project, source, observation)
					})
				}
				var sourceRows int
				if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM github_pull_request_refresh WHERE organization_id=$1 AND project_id=$2 AND source_kind='webhook' AND repository=$3 AND pull_number=$4 AND change_id IS NULL AND work_item_id IS NULL AND requested_version IS NULL AND requested_head_sha IS NULL AND request_count IS NULL AND requested_at IS NULL AND next_attempt_at IS NULL AND checks_total IS NULL AND reviews_approvals IS NULL AND NOT pending`, org, project, observation.Repository, observation.Number).Scan(&sourceRows); err != nil || sourceRows != 1 {
					t.Fatalf("standalone source rows=%d want1 without invented origin/request/check facts err=%v", sourceRows, err)
				}
				older := observation
				older.SourceUpdatedAt = now.Add(-time.Hour)
				older.State, older.MergedAt, older.MergeSHA, older.MergedBy = "closed", nil, new(""), new("")
				olderSource := prepareSource(older, "older_landing_"+string(project))
				write(func(w tenantstore.Writer) error {
					return w.(projectstore.GitHubIntakeWriter).RecordGitHubPullRequestSource(t.Context(), project, olderSource, older)
				})
				for _, test := range []struct {
					name, repository, commit, branch string
					found                            bool
				}{
					{"matching", observation.Repository, *observation.MergeSHA, "develop", true},
					{"branchless history", observation.Repository, *observation.MergeSHA, "", true},
					{"different repository", "other/repository", *observation.MergeSHA, "develop", false},
					{"different commit", observation.Repository, strings.Repeat("c", 40), "develop", false},
					{"different branch", observation.Repository, *observation.MergeSHA, "feature", false},
				} {
					t.Run(string(org)+"/"+string(project)+"/"+test.name, func(t *testing.T) {
						err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
							got, err := r.ProjectPolicyLanding(t.Context(), project, test.repository, test.commit, test.branch)
							if !test.found {
								if !errors.Is(err, corestore.ErrNotFound) {
									t.Fatalf("unrelated PR landing=%+v err=%v", got, err)
								}
								return nil
							}
							if err != nil {
								return err
							}
							if got.ChangeID != "" || got.Native != nil || got.PullRequestURL != "https://github.com/example-org/example-project/pull/18" || got.Actor != *observation.MergedBy {
								t.Fatalf("standalone PR provenance=%+v", got)
							}
							return nil
						})
						if err != nil {
							t.Fatal(err)
						}
					})
				}
				foreign := source
				foreign.Digest = "foreign"
				if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
					return w.(projectstore.GitHubIntakeWriter).RecordGitHubPullRequestSource(t.Context(), project, foreign, observation)
				}); !errors.Is(err, corestore.ErrRevisionConflict) {
					t.Fatalf("unverified webhook source error=%v", err)
				}
				write(func(w tenantstore.Writer) error {
					row, err := w.ProjectRecord(t.Context(), project)
					if err != nil {
						return err
					}
					row.Settings.Repository = "other/repository"
					_, err = w.UpdateProjectRecord(t.Context(), row)
					return err
				})
				if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
					return w.(projectstore.GitHubIntakeWriter).RecordGitHubPullRequestSource(t.Context(), project, source, observation)
				}); !errors.Is(err, corestore.ErrRevisionConflict) {
					t.Fatalf("removed project binding authorized source=%v", err)
				}
				if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
					if got, err := r.ProjectPolicyLanding(t.Context(), project, observation.Repository, *observation.MergeSHA, "develop"); !errors.Is(err, corestore.ErrNotFound) {
						t.Fatalf("removed current binding authorized new policy=%+v err=%v", got, err)
					}
					got, err := r.ProjectPolicyLanding(t.Context(), project, observation.Repository, *observation.MergeSHA, "")
					if err != nil {
						return err
					}
					if got.Actor != *observation.MergedBy || got.PullRequestURL != "https://github.com/example-org/example-project/pull/18" {
						t.Fatalf("historical binding lost provenance=%+v", got)
					}
					_, err = r.(projectstore.GitHubPullRequestRefreshReader).NextGitHubPullRequestRefresh(t.Context(), now.Add(time.Hour))
					if !errors.Is(err, corestore.ErrNotFound) {
						t.Fatalf("standalone source became refresh demand=%v", err)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
		}
	})

	t.Run("pull_request_refresh", func(t *testing.T) {
		saved := make(map[tracker.OrganizationID]projectstore.GitHubPullRequestRefresh)
		for orgIndex, org := range []tracker.OrganizationID{"org_refresh_first", "org_refresh_second"} {
			t.Run(string(org), func(t *testing.T) {
				content := eventstore.NewContent(pgstoretest.NewHistoryObjects())
				write := func(fn func(tenantstore.Writer) error) {
					t.Helper()
					if err := repository.WriteTenant(t.Context(), org, corestore.Serving, fn); err != nil {
						t.Fatal(err)
					}
				}
				rows := make([]projectstore.GitHubPullRequestRefresh, 0, 3)
				changes := make(map[string]eventstore.Change)
				versions := make(map[string]eventstore.Version)
				for _, name := range []string{"a", "b", "c"} {
					project := tracker.ProjectID("prj_shared")
					if name == "c" {
						project = "prj_other"
					}
					item := tracker.NativeWorkItemID("wi_" + name)
					change, err := content.PrepareChange(t.Context(), org, pgstoretest.HistoryJSON(t, tracker.ChangeRequest{ID: "change_" + name, OrganizationID: org, ProjectID: project, WorkItemID: item, Title: "Example " + name, Body: "Change body", Revision: 1, CreatedAt: now, UpdatedAt: now}))
					if err != nil {
						t.Fatal(err)
					}
					version, err := content.PrepareVersion(t.Context(), org, pgstoretest.HistoryJSON(t, tracker.ChangeVersion{ID: "version_" + name, ChangeID: change.ID, Number: 1, ChangeVersionInput: tracker.ChangeVersionInput{HeadSHA: "head_" + name, BaseSHA: "base", Repository: "example-org/example-project", PolicyID: "policy"}, Actor: pgstoretest.HistoryActor(), CreatedAt: now}))
					if err != nil {
						t.Fatal(err)
					}
					source, err := content.PrepareSourceBundle(t.Context(), org, []byte("source_"+string(org)+name))
					if err != nil {
						t.Fatal(err)
					}
					version.SourceObjectKey, version.SourceSHA256, version.SourceBytes, version.SourceContentBytes = source.Key, source.SHA256, source.CompressedSize, source.UncompressedSize
					changes[name], versions[name] = change, version
				}
				write(func(w tenantstore.Writer) error {
					if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: string(org), Slug: string(org), CreatedAt: now}); err != nil {
						return err
					}
					for _, project := range []tracker.ProjectID{"prj_shared", "prj_other"} {
						record, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: project, Name: string(project), Profile: "native"}, States: []tracker.NativeState{{Name: "Todo"}}})
						if err != nil {
							return err
						}
						record.Settings.CheckoutRepository, record.Settings.RepositoryEnabled = "example-org/example-project", true
						if _, err := w.UpdateProjectRecord(t.Context(), record); err != nil {
							return err
						}
					}
					for index, name := range []string{"a", "b", "c"} {
						project, number := tracker.ProjectID("prj_shared"), int64(index+1)
						if name == "c" {
							project, number = "prj_other", 1
						}
						item := tracker.NativeWorkItemID("wi_" + name)
						if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: project, NativeID: item, Number: number, Title: "Example " + name, ActorKind: new("human"), ActorPrincipalID: new("example"), CreatedAt: now, UpdatedAt: now, NativeCreatedAt: now, NativeUpdatedAt: now, LastActivityAt: now}); err != nil {
							return err
						}
						change, err := w.CreateChange(t.Context(), changes[name])
						if err != nil {
							return err
						}
						version := versions[name]
						if _, err := w.CreateVersion(t.Context(), version); err != nil {
							return err
						}
						change.CurrentVersionID = new(version.ID)
						if _, err := w.UpdateChange(t.Context(), change); err != nil {
							return err
						}
						if err := w.LinkIssue(t.Context(), eventstore.Link{OrganizationID: org, ProjectID: project, ChangeID: change.ID, WorkItemID: item}); err != nil {
							return err
						}
						rows = append(rows, projectstore.GitHubPullRequestRefresh{GitHubPullRequestRefreshKey: projectstore.GitHubPullRequestRefreshKey{ProjectID: project, ChangeID: change.ID, Repository: "example-org/example-project", Number: 18}, WorkItemID: item, RequestedVersion: version.ID, RequestedHeadSHA: version.HeadSHA, RequestedAt: now})
					}
					if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: "prj_shared", NativeID: "wi_a_second", Number: 99, Title: "Second linked item", ActorKind: new("human"), ActorPrincipalID: new("example"), CreatedAt: now, UpdatedAt: now, NativeCreatedAt: now, NativeUpdatedAt: now, LastActivityAt: now}); err != nil {
						return err
					}
					if err := w.LinkIssue(t.Context(), eventstore.Link{OrganizationID: org, ProjectID: "prj_shared", ChangeID: changes["a"].ID, WorkItemID: "wi_a_second"}); err != nil {
						return err
					}
					return w.(projectstore.GitHubPullRequestRefreshWriter).RequestGitHubPullRequests(t.Context(), rows)
				})
				read := func(key projectstore.GitHubPullRequestRefreshKey) projectstore.GitHubPullRequestRefresh {
					t.Helper()
					var row projectstore.GitHubPullRequestRefresh
					if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
						var err error
						row, err = r.(projectstore.GitHubPullRequestRefreshReader).GitHubPullRequestRefresh(t.Context(), key)
						return err
					}); err != nil {
						t.Fatal(err)
					}
					return row
				}
				readItem := func(id tracker.NativeWorkItemID) corestore.WorkItem {
					t.Helper()
					var item corestore.WorkItem
					if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
						var err error
						item, err = r.WorkItem(t.Context(), "prj_shared", id)
						return err
					}); err != nil {
						t.Fatal(err)
					}
					return item
				}
				firstBefore, secondBefore := readItem("wi_a"), readItem("wi_a_second")
				a, b, c := read(rows[0].GitHubPullRequestRefreshKey), read(rows[1].GitHubPullRequestRefreshKey), read(rows[2].GitHubPullRequestRefreshKey)
				for _, row := range []projectstore.GitHubPullRequestRefresh{a, b, c} {
					if !row.Pending || row.RequestCount != 1 || row.Attempts != 0 || row.Observation != nil {
						t.Fatalf("new view=%+v", row)
					}
				}
				observation := projectstore.GitHubPullRequestObservation{MergeSHA: new(""), BaseRef: new("develop"), MergedBy: new(""), Repository: a.Repository, Number: a.Number, HeadSHA: a.RequestedHeadSHA, SourceUpdatedAt: now.Add(time.Second), ObservedAt: now.Add(time.Duration(orgIndex+2) * time.Second), State: "closed", Draft: true, Mergeable: new(false), Checks: tracker.CheckSummary{Status: "completed", Conclusion: "failure", Total: 3, Passed: 2, Failed: 1}, Reviews: tracker.ReviewSummary{State: "p1", Decision: "changes_requested", Approvals: 1, Comments: 1}}
				write(func(w tenantstore.Writer) error {
					return w.(projectstore.GitHubPullRequestRefreshWriter).CompleteGitHubPullRequestRefresh(t.Context(), a, observation)
				})
				continuation := read(a.GitHubPullRequestRefreshKey)
				if !continuation.Pending || continuation.WorkItemID != "wi_a_second" || continuation.ObservedRequestCount != a.RequestCount || continuation.Observation == nil {
					t.Fatalf("missing bounded continuation: %+v", continuation)
				}
				firstAfter, secondAfter := readItem("wi_a"), readItem("wi_a_second")
				if !firstAfter.LastActivityAt.Equal(observation.ObservedAt) || !secondAfter.LastActivityAt.Equal(secondBefore.LastActivityAt) || !firstAfter.UpdatedAt.Equal(firstBefore.UpdatedAt) || firstAfter.Revision != firstBefore.Revision || firstAfter.EventSequence != firstBefore.EventSequence {
					t.Fatalf("completion changed more than one item's activity: first=%+v second=%+v", firstAfter, secondAfter)
				}
				if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
					return w.(projectstore.GitHubPullRequestRefreshWriter).CompleteGitHubPullRequestRefresh(t.Context(), a, observation)
				}); !errors.Is(err, corestore.ErrRevisionConflict) {
					t.Fatalf("stale item cursor completed: %v", err)
				}
				write(func(w tenantstore.Writer) error {
					return w.(projectstore.GitHubPullRequestRefreshWriter).CompleteGitHubPullRequestRefresh(t.Context(), continuation, observation)
				})
				secondAfter = readItem("wi_a_second")
				if !secondAfter.LastActivityAt.Equal(observation.ObservedAt) || !secondAfter.UpdatedAt.Equal(secondBefore.UpdatedAt) || secondAfter.Revision != secondBefore.Revision || secondAfter.EventSequence != secondBefore.EventSequence {
					t.Fatalf("linked completion changed issue identity: %+v", secondAfter)
				}
				if got := read(a.GitHubPullRequestRefreshKey); got.Pending || got.Observation == nil || !reflect.DeepEqual(*got.Observation, observation) {
					t.Fatalf("published=%+v", got)
				}
				if !reflect.DeepEqual(read(b.GitHubPullRequestRefreshKey), b) || !reflect.DeepEqual(read(c.GitHubPullRequestRefreshKey), c) {
					t.Fatal("publishing A changed another view of the same PR")
				}
				if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
					due, err := r.(projectstore.GitHubPullRequestRefreshReader).NextGitHubPullRequestRefresh(t.Context(), now)
					if err == nil && due.GitHubPullRequestRefreshKey != c.GitHubPullRequestRefreshKey {
						t.Fatalf("due key=%+v", due)
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
				if _, err := admin.ExecContext(t.Context(), "UPDATE github_pull_request_refresh SET observed_request_count=NULL WHERE organization_id=$1 AND project_id=$2 AND change_id=$3", org, a.ProjectID, a.ChangeID); err != nil {
					t.Fatal(err)
				}
				if legacy := read(a.GitHubPullRequestRefreshKey); legacy.ObservedRequestCount != 0 || legacy.Observation == nil || !reflect.DeepEqual(*legacy.Observation, observation) {
					t.Fatalf("legacy observation lost or invented generation: %+v", legacy)
				}
				rollback := errors.New("rollback demand")
				if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
					if err := w.(projectstore.GitHubPullRequestRefreshWriter).RequestGitHubPullRequests(t.Context(), rows[:1]); err != nil {
						return err
					}
					return rollback
				}); !errors.Is(err, rollback) {
					t.Fatalf("rollback=%v", err)
				}
				committed := read(a.GitHubPullRequestRefreshKey)
				if committed.RequestCount != 1 || committed.Pending || committed.Observation == nil {
					t.Fatalf("rolled back request=%+v", committed)
				}
				invalid := rows[0]
				invalid.ChangeID = "missing"
				if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
					return w.(projectstore.GitHubPullRequestRefreshWriter).RequestGitHubPullRequests(t.Context(), []projectstore.GitHubPullRequestRefresh{rows[0], invalid})
				}); !errors.Is(err, corestore.ErrRevisionConflict) {
					t.Fatalf("invalid batch=%v", err)
				}
				if !reflect.DeepEqual(read(a.GitHubPullRequestRefreshKey), committed) {
					t.Fatal("invalid batch partially updated a view")
				}
				write(func(w tenantstore.Writer) error {
					return w.(projectstore.GitHubPullRequestRefreshWriter).RequestGitHubPullRequests(t.Context(), rows[:1])
				})
				fresh := read(a.GitHubPullRequestRefreshKey)
				if fresh.WorkItemID != "wi_a" || fresh.ObservedRequestCount == fresh.RequestCount {
					t.Fatalf("new request reused previous generation: %+v", fresh)
				}
				write(func(w tenantstore.Writer) error {
					return w.(projectstore.GitHubPullRequestRefreshWriter).CompleteGitHubPullRequestRefresh(t.Context(), fresh, observation)
				})
				staleContinuation := read(a.GitHubPullRequestRefreshKey)
				write(func(w tenantstore.Writer) error {
					return w.(projectstore.GitHubPullRequestRefreshWriter).RequestGitHubPullRequests(t.Context(), rows[:1])
				})
				newRequest := read(a.GitHubPullRequestRefreshKey)
				if newRequest.WorkItemID != "wi_a" || newRequest.ObservedRequestCount == newRequest.RequestCount {
					t.Fatalf("request failed to reset continuation: %+v", newRequest)
				}
				if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
					return w.(projectstore.GitHubPullRequestRefreshWriter).CompleteGitHubPullRequestRefresh(t.Context(), staleContinuation, observation)
				}); !errors.Is(err, corestore.ErrRevisionConflict) {
					t.Fatalf("superseded continuation completed: %v", err)
				}
				if !reflect.DeepEqual(read(a.GitHubPullRequestRefreshKey), newRequest) {
					t.Fatal("stale continuation changed current demand")
				}
				lowerObservation := observation
				lowerObservation.ObservedAt = observation.SourceUpdatedAt
				for range 2 {
					current := read(a.GitHubPullRequestRefreshKey)
					write(func(w tenantstore.Writer) error {
						return w.(projectstore.GitHubPullRequestRefreshWriter).CompleteGitHubPullRequestRefresh(t.Context(), current, lowerObservation)
					})
				}
				for _, item := range []tracker.NativeWorkItemID{"wi_a", "wi_a_second"} {
					if got := readItem(item); !got.LastActivityAt.Equal(observation.ObservedAt) {
						t.Fatalf("older observation regressed item activity: %+v", got)
					}
				}
				write(func(w tenantstore.Writer) error {
					if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: "prj_shared", NativeID: "wi_a_middle", Number: 100, Title: "Removed linked item", ActorKind: new("human"), ActorPrincipalID: new("example"), CreatedAt: now, UpdatedAt: now, NativeCreatedAt: now, NativeUpdatedAt: now, LastActivityAt: now}); err != nil {
						return err
					}
					return w.LinkIssue(t.Context(), eventstore.Link{OrganizationID: org, ProjectID: "prj_shared", ChangeID: a.ChangeID, WorkItemID: "wi_a_middle"})
				})
				write(func(w tenantstore.Writer) error {
					return w.(projectstore.GitHubPullRequestRefreshWriter).RequestGitHubPullRequests(t.Context(), rows[:1])
				})
				walkObservation := observation
				walkObservation.SourceUpdatedAt, walkObservation.ObservedAt = now.Add(time.Hour), now.Add(time.Hour+time.Second)
				walk := read(a.GitHubPullRequestRefreshKey)
				write(func(w tenantstore.Writer) error {
					return w.(projectstore.GitHubPullRequestRefreshWriter).CompleteGitHubPullRequestRefresh(t.Context(), walk, walkObservation)
				})
				walk = read(a.GitHubPullRequestRefreshKey)
				if !walk.Pending || walk.WorkItemID != "wi_a_middle" {
					t.Fatalf("missing middle cursor: %+v", walk)
				}
				if _, err := admin.ExecContext(t.Context(), "DELETE FROM change_issue_links WHERE organization_id=$1 AND project_id=$2 AND change_id=$3 AND work_item_id='wi_a_middle'", org, a.ProjectID, a.ChangeID); err != nil {
					t.Fatal(err)
				}
				write(func(w tenantstore.Writer) error {
					return w.(projectstore.GitHubPullRequestRefreshWriter).CompleteGitHubPullRequestRefresh(t.Context(), walk, walkObservation)
				})
				walk = read(a.GitHubPullRequestRefreshKey)
				if !walk.Pending || walk.WorkItemID != "wi_a_second" || !readItem("wi_a_middle").LastActivityAt.Equal(now) || !readItem("wi_a_second").LastActivityAt.Equal(observation.ObservedAt) {
					t.Fatalf("removed target ended walk or changed an unlinked item: %+v", walk)
				}
				write(func(w tenantstore.Writer) error {
					return w.(projectstore.GitHubPullRequestRefreshWriter).CompleteGitHubPullRequestRefresh(t.Context(), walk, walkObservation)
				})
				if read(a.GitHubPullRequestRefreshKey).Pending || !readItem("wi_a_second").LastActivityAt.Equal(walkObservation.ObservedAt) || !readItem("wi_a_middle").LastActivityAt.Equal(now) {
					t.Fatal("remaining linked item lost the observation after mid-walk unlink")
				}
				write(func(w tenantstore.Writer) error {
					return w.(projectstore.GitHubPullRequestRefreshWriter).RetryGitHubPullRequestRefresh(t.Context(), b, now.Add(time.Hour))
				})
				backoff := read(b.GitHubPullRequestRefreshKey)
				rows[1].RequestedAt = now.Add(time.Minute)
				write(func(w tenantstore.Writer) error {
					return w.(projectstore.GitHubPullRequestRefreshWriter).RequestGitHubPullRequests(t.Context(), rows[1:2])
				})
				newer := read(b.GitHubPullRequestRefreshKey)
				if newer.RequestCount != 2 || newer.Attempts != backoff.Attempts || newer.Attempts != 1 || !newer.NextAttemptAt.Equal(backoff.NextAttemptAt) {
					t.Fatalf("same-source request reset backoff: before=%+v after=%+v", backoff, newer)
				}
				staleObservation := observation
				staleObservation.HeadSHA = b.RequestedHeadSHA
				if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
					return w.(projectstore.GitHubPullRequestRefreshWriter).CompleteGitHubPullRequestRefresh(t.Context(), b, staleObservation)
				}); !errors.Is(err, corestore.ErrRevisionConflict) {
					t.Fatalf("stale completion=%v", err)
				}
				if !reflect.DeepEqual(read(b.GitHubPullRequestRefreshKey), newer) {
					t.Fatal("stale response changed a newer demand")
				}
				updateVersion := func(row projectstore.GitHubPullRequestRefresh, id, head string) projectstore.GitHubPullRequestRefresh {
					t.Helper()
					version, err := content.PrepareVersion(t.Context(), org, pgstoretest.HistoryJSON(t, tracker.ChangeVersion{ID: id, ChangeID: row.ChangeID, Number: 2, ChangeVersionInput: tracker.ChangeVersionInput{HeadSHA: head, BaseSHA: "base", Repository: row.Repository, PolicyID: "policy"}, Actor: pgstoretest.HistoryActor(), CreatedAt: now.Add(time.Minute)}))
					if err != nil {
						t.Fatal(err)
					}
					write(func(w tenantstore.Writer) error {
						if _, err := w.CreateVersion(t.Context(), version); err != nil {
							return err
						}
						change, err := w.Change(t.Context(), row.ChangeID)
						if err != nil {
							return err
						}
						change.CurrentVersionID = new(version.ID)
						_, err = w.UpdateChange(t.Context(), change)
						return err
					})
					row.RequestedVersion, row.RequestedHeadSHA, row.RequestedAt = version.ID, version.HeadSHA, now.Add(2*time.Minute)
					return row
				}
				rows[1] = updateVersion(newer, "version_b_next", "head_b_next")
				if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
					return w.(projectstore.GitHubPullRequestRefreshWriter).CancelGitHubPullRequestRefresh(t.Context(), newer)
				}); !errors.Is(err, corestore.ErrRevisionConflict) {
					t.Fatalf("live changed-version demand cancelled=%v", err)
				}
				if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
					return w.(projectstore.GitHubPullRequestRefreshWriter).CompleteGitHubPullRequestRefresh(t.Context(), newer, staleObservation)
				}); !errors.Is(err, corestore.ErrRevisionConflict) {
					t.Fatalf("old canonical version published=%v", err)
				}
				write(func(w tenantstore.Writer) error {
					return w.(projectstore.GitHubPullRequestRefreshWriter).RequestGitHubPullRequests(t.Context(), rows[1:2])
				})
				changed := read(b.GitHubPullRequestRefreshKey)
				if changed.RequestCount != 3 || changed.Attempts != 0 || !changed.NextAttemptAt.Equal(rows[1].RequestedAt) || changed.RequestedHeadSHA != "head_b_next" {
					t.Fatalf("changed canonical source did not reset retry=%+v", changed)
				}
				write(func(w tenantstore.Writer) error {
					return w.(projectstore.GitHubPullRequestRefreshWriter).RequestGitHubPullRequests(t.Context(), rows[:1])
				})
				a = read(a.GitHubPullRequestRefreshKey)
				older := observation
				older.SourceUpdatedAt = now
				for _, obs := range []projectstore.GitHubPullRequestObservation{older, func() projectstore.GitHubPullRequestObservation {
					v := observation
					v.HeadSHA = "foreign_head"
					return v
				}()} {
					if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
						return w.(projectstore.GitHubPullRequestRefreshWriter).CompleteGitHubPullRequestRefresh(t.Context(), a, obs)
					}); !errors.Is(err, corestore.ErrRevisionConflict) {
						t.Fatalf("invalid observation=%v", err)
					}
					if !reflect.DeepEqual(read(a.GitHubPullRequestRefreshKey), a) {
						t.Fatal("invalid observation mutated demand or prior facts")
					}
				}

				{
					for _, field := range []string{"count", "version", "head", "item", "project", "change", "repository", "number"} {
						stale := a
						switch field {
						case "count":
							stale.RequestCount--
						case "version":
							stale.RequestedVersion = "another_version"
						case "head":
							stale.RequestedHeadSHA = "another_head"
						case "item":
							stale.WorkItemID = "wi_another"
						case "project":
							stale.ProjectID = "another_project"
						case "change":
							stale.ChangeID = "another_change"
						case "repository":
							stale.Repository = "example-org/another"
						case "number":
							stale.Number++
						}
						if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
							return w.(projectstore.GitHubPullRequestRefreshWriter).SupersedeGitHubPullRequestRefresh(t.Context(), stale)
						}); !errors.Is(err, corestore.ErrRevisionConflict) {
							t.Fatalf("%s supersede ignored fence: %v", field, err)
						}
						if !reflect.DeepEqual(read(a.GitHubPullRequestRefreshKey), a) {
							t.Fatalf("%s stale supersede changed demand", field)
						}
					}
					if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
						if err := w.(projectstore.GitHubPullRequestRefreshWriter).SupersedeGitHubPullRequestRefresh(t.Context(), a); err != nil {
							return err
						}
						return rollback
					}); !errors.Is(err, rollback) {
						t.Fatalf("supersede rollback=%v", err)
					}
					if !reflect.DeepEqual(read(a.GitHubPullRequestRefreshKey), a) {
						t.Fatal("supersede escaped rollback")
					}
					write(func(w tenantstore.Writer) error {
						return w.(projectstore.GitHubPullRequestRefreshWriter).SupersedeGitHubPullRequestRefresh(t.Context(), a)
					})
					expected := a
					expected.Pending = false
					if !reflect.DeepEqual(read(a.GitHubPullRequestRefreshKey), expected) {
						t.Fatal("supersede altered observation, attempts or source")
					}
					if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
						return w.(projectstore.GitHubPullRequestRefreshWriter).SupersedeGitHubPullRequestRefresh(t.Context(), a)
					}); !errors.Is(err, corestore.ErrRevisionConflict) {
						t.Fatalf("already completed supersede=%v", err)
					}
					if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
						next, err := r.(projectstore.GitHubPullRequestRefreshReader).NextGitHubPullRequestRefresh(t.Context(), now.Add(24*time.Hour))
						if err == nil && next.GitHubPullRequestRefreshKey == a.GitHubPullRequestRefreshKey {
							t.Fatal("superseded demand retained due slot")
						}
						return err
					}); err != nil {
						t.Fatal(err)
					}
					write(func(w tenantstore.Writer) error {
						request := rows[0]
						request.RequestedAt = now.Add(7 * time.Hour)
						return w.(projectstore.GitHubPullRequestRefreshWriter).RequestGitHubPullRequests(t.Context(), []projectstore.GitHubPullRequestRefresh{request})
					})
					rearmed := read(a.GitHubPullRequestRefreshKey)
					if !rearmed.Pending || rearmed.RequestCount != a.RequestCount+1 || !reflect.DeepEqual(rearmed.Observation, a.Observation) {
						t.Fatalf("existing request owner did not rearm superseded demand=%+v", rearmed)
					}
					if !rearmed.NextAttemptAt.Equal(now.Add(7 * time.Hour)) {
						t.Fatalf("rearmed demand retained its completed due time: %s", rearmed.NextAttemptAt)
					}
					a = rearmed
				}

				pollMerged := observation
				pollMerged.State, pollMerged.MergeSHA, pollMerged.BaseRef, pollMerged.MergedAt, pollMerged.MergedBy, pollMerged.RepositoryDefaultBranch = "merged", new(strings.Repeat("d", 40)), new("develop"), new(now.Add(5*time.Hour)), new("poll-owner"), new("develop")
				pollMerged.SourceUpdatedAt, pollMerged.ObservedAt = now.Add(6*time.Hour), now.Add(6*time.Hour)
				write(func(w tenantstore.Writer) error {
					return w.(projectstore.GitHubPullRequestRefreshWriter).CompleteGitHubPullRequestRefresh(t.Context(), a, pollMerged)
				})
				expectPoll := func(branch string, found bool) {
					t.Helper()
					if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
						got, err := r.ProjectPolicyLanding(t.Context(), a.ProjectID, a.Repository, *pollMerged.MergeSHA, branch)
						if !found {
							if !errors.Is(err, corestore.ErrNotFound) {
								t.Fatalf("unverified poll binding authorized landing=%+v err=%v", got, err)
							}
							return nil
						}
						if err != nil {
							return err
						}
						if got.PullRequestURL != "https://github.com/example-org/example-project/pull/18" || got.Actor != "poll-owner" || got.ChangeID != "" {
							t.Fatalf("poll-only landing provenance=%+v", got)
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				}
				expectPoll("", false)
				write(func(w tenantstore.Writer) error {
					row, err := w.ProjectRecord(t.Context(), a.ProjectID)
					if err != nil {
						return err
					}
					row.Settings.Repository = a.Repository
					_, err = w.UpdateProjectRecord(t.Context(), row)
					return err
				})
				expectPoll("develop", false)
				write(func(w tenantstore.Writer) error {
					return w.(projectstore.GitHubPullRequestRefreshWriter).RequestGitHubPullRequests(t.Context(), rows[:1])
				})
				a = read(a.GitHubPullRequestRefreshKey)
				pollMerged.SourceUpdatedAt, pollMerged.ObservedAt = now.Add(7*time.Hour), now.Add(7*time.Hour)
				write(func(w tenantstore.Writer) error {
					return w.(projectstore.GitHubPullRequestRefreshWriter).CompleteGitHubPullRequestRefresh(t.Context(), a, pollMerged)
				})
				a = read(a.GitHubPullRequestRefreshKey)
				expectPoll("develop", true)
				expectPoll("", true)
				write(func(w tenantstore.Writer) error {
					row, err := w.ProjectRecord(t.Context(), a.ProjectID)
					if err != nil {
						return err
					}
					row.Settings.Repository = "other/repository"
					_, err = w.UpdateProjectRecord(t.Context(), row)
					return err
				})
				expectPoll("develop", false)
				expectPoll("", true)
				write(func(w tenantstore.Writer) error {
					project, err := w.ProjectRecord(t.Context(), a.ProjectID)
					if err != nil {
						return err
					}
					project.Settings.RepositoryEnabled = false
					if _, err = w.UpdateProjectRecord(t.Context(), project); err != nil {
						return err
					}
					return w.(projectstore.GitHubPullRequestRefreshWriter).CancelGitHubPullRequestRefresh(t.Context(), a)
				})
				dead := read(a.GitHubPullRequestRefreshKey)
				if dead.Pending || !reflect.DeepEqual(dead.Observation, a.Observation) || dead.RequestCount != a.RequestCount || dead.Attempts != a.Attempts {
					t.Fatalf("dead origin lost observation=%+v", dead)
				}
				write(func(w tenantstore.Writer) error {
					return w.(projectstore.GitHubPullRequestRefreshWriter).CancelGitHubPullRequestRefresh(t.Context(), changed)
				})
				write(func(w tenantstore.Writer) error {
					project, err := w.ProjectRecord(t.Context(), c.ProjectID)
					if err != nil {
						return err
					}
					if err = w.RetireProject(t.Context(), c.ProjectID, project.Settings.Revision, now.Add(time.Hour)); err != nil {
						return err
					}
					return w.(projectstore.GitHubPullRequestRefreshWriter).CancelGitHubPullRequestRefresh(t.Context(), c)
				})
				if got := read(c.GitHubPullRequestRefreshKey); got.Pending || got.Observation != nil {
					t.Fatalf("deleted project retained due work=%+v", got)
				}
				if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
					_, err := r.(projectstore.GitHubPullRequestRefreshReader).NextGitHubPullRequestRefresh(t.Context(), now.Add(24*time.Hour))
					return err
				}); !errors.Is(err, corestore.ErrNotFound) {
					t.Fatalf("dead demand selected=%v", err)
				}
				saved[org] = dead
			})
		}
		for org, want := range saved {
			if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
				got, err := r.(projectstore.GitHubPullRequestRefreshReader).GitHubPullRequestRefresh(t.Context(), want.GitHubPullRequestRefreshKey)
				if err == nil && !reflect.DeepEqual(got, want) {
					t.Fatalf("tenant %s observed another tenant view", org)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
	})

	t.Run("moved_head_observation_preserves_requested_authority", func(t *testing.T) {
		content := eventstore.NewContent(pgstoretest.NewHistoryObjects())
		key := projectstore.GitHubPullRequestRefreshKey{ProjectID: "prj_moved", ChangeID: "change_moved", Repository: "example-org/example-project", Number: 18}
		organizations := []tracker.OrganizationID{"org_moved_first", "org_moved_second"}
		saved := make(map[tracker.OrganizationID]projectstore.GitHubPullRequestRefresh)
		read := func(org tracker.OrganizationID) projectstore.GitHubPullRequestRefresh {
			t.Helper()
			var row projectstore.GitHubPullRequestRefresh
			if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
				var err error
				row, err = r.(projectstore.GitHubPullRequestRefreshReader).GitHubPullRequestRefresh(t.Context(), key)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			return row
		}
		for _, org := range organizations {
			change, err := content.PrepareChange(t.Context(), org, pgstoretest.HistoryJSON(t, tracker.ChangeRequest{ID: key.ChangeID, OrganizationID: org, ProjectID: key.ProjectID, WorkItemID: "wi_moved", Title: "Moved head", Revision: 1, CreatedAt: now, UpdatedAt: now}))
			if err != nil {
				t.Fatal(err)
			}
			version, err := content.PrepareVersion(t.Context(), org, pgstoretest.HistoryJSON(t, tracker.ChangeVersion{ID: "version_moved", ChangeID: change.ID, Number: 1, ChangeVersionInput: tracker.ChangeVersionInput{HeadSHA: "original_head", BaseSHA: "base", Repository: key.Repository, PolicyID: "policy"}, Actor: pgstoretest.HistoryActor(), CreatedAt: now}))
			if err != nil {
				t.Fatal(err)
			}
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: string(org), Slug: string(org), CreatedAt: now}); err != nil {
					return err
				}
				project, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: key.ProjectID, Name: "Example", Profile: "native"}, States: []tracker.NativeState{{Name: "Todo"}}})
				if err != nil {
					return err
				}
				project.Settings.CheckoutRepository, project.Settings.RepositoryEnabled = key.Repository, true
				if _, err := w.UpdateProjectRecord(t.Context(), project); err != nil {
					return err
				}
				if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: key.ProjectID, NativeID: "wi_moved", Number: 1, Title: "Moved head", ActorKind: new("human"), ActorPrincipalID: new("example"), CreatedAt: now, UpdatedAt: now, NativeCreatedAt: now, NativeUpdatedAt: now, LastActivityAt: now}); err != nil {
					return err
				}
				created, err := w.CreateChange(t.Context(), change)
				if err != nil {
					return err
				}
				if _, err := w.CreateVersion(t.Context(), version); err != nil {
					return err
				}
				created.CurrentVersionID = new(version.ID)
				if _, err := w.UpdateChange(t.Context(), created); err != nil {
					return err
				}
				if err := w.LinkIssue(t.Context(), eventstore.Link{OrganizationID: org, ProjectID: key.ProjectID, ChangeID: change.ID, WorkItemID: "wi_moved"}); err != nil {
					return err
				}
				return w.(projectstore.GitHubPullRequestRefreshWriter).RequestGitHubPullRequests(t.Context(), []projectstore.GitHubPullRequestRefresh{{GitHubPullRequestRefreshKey: key, WorkItemID: "wi_moved", RequestedVersion: version.ID, RequestedHeadSHA: version.HeadSHA, RequestedAt: now}})
			}); err != nil {
				t.Fatal(err)
			}
			saved[org] = read(org)
		}
		for index, org := range organizations {
			before := saved[org]
			observation := projectstore.GitHubPullRequestObservation{Repository: key.Repository, Number: key.Number, HeadSHA: "observed_moved_head", SourceUpdatedAt: now.Add(time.Second), ObservedAt: now.Add(2 * time.Second), State: "open", Reviews: tracker.ReviewSummary{State: "known", Decision: "changes_requested"}}
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				return w.(projectstore.GitHubPullRequestRefreshWriter).CompleteGitHubPullRequestRefresh(t.Context(), before, observation)
			}); err != nil {
				t.Fatal(err)
			}
			after := read(org)
			if after.Pending || after.Observation == nil || !reflect.DeepEqual(*after.Observation, observation) || after.RequestedHeadSHA != "original_head" || after.RequestedVersion != before.RequestedVersion || after.ObservedRequestCount != before.RequestCount {
				t.Fatalf("moved head changed source authority: %+v", after)
			}
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				return w.(projectstore.GitHubPullRequestRefreshWriter).RequestGitHubPullRequests(t.Context(), []projectstore.GitHubPullRequestRefresh{before})
			}); err != nil {
				t.Fatal(err)
			}
			current := read(org)
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				return w.(projectstore.GitHubPullRequestRefreshWriter).CompleteGitHubPullRequestRefresh(t.Context(), before, observation)
			}); !errors.Is(err, corestore.ErrRevisionConflict) {
				t.Fatalf("stale request accepted moved head: %v", err)
			}
			if !reflect.DeepEqual(read(org), current) {
				t.Fatal("stale completion changed current demand")
			}
			saved[org] = current
			if index == 0 && !reflect.DeepEqual(read(organizations[1]), saved[organizations[1]]) {
				t.Fatal("moved head changed peer tenant")
			}
			if index == 1 && !reflect.DeepEqual(read(organizations[0]), saved[organizations[0]]) {
				t.Fatal("peer completion changed first tenant")
			}
		}
	})

}
