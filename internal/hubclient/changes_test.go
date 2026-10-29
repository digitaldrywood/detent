package hubclient

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestChangeClientIdentityValidation(t *testing.T) {
	t.Parallel()
	for _, test := range []struct{ item, change, version string }{
		{"../item", "change_good", "version_good"},
		{"wi_item", "change_bad/path", "version_good"},
		{"wi_item", "change_good", "version_bad?query"},
		{"wi_item", "wrong", "version_good"},
		{"wi_item", "change_good", "version_"},
	} {
		t.Run(test.item+test.change+test.version, func(t *testing.T) {
			if _, err := changePath(tracker.NativeWorkItemID(test.item), test.change, test.version); err == nil {
				t.Fatal("unsafe identity accepted")
			}
		})
	}
}

func TestChangeClientFencingAndConnectorRead(t *testing.T) {
	if testing.Short() {
		t.Skip("loopback network listener integration")
	}

	t.Parallel()
	lease := tracker.NativeLease{ID: "lease", WorkItemID: "wi_item", FencingToken: 10}
	base := "/api/v2/organizations/org_example/projects/prj_example"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			var input tracker.Mutation
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.LeaseID != lease.ID || input.FencingToken != lease.FencingToken {
				t.Errorf("unfenced native mutation: %#v, %v", input, err)
			}
			json.NewEncoder(w).Encode(struct{}{})
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/changes"):
			json.NewEncoder(w).Encode([]tracker.ChangeRequest{{ID: "change_example", WorkItemID: "wi_item"}})
		case strings.HasSuffix(r.URL.Path, "/changes/change_example"):
			json.NewEncoder(w).Encode(tracker.ChangeDetail{Change: tracker.ChangeRequest{ID: "change_example", CurrentVersion: "version_current"}})
		case strings.HasSuffix(r.URL.Path, "/attempts"):
			page := tracker.Page[tracker.NativeAttempt]{Items: []tracker.NativeAttempt{{NativeRunData: tracker.NativeRunData{AttemptID: "attempt_first"}}}, NextCursor: "next"}
			if r.URL.Query().Get("cursor") == "next" {
				page.Items[0].AttemptID, page.NextCursor = "attempt_second", ""
			}
			json.NewEncoder(w).Encode(page)
		default:
			t.Errorf("unexpected route %s", r.URL)
		}
	}))
	t.Cleanup(server.Close)
	client, err := New(Config{URL: server.URL, TokenSource: func() string { return "token" }})
	if err != nil {
		t.Fatal(err)
	}
	native, err := client.Native("org_example", "prj_example")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(t.Context(), nativeMutationAuthorityKey{}, nativeMutationAuthority{scope: base, lease: lease})
	if _, err := native.CreateChange(ctx, lease.WorkItemID, tracker.CreateChange{Mutation: nativeMutationKey(), Title: "Change"}); err != nil {
		t.Fatal(err)
	}
	if _, err := native.PublishChangeVersion(ctx, lease.WorkItemID, "change_example", tracker.PublishChangeVersion{Mutation: nativeMutationKey()}); err != nil {
		t.Fatal(err)
	}
	if _, err := native.DiscussChange(ctx, lease.WorkItemID, "change_example", tracker.DiscussChange{Mutation: nativeMutationKey(), Body: "Comment"}); err != nil {
		t.Fatal(err)
	}
	source, err := NewNativeConnector(native)
	if err != nil {
		t.Fatal(err)
	}
	changes, err := source.FetchChanges(ctx, lease.WorkItemID)
	if err != nil || len(changes) != 1 || changes[0].ID != "change_example" {
		t.Fatalf("change list = %#v, %v", changes, err)
	}
	detail, err := source.FetchChange(ctx, lease.WorkItemID, "change_example")
	if err != nil || detail.Change.CurrentVersion != "version_current" {
		t.Fatalf("change detail = %#v, %v", detail, err)
	}
	attempts, err := source.FetchNativeAttempts(ctx, lease.WorkItemID)
	if err != nil || len(attempts) != 2 || attempts[1].AttemptID != "attempt_second" {
		t.Fatalf("run navigation = %#v, %v", attempts, err)
	}
	for _, scenario := range []string{"complete current evidence and history", "missing section", "repeated cursor", "head changed", "scope changed", "page failed"} {
		t.Run(scenario, func(t *testing.T) {
			current := tracker.ChangeVersion{ID: "version_current", ChangeID: "change_example", Number: 2, ChangeVersionInput: tracker.ChangeVersionInput{HeadSHA: strings.Repeat("b", 40)}}
			previous := tracker.ChangeVersion{ID: "version_previous", ChangeID: current.ChangeID, Number: 1, ChangeVersionInput: tracker.ChangeVersionInput{HeadSHA: strings.Repeat("a", 40), External: &tracker.ChangeExternalReference{Provider: "github", ID: "7", URL: "https://github.com/acme/repo/pull/7"}, Source: &tracker.ChangeSource{Format: "git-bundle", BundleSHA256: strings.Repeat("c", 64)}}}
			want := tracker.ChangeDetail{
				PublicationPushes: []tracker.ChangePublicationPush{{Change: tracker.NativeChangeReference{ChangeID: previous.ChangeID, VersionID: previous.ID, HeadSHA: previous.HeadSHA}, RecordingAttemptID: "attempt_resume", EffectID: "effect_push"}},
				Change:            tracker.ChangeRequest{ID: current.ChangeID, OrganizationID: "org_example", ProjectID: "prj_example", WorkItemID: lease.WorkItemID, CurrentVersion: current.ID},
				Versions:          []tracker.ChangeVersion{previous, current},
				Reviews:           []tracker.ChangeReview{{ID: "review_current", VersionID: current.ID, Decision: "changes_requested", Validator: &gate.ValidatorResult{HeadSHA: current.HeadSHA, Verdict: "rework", NotVerified: []string{"journey"}, CriteriaEvidence: []gate.CriterionEvidence{{Criterion: "journey", Kind: gate.EvidenceNotVerified, ReceiptIDs: []string{"receipt_failed"}}}, Commands: []gate.CommandResult{{ReceiptID: "receipt_failed", HeadSHA: current.HeadSHA, ExitCode: 1, Output: "excerpt", OutputTruncated: true}}}}},
				Checks:            []tracker.ChangeCheck{{VersionID: current.ID, ChangeCheckResult: tracker.ChangeCheckResult{CheckRunID: "check_current", Conclusion: "failure"}}},
				Discussion:        []tracker.ChangeDiscussion{{ID: "discussion_current", VersionID: current.ID, Body: "preserve feedback"}},
				Summary:           tracker.ChangeSummary{Status: "changes_requested"},
				SourceIssues:      []tracker.ExternalReference{{Provider: "native", ID: "source_issue"}},
			}
			readClient, err := New(Config{URL: "http://native-hub.test", TokenSource: func() string { return "token" }, HTTPClient: providerHandlerClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("view") != "bounded" {
					t.Error("runtime requested aggregate canonical Change")
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				section := r.URL.Query().Get("section")
				value := operatortool.ChangeResult{OrganizationID: "org_example", ProjectID: "prj_example", WorkItemID: string(lease.WorkItemID), Detail: &tracker.ChangeDetail{Change: want.Change, Versions: []tracker.ChangeVersion{current}, Summary: want.Summary, SourceIssues: want.SourceIssues}, Read: &operatortool.ChangeRead{VersionID: current.ID, Sections: map[string]operatortool.ChangeReadPage{}}}
				for _, name := range []string{"versions", "reviews", "checks", "discussion"} {
					value.Read.Sections[name] = operatortool.ChangeReadPage{NextCursor: name}
				}
				if section != "" {
					if r.URL.Query().Get("cursor") != section || r.URL.Query().Get("version_id") != current.ID {
						t.Error("runtime lost exact cursor or current version")
					}
					value.Read.Sections[section] = operatortool.ChangeReadPage{Complete: true}
					switch section {
					case "versions":
						value.Detail.Versions = []tracker.ChangeVersion{previous}
						value.Detail.PublicationPushes = want.PublicationPushes
					case "reviews":
						value.Detail.Reviews = want.Reviews
					case "checks":
						value.Detail.Checks = want.Checks
					case "discussion":
						value.Detail.Discussion = want.Discussion
					}
					if section == "discussion" {
						switch scenario {
						case "missing section":
							delete(value.Read.Sections, section)
						case "repeated cursor":
							value.Read.Sections[section] = operatortool.ChangeReadPage{NextCursor: section}
						case "head changed":
							value.Detail.Change.CurrentVersion, value.Read.VersionID = "version_new", "version_new"
						case "scope changed":
							value.ProjectID = "prj_foreign"
						case "page failed":
							w.WriteHeader(http.StatusConflict)
							return
						}
					}
				}
				json.NewEncoder(w).Encode(value)
			}))})
			if err != nil {
				t.Fatal(err)
			}
			reader, err := readClient.Native("org_example", "prj_example")
			if err != nil {
				t.Fatal(err)
			}
			got, err := reader.runtimeChange(t.Context(), lease.WorkItemID, current.ChangeID)
			if scenario != "complete current evidence and history" {
				if err == nil || !reflect.DeepEqual(got, tracker.ChangeDetail{}) {
					t.Fatalf("partial runtime evidence escaped: %v", err)
				}
				if scenario == "page failed" {
					var refusal *APIError
					if !errors.As(err, &refusal) || refusal.Status != http.StatusConflict {
						t.Fatalf("lost paging conflict: %v", err)
					}
				}
				return
			}
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("runtime evidence lost canonical semantics: %#v, %v", got, err)
			}
		})
	}
}

