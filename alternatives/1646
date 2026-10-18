package hubserver

import (
	"testing"

	"github.com/digitaldrywood/detent/internal/issueorigin"
)

func TestReportsTrackingIssues(t *testing.T) {
	failures := []nativeFailure{
		{workItem: "wi_a", example: "hub request timed out"},
		{workItem: "wi_b", example: "landing gate failed"},
		{workItem: "wi_c", example: "something nobody filed"},
	}
	for _, tt := range []struct {
		name     string
		body     func() string
		archived bool
		linked   bool
	}{
		{"signature digest", func() string {
			return issueorigin.Stamp("Hub timeouts", issueorigin.Origin{Kind: "worker", Fingerprint: issueorigin.Fingerprint(normalizeFailureSignature("hub request timed out"))})
		}, false, true},
		{"class name", func() string {
			return issueorigin.Stamp("Hub timeouts", issueorigin.Origin{Kind: "audit", Fingerprint: "Hub request timed out"})
		}, false, true},
		{"other fingerprint", func() string {
			return issueorigin.Stamp("Unrelated", issueorigin.Origin{Kind: "worker", Fingerprint: "unrelated"})
		}, false, false},
		{"unstamped issue", func() string { return "Hub request timed out again" }, false, false},
		{"archived issue", func() string {
			return issueorigin.Stamp("Hub timeouts", issueorigin.Origin{Kind: "audit", Fingerprint: "Hub request timed out"})
		}, true, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newNativeFixture(t, nil, "", "tracking")
			issue := f.create(t, "Tracking")
			if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET body = ?, archived = ? WHERE native_id = ?", tt.body(), tt.archived, issue.WorkItemID); err != nil {
				t.Fatal(err)
			}
			classes := reportFailureClasses(failures)
			if err := f.service.reportsTrackingIssues(t.Context(), nativeScope{organization: f.project.OrganizationID, project: f.project.ID}, classes, failures); err != nil {
				t.Fatal(err)
			}
			for _, class := range classes {
				want := ""
				if tt.linked && class.Class == "Hub request timed out" {
					want = string(issue.WorkItemID)
				}
				if class.TrackingWorkItemID != want {
					t.Fatalf("%s tracking=%q want %q", class.Class, class.TrackingWorkItemID, want)
				}
			}
		})
	}
}
