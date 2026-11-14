package searchstore

import (
	"regexp"
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
			source := DuplicateSource{Document: Document{OrganizationID: "org_example", ProjectID: test.project, WorkItemID: "wi_example", Kind: "comment"}}
			match, found := MatchDuplicateSource(source, Text{Body: "Another occurrence of the reported failure"}, report(test.previous), report(test.incoming))
			if match.Fingerprint != test.want || test.want && !found {
				t.Fatalf("comment fingerprint=%t found=%t, want fingerprint=%t", match.Fingerprint, found, test.want)
			}
		})
	}
}

func TestDuplicateFingerprintTermsAreExactVectorLexemes(t *testing.T) {
	quoted := regexp.MustCompile(`'([^']*)'`)
	lexemes := func(value string) []string {
		var terms []string
		for _, match := range quoted.FindAllStringSubmatch(value, -1) {
			terms = append(terms, match[1])
		}
		return terms
	}
	stamped := func(body, fingerprint string) string {
		return issueorigin.Stamp(body, issueorigin.Origin{Kind: "worker", Fingerprint: fingerprint})
	}
	for _, test := range []struct {
		name            string
		kind            string
		document        Text
		project         tracker.ProjectID
		request         string
		requestProject  tracker.ProjectID
		want            bool
		wantFingerprint int
	}{
		{"same diagnostic under different titles and stamps", "issue", Text{Title: "Original failure", Body: stamped("Initial diagnostic", "original-failure") + "\n internal/example/pool.go:23:4: undefined: capacity"}, "prj_example", stamped("internal/example/pool.go:31:2: undefined: capacity", "later-failure"), "prj_example", true, 2},
		{"same stamp is one unsplit unstemmed lexeme", "issue", Text{Title: "First title", Body: stamped("Body", "flaky-retried-failed")}, "prj_example", stamped("Other body", "flaky-retried-failed"), "prj_example", true, 1},
		{"similar words with distinct fingerprints", "issue", Text{Title: "Undefined capacity", Body: stamped("internal/example/pool.go:23:4: undefined: capacity", "first-failure")}, "prj_example", stamped("internal/example/pool.go:23:4: undefined: capacities", "second-failure"), "prj_example", false, 2},
		{"unstamped issue carries no fingerprint", "issue", Text{Title: "Plain", Body: "internal/example/pool.go:23:4: undefined: capacity"}, "prj_example", stamped("internal/example/pool.go:23:4: undefined: capacity", "later-failure"), "prj_example", false, 0},
		{"comment diagnostic carries its defect", "comment", Text{Body: "internal/example/pool.go:23:4: undefined: capacity"}, "prj_example", stamped("internal/example/pool.go:31:2: undefined: capacity", "later-failure"), "prj_example", true, 1},
		{"other project never matches", "issue", Text{Title: "Original failure", Body: stamped("Body", "shared-failure")}, "prj_example", stamped("Body", "shared-failure"), "prj_other", false, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			document := Document{OrganizationID: "org_example", ProjectID: test.project, WorkItemID: "wi_example", Kind: test.kind}
			indexed := lexemes(Vector(document, test.document))
			prefix := Prefix("org_example", test.project) + "f"
			fingerprints := 0
			for _, term := range indexed {
				if strings.HasPrefix(term, prefix) {
					fingerprints++
					if len(term) != len(prefix)+64 {
						t.Fatalf("fingerprint lexeme %q is not one exact digest", term)
					}
				}
			}
			if fingerprints != test.wantFingerprint {
				t.Fatalf("fingerprint lexemes=%d want %d in %v", fingerprints, test.wantFingerprint, indexed)
			}
			query := lexemes(DuplicateFingerprintExpression("org_example", test.requestProject, test.request))
			if len(query) == 0 || strings.Contains(DuplicateFingerprintExpression("org_example", test.requestProject, test.request), ":*") {
				t.Fatalf("fingerprint expression=%q", DuplicateFingerprintExpression("org_example", test.requestProject, test.request))
			}
			matched := slices.ContainsFunc(query, func(term string) bool { return slices.Contains(indexed, term) })
			if matched != test.want {
				t.Fatalf("fingerprint match=%t want %t: query=%v", matched, test.want, query)
			}
		})
	}
}
