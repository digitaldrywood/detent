package hubclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

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
