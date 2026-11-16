package searchstore

import (
	"slices"
	"strings"
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

func TestFingerprintRowsCarryStampedIdentity(t *testing.T) {
	problem := "Problem: `go-test:example/pkg:TestOne`"
	defect := FingerprintDigest(issueorigin.DefectFingerprint(problem))
	release := issueorigin.Stamp("Release stabilization: `release/v1.2`", issueorigin.Origin{Kind: "doctor", Instance: "github-actions", Source: "https://github.com/digitaldrywood/detent/actions/runs/1", Fingerprint: issueorigin.ReleaseStabilizationFingerprint("release/v1.2")})
	for _, test := range []struct {
		name    string
		project tracker.ProjectID
		body    string
		want    []FingerprintRow
	}{
		{"unstamped", "prj_example", problem, nil},
		{"origin only", "prj_example", issueorigin.Stamp("Report", issueorigin.Origin{Kind: "doctor", Fingerprint: "example-key"}), []FingerprintRow{{Digest: FingerprintDigest("example-key"), Origin: true, OriginKind: "doctor"}}},
		{"origin and defect key", "prj_example", issueorigin.Stamp(problem, issueorigin.Origin{Kind: "worker", Fingerprint: "example-key"}), sortedRows(FingerprintRow{Digest: FingerprintDigest("example-key"), Origin: true, OriginKind: "worker"}, FingerprintRow{Digest: defect, OriginKind: "worker"})},
		{"origin equal to its defect key", "prj_example", issueorigin.Stamp(problem, issueorigin.Origin{Kind: "worker", Fingerprint: issueorigin.DefectFingerprint(problem)}), []FingerprintRow{{Digest: defect, Origin: true, OriginKind: "worker"}}},
		{"release series in the cloud project", issueorigin.DetentCloudProjectID, release, []FingerprintRow{{Digest: FingerprintDigest(issueorigin.ReleaseStabilizationFingerprint("release/v1.2")), Origin: true, OriginKind: "doctor", ReleaseSeries: "release/v1.2"}}},
		{"release series ignored elsewhere", "prj_example", release, []FingerprintRow{{Digest: FingerprintDigest(issueorigin.ReleaseStabilizationFingerprint("release/v1.2")), Origin: true, OriginKind: "doctor"}}},
		{"long fingerprint", "prj_example", issueorigin.Stamp("Report", issueorigin.Origin{Kind: "doctor", Fingerprint: strings.Repeat("k", 4096)}), []FingerprintRow{{Digest: FingerprintDigest(strings.Repeat("k", 4096)), Origin: true, OriginKind: "doctor"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := FingerprintRows(test.project, test.body); !slices.Equal(got, test.want) {
				t.Fatalf("rows=%+v want %+v", got, test.want)
			}
		})
	}
}

func sortedRows(rows ...FingerprintRow) []FingerprintRow {
	slices.SortFunc(rows, func(a, b FingerprintRow) int { return strings.Compare(a.Digest, b.Digest) })
	return rows
}
