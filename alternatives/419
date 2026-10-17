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
	declarations, _ := dependencyline.Declarations(issue.Description)
	for _, text := range declarations {
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
	return resolveAdmissionDependenciesWithEvidence(ctx, settings, issue, at, nil)
}

func resolveAdmissionDependenciesWithEvidence(ctx context.Context, settings Settings, issue connector.Issue, at time.Time, previous map[string]*runner.AdmissionDependencies) *runner.AdmissionDependencies {
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
	resolved := make(map[string]runner.AdmissionDependency, len(refs))
	for _, ref := range refs {
		resolved[ref] = runner.AdmissionDependency{Identifier: ref, Error: "tracker cannot resolve issue references"}
	}
	for _, snapshot := range previous {
		if snapshot == nil || snapshot.Readiness != rule || !snapshot.ObservedAt.Equal(evidence.ObservedAt) {
			continue
		}
		for _, entry := range snapshot.References {
			if _, requested := resolved[entry.Identifier]; !requested || entry.Error != "" {
				continue
			}
			if canonical, err := dependencyline.CanonicalReference(entry.Identifier, ""); err == nil && canonical == entry.Identifier {
				resolved[entry.Identifier] = entry
			}
		}
	}
	pending := make([]string, 0, len(refs))
	for _, ref := range refs {
		if resolved[ref].Error != "" {
			pending = append(pending, ref)
		}
	}
	fresh := make(map[string]connector.Issue)
	resolutionError := "tracker cannot resolve issue references"
	if resolver, ok := settings.Issues.(connector.IssueReferenceResolver); ok && len(pending) > 0 {
		issues, err := resolver.FetchIssueStatesByIdentifiers(ctx, pending)
		if err != nil {
			resolutionError = "dependency resolution failed: " + redactAdmissionOutput([]byte(err.Error()))
		} else {
			resolutionError = "dependency was not returned by tracker"
			fresh = indexAdmissionDependencies(issues, pending)
		}
	}
	for _, ref := range pending {
		entry := runner.AdmissionDependency{Identifier: ref}
		dependency, found := fresh[ref]
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
			if connector.HumanOwned(dependency) {
				entry.Ready = connector.HumanPrerequisiteReady(dependency)
			}
		}
		resolved[ref] = entry
	}
	for _, ref := range refs {
		entry := resolved[ref]
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
