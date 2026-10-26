package changerequest

import (
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/securityaudit"
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
	sourcePass := tracker.ChangeReview{VersionID: "v1", Decision: "approved", ImplementationReview: &gate.ValidatorResult{Verdict: "pass", SessionID: 42}}
	sourceRework := tracker.ChangeReview{VersionID: "v1", Decision: "changes_requested", ImplementationReview: &gate.ValidatorResult{Verdict: "rework", SessionID: 42}}
	sourceWait := tracker.ChangeReview{VersionID: "v1", Decision: "commented", ImplementationReview: &gate.ValidatorResult{Verdict: "wait", SessionID: 42}}
	acceptancePass := tracker.ChangeReview{VersionID: "v1", Decision: "approved", Validator: &gate.ValidatorResult{Verdict: "pass", SessionID: 43}}
	stages := func(mode string, validator bool, reviews ...tracker.ChangeReview) func(*tracker.ChangeDetail, *time.Time) {
		return func(d *tracker.ChangeDetail, now *time.Time) {
			d.Versions[0].Policy.Gates.AutomatedReview = mode
			d.Versions[0].Policy.Gates.Validator = validator
			for _, review := range reviews {
				review.CreatedAt = *now
				d.Reviews = append(d.Reviews, review)
			}
		}
	}
	audits := func(blockedVersion string, landing bool, laterPass bool) func(*tracker.ChangeDetail, *time.Time) {
		return func(d *tracker.ChangeDetail, now *time.Time) {
			d.Change.ProjectID = "prj_example"
			version := &d.Versions[0]
			version.Repository, version.BaseSHA = "https://github.com/example-org/example-project", strings.Repeat("b", 40)
			version.Policy.Gates.SecurityAudit = true
			run := securityaudit.Run{ID: 42, InvocationID: "audit", ProjectID: "prj_example", Repository: version.Repository, BaseSHA: version.BaseSHA, HeadSHA: version.HeadSHA, ReviewerVersion: securityaudit.ReviewerVersion, ReviewerDigest: securityaudit.ReviewerDigest(), ExitStatus: securityaudit.ExitStatusSuccess, OutputBytes: 1, OutputDigest: securityaudit.OutputDigest("result"), WorkerPID: 1, WorkerPGID: 1, WorkerStartedAt: *now, StartedAt: *now, CompletedAt: *now, Verdict: "fail", Summary: "Authorization bypass", Findings: []securityaudit.Finding{{ID: "auth", Severity: "p1", Body: "Authorization bypass"}}}
			d.Reviews = append(d.Reviews, tracker.ChangeReview{VersionID: blockedVersion, Decision: "changes_requested", SecurityAudit: &tracker.SecurityAuditReview{Landing: landing, Verdict: "rework", Run: run}})
			if laterPass {
				run.ID, run.Verdict, run.Findings = 43, "pass", nil
				d.Reviews = append(d.Reviews, tracker.ChangeReview{VersionID: version.ID, Decision: "approved", SecurityAudit: &tracker.SecurityAuditReview{Verdict: "pass", Run: run}})
			}
		}
	}
	for _, test := range []struct {
		name   string
		edit   func(*tracker.ChangeDetail, *time.Time)
		review string
		checks string
		status string
	}{
		{"unchanged source audit cannot be cleared by pass", audits("v1", false, true), "approved", "success", "needs_evidence"},
		{"unchanged landing audit cannot be cleared by pass", audits("v1", true, true), "approved", "success", "needs_evidence"},
		{"new source version can pass after blocking audit", audits("old", false, true), "approved", "success", "reviewed"},
		{"reviewed", func(_ *tracker.ChangeDetail, _ *time.Time) {}, "approved", "success", "reviewed"},
		{"required source acceptance only", stages(gate.AutomatedReviewRequired, true, acceptancePass), "approved", "success", "needs_evidence"},
		{"required source reversed retained acceptance", stages(gate.AutomatedReviewRequired, true, acceptancePass, sourcePass), "approved", "success", "needs_evidence"},
		{"required source then fresh acceptance", stages(gate.AutomatedReviewRequired, true, sourcePass, acceptancePass), "approved", "success", "reviewed"},
		{"retained acceptance then source then fresh acceptance", stages(gate.AutomatedReviewRequired, true, acceptancePass, sourcePass, acceptancePass), "approved", "success", "reviewed"},
		{"source rerun needs fresh acceptance", stages(gate.AutomatedReviewRequired, true, sourcePass, acceptancePass, sourcePass), "approved", "success", "needs_evidence"},
		{"source rework invalidates acceptance", stages(gate.AutomatedReviewRequired, true, sourcePass, acceptancePass, sourceRework), "approved", "success", "needs_evidence"},
		{"source wait invalidates acceptance", stages(gate.AutomatedReviewRequired, true, sourcePass, acceptancePass, sourceWait), "approved", "success", "needs_evidence"},
		{"source rework recovery needs fresh acceptance", stages(gate.AutomatedReviewRequired, true, sourcePass, acceptancePass, sourceRework, sourcePass), "approved", "success", "needs_evidence"},
		{"source wait recovery needs fresh acceptance", stages(gate.AutomatedReviewRequired, true, sourcePass, acceptancePass, sourceWait, sourcePass), "approved", "success", "needs_evidence"},
		{"source wait recovery then fresh acceptance", stages(gate.AutomatedReviewRequired, true, sourceWait, sourcePass, acceptancePass), "approved", "success", "reviewed"},
		{"source rework recovery then fresh acceptance", stages(gate.AutomatedReviewRequired, true, sourceRework, sourcePass, acceptancePass), "approved", "success", "reviewed"},
		{"optional source preserves acceptance", stages(gate.AutomatedReviewOptional, true, acceptancePass, sourcePass), "approved", "success", "reviewed"},
		{"optional source acceptance only", stages(gate.AutomatedReviewOptional, true, acceptancePass), "approved", "success", "reviewed"},
		{"required source disabled acceptance", stages(gate.AutomatedReviewRequired, false, sourcePass), "approved", "success", "reviewed"},
		{"required source disabled acceptance wait", stages(gate.AutomatedReviewRequired, false, sourceWait), "approved", "success", "needs_evidence"},
		{"validator missing", func(d *tracker.ChangeDetail, _ *time.Time) {
			d.Versions[0].Policy.Gates.Validator = true
		}, "approved", "success", "needs_evidence"},
		{"validator pass", func(d *tracker.ChangeDetail, _ *time.Time) {
			d.Versions[0].Policy.Gates.Validator = true
			d.Reviews = append(d.Reviews, tracker.ChangeReview{VersionID: "v1", Decision: "approved", Validator: &gate.ValidatorResult{Verdict: "pass"}})
		}, "approved", "success", "reviewed"},
		{"validator rework", func(d *tracker.ChangeDetail, _ *time.Time) {
			d.Versions[0].Policy.Gates.Validator = true
			d.Reviews = append(d.Reviews, tracker.ChangeReview{VersionID: "v1", Decision: "changes_requested", Validator: &gate.ValidatorResult{Verdict: "rework"}})
		}, "approved", "success", "needs_evidence"},
		{"validator cannot approve human review", func(d *tracker.ChangeDetail, _ *time.Time) {
			d.Versions[0].Policy.Gates.Validator = true
			d.Reviews = []tracker.ChangeReview{{VersionID: "v1", Decision: "approved", Validator: &gate.ValidatorResult{Verdict: "pass"}}}
		}, "pending", "success", "needs_evidence"},
		{"old validator cannot make human approval stale", func(d *tracker.ChangeDetail, _ *time.Time) {
			d.Versions[0].Policy.Gates.Validator = true
			d.Reviews = []tracker.ChangeReview{{VersionID: "old", Decision: "approved", Validator: &gate.ValidatorResult{Verdict: "pass"}}}
		}, "pending", "success", "needs_evidence"},
		{"validator approval belongs to old version", func(d *tracker.ChangeDetail, _ *time.Time) {
			d.Versions[0].Policy.Gates.Validator = true
			d.Reviews = append(d.Reviews, tracker.ChangeReview{VersionID: "old", Decision: "approved", Validator: &gate.ValidatorResult{Verdict: "pass"}})
		}, "approved", "success", "needs_evidence"},
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
		{"landed after review", func(d *tracker.ChangeDetail, _ *time.Time) {
			d.Change.Landed = &tracker.ChangeLanding{VersionID: "v1", MergeSHA: "abc", BaseRef: "main"}
		}, "approved", "not_required", "landed"},
		{"earlier landed version does not finish reopened work", func(d *tracker.ChangeDetail, _ *time.Time) {
			d.Change.Landed = &tracker.ChangeLanding{VersionID: "v0", MergeSHA: "abc", BaseRef: "main"}
		}, "approved", "success", "reviewed"},
		{"earlier landing does not satisfy current review", func(d *tracker.ChangeDetail, _ *time.Time) {
			d.Change.Landed = &tracker.ChangeLanding{VersionID: "v0", MergeSHA: "abc", BaseRef: "main"}
			d.Reviews = nil
		}, "pending", "success", "needs_evidence"},
		{"landed without a reviewer", func(d *tracker.ChangeDetail, _ *time.Time) {
			d.Change.Landed = &tracker.ChangeLanding{VersionID: "v1", MergeSHA: "abc", BaseRef: "main"}
			d.Reviews = nil
			d.Versions[0].ReviewPolicy.RequireReview = false
		}, "not_required", "not_required", "landed"},
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
	for _, test := range []struct {
		name  string
		edit  func(*tracker.ChangeVersion, *policy.Descriptor, *tracker.ChangeReviewPolicy)
		stale bool
	}{
		{name: "identical requirements"},
		{name: "validator dropped", edit: func(_ *tracker.ChangeVersion, p *policy.Descriptor, _ *tracker.ChangeReviewPolicy) {
			p.Gates.Validator = false
		}},
		{name: "security audit dropped", edit: func(_ *tracker.ChangeVersion, p *policy.Descriptor, _ *tracker.ChangeReviewPolicy) {
			p.Gates.SecurityAudit = false
		}},
		{name: "validator added", stale: true, edit: func(v *tracker.ChangeVersion, _ *policy.Descriptor, _ *tracker.ChangeReviewPolicy) {
			v.Policy.Gates.Validator = false
		}},
		{name: "security audit added", stale: true, edit: func(v *tracker.ChangeVersion, _ *policy.Descriptor, _ *tracker.ChangeReviewPolicy) {
			v.Policy.Gates.SecurityAudit = false
		}},
		{name: "required source review added", stale: true, edit: func(v *tracker.ChangeVersion, _ *policy.Descriptor, _ *tracker.ChangeReviewPolicy) {
			v.Policy.Gates.AutomatedReview = gate.AutomatedReviewOff
		}},
		{name: "source review optional", edit: func(_ *tracker.ChangeVersion, p *policy.Descriptor, _ *tracker.ChangeReviewPolicy) {
			p.Gates.AutomatedReview = gate.AutomatedReviewOptional
		}},
		{name: "required checks shrink", edit: func(_ *tracker.ChangeVersion, p *policy.Descriptor, r *tracker.ChangeReviewPolicy) {
			p.Gates.RequiredChecks = 0
			r.RequiredChecks = nil
		}},
		{name: "admin checks already cover increased floor", edit: func(v *tracker.ChangeVersion, _ *policy.Descriptor, _ *tracker.ChangeReviewPolicy) {
			v.Policy.Gates.RequiredChecks = 0
		}},
		{name: "required check added", stale: true, edit: func(_ *tracker.ChangeVersion, p *policy.Descriptor, r *tracker.ChangeReviewPolicy) {
			check := r.RequiredChecks[0]
			check.Name = "additional"
			r.RequiredChecks = append(r.RequiredChecks, check)
			p.Gates.RequiredChecks++
		}},
		{name: "admin required check added", stale: true, edit: func(_ *tracker.ChangeVersion, _ *policy.Descriptor, r *tracker.ChangeReviewPolicy) {
			check := r.RequiredChecks[0]
			check.Name = "admin-pinned"
			r.RequiredChecks = append(r.RequiredChecks, check)
		}},
		{name: "check principal changed", stale: true, edit: func(_ *tracker.ChangeVersion, _ *policy.Descriptor, r *tracker.ChangeReviewPolicy) {
			r.RequiredChecks[0].PrincipalID = "other-ci"
		}},
		{name: "check workflow changed", stale: true, edit: func(_ *tracker.ChangeVersion, _ *policy.Descriptor, r *tracker.ChangeReviewPolicy) {
			r.RequiredChecks[0].WorkflowID = "other.yml"
		}},
		{name: "check workflow digest changed", stale: true, edit: func(_ *tracker.ChangeVersion, _ *policy.Descriptor, r *tracker.ChangeReviewPolicy) {
			r.RequiredChecks[0].WorkflowSHA256 = strings.Repeat("e", 64)
		}},
		{name: "check source changed", stale: true, edit: func(_ *tracker.ChangeVersion, _ *policy.Descriptor, r *tracker.ChangeReviewPolicy) {
			r.RequiredChecks[0].Source = "customer"
		}},
		{name: "check freshness tightened", stale: true, edit: func(_ *tracker.ChangeVersion, _ *policy.Descriptor, r *tracker.ChangeReviewPolicy) {
			r.RequiredChecks[0].MaxAgeSeconds = 60
		}},
		{name: "check freshness relaxed", edit: func(_ *tracker.ChangeVersion, _ *policy.Descriptor, r *tracker.ChangeReviewPolicy) {
			r.RequiredChecks[0].MaxAgeSeconds = 7200
		}},
		{name: "review newly required", stale: true, edit: func(v *tracker.ChangeVersion, _ *policy.Descriptor, _ *tracker.ChangeReviewPolicy) {
			v.ReviewPolicy.RequireReview = false
		}},
		{name: "review no longer required", edit: func(_ *tracker.ChangeVersion, _ *policy.Descriptor, r *tracker.ChangeReviewPolicy) {
			r.RequireReview = false
		}},
		{name: "landing mode only", edit: func(_ *tracker.ChangeVersion, p *policy.Descriptor, _ *tracker.ChangeReviewPolicy) {
			p.Gates.LandingMode = gate.LandingPerChange
		}},
		{name: "merge method only", edit: func(_ *tracker.ChangeVersion, p *policy.Descriptor, _ *tracker.ChangeReviewPolicy) {
			p.Gates.MergeMethod = "rebase"
		}},
		{name: "human gate added", stale: true, edit: func(_ *tracker.ChangeVersion, p *policy.Descriptor, _ *tracker.ChangeReviewPolicy) {
			p.Gates.HumanReview = true
		}},
		{name: "planning added", stale: true, edit: func(_ *tracker.ChangeVersion, p *policy.Descriptor, _ *tracker.ChangeReviewPolicy) {
			p.Gates.PlanEnabled = true
		}},
		{name: "plan review requires human approval", stale: true, edit: func(v *tracker.ChangeVersion, p *policy.Descriptor, _ *tracker.ChangeReviewPolicy) {
			v.Policy.Gates.PlanEnabled, v.Policy.Gates.PlanReview = true, gate.PlanReviewBoth
			p.Gates.PlanEnabled, p.Gates.PlanReview = true, gate.PlanReviewHuman
		}},
		{name: "plan review requires automated approval", stale: true, edit: func(v *tracker.ChangeVersion, p *policy.Descriptor, _ *tracker.ChangeReviewPolicy) {
			v.Policy.Gates.PlanEnabled, v.Policy.Gates.PlanReview = true, gate.PlanReviewBoth
			p.Gates.PlanEnabled, p.Gates.PlanReview = true, gate.PlanReviewAutomated
		}},
		{name: "plan review accepts automated or human approval", edit: func(v *tracker.ChangeVersion, p *policy.Descriptor, _ *tracker.ChangeReviewPolicy) {
			v.Policy.Gates.PlanEnabled, v.Policy.Gates.PlanReview = true, gate.PlanReviewAutomated
			p.Gates.PlanEnabled, p.Gates.PlanReview = true, gate.PlanReviewBoth
		}},
		{name: "plan review accepts human or automated approval", edit: func(v *tracker.ChangeVersion, p *policy.Descriptor, _ *tracker.ChangeReviewPolicy) {
			v.Policy.Gates.PlanEnabled, v.Policy.Gates.PlanReview = true, gate.PlanReviewHuman
			p.Gates.PlanEnabled, p.Gates.PlanReview = true, gate.PlanReviewBoth
		}},
		{name: "external gate added", stale: true, edit: func(_ *tracker.ChangeVersion, p *policy.Descriptor, _ *tracker.ChangeReviewPolicy) {
			p.Gates.GitHubPullRequest = true
		}},
		{name: "external workflow changed", stale: true, edit: func(_ *tracker.ChangeVersion, p *policy.Descriptor, _ *tracker.ChangeReviewPolicy) {
			p.Gates.GitHubCIDigest = "changed"
		}},
		{name: "landing command changed", stale: true, edit: func(_ *tracker.ChangeVersion, p *policy.Descriptor, _ *tracker.ChangeReviewPolicy) {
			p.Gates.LandingCommandDigest = "changed"
		}},
		{name: "gate kind changed", stale: true, edit: func(_ *tracker.ChangeVersion, p *policy.Descriptor, _ *tracker.ChangeReviewPolicy) {
			p.Gates.Kind = gate.KindHumanReview
		}},
		{name: "missing approved policy", edit: func(_ *tracker.ChangeVersion, p *policy.Descriptor, _ *tracker.ChangeReviewPolicy) {
			*p = policy.Descriptor{}
		}},
		{name: "missing approved policy and review policy", edit: func(_ *tracker.ChangeVersion, p *policy.Descriptor, r *tracker.ChangeReviewPolicy) {
			*p, *r = policy.Descriptor{}, tracker.ChangeReviewPolicy{}
		}},
		{name: "rolling barrier version", stale: true, edit: func(v *tracker.ChangeVersion, _ *policy.Descriptor, _ *tracker.ChangeReviewPolicy) {
			v.Policy.Gates.LandingMode = gate.LandingRollingBarrier
		}},
		{name: "unapproved check floor", stale: true, edit: func(_ *tracker.ChangeVersion, p *policy.Descriptor, _ *tracker.ChangeReviewPolicy) {
			p.Gates.RequiredChecks++
		}},
		{name: "mismatched version snapshot", stale: true, edit: func(v *tracker.ChangeVersion, _ *policy.Descriptor, _ *tracker.ChangeReviewPolicy) {
			v.Policy.ID = "other"
		}},
		{name: "missing version review policy", stale: true, edit: func(v *tracker.ChangeVersion, _ *policy.Descriptor, _ *tracker.ChangeReviewPolicy) {
			v.ReviewPolicy.ID = ""
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			detail, now := reviewFixture()
			detail.Change.ProjectID = "prj_example"
			version := &detail.Versions[0]
			version.Repository, version.BaseSHA = "https://github.com/example-org/example-project", strings.Repeat("b", 40)
			version.Policy.ID = version.PolicyID
			version.Policy.Gates = policy.Gates{Kind: gate.KindCommand, Validator: true, SecurityAudit: true, AutomatedReview: gate.AutomatedReviewRequired, RequiredChecks: 1, MergeMethod: "squash"}
			version.External = &tracker.ChangeExternalReference{Provider: "github", ID: "1", URL: "https://github.com/example-org/example-project/pull/1"}
			detail.External = &tracker.PullRequestSummary{Reviews: tracker.ReviewSummary{Decision: "approved"}, Merge: tracker.MergeSummary{Ready: true}}
			run := securityaudit.Run{ID: 42, InvocationID: "audit", ProjectID: "prj_example", Repository: version.Repository, BaseSHA: version.BaseSHA, HeadSHA: version.HeadSHA, ReviewerVersion: securityaudit.ReviewerVersion, ReviewerDigest: securityaudit.ReviewerDigest(), ExitStatus: securityaudit.ExitStatusSuccess, OutputBytes: 1, OutputDigest: securityaudit.OutputDigest("result"), WorkerPID: 1, WorkerPGID: 1, WorkerStartedAt: now, StartedAt: now, CompletedAt: now, Verdict: "pass", Summary: "No findings"}
			detail.Reviews = append(detail.Reviews,
				tracker.ChangeReview{VersionID: version.ID, Decision: "approved", ImplementationReview: &gate.ValidatorResult{Verdict: "pass"}},
				tracker.ChangeReview{VersionID: version.ID, Decision: "approved", Validator: &gate.ValidatorResult{Verdict: "pass"}},
				tracker.ChangeReview{VersionID: version.ID, Decision: "approved", SecurityAudit: &tracker.SecurityAuditReview{Verdict: "pass", Run: run}},
			)
			approved, rules := version.Policy, version.ReviewPolicy
			approved.ID, rules.ID = "current-policy", "current-review"
			rules.RequiredChecks = append([]tracker.ChangeCheckSpec(nil), rules.RequiredChecks...)
			if test.edit != nil {
				test.edit(version, &approved, &rules)
			}
			policyID, reviewID := CurrentPolicyIdentities(version, approved, rules)
			detail.Summary = Summarize(detail, policyID, reviewID, now)
			want := "reviewed"
			if test.stale {
				want = "stale_policy"
			}
			if detail.Summary.Status != want || detail.Summary.ExternalReview != "external_gate" {
				t.Fatalf("summary = %#v; want %s with external gate", detail.Summary, want)
			}
			if state := ReviewState(detail, version.ID, version.HeadSHA); state.Current == test.stale || state.Reviewed == test.stale {
				t.Fatalf("review state = %#v; stale = %t", state, test.stale)
			}
		})
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
		{"validator without checks", func(r *tracker.ChangeReviewPolicy, p *policy.Descriptor) {
			p.Gates.Validator = true
			r.RequiredChecks = nil
		}, false},
		{"validator", func(r *tracker.ChangeReviewPolicy, p *policy.Descriptor) {
			p.Gates.Validator = true
			r.RequiredChecks[0].Source = "customer"
		}, false},
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

func TestReviewState(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		edit    func(*tracker.ChangeDetail)
		version string
		head    string
		want    connector.ChangeReview
	}{
		{name: "passing checks with missing review", edit: func(d *tracker.ChangeDetail) { d.Reviews = nil }, want: connector.ChangeReview{Current: true}},
		{name: "failed checks require rework", edit: func(d *tracker.ChangeDetail) { d.Checks[0].Conclusion = "failure" }, want: connector.ChangeReview{Current: true, ChangesRequested: true}},
		{name: "cancelled checks require a fresh version", edit: func(d *tracker.ChangeDetail) { d.Checks[0].Conclusion = "cancelled" }, want: connector.ChangeReview{Current: true, ChangesRequested: true}},
		{name: "skipped checks require a fresh version", edit: func(d *tracker.ChangeDetail) { d.Checks[0].Conclusion = "skipped" }, want: connector.ChangeReview{Current: true, ChangesRequested: true}},
		{name: "missing checks wait for evidence", edit: func(d *tracker.ChangeDetail) { d.Checks = nil }, want: connector.ChangeReview{Current: true}},
		{name: "stale successful checks wait for evidence", edit: func(d *tracker.ChangeDetail) { d.Checks[0].CompletedAt = d.Checks[0].CompletedAt.Add(-24 * time.Hour) }, want: connector.ChangeReview{Current: true}},
		{name: "unrequested failure does not require rework", edit: func(d *tracker.ChangeDetail) {
			other := d.Checks[0]
			other.CheckRunID, other.Conclusion = "unrequested", "failure"
			d.Checks = append(d.Checks, other)
		}, want: connector.ChangeReview{Current: true, Reviewed: true}},
		{name: "failed check is actionable despite other missing evidence", edit: func(d *tracker.ChangeDetail) {
			d.Checks[0].Conclusion = "failure"
			d.Checks[0].Evidence[0].Availability = "missing"
			d.Versions[0].Checks = append(d.Versions[0].Checks, d.Versions[0].Checks[0])
			d.Versions[0].Checks[1].CheckRunID = "missing-run"
		}, want: connector.ChangeReview{Current: true, ChangesRequested: true}},
		{name: "source review wait needs implementation", edit: func(d *tracker.ChangeDetail) {
			d.Reviews = append(d.Reviews, tracker.ChangeReview{VersionID: "v1", Decision: "commented", ImplementationReview: &gate.ValidatorResult{Verdict: "wait"}})
			d.Versions[0].Policy.Gates.AutomatedReview = gate.AutomatedReviewRequired
		}, want: connector.ChangeReview{Current: true, ChangesRequested: true}},
		{name: "source findings need implementation", edit: func(d *tracker.ChangeDetail) {
			d.Reviews = append(d.Reviews, tracker.ChangeReview{VersionID: "v1", Decision: "changes_requested", ImplementationReview: &gate.ValidatorResult{Verdict: "rework"}})
		}, want: connector.ChangeReview{Current: true, ChangesRequested: true}},
		{name: "acceptance wait needs implementation", edit: func(d *tracker.ChangeDetail) {
			d.Reviews = append(d.Reviews, tracker.ChangeReview{VersionID: "v1", Decision: "commented", Validator: &gate.ValidatorResult{Verdict: "wait"}})
		}, want: connector.ChangeReview{Current: true, ChangesRequested: true}},
		{name: "acceptance findings need implementation", edit: func(d *tracker.ChangeDetail) {
			d.Reviews = append(d.Reviews, tracker.ChangeReview{VersionID: "v1", Decision: "changes_requested", Validator: &gate.ValidatorResult{Verdict: "rework"}})
		}, want: connector.ChangeReview{Current: true, ChangesRequested: true}},
		{name: "security findings survive later pass on unchanged source", edit: func(d *tracker.ChangeDetail) {
			d.Reviews = append(d.Reviews, tracker.ChangeReview{VersionID: "v1", Decision: "changes_requested", SecurityAudit: &tracker.SecurityAuditReview{Verdict: "rework"}}, tracker.ChangeReview{VersionID: "v1", Decision: "approved", SecurityAudit: &tracker.SecurityAuditReview{Verdict: "pass"}})
		}, want: connector.ChangeReview{Current: true, ChangesRequested: true}},
		{name: "findings on older version do not restart implementation", edit: func(d *tracker.ChangeDetail) {
			d.Reviews = append(d.Reviews, tracker.ChangeReview{VersionID: "old", Decision: "changes_requested", ImplementationReview: &gate.ValidatorResult{Verdict: "rework"}})
		}, want: connector.ChangeReview{Current: true, Reviewed: true}},
		{name: "fresh source pass supersedes source rework", edit: func(d *tracker.ChangeDetail) {
			d.Reviews = append(d.Reviews, tracker.ChangeReview{VersionID: "v1", Decision: "changes_requested", ImplementationReview: &gate.ValidatorResult{Verdict: "rework"}}, tracker.ChangeReview{VersionID: "v1", Decision: "approved", ImplementationReview: &gate.ValidatorResult{Verdict: "pass"}})
		}, want: connector.ChangeReview{Current: true, Reviewed: true}},
		{name: "changed head", head: strings.Repeat("b", 40)},
		{name: "changed version", version: "old"},
		{name: "changed policy", edit: func(d *tracker.ChangeDetail) { d.Versions[0].PolicyID = "other" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			detail, now := reviewFixture()
			policyID, reviewID := detail.Versions[0].PolicyID, detail.Versions[0].ReviewPolicy.ID
			if test.edit != nil {
				test.edit(&detail)
			}
			detail.Summary = Summarize(detail, policyID, reviewID, now)
			version, head := test.version, test.head
			if version == "" {
				version = "v1"
			}
			if head == "" {
				head = strings.Repeat("a", 40)
			}
			if got := ReviewState(detail, version, head); got != test.want {
				t.Fatalf("review state=%+v want=%+v", got, test.want)
			}
		})
	}
}
