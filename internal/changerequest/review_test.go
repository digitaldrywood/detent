package changerequest

import (
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func reviewFixture() (tracker.ChangeDetail, time.Time) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	rules := tracker.ChangeReviewPolicy{PolicyID: "policy", RequireReview: true, RequiredChecks: []tracker.ChangeCheckSpec{{Name: "test", PrincipalID: "ci", WorkflowID: "ci.yml", WorkflowSHA256: strings.Repeat("f", 64), Source: "independent", MaxAgeSeconds: 3600}}}
	rules.ID = PolicyID(rules)
	version := tracker.ChangeVersion{ID: "v1", ChangeVersionInput: tracker.ChangeVersionInput{HeadSHA: strings.Repeat("a", 40), PolicyID: "policy", RunID: "run"}, Policy: policy.Descriptor{ConfigDigest: "config"}, ReviewPolicy: rules, CreatedAt: now.Add(-time.Minute), Checks: []tracker.ChangeCheckExpectation{{ChangeCheckSpec: rules.RequiredChecks[0], CheckRunID: "ci-run"}}}
	check := tracker.ChangeCheck{VersionID: version.ID, ChangeCheckResult: tracker.ChangeCheckResult{CheckRunID: "ci-run", HeadSHA: version.HeadSHA, PolicyID: "policy", ConfigDigest: "config", RunID: "run", WorkflowID: "ci.yml", WorkflowSHA256: strings.Repeat("f", 64), Source: "independent", Conclusion: "success", CompletedAt: now, Evidence: []tracker.ChangeArtifact{{Kind: "test", URI: "s3://customer/test", SHA256: strings.Repeat("b", 64), Availability: "available"}}}, Actor: tracker.Actor{PrincipalID: "ci"}, ReceivedAt: now}
	return tracker.ChangeDetail{Change: tracker.ChangeRequest{CurrentVersion: version.ID}, Versions: []tracker.ChangeVersion{version}, Checks: []tracker.ChangeCheck{check}, Reviews: []tracker.ChangeReview{{VersionID: "v1", Actor: tracker.Actor{PrincipalID: "human"}, Decision: "approved"}}}, now
}

func TestSummarizeVersionReview(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		edit   func(*tracker.ChangeDetail, *time.Time)
		review string
		checks string
		status string
	}{
		{"reviewed", func(_ *tracker.ChangeDetail, _ *time.Time) {}, "approved", "success", "reviewed"},
		{"empty", func(d *tracker.ChangeDetail, _ *time.Time) { d.Versions = nil }, "pending", "missing", "draft"},
		{"no checks", func(d *tracker.ChangeDetail, _ *time.Time) { d.Checks = nil }, "approved", "missing", "needs_evidence"},
		{"empty check set", func(d *tracker.ChangeDetail, _ *time.Time) { d.Versions[0].Checks = nil }, "approved", "missing", "needs_evidence"},
		{"approved no CI policy", func(d *tracker.ChangeDetail, _ *time.Time) {
			d.Versions[0].Checks = nil
			d.Versions[0].ReviewPolicy.RequiredChecks = nil
			d.Checks = nil
		}, "approved", "not_required", "reviewed"},
		{"no CI still requires review", func(d *tracker.ChangeDetail, _ *time.Time) {
			d.Versions[0].Checks = nil
			d.Versions[0].ReviewPolicy.RequiredChecks = nil
			d.Checks = nil
			d.Reviews = nil
		}, "pending", "not_required", "needs_evidence"},
		{"failure", func(d *tracker.ChangeDetail, _ *time.Time) { d.Checks[0].Conclusion = "failure" }, "approved", "failure", "needs_evidence"},
		{"skipped", func(d *tracker.ChangeDetail, _ *time.Time) { d.Checks[0].Conclusion = "skipped" }, "approved", "failure", "needs_evidence"},
		{"expiry boundary", func(_ *tracker.ChangeDetail, n *time.Time) { *n = n.Add(time.Hour) }, "approved", "stale", "needs_evidence"},
		{"old check", func(d *tracker.ChangeDetail, _ *time.Time) { d.Checks[0].VersionID = "old" }, "approved", "missing", "needs_evidence"},
		{"old approval", func(d *tracker.ChangeDetail, _ *time.Time) { d.Reviews[0].VersionID = "old" }, "stale", "success", "needs_evidence"},
		{"required review", func(d *tracker.ChangeDetail, _ *time.Time) { d.Reviews = nil }, "pending", "success", "needs_evidence"},
		{"review opt out", func(d *tracker.ChangeDetail, _ *time.Time) {
			d.Reviews = nil
			d.Versions[0].ReviewPolicy.RequireReview = false
		}, "not_required", "success", "reviewed"},
		{"requested changes", func(d *tracker.ChangeDetail, _ *time.Time) {
			d.Reviews = append(d.Reviews, tracker.ChangeReview{VersionID: "v1", Actor: tracker.Actor{PrincipalID: "another"}, Decision: "changes_requested"})
		}, "changes_requested", "success", "needs_evidence"},
		{"comment retains decision", func(d *tracker.ChangeDetail, _ *time.Time) {
			d.Reviews = append(d.Reviews, tracker.ChangeReview{VersionID: "v1", Actor: tracker.Actor{PrincipalID: "human"}, Decision: "commented"})
		}, "approved", "success", "reviewed"},
		{"missing artifact", func(d *tracker.ChangeDetail, _ *time.Time) { d.Checks[0].Evidence[0].Availability = "missing" }, "approved", "missing", "needs_evidence"},
	} {
		t.Run(test.name, func(t *testing.T) {
			detail, now := reviewFixture()
			policyID, reviewID := detail.Versions[0].PolicyID, detail.Versions[0].ReviewPolicy.ID
			test.edit(&detail, &now)
			got := Summarize(detail, policyID, reviewID, now)
			if got.NativeReview != test.review || got.Checks != test.checks || got.Status != test.status {
				t.Fatalf("summary = %#v; want review %s, checks %s, status %s", got, test.review, test.checks, test.status)
			}
		})
	}
}

