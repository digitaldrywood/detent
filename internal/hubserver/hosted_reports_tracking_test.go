package hubserver

import (
	"testing"

	"github.com/digitaldrywood/detent/internal/hubanalytics"
	"github.com/digitaldrywood/detent/internal/hubhealth"
	"github.com/digitaldrywood/detent/internal/issueorigin"
)

func TestReportsTrackingIssues(t *testing.T) {
	failures := []hubanalytics.Failure{
		{WorkItem: "wi_a", Example: "hub request timed out"},
		{WorkItem: "wi_b", Example: "landing gate failed"},
		{WorkItem: "wi_c", Example: "something nobody filed"},
	}
	for _, tt := range []struct {
		name     string
		body     func() string
		archived bool
		linked   bool
	}{
		{"signature digest", func() string {
			return issueorigin.Stamp("Hub timeouts", issueorigin.Origin{Kind: "worker", Fingerprint: issueorigin.Fingerprint(hubhealth.NormalizeFailureSignature("hub request timed out"))})
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
