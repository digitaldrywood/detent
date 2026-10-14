package hubserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRecoveryRetainsCurrentAutomaticAndOrdinaryReviews(t *testing.T) {
	f := newChangeFixture(t, nil)
	version := f.publish(t, "version", "")
	stored := make([]tracker.ChangeReview, 0, 9)
	insert := func(review tracker.ChangeReview) {
		t.Helper()
		review.VersionID = version.ID
		raw, err := json.Marshal(review)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO change_evidence (change_id,version_id,kind,record_json) VALUES (?,?,'review',?)", f.change.ID, version.ID, raw); err != nil {
			t.Fatal(err)
		}
		stored = append(stored, review)
	}
	for i := range 7 {
		review := tracker.ChangeReview{ID: fmt.Sprintf("automatic_%d", i), Decision: "changes_requested", Body: strings.Repeat("x", 180<<10), Actor: tracker.Actor{Kind: "runner", PrincipalID: "runner_test"}}
		if i%2 == 0 {
			review.ImplementationReview = &gate.ValidatorResult{Verdict: "rework"}
		} else {
			review.Validator = &gate.ValidatorResult{Verdict: "wait"}
		}
		insert(review)
		if i == 1 || i == 4 {
			kind := "human"
			if i == 4 {
				kind = "codex"
			}
			insert(tracker.ChangeReview{ID: "ordinary_" + kind, Decision: "changes_requested", Body: "Retain this review", Actor: tracker.Actor{Kind: kind}})
		}
	}
	full := f.detail(t)
	raw, err := json.Marshal(full)
	if err != nil || len(raw) <= 1<<20 {
		t.Fatalf("fixture bytes=%d error=%v", len(raw), err)
	}
	response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(f.issue.WorkItemID)+"?view=recovery", f.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	if response.Body.Len() > 1<<20 {
		t.Fatalf("recovery exceeds client bound: %d bytes", response.Body.Len())
	}
	var recovery tracker.NativeRecovery
	decodeHubResponse(t, response, &recovery)
	server := httptest.NewServer(f.service.Handler())
	defer server.Close()
	client, err := hubclient.New(hubclient.Config{URL: server.URL, TokenSource: func() string { return f.token }, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	native, err := client.Native(f.project.OrganizationID, f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	recovery, err = native.Recovery(t.Context(), f.issue.WorkItemID)
	if err != nil {
		t.Fatalf("real bounded client cannot recover source: %v", err)
	}
	want := []tracker.ChangeReview{stored[2], stored[6], stored[7], stored[8]}
	if recovery.ChangeDetail == nil || !reflect.DeepEqual(recovery.ChangeDetail.Reviews, want) {
		t.Fatal("recovery lost current authority, ordinary reviews, or original review order")
	}
	if !reflect.DeepEqual(recovery.ChangeDetail.Versions, full.Versions) || !reflect.DeepEqual(recovery.ChangeDetail.Summary, full.Summary) {
		t.Fatal("recovery changed source identity or review summary")
	}
	if !reflect.DeepEqual(f.detail(t).Reviews, stored) {
		t.Fatal("durable review history changed")
	}
	t.Logf("durable_bytes=%d recovery_bytes=%d durable_reviews=%d recovery_reviews=%d", len(raw), response.Body.Len(), len(stored), len(want))
	insert(tracker.ChangeReview{ID: "authoritative_human", Decision: "changes_requested", Body: strings.Repeat("x", 1<<20), Actor: tracker.Actor{Kind: "human"}})
	if _, err := native.Recovery(t.Context(), f.issue.WorkItemID); err == nil || !strings.Contains(err.Error(), "response exceeds 1048576 bytes") {
		t.Fatalf("individually oversized authority must remain explicit: %v", err)
	}
	if !reflect.DeepEqual(f.detail(t).Reviews, stored) {
		t.Fatal("oversized authority was deleted")
	}
}
