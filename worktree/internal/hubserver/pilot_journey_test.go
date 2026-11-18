package hubserver

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func pilotHostedJourney(t *testing.T) (*browserHostedFixture, string, string) {
	t.Helper()
	f := seedHostedOnboardingJourney(t, newBrowserHostedOrganizationFixture(t, true, artifact.NewID("org")))
	runner := pilotHostedRunner(t, f, 2)
	base := "/api/v2/organizations/" + f.service.config.Hosted.OrganizationID + "/projects/" + f.project
	if err := f.service.maintainBoardRecords(t.Context()); err != nil {
		t.Fatalf("refresh board projection: %v", err)
	}
	response := f.page(t, "owner", base+"/work-items")
	requireNativeStatus(t, response, http.StatusOK)
	var issues tracker.Page[tracker.NativeIssue]
	decodeHubResponse(t, response, &issues)
	var issue tracker.NativeIssue
	for _, item := range issues.Items {
		if item.Title == "First native run" {
			issue = item
		}
	}
	if issue.WorkItemID == "" {
		t.Fatal("completed onboarding run is missing")
	}
	path := base + "/work-items/" + string(issue.WorkItemID)
	requireNativeStatus(t, f.setupRequest(t, "owner", http.MethodPost, path+"/comments", tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "pilot-discussion"}, Body: "Pilot discussion remains available with runners offline."}), http.StatusOK)
	response = f.setupRequest(t, "owner", http.MethodPost, path+"/changes", tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "pilot-change"}, Title: "Pilot native Change Request", Body: "Synthetic pilot work; no customer repository was cloned."})
	requireNativeStatus(t, response, http.StatusOK)
	var change tracker.ChangeRequest
	decodeHubResponse(t, response, &change)
	pilotUploadArtifact(t, f, issue, runner)
	return f, path, path + "/changes/" + change.ID
}

func pilotExpireRunners(t *testing.T, f *browserHostedFixture) {
	t.Helper()
	admin := hostedPostgresAdmin(t, f.service)
	expired := time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC)
	for _, query := range []string{"UPDATE runner_identities SET last_heartbeat_at=$1 WHERE organization_id=$2", "UPDATE machines SET last_heartbeat_at=$1 WHERE organization_id=$2"} {
		result, err := admin.ExecContext(t.Context(), query, expired, f.service.config.Hosted.OrganizationID)
		if err != nil {
			t.Fatal(err)
		}
		if count, err := result.RowsAffected(); err != nil || count == 0 {
			t.Fatalf("runner expiry fixture affected=%d error=%v", count, err)
		}
	}
}

func TestPilotHostedHistoryAfterRunnerLossAndRestart(t *testing.T) {
	t.Parallel()
	f, issue, change := pilotHostedJourney(t)
	pilotExpireRunners(t, f)
	for _, restart := range []bool{false, true} {
		if restart {
			config := f.service.config
			if err := f.service.Close(); err != nil {
				t.Fatal(err)
			}
			service, err := Open(t.Context(), config)
			if err != nil {
				t.Fatal(err)
			}
			f.service = service
			t.Cleanup(func() {
				if err := service.Close(); err != nil {
					t.Error(err)
				}
			})
		}
		for _, test := range []struct {
			path     string
			contains string
		}{
			{issue, "First native run"},
			{issue + "/comments", "Pilot discussion remains available"},
			{issue + "/history", "run.finished"},
			{change, "Pilot native Change Request"},
		} {
			response := f.page(t, "owner", test.path)
			requireNativeStatus(t, response, http.StatusOK)
			if !strings.Contains(response.Body.String(), test.contains) {
				t.Fatalf("restart=%t history %s missing %q", restart, test.path, test.contains)
			}
			requireNativeStatus(t, f.page(t, "wrong-organization", test.path), http.StatusForbidden)
			requireNativeStatus(t, f.page(t, "revoked", test.path), http.StatusUnauthorized)
		}
		var detail tracker.ChangeDetail
		decodeHubResponse(t, f.page(t, "owner", change), &detail)
		if detail.Summary.Status != "draft" || len(detail.Versions) != 0 {
			t.Fatalf("unpublished change is falsely ready: %+v", detail.Summary)
		}
	}
}