func TestSummarizePolicyAndExternalAuthority(t *testing.T) {
	t.Parallel()
	for _, test := range []struct{ policyID, reviewID string }{{"revoked", "review"}, {"policy", "changed"}} {
		detail, now := reviewFixture()
		detail.Versions[0].External = &tracker.ChangeExternalReference{Provider: "github", ID: "1", URL: "https://github.com/customer/repo/pull/1"}
		detail.External = &tracker.PullRequestSummary{Reviews: tracker.ReviewSummary{Decision: "approved"}, Merge: tracker.MergeSummary{Ready: true}}
		got := Summarize(detail, test.policyID, test.reviewID, now)
		if got.Status != "stale_policy" || got.ExternalReview != "external_gate" {
			t.Fatalf("external projection bypassed policy: %#v", got)
		}
	}
}

func TestReviewPolicyPreservesRepositoryGates(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		edit    func(*tracker.ChangeReviewPolicy, *policy.Descriptor)
		invalid bool
	}{
		{"valid", func(_ *tracker.ChangeReviewPolicy, _ *policy.Descriptor) {}, false},
		{"policy identity", func(r *tracker.ChangeReviewPolicy, _ *policy.Descriptor) { r.PolicyID = "other" }, true},
		{"human review", func(r *tracker.ChangeReviewPolicy, p *policy.Descriptor) {
			r.RequireReview = false
			p.Gates.Kind = "human_review"
		}, true},
		{"configured opt out", func(r *tracker.ChangeReviewPolicy, _ *policy.Descriptor) { r.RequireReview = false }, false},
		{"check floor", func(_ *tracker.ChangeReviewPolicy, p *policy.Descriptor) { p.Gates.RequiredChecks = 2 }, true},
		{"validator", func(r *tracker.ChangeReviewPolicy, p *policy.Descriptor) {
			p.Gates.Validator = true
			r.RequiredChecks[0].Source = "customer"
		}, true},
		{"independent validator", func(_ *tracker.ChangeReviewPolicy, p *policy.Descriptor) { p.Gates.Validator = true }, false},
		{"duplicate check", func(r *tracker.ChangeReviewPolicy, _ *policy.Descriptor) {
			r.RequiredChecks = append(r.RequiredChecks, r.RequiredChecks[0])
		}, true},
		{"missing principal", func(r *tracker.ChangeReviewPolicy, _ *policy.Descriptor) { r.RequiredChecks[0].PrincipalID = "" }, true},
		{"missing workflow", func(r *tracker.ChangeReviewPolicy, _ *policy.Descriptor) { r.RequiredChecks[0].WorkflowSHA256 = "" }, true},
		{"unbounded age", func(r *tracker.ChangeReviewPolicy, _ *policy.Descriptor) { r.RequiredChecks[0].MaxAgeSeconds = 0 }, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			detail, _ := reviewFixture()
			rules := detail.Versions[0].ReviewPolicy
			approved := policy.Descriptor{ID: "policy", Gates: policy.Gates{Kind: "command", AutoPromote: false, MergeMethod: "squash"}}
			test.edit(&rules, &approved)
			original := approved
			err := ValidatePolicy(rules, approved)
			if (err != nil) != test.invalid || approved.Gates != original.Gates {
				t.Fatalf("policy validation = %v, invalid = %v", err, test.invalid)
			}
		})
	}
}

func TestCIPrincipalAndSourceValidation(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, principal, source string
		worker, valid           bool
	}{
		{"independent", "ci", "independent", false, true},
		{"customer", "ci", "customer", true, true},
		{"worker independent", "ci", "independent", true, false},
		{"wrong principal", "runner", "independent", false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			detail, _ := reviewFixture()
			version := detail.Versions[0]
			version.Checks[0].Source = test.source
			result := detail.Checks[0].ChangeCheckResult
			result.Source = test.source
			err := ValidateResult(version, result, test.principal, test.worker)
			if (err == nil) != test.valid {
				t.Fatalf("validation = %v, valid = %v", err, test.valid)
			}
		})
	}
}

func TestArtifactMetadataBoundary(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		uri   string
		valid bool
	}{
		{"s3://bucket/artifact", true}, {"gs://bucket/artifact", true}, {"https://customer.example/artifact", true},
		{"https://customer.example/artifact?token=secret", false}, {"https://user:secret@customer.example/artifact", false},
		{"file:///workspace/main.go", false}, {"data:text/plain,source", false}, {"https://customer.example/artifact#secret", false}, {"https://", false},
	} {
		t.Run(test.uri, func(t *testing.T) {
			err := ValidateArtifacts([]tracker.ChangeArtifact{{Kind: "manifest", URI: test.uri, SHA256: strings.Repeat("a", 64), Availability: "unverified"}})
			if (err == nil) != test.valid {
				t.Fatalf("artifact validation = %v", err)
			}
		})
	}
	if err := ValidateArtifacts(make([]tracker.ChangeArtifact, 65)); err == nil {
		t.Fatal("unbounded artifact set accepted")
	}
}
