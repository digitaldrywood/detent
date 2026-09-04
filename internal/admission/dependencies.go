package admission

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	admissionmodel "github.com/digitaldrywood/detent/internal/admission/model"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/dependencyline"
	"github.com/digitaldrywood/detent/internal/runner"
)

const admissionDependencyLimit = 50

func admissionDependencyReferences(issue connector.Issue) []string {
	repo, _, _ := strings.Cut(issue.Identifier, "#")
	if !strings.Contains(repo, "/") {
		repo = ""
	}
	refs := make(map[string]struct{})
	add := func(ref string) {
		ref = strings.TrimSpace(ref)
		if strings.HasPrefix(ref, "https://github.com/") {
			ref = strings.Replace(strings.TrimPrefix(ref, "https://github.com/"), "/issues/", "#", 1)
		}
		if strings.HasPrefix(ref, "#") {
			ref = repo + ref
		}
		if ref != "" && !strings.EqualFold(ref, issue.Identifier) {
			refs[strings.ToLower(ref)] = struct{}{}
		}
	}
	for _, ref := range issue.BlockedBy {
		add(ref.Identifier)
	}
	for _, line := range strings.Split(issue.Description, "\n") {
		text, ok := dependencyline.Match(line)
		if !ok {
			continue
		}
		for _, ref := range issueReferencePattern.FindAllString(text, -1) {
			add(ref)
		}
	}
	out := make([]string, 0, len(refs))
	for ref := range refs {
		out = append(out, ref)
	}
	sort.Strings(out)
	return out
}

func resolveAdmissionDependencies(ctx context.Context, settings Settings, issue connector.Issue, at time.Time) *runner.AdmissionDependencies {
	refs := admissionDependencyReferences(issue)
	if len(refs) == 0 {
		return nil
	}
	rule := strings.ToLower(strings.TrimSpace(settings.DependencyReadiness))
	if rule == "" {
		rule = config.DependencyReadinessTerminalOrMerged
	}
	evidence := &runner.AdmissionDependencies{ObservedAt: at.UTC(), Readiness: rule, Ready: true}
	if len(refs) > admissionDependencyLimit {
		evidence.Ready = false
		evidence.References = append(evidence.References, runner.AdmissionDependency{Error: "dependency reference limit exceeded"})
		refs = refs[:admissionDependencyLimit]
	}
	resolved := make(map[string]connector.Issue)
	resolutionError := "tracker cannot resolve issue references"
	if resolver, ok := settings.Issues.(connector.IssueReferenceResolver); ok {
		issues, err := resolver.FetchIssueStatesByIdentifiers(ctx, refs)
		if err != nil {
			resolutionError = "dependency resolution failed: " + redactAdmissionOutput([]byte(err.Error()))
		} else {
			resolutionError = "dependency was not returned by tracker"
			resolved = indexAdmissionDependencies(issues, refs)
		}
	}
	for _, ref := range refs {
		entry := runner.AdmissionDependency{Identifier: ref}
		dependency, found := resolved[ref]
		if !found {
			entry.Error = resolutionError
		} else {
			entry.State = dependency.State
			entry.Closed = dependency.Closed
			if dependency.PullRequest != nil {
				entry.PullRequestState = strings.ToLower(strings.TrimSpace(dependency.PullRequest.State))
			}
			entry.Ready = dependency.Closed || containsFold(settings.TerminalStates, dependency.State) ||
				(rule == config.DependencyReadinessTerminalOrMerged && entry.PullRequestState == "merged")
		}
		evidence.Ready = evidence.Ready && entry.Ready
		evidence.References = append(evidence.References, entry)
	}
	return evidence
}

func indexAdmissionDependencies(issues []connector.Issue, refs []string) map[string]connector.Issue {
	resolved := make(map[string]connector.Issue, len(issues))
	for _, issue := range issues {
		resolved[strings.ToLower(strings.TrimSpace(issue.Identifier))] = issue
	}
	for _, ref := range refs {
		if _, found := resolved[ref]; found || !strings.HasPrefix(ref, "#") {
			continue
		}
		number, err := strconv.Atoi(strings.TrimPrefix(ref, "#"))
		if err != nil || number <= 0 {
			continue
		}
		var match connector.Issue
		matches := 0
		for _, issue := range issues {
			if issue.Number == number {
				match = issue
				matches++
			}
		}
		if matches == 1 {
			resolved[ref] = match
		}
	}
	return resolved
}

func dependencyFingerprint(base string, snapshots ...*runner.AdmissionDependencies) string {
	if len(snapshots) == 0 || snapshots[0] == nil {
		return base
	}
	evidence := snapshots[0]
	parts := []string{base, evidence.Readiness, strconv.FormatBool(evidence.Ready)}
	for _, ref := range evidence.References {
		parts = append(parts, ref.Identifier, ref.State, strconv.FormatBool(ref.Closed), ref.PullRequestState, strconv.FormatBool(ref.Ready), ref.Error)
	}
	return "dependencies-v1:" + stableAdmissionFingerprint(parts...)
}

func admissionDependencySummary(evidence *runner.AdmissionDependencies) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Dependency evidence observed at %s (rule %s):", evidence.ObservedAt.Format(time.RFC3339), evidence.Readiness)
	for _, ref := range evidence.References {
		fmt.Fprintf(&b, " %s [state=%q, closed=%t, pull_request=%q, ready=%t", ref.Identifier, ref.State, ref.Closed, ref.PullRequestState, ref.Ready)
		if ref.Error != "" {
			fmt.Fprintf(&b, ", error=%q", ref.Error)
		}
		b.WriteString("];")
	}
	b.WriteString(" historical evidence, not a claim of present readiness.")
	return b.String()
}

func admissionDependencyFindings(evaluation AgentEvaluation, evidence *runner.AdmissionDependencies) AgentEvaluation {
	if evidence == nil {
		return evaluation
	}
	for index := range evaluation.Findings {
		finding := &evaluation.Findings[index]
		if strings.EqualFold(finding.Dimension, "Readiness") && !evidence.Ready {
			finding.Matched = false
		}
		finding.Rationale += " " + admissionDependencySummary(evidence)
	}
	return evaluation
}

func (m *Manager) supersedeChangedDependencies(ctx context.Context, settings Settings, issue connector.Issue, proposal admissionmodel.Proposal, at time.Time, accepting bool) (bool, error) {
	evidence := resolveAdmissionDependencies(ctx, settings, issue, at)
	if evidence == nil && !strings.HasPrefix(proposal.Fingerprint, "dependencies-v1:") {
		return false, nil
	}
	if proposal.Fingerprint == issueFingerprint(issue, evidence) {
		return accepting && evidence != nil && !evidence.Ready, nil
	}
	return true, m.store.ResolveAdmissionProposal(ctx, admissionmodel.Decision{
		ProposalID: proposal.ID,
		Outcome:    admissionmodel.ProposalSuperseded,
		DecidedAt:  at,
		Reason:     "dependency_or_candidate_evidence_changed",
		Implicit:   true,
	})
}
