package hubserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

func TestPullRequestMergeableJSON(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		state string
		want  string
	}{
		{name: "clean", state: "clean", want: "true"},
		{name: "blocked is still mergeable as far as the projection knows", state: "blocked", want: "true"},
		{name: "dirty", state: "dirty", want: "false"},
		{name: "empty", state: "", want: `"unknown"`},
		{name: "unknown", state: "UNKNOWN", want: `"unknown"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			encoded, err := json.Marshal(mergeableFromState(test.state))
			if err != nil {
				t.Fatal(err)
			}
			if string(encoded) != test.want {
				t.Fatalf("mergeable = %s, want %s", encoded, test.want)
			}
			var back pullRequestMergeable
			if err := json.Unmarshal(encoded, &back); err != nil {
				t.Fatalf("decode %s: %v", encoded, err)
			}
			if back != mergeableFromState(test.state) {
				t.Fatalf("round trip = %#v", back)
			}
			if err := json.Unmarshal([]byte(`"maybe"`), &back); err == nil {
				t.Fatal("an unknown string must be refused")
			}
		})
	}
}

func TestExternalPullRequestNumber(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		reference tracker.ChangeExternalReference
		want      int
	}{
		{name: "github number", reference: tracker.ChangeExternalReference{Provider: "github", ID: "42"}, want: 42},
		{name: "padded provider and id", reference: tracker.ChangeExternalReference{Provider: " GitHub ", ID: " 7 "}, want: 7},
		{name: "node id", reference: tracker.ChangeExternalReference{Provider: "github", ID: "PR_kwDO"}},
		{name: "zero", reference: tracker.ChangeExternalReference{Provider: "github", ID: "0"}},
		{name: "negative", reference: tracker.ChangeExternalReference{Provider: "github", ID: "-3"}},
		{name: "other provider", reference: tracker.ChangeExternalReference{Provider: "gitlab", ID: "42"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := externalPullRequestNumber(test.reference); got != test.want {
				t.Fatalf("externalPullRequestNumber(%#v) = %d, want %d", test.reference, got, test.want)
			}
		})
	}
}

// The assembled view is served from memory for sixty seconds, so a client that
// polls the panel does not re-run the join on every tick.
func TestPullRequestViewCache(t *testing.T) {
	cache := newPullRequestCache()
	now := time.Now()
	views := []pullRequestView{{ID: "change", State: "open"}}
	cache.set("issue", views, now)
	views[0].State = "closed"
	cached, ok := cache.get("issue", now)
	if !ok || len(cached) != 1 || cached[0].State != "open" {
		t.Fatalf("cache changed: %+v", cached)
	}
	if _, ok := cache.get("issue", now.Add(pullRequestViewTTL)); ok {
		t.Fatal("expired view retained")
	}
}

func TestPullRequestViewGoldenShape(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("testdata", "pull-requests.json"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	var views []pullRequestView
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&views); err != nil {
		t.Fatalf("the golden must decode into the served shape: %v", err)
	}
	if len(views) != 2 {
		t.Fatalf("golden views = %d, want 2", len(views))
	}
	if views[0].Connector == nil || views[0].Number != 4211 || !views[0].Mergeable.Known || !views[0].Mergeable.Value {
		t.Fatalf("connected row = %#v", views[0])
	}
	if views[1].Connector != nil || views[1].Mergeable.Known {
		t.Fatalf("unavailable row = %#v", views[1])
	}
	encoded, err := json.Marshal(views)
	if err != nil {
		t.Fatal(err)
	}
	var want, got any
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	comparePullRequestShape(t, "", want, got)
}

// comparePullRequestShape asserts that the Go payload carries exactly the
// golden's key set at every level and agrees on whether a value is null.
// Values themselves are not compared: the golden is an example, the shape is
// the contract.
func comparePullRequestShape(t *testing.T, path string, want, got any) {
	t.Helper()
	label := path
	if label == "" {
		label = "(root)"
	}
	if (want == nil) != (got == nil) {
		t.Errorf("%s: golden null = %t, Go null = %t", label, want == nil, got == nil)
		return
	}
	switch wanted := want.(type) {
	case nil:
	case map[string]any:
		gotten, ok := got.(map[string]any)
		if !ok {
			t.Errorf("%s: golden is an object, Go value is %T", label, got)
			return
		}
		for key, value := range wanted {
			other, present := gotten[key]
			if !present {
				t.Errorf("%s: Go value is missing key %q", label, key)
				continue
			}
			comparePullRequestShape(t, strings.TrimPrefix(path+"."+key, "."), value, other)
		}
		for key := range gotten {
			if _, present := wanted[key]; !present {
				t.Errorf("%s: Go value has extra key %q", label, key)
			}
		}
	case []any:
		gotten, ok := got.([]any)
		if !ok {
			t.Errorf("%s: golden is an array, Go value is %T", label, got)
			return
		}
		if len(wanted) != len(gotten) {
			t.Errorf("%s: golden has %d elements, Go value has %d", label, len(wanted), len(gotten))
			return
		}
		for i := range wanted {
			comparePullRequestShape(t, path+"[]", wanted[i], gotten[i])
		}
	default:
		if fmt.Sprintf("%T", want) != fmt.Sprintf("%T", got) {
			t.Errorf("%s: golden is %T, Go value is %T", label, want, got)
		}
	}
}
