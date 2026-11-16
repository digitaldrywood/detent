package searchstore

import (
	"testing"

	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestDuplicateCommentsPreserveReleaseIdentity(t *testing.T) {
	report := func(series string) Text {
		body := "Problem: `go-test:example/pkg:TestOne`\n"
		origin := issueorigin.Origin{Kind: "worker", Instance: "runner", Source: "example-run", Fingerprint: issueorigin.Fingerprint("go-test:example/pkg:TestOne")}
		if series != "" {
			body += "Release stabilization: `" + series + "`\n"
			origin = issueorigin.Origin{Kind: "doctor", Instance: "github-actions", Source: "https://github.com/digitaldrywood/detent/actions/runs/1/attempts/1", Fingerprint: issueorigin.ReleaseStabilizationFingerprint(series)}
		}
		return Text{Body: issueorigin.Stamp(body, origin)}
	}
	for _, test := range []struct {
		name     string
		project  tracker.ProjectID
		previous string
		incoming string
		want     bool
	}{
		{"release cannot reuse ordinary repair comment", issueorigin.DetentCloudProjectID, "", "release/v1.2", false},
		{"ordinary repair cannot reuse release comment", issueorigin.DetentCloudProjectID, "release/v1.2", "", false},
		{"same release occurrence", issueorigin.DetentCloudProjectID, "release/v1.2", "release/v1.2", true},
		{"different release occurrence", issueorigin.DetentCloudProjectID, "release/v1.2", "release/v1.3", false},
		{"other project retains defect matching", "prj_example", "", "release/v1.2", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := DuplicateSource{Document: Document{OrganizationID: "org_example", ProjectID: test.project, WorkItemID: "wi_example", Kind: "issue"}, Text: report(test.previous)}
			match, found := MatchDuplicateSource(source, report(test.incoming))
			if match.Fingerprint != test.want || test.want && !found {
				t.Fatalf("comment fingerprint=%t found=%t, want fingerprint=%t", match.Fingerprint, found, test.want)
			}
		})
	}
}
