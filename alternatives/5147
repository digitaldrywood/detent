package tracker

import (
	"github.com/digitaldrywood/detent/internal/failureattribution"
	"github.com/digitaldrywood/detent/internal/gate"
)

func IndependentGateFailures(result *gate.ValidatorResult) []failureattribution.Evidence {
	if result == nil || result.Verdict != gate.ValidatorVerdictRework || len(result.NotVerified) != 0 {
		return nil
	}
	var failures []failureattribution.Evidence
	add := func(findings []gate.Finding) bool {
		for _, finding := range findings {
			if finding.Attribution != failureattribution.Independent || !failureattribution.Valid(finding.Attribution, finding.AttributionEvidence, result.BaseSHA, result.HeadSHA) {
				return false
			}
			failures = append(failures, *finding.AttributionEvidence)
		}
		return true
	}
	if !add(result.Findings) {
		return nil
	}
	for _, command := range result.Commands {
		if command.ExitCode != 0 {
			if command.Attribution != failureattribution.Independent || len(command.Findings) == 0 || !add(command.Findings) {
				return nil
			}
		}
	}
	return failures
}

func (r ChangeReview) IndependentFailures() []failureattribution.Evidence {
	roles := 0
	if r.Validator != nil {
		roles++
	}
	if r.ImplementationReview != nil {
		roles++
	}
	if r.SecurityAudit != nil {
		roles++
	}
	if r.Decision != "changes_requested" || roles != 1 {
		return nil
	}
	switch {
	case r.Validator != nil:
		return IndependentGateFailures(r.Validator)
	case r.ImplementationReview != nil:
		return IndependentGateFailures(r.ImplementationReview)
	case r.SecurityAudit != nil:
		run := r.SecurityAudit.Run
		if r.SecurityAudit.Verdict != "rework" {
			return nil
		}
		var failures []failureattribution.Evidence
		for _, finding := range run.Findings {
			if finding.Status == "resolved" {
				continue
			}
			if finding.Attribution != failureattribution.Independent || !failureattribution.Valid(finding.Attribution, finding.AttributionEvidence, run.BaseSHA, run.HeadSHA) {
				return nil
			}
			failures = append(failures, *finding.AttributionEvidence)
		}
		return failures
	}
	return nil
}

func RepairDependenciesReady(issue NativeIssue, dependencies []NativeWorkItemID) bool {
	if !issue.DependencyReady || len(dependencies) == 0 {
		return false
	}
	for _, id := range dependencies {
		found := false
		for _, blocker := range issue.Blockers {
			if blocker.ID == id && blocker.ID != issue.WorkItemID && blocker.Terminal {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