func TestNativeRecoveryUsesPublishedVersion(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name        string
		noChange    bool
		draft       bool
		unavailable bool
		unscoped    bool
	}{
		{name: "current detail supersedes stale history and checkpoint"},
		{name: "no Change", noChange: true},
		{name: "draft Change", draft: true},
		{name: "current recovery unavailable", unavailable: true},
		{name: "missing scoped recovery on older Hub", unscoped: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			previous := &tracker.NativeChangeReference{ChangeID: "change_example", VersionID: "version_old", HeadSHA: strings.Repeat("a", 40)}
			current := &tracker.NativeChangeReference{ChangeID: "change_example", VersionID: "version_current", HeadSHA: strings.Repeat("b", 40)}
			detail := tracker.ChangeDetail{
				Change: tracker.ChangeRequest{ID: current.ChangeID, WorkItemID: "wi_item", CurrentVersion: current.VersionID},
				Versions: []tracker.ChangeVersion{
					{ID: current.VersionID, ChangeID: current.ChangeID, ChangeVersionInput: tracker.ChangeVersionInput{HeadSHA: current.HeadSHA}},
				},
				Discussion: []tracker.ChangeDiscussion{
					{ID: "cmt_current", VersionID: current.VersionID, Body: "Global config loses concurrent updates", Actor: tracker.Actor{Kind: "human", PrincipalID: "operator"}, Provenance: &tracker.Provenance{Provider: "native", ExternalID: "original"}},
					{ID: "cmt_general", Body: "General discussion"},
				},
				Reviews: []tracker.ChangeReview{
					{ID: "review_current", VersionID: current.VersionID, Decision: "changes_requested", Body: "Serialize config updates", Actor: tracker.Actor{Kind: "human", PrincipalID: "reviewer"}},
				},
			}
			if test.draft {
				detail.Change.CurrentVersion, detail.Versions = "", nil
			}
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if !strings.HasSuffix(r.URL.Path, "/work-items/wi_item") || r.URL.Query().Get("view") != "recovery" {
					t.Errorf("recovery fetched historical resource: %s", r.URL)
					http.Error(w, "unexpected historical read", http.StatusBadRequest)
					return
				}
				if test.unavailable {
					http.Error(w, "current recovery unavailable", http.StatusForbidden)
					return
				}
				result := tracker.NativeRecovery{
					Issue:      tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: "org_example", ProjectID: "prj_example", WorkItemID: "wi_item"}},
					Discussion: []tracker.NativeComment{{Body: "Issue discussion"}},
					Attempts:   []tracker.NativeAttempt{{Checkpoint: &tracker.NativeCheckpoint{Change: previous}}},
				}
				if !test.noChange {
					result.ChangeDetail = &detail
					if !test.draft {
						result.Change = current
					}
				}
				if test.unscoped {
					json.NewEncoder(w).Encode(result.Issue)
					return
				}
				json.NewEncoder(w).Encode(result)
			})
			client, err := New(Config{URL: "http://native-hub.test", TokenSource: func() string { return "token" }, HTTPClient: providerHandlerClient(handler)})
			if err != nil {
				t.Fatal(err)
			}
			native, err := client.Native("org_example", "prj_example")
			if err != nil {
				t.Fatal(err)
			}
			recovery, err := native.Recovery(t.Context(), "wi_item")
			if test.unavailable || test.unscoped {
				if err == nil {
					t.Fatal("missing scoped Change evidence allowed recovery")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if test.noChange {
				if recovery.Change != nil || recovery.ChangeDetail != nil {
					t.Fatal("historical reference became current Change")
				}
			} else {
				if recovery.ChangeDetail == nil || !reflect.DeepEqual(*recovery.ChangeDetail, detail) {
					t.Fatalf("recovery omitted scoped Change detail: %+v", recovery.ChangeDetail)
				}
				if test.draft {
					if recovery.Change != nil {
						t.Fatal("draft reused historical version")
					}
				} else if recovery.Change == nil || *recovery.Change != *current {
					t.Fatalf("recovered current change = %#v", recovery.Change)
				}
			}
			if len(recovery.Discussion) != 1 || len(recovery.History) != 0 || len(recovery.Attempts) != 1 {
				t.Fatal("existing issue recovery context lost")
			}
		})
	}
}

