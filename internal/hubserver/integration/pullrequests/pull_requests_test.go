package pullrequests_test

import (
	"fmt"
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const pullRequestTestURL = "https://github.com/example/repo/pull/1"

func TestWorkItemPullRequestProjection(t *testing.T) {
	t.Parallel()
	var clock atomic.Int64
	clock.Store(time.Now().UTC().Truncate(time.Microsecond).UnixNano())
	repository, f, owner, _ := newExportPostgresFixtureInOrganization(t, postgresStartupTemplate.Database(t), "org_security", func(cfg *Config) {
		cfg.now = func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	})
	response := f.request(t, owner, http.MethodPut, f.base+"/change-review-policy", tracker.ApproveChangeReviewPolicy{Mutation: tracker.Mutation{IdempotencyKey: "projection-review-policy"}, Policy: tracker.ChangeReviewPolicy{PolicyID: hubTestPolicy().ID, RequireReview: true}})
	requireNativeStatus(t, response, http.StatusOK)
	t.Run("cross Change precedence", func(t *testing.T) {
		for _, entity := range []string{"first", "second"} {
			t.Run(entity, func(t *testing.T) {
				response := f.request(t, owner, http.MethodPost, f.base+"/work-items", tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "cross-projection-" + entity}, Title: "Example cross Change " + entity, Body: workItemRecordBody, State: "Todo"})
				requireNativeStatus(t, response, http.StatusOK)
				var item tracker.NativeIssue
				decodeHubResponse(t, response, &item)
				itemPath := f.base + "/work-items/" + string(item.WorkItemID)
				create := func(name string) tracker.ChangeRequest {
					t.Helper()
					clock.Add(int64(time.Second))
					response := f.request(t, owner, http.MethodPost, itemPath+"/changes", tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "cross-change-" + entity + "-" + name}, Title: "Example " + name + " change", Body: "Proposed work"})
					requireNativeStatus(t, response, http.StatusOK)
					var result tracker.ChangeRequest
					decodeHubResponse(t, response, &result)
					return result
				}
				older := create("older")
				versions := map[string]string{}
				numbers := map[string]int64{}
				heads := map[string]string{}
				newer := tracker.ChangeRequest{}
				for _, test := range []struct {
					name      string
					newChange bool
					older     bool
					external  string
					want      int
				}{
					{name: "older PR", older: true, external: "11", want: 11},
					{name: "new Change without PR retains older PR", newChange: true, want: 11},
					{name: "older Change next PR retains canonical new Change", older: true, external: "12", want: 12},
					{name: "newer Change PR wins", external: "21", want: 21},
					{name: "later publication on older Change cannot win", older: true, external: "13", want: 21},
					{name: "newer Change retains its PR", want: 21},
				} {
					t.Run(test.name, func(t *testing.T) {
						if test.newChange {
							newer = create("newer")
						}
						target := newer
						if test.older {
							target = older
						}
						input := changeTestInput()
						if test.external != "" {
							input.External = &tracker.ChangeExternalReference{Provider: "github", ID: test.external, URL: "https://github.com/example/repo/pull/" + test.external}
						}
						bundle := changeTestSource(t, &input, []byte("Example cross Change "+entity+" "+test.name+"\n"))
						response := f.request(t, owner, http.MethodPost, itemPath+"/changes/"+target.ID+"/versions", tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "cross/" + entity + "/" + test.name}, ExpectedVersionID: versions[target.ID], ChangeVersionInput: input, SourceBundle: bundle})
						requireNativeStatus(t, response, http.StatusOK)
						var version tracker.ChangeVersion
						decodeHubResponse(t, response, &version)
						if version.ID == "" || version.ID == versions[target.ID] || version.ChangeID != target.ID || version.Number <= numbers[target.ID] || version.HeadSHA != input.HeadSHA {
							t.Fatalf("publication did not create target Change's next version: %+v", version)
						}
						if (input.External == nil) != (version.External == nil) || input.External != nil && *input.External != *version.External {
							t.Fatalf("immutable version external changed: got %+v, want %+v", version.External, input.External)
						}
						versions[target.ID], numbers[target.ID], heads[target.ID] = version.ID, version.Number, input.HeadSHA
						canonical := older
						if newer.ID != "" {
							canonical = newer
						}
						drainTestNativeBoard(t, f.service)
						for _, compact := range []bool{false, true} {
							var issue tracker.NativeIssue
							if compact {
								if err := repository.ReadTenant(t.Context(), "org_security", corestore.Serving, func(r tenantstore.Reader) error {
									record, err := r.BoardItem(t.Context(), f.project, item.WorkItemID)
									if err != nil {
										return err
									}
									if record.ListIssue == nil {
										return workItemStorageUnavailable()
									}
									issue = *record.ListIssue
									return nil
								}); err != nil {
									t.Fatal(err)
								}
							} else {
								response := f.request(t, owner, http.MethodGet, itemPath+"?include=change", nil)
								requireNativeStatus(t, response, http.StatusOK)
								decodeHubResponse(t, response, &issue)
								if issue.Change == nil || issue.Change.ChangeID != canonical.ID || issue.Change.HeadSHA != heads[canonical.ID] {
									t.Fatalf("retained PR changed canonical Change/head: %+v, want %s/%s", issue.Change, canonical.ID, heads[canonical.ID])
								}
							}
							if issue.PullRequest == nil || issue.PullRequest.Number != test.want || issue.PullRequest.URL != fmt.Sprintf("https://github.com/example/repo/pull/%d", test.want) || issue.Terminal {
								t.Fatalf("compact=%v PR=%+v terminal=%v, want PR=%d nonterminal", compact, issue.PullRequest, issue.Terminal, test.want)
							}
						}
					})
				}
			})
		}
	})
	for _, entity := range []string{"first", "second"} {
		t.Run(entity, func(t *testing.T) {
			response := f.request(t, owner, http.MethodPost, f.base+"/work-items", tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "projection-" + entity}, Title: "Example " + entity, Body: workItemRecordBody, State: "Todo"})
			requireNativeStatus(t, response, http.StatusOK)
			var item tracker.NativeIssue
			decodeHubResponse(t, response, &item)
			itemPath := f.base + "/work-items/" + string(item.WorkItemID)
			response = f.request(t, owner, http.MethodPost, itemPath+"/changes", tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "projection-change-" + entity}, Title: "Example " + entity + " change", Body: "Proposed work"})
			requireNativeStatus(t, response, http.StatusOK)
			var change tracker.ChangeRequest
			decodeHubResponse(t, response, &change)
			var current string
			var previousNumber int64
			var want *tracker.NativePullRequest
			for _, test := range []struct {
				name     string
				external *tracker.ChangeExternalReference
				terminal bool
			}{
				{name: "native version without PR"},
				{name: "linked PR", external: &tracker.ChangeExternalReference{Provider: "github", ID: "1", URL: pullRequestTestURL}},
				{name: "new version retains PR"},
				{name: "latest PR", external: &tracker.ChangeExternalReference{Provider: "github", ID: "42", URL: "https://github.com/example/repo/pull/42"}},
				{name: "completion retains PR", terminal: true},
			} {
				t.Run(test.name, func(t *testing.T) {
					input := changeTestInput()
					input.External = test.external
					bundle := changeTestSource(t, &input, []byte("Example projection "+entity+" "+test.name+"\n"))
					response := f.request(t, owner, http.MethodPost, itemPath+"/changes/"+change.ID+"/versions", tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: entity + "/" + test.name}, ExpectedVersionID: current, ChangeVersionInput: input, SourceBundle: bundle})
					requireNativeStatus(t, response, http.StatusOK)
					var version tracker.ChangeVersion
					decodeHubResponse(t, response, &version)
					if version.ID == "" || version.ID == current || version.ChangeID != change.ID || version.Number <= previousNumber || version.HeadSHA != input.HeadSHA {
						t.Fatalf("publication did not create this Change's next version: %+v", version)
					}
					current, previousNumber = version.ID, version.Number
					if test.external != nil {
						number, err := strconv.Atoi(test.external.ID)
						if err != nil {
							t.Fatal(err)
						}
						want = &tracker.NativePullRequest{Number: number, URL: test.external.URL}
					}
					if test.terminal {
						response = f.request(t, owner, http.MethodGet, itemPath, nil)
						requireNativeStatus(t, response, http.StatusOK)
						var latest tracker.NativeIssue
						decodeHubResponse(t, response, &latest)
						response = f.request(t, owner, http.MethodPost, itemPath+"/workflow", tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: entity + "/complete-projection"}, ExpectedRevision: latest.Revision, State: "Done", Reason: "user_requested"})
						requireNativeStatus(t, response, http.StatusOK)
					}
					drainTestNativeBoard(t, f.service)
					for _, compact := range []bool{false, true} {
						var issue tracker.NativeIssue
						if compact {
							if err := repository.ReadTenant(t.Context(), "org_security", corestore.Serving, func(r tenantstore.Reader) error {
								record, err := r.BoardItem(t.Context(), f.project, item.WorkItemID)
								if err != nil {
									return err
								}
								if record.ListIssue == nil {
									return workItemStorageUnavailable()
								}
								issue = *record.ListIssue
								return nil
							}); err != nil {
								t.Fatal(err)
							}
						} else {
							response = f.request(t, owner, http.MethodGet, itemPath+"?include=change", nil)
							requireNativeStatus(t, response, http.StatusOK)
							decodeHubResponse(t, response, &issue)
							if issue.Change == nil || issue.Change.ChangeID != change.ID || issue.Change.HeadSHA != input.HeadSHA {
								t.Fatalf("full projection did not cover current immutable version: %+v", issue.Change)
							}
						}
						if (want == nil && issue.PullRequest != nil) || (want != nil && (issue.PullRequest == nil || *issue.PullRequest != *want)) || issue.Terminal != test.terminal {
							t.Fatalf("compact=%v PR=%#v terminal=%v, want PR=%#v terminal=%v", compact, issue.PullRequest, issue.Terminal, want, test.terminal)
						}
					}
				})
			}
		})
	}
}