func TestNativeRetainedChangeReviewBoundaries(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"complete", "denied", "unsupported refusal", "unsupported section", "corrupt bytes", "changed digest", "changed record", "changed version", "foreign item", "foreign project", "repeated offset", "repeated cursor", "early end", "empty fragment", "invalid JSON", "wrong review version", "head changed", "missing fragment", "duplicate review", "oversized fragment"} {
		t.Run(mode, func(t *testing.T) {
			version := "version_current"
			want := tracker.ChangeReview{ID: "review_large", VersionID: version, Decision: "changes_requested", Body: strings.Repeat("<\"\n界", 8000)}
			encoded := want
			if mode == "wrong review version" {
				encoded.VersionID = "version_other"
			}
			raw, err := json.Marshal(encoded)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "invalid JSON" {
				raw[0] = '!'
			}
			digest := sha256.Sum256(raw)
			sha := hex.EncodeToString(digest[:])
			calls := 0
			client, err := New(Config{URL: "http://native-hub.test", TokenSource: func() string { return "token" }, HTTPClient: providerHandlerClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if calls > 16 || r.URL.Query().Get("view") != "bounded" || !strings.HasSuffix(r.URL.Path, "/changes/change_example") {
					t.Error("recovery escaped bounded Change traversal")
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				change := tracker.ChangeRequest{ID: "change_example", OrganizationID: "org_example", ProjectID: "prj_example", WorkItemID: "wi_item", CurrentVersion: version}
				response := operatortool.ChangeResult{OrganizationID: "org_example", ProjectID: "prj_example", WorkItemID: "wi_item", Detail: &tracker.ChangeDetail{Change: change}, Read: &operatortool.ChangeRead{VersionID: version, Sections: map[string]operatortool.ChangeReadPage{"versions": {Complete: true}, "reviews": {NextCursor: "0"}, "checks": {Complete: true}, "discussion": {Complete: true}}}}
				if r.URL.Query().Get("section") == "reviews" {
					if mode == "denied" || mode == "unsupported refusal" {
						status := http.StatusForbidden
						if mode == "unsupported refusal" {
							status = http.StatusUnprocessableEntity
						}
						w.WriteHeader(status)
						json.NewEncoder(w).Encode(map[string]string{"code": "unsupported_record"})
						return
					}
					start, err := strconv.Atoi(r.URL.Query().Get("cursor"))
					if err != nil || start < 0 || start >= len(raw) {
						t.Error("invalid fragment cursor")
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					end := min(start+(64<<10), len(raw))
					page := &tracker.NativeRecoveryPage{NativeReference: tracker.NativeReference{OrganizationID: "org_example", ProjectID: "prj_example", WorkItemID: "wi_item"}, Section: "reviews", RecordID: "17", VersionID: version, Offset: int64(start), TotalBytes: int64(len(raw)), SHA256: sha, Data: append([]byte(nil), raw[start:end]...)}
					response.Fragment = page
					response.Read.Sections["reviews"] = operatortool.ChangeReadPage{Complete: end == len(raw), NextCursor: strconv.Itoa(end)}
					if start > 0 {
						switch mode {
						case "changed digest":
							page.SHA256 = strings.Repeat("0", 64)
						case "changed record":
							page.RecordID = "18"
						case "changed version":
							page.VersionID = "version_other"
						case "repeated offset":
							page.Offset = 0
						case "missing fragment":
							response.Fragment = nil
						case "head changed":
							response.Detail.Change.CurrentVersion = "version_new"
							response.Read.VersionID = "version_new"
						}
					}
					switch mode {
					case "unsupported section":
						page.Section = "unsupported"
					case "corrupt bytes":
						if start == 0 {
							page.Data[0] ^= 1
						}
					case "foreign item":
						page.WorkItemID = "wi_other"
					case "foreign project":
						page.ProjectID = "prj_other"
					case "empty fragment":
						page.Data = nil
					case "oversized fragment":
						page.Data = append(page.Data, make([]byte, 64<<10)...)
					case "repeated cursor":
						response.Read.Sections["reviews"] = operatortool.ChangeReadPage{NextCursor: "0"}
					case "early end":
						response.Read.Sections["reviews"] = operatortool.ChangeReadPage{Complete: true}
					}
				} else if mode == "duplicate review" {
					response.Detail.Reviews = []tracker.ChangeReview{want}
				}
				body, err := json.Marshal(response)
				if err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				if len(body) > operatortool.MaxResultBytes {
					t.Errorf("transport exceeded byte cap: %d", len(body))
				}
				w.Header().Set("Content-Type", "application/json")
				w.Write(body)
			}))})
			if err != nil {
				t.Fatal(err)
			}
			native, err := client.Native("org_example", "prj_example")
			if err != nil {
				t.Fatal(err)
			}
			detail, err := native.runtimeChange(t.Context(), "wi_item", "change_example")
			if mode != "complete" {
				if err == nil || len(detail.Reviews) != 0 {
					t.Fatalf("invalid retained evidence became success: %v", err)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(detail.Reviews, []tracker.ChangeReview{want}) {
				t.Fatalf("complete fragments lost review: %v", err)
			}
		})
	}
}
