package cli

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/dependencyline"
	"github.com/digitaldrywood/detent/internal/orchestrator"
)

var (
	doctorDependencyIssueURLPattern = regexp.MustCompile(`https://github\.com/([^/\s]+/[^/\s]+)/(?:issues|pull)/(\d+)`)
	doctorDependencyIssueRefPattern = regexp.MustCompile(`(?:^|[\s(,;:])([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)?#(\d+)\b`)
)

const (
	doctorDependencyAutoUnblockSampleLimit = 5
	doctorBlockedRecoverySampleLimit       = 5
)

type doctorBlockedRecoveryCandidateDiagnostic struct {
	IssueID         string `json:"issue_id,omitempty"`
	IssueIdentifier string `json:"issue_identifier,omitempty"`
	IssueURL        string `json:"issue_url,omitempty"`
	PRNumber        int    `json:"pr_number,omitempty"`
	PRURL           string `json:"pr_url,omitempty"`
	PRHeadSHA       string `json:"pr_head_sha,omitempty"`
	TargetState     string `json:"target_state,omitempty"`
	Reason          string `json:"reason"`
	Detail          string `json:"detail,omitempty"`
}

type doctorDependencyAutoUnblockSettings struct {
	Enabled      bool
	SourceStates []string
	TargetState  string
	Readiness    string
}

type doctorDependencyBlocker struct {
	Ref      connector.BlockedRef
	Issue    connector.Issue
	Resolved bool
}

type doctorDependencyDiagnostic struct {
	Code       string
	Issue      connector.Issue
	References []string
}

func checkDoctorBlockedRecovery(ctx context.Context, id string, cfg workflowconfig.Config, deps doctorDeps) doctorCheck {
	name := "Project " + id + " blocked recovery"
	if !doctorTrackerUsesGitHubReads(cfg.Tracker.Kind) {
		return doctorCheck{
			Name:   name,
			Status: doctorOK,
			Detail: "live blocked recovery diagnostics skipped for " + cfg.Tracker.Kind + " tracker",
		}
	}

	if deps.autoPromoteConnector == nil {
		deps.autoPromoteConnector = defaultDoctorAutoPromoteConnector
	}
	projectConnector, err := deps.autoPromoteConnector(cfg)
	if err != nil {
		return doctorCheck{
			Name:   name,
			Status: doctorFail,
			Detail: fmt.Sprintf("create blocked recovery diagnostic connector: %v", err),
			Hint:   "Fix GitHub tracker credentials and ProjectV2 configuration.",
		}
	}
	if projectConnector == nil {
		return doctorCheck{
			Name:   name,
			Status: doctorFail,
			Detail: "create blocked recovery diagnostic connector: connector is nil",
			Hint:   "Fix GitHub tracker configuration.",
		}
	}

	check := checkDoctorBlockedRecoveryLive(ctx, name, projectConnector)
	if err := closeDoctorAutoPromoteConnector(projectConnector); err != nil && check.Status != doctorFail {
		check.Status = doctorWarn
		check.Detail = check.Detail + "; connector close failed: " + err.Error()
		check.Hint = "Rerun detent doctor and check local network resources."
	}
	return check
}

func checkDoctorBlockedRecoveryLive(
	ctx context.Context,
	name string,
	projectConnector doctorAutoPromoteConnector,
) doctorCheck {
	if verifier, ok := projectConnector.(doctorStatusOptionVerifier); ok {
		if err := verifier.VerifyStatusOptions(ctx, []string{"Blocked", "Rework"}); err != nil {
			return doctorCheck{
				Name:   name,
				Status: doctorFail,
				Detail: fmt.Sprintf("status option verification failed: %v", err),
				Hint:   "Ensure Blocked and Rework resolve through tracker.state_map to existing GitHub Project Status options.",
			}
		}
	}

	issues, err := fetchDoctorBlockedRecoveryIssues(ctx, projectConnector)
	if err != nil {
		return doctorCheck{
			Name:   name,
			Status: doctorFail,
			Detail: fmt.Sprintf("fetch blocked candidates: %v", err),
			Hint:   "Check GitHub Project access, Status field options, and repository pull request access.",
		}
	}

	candidates := []doctorBlockedRecoveryCandidateDiagnostic{}
	for _, issue := range issues {
		decision := orchestrator.EvaluateBlockedRecovery(issue)
		if decision.Action != orchestrator.BlockedRecoveryActionRework {
			continue
		}
		candidates = append(candidates, doctorBlockedRecoveryCandidateDiagnosticFromIssue(issue, decision))
	}

	detail := fmt.Sprintf("sampled %d Blocked candidate(s)", len(issues))
	if len(candidates) == 0 {
		return doctorCheck{
			Name:   name,
			Status: doctorOK,
			Detail: detail + "; no agent-recoverable blocked candidates found",
		}
	}
	detail += "; " + doctorBlockedRecoveryCandidateSummaries(candidates)
	return doctorCheck{
		Name:                      name,
		Status:                    doctorWarn,
		Detail:                    detail,
		Hint:                      "Detent can recover these Blocked issues to Rework because the next action is PR maintenance.",
		BlockedRecoveryCandidates: candidates,
	}
}

func fetchDoctorBlockedRecoveryIssues(
	ctx context.Context,
	projectConnector doctorAutoPromoteConnector,
) ([]connector.Issue, error) {
	if limited, ok := projectConnector.(doctorAutoPromoteLimitedConnector); ok {
		return limited.FetchIssuesByStatesLimit(ctx, []string{"Blocked"}, doctorBlockedRecoverySampleLimit)
	}
	issues, err := projectConnector.FetchIssuesByStates(ctx, []string{"Blocked"})
	if err != nil {
		return nil, err
	}
	if len(issues) > doctorBlockedRecoverySampleLimit {
		issues = issues[:doctorBlockedRecoverySampleLimit]
	}
	return issues, nil
}

func doctorBlockedRecoveryCandidateDiagnosticFromIssue(
	issue connector.Issue,
	decision orchestrator.BlockedRecoveryDecision,
) doctorBlockedRecoveryCandidateDiagnostic {
	diagnostic := doctorBlockedRecoveryCandidateDiagnostic{
		IssueID:         strings.TrimSpace(issue.ID),
		IssueIdentifier: strings.TrimSpace(issue.Identifier),
		IssueURL:        strings.TrimSpace(issue.URL),
		TargetState:     strings.TrimSpace(decision.TargetState),
		Reason:          string(decision.Reason),
		Detail:          strings.TrimSpace(decision.Detail),
	}
	if issue.PullRequest == nil {
		return diagnostic
	}
	pullRequest := issue.PullRequest
	diagnostic.PRNumber = pullRequest.Number
	diagnostic.PRURL = strings.TrimSpace(pullRequest.URL)
	diagnostic.PRHeadSHA = strings.TrimSpace(pullRequest.HeadSHA)
	return diagnostic
}

func doctorBlockedRecoveryCandidateSummaries(candidates []doctorBlockedRecoveryCandidateDiagnostic) string {
	parts := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		summary := "pr_recoverable_blocked: " + doctorBlockedRecoveryIssueLabel(candidate)
		if candidate.PRNumber > 0 {
			summary += fmt.Sprintf(" PR #%d", candidate.PRNumber)
		}
		if candidate.Reason != "" {
			summary += " reason=" + candidate.Reason
		}
		if candidate.TargetState != "" {
			summary += " target=" + candidate.TargetState
		}
		parts = append(parts, summary)
	}
	return strings.Join(parts, "; ")
}

func doctorBlockedRecoveryIssueLabel(candidate doctorBlockedRecoveryCandidateDiagnostic) string {
	id := strings.TrimSpace(candidate.IssueID)
	identifier := strings.TrimSpace(candidate.IssueIdentifier)
	switch {
	case id != "" && identifier != "":
		return id + " (" + identifier + ")"
	case id != "":
		return id
	case identifier != "":
		return identifier
	default:
		return "sampled issue"
	}
}

func checkDoctorDependencyAutoUnblock(ctx context.Context, id string, cfg workflowconfig.Config, deps doctorDeps) doctorCheck {
	name := "Project " + id + " dependency auto-unblock"
	if !doctorTrackerUsesGitHubReads(cfg.Tracker.Kind) {
		return doctorCheck{
			Name:   name,
			Status: doctorOK,
			Detail: "live dependency auto-unblock diagnostics skipped for " + cfg.Tracker.Kind + " tracker",
		}
	}
	if cfg.Tracker.DependencyAutoUnblock.Enabled && !doctorStateInList("Rework", cfg.Tracker.ActiveStates) {
		return doctorCheck{
			Name:   name,
			Status: doctorFail,
			Detail: "tracker.dependency_auto_unblock.enabled=true but tracker.active_states does not include Rework",
			Hint:   "Add Rework to tracker.active_states so started dependency-unblocked issues can resume.",
		}
	}

	if deps.autoPromoteConnector == nil {
		deps.autoPromoteConnector = defaultDoctorAutoPromoteConnector
	}
	projectConnector, err := deps.autoPromoteConnector(cfg)
	if err != nil {
		return doctorCheck{
			Name:   name,
			Status: doctorFail,
			Detail: fmt.Sprintf("create dependency auto-unblock diagnostic connector: %v", err),
			Hint:   "Fix GitHub tracker credentials and ProjectV2 configuration.",
		}
	}
	if projectConnector == nil {
		return doctorCheck{
			Name:   name,
			Status: doctorFail,
			Detail: "create dependency auto-unblock diagnostic connector: connector is nil",
			Hint:   "Fix GitHub tracker configuration.",
		}
	}

	check := checkDoctorDependencyAutoUnblockLive(ctx, name, cfg, projectConnector)
	if err := closeDoctorAutoPromoteConnector(projectConnector); err != nil && check.Status != doctorFail {
		check.Status = doctorWarn
		check.Detail = check.Detail + "; connector close failed: " + err.Error()
		check.Hint = "Rerun detent doctor and check local network resources."
	}
	return check
}

func checkDoctorDependencyAutoUnblockLive(
	ctx context.Context,
	name string,
	cfg workflowconfig.Config,
	projectConnector doctorAutoPromoteConnector,
) doctorCheck {
	dependencyCfg := doctorDependencyAutoUnblockConfig(cfg)
	if verifier, ok := projectConnector.(doctorStatusOptionVerifier); ok {
		states := append([]string(nil), dependencyCfg.SourceStates...)
		if dependencyCfg.Enabled {
			states = append(states, dependencyCfg.TargetState)
			states = append(states, "Rework")
		}
		if len(states) > 0 {
			if err := verifier.VerifyStatusOptions(ctx, states); err != nil {
				return doctorCheck{
					Name:   name,
					Status: doctorFail,
					Detail: fmt.Sprintf("status option verification failed: %v", err),
					Hint:   "Ensure dependency auto-unblock source_states, target_state, and Rework resolve through tracker.state_map to existing GitHub Project Status options.",
				}
			}
		}
	}

	issues, err := fetchDoctorDependencyAutoUnblockIssues(ctx, projectConnector, dependencyCfg.SourceStates)
	if err != nil {
		return doctorCheck{
			Name:   name,
			Status: doctorFail,
			Detail: fmt.Sprintf("fetch dependency waiting candidates: %v", err),
			Hint:   "Check GitHub Project access, Status field options, and repository issue access.",
		}
	}

	diagnostics, err := doctorDependencyDiagnostics(ctx, projectConnector, dependencyCfg, cfg.Tracker.TerminalStates, issues)
	if err != nil {
		return doctorCheck{
			Name:   name,
			Status: doctorFail,
			Detail: fmt.Sprintf("inspect dependency waiting candidates: %v", err),
			Hint:   "Check GitHub issue access and dependency references.",
		}
	}

	detail := doctorDependencyAutoUnblockDetail(dependencyCfg, len(issues), diagnostics)
	if len(diagnostics) == 0 {
		return doctorCheck{
			Name:   name,
			Status: doctorOK,
			Detail: detail,
		}
	}
	return doctorCheck{
		Name:   name,
		Status: doctorWarn,
		Detail: detail,
		Hint:   doctorDependencyAutoUnblockHint(diagnostics),
	}
}

func doctorDependencyAutoUnblockConfig(cfg workflowconfig.Config) doctorDependencyAutoUnblockSettings {
	dependencyCfg := cfg.Tracker.DependencyAutoUnblock
	dependencyCfg.Normalize()
	sourceStates := doctorDependencySourceStates(dependencyCfg.SourceStates)
	targetState := strings.TrimSpace(dependencyCfg.TargetState)
	if targetState == "" {
		targetState = "Todo"
	}
	readiness := strings.ToLower(strings.TrimSpace(dependencyCfg.Readiness))
	if readiness == "" {
		readiness = workflowconfig.DependencyReadinessTerminalOrMerged
	}
	return doctorDependencyAutoUnblockSettings{
		Enabled:      dependencyCfg.Enabled,
		SourceStates: sourceStates,
		TargetState:  targetState,
		Readiness:    readiness,
	}
}

func doctorDependencySourceStates(states []string) []string {
	if len(states) == 0 {
		states = []string{"Blocked"}
	}
	out := make([]string, 0, len(states))
	seen := make(map[string]struct{}, len(states))
	for _, state := range states {
		display := doctorDisplayStateName(state)
		key := strings.ToLower(strings.TrimSpace(display))
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, display)
	}
	return out
}

func doctorDisplayStateName(state string) string {
	state = strings.TrimSpace(state)
	switch strings.ToLower(state) {
	case "blocked":
		return "Blocked"
	case "human review":
		return "Human Review"
	case "merging":
		return "Merging"
	case "rework":
		return "Rework"
	case "todo":
		return "Todo"
	case "in progress":
		return "In Progress"
	default:
		return state
	}
}

func fetchDoctorDependencyAutoUnblockIssues(
	ctx context.Context,
	projectConnector doctorAutoPromoteConnector,
	states []string,
) ([]connector.Issue, error) {
	if limited, ok := projectConnector.(doctorAutoPromoteLimitedConnector); ok {
		return limited.FetchIssuesByStatesLimit(ctx, states, doctorDependencyAutoUnblockSampleLimit)
	}
	issues, err := projectConnector.FetchIssuesByStates(ctx, states)
	if err != nil {
		return nil, err
	}
	if len(issues) > doctorDependencyAutoUnblockSampleLimit {
		issues = issues[:doctorDependencyAutoUnblockSampleLimit]
	}
	return issues, nil
}

func doctorDependencyDiagnostics(
	ctx context.Context,
	projectConnector doctorAutoPromoteConnector,
	cfg doctorDependencyAutoUnblockSettings,
	terminalStates []string,
	issues []connector.Issue,
) ([]doctorDependencyDiagnostic, error) {
	diagnostics := []doctorDependencyDiagnostic{}
	for _, issue := range issues {
		hydrated, ok, err := hydrateDoctorDependencyIssue(ctx, projectConnector, issue, cfg.SourceStates)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}

		references := doctorDependencyReferenceLabels(hydrated)
		if len(references) == 0 {
			continue
		}
		if !cfg.Enabled {
			diagnostics = append(diagnostics, doctorDependencyDiagnostic{
				Code:       "dependency_auto_unblock_disabled",
				Issue:      hydrated,
				References: references,
			})
			continue
		}
		if len(hydrated.BlockedBy) > 0 && len(doctorDependencyTextBlockedRefs(hydrated)) == 0 {
			diagnostics = append(diagnostics, doctorDependencyDiagnostic{
				Code:       "dependency_metadata_missing",
				Issue:      hydrated,
				References: references,
			})
			continue
		}
		if len(hydrated.BlockedBy) == 0 {
			diagnostics = append(diagnostics, doctorDependencyDiagnostic{
				Code:       "dependency_reference_unresolved",
				Issue:      hydrated,
				References: references,
			})
			continue
		}

		blockers, err := resolveDoctorDependencyBlockers(ctx, projectConnector, hydrated)
		if err != nil {
			return nil, err
		}
		if unresolved := unresolvedDoctorDependencyReferences(blockers); len(unresolved) > 0 {
			diagnostics = append(diagnostics, doctorDependencyDiagnostic{
				Code:       "dependency_reference_unresolved",
				Issue:      hydrated,
				References: unresolved,
			})
			continue
		}
		if doctorDependencyBlockersReady(blockers, cfg, terminalStates) {
			diagnostics = append(diagnostics, doctorDependencyDiagnostic{
				Code:       "dependency_ready_but_still_blocked",
				Issue:      hydrated,
				References: doctorDependencyBlockerLabels(blockers),
			})
		}
	}
	return diagnostics, nil
}

func hydrateDoctorDependencyIssue(
	ctx context.Context,
	projectConnector doctorAutoPromoteConnector,
	issue connector.Issue,
	sourceStates []string,
) (connector.Issue, bool, error) {
	issue = doctorIssueWithTextDependencyRefs(issue)
	if strings.TrimSpace(issue.Identifier) == "" {
		return issue, doctorStateInList(issue.State, sourceStates), nil
	}
	resolver, ok := projectConnector.(connector.IssueReferenceResolver)
	if !ok {
		return issue, doctorStateInList(issue.State, sourceStates), nil
	}
	issues, err := resolver.FetchIssueStatesByIdentifiers(ctx, []string{issue.Identifier})
	if err != nil {
		return connector.Issue{}, false, err
	}
	for _, hydrated := range issues {
		if sameDoctorIssueIdentity(issue, hydrated) {
			previousBlockedBy := append([]connector.BlockedRef(nil), issue.BlockedBy...)
			merged := mergeDoctorDependencyIssue(issue, hydrated)
			merged.BlockedBy = mergeDoctorDependencyBlockedRefs(previousBlockedBy, merged.BlockedBy)
			merged = doctorIssueWithTextDependencyRefs(merged)
			return merged, doctorStateInList(merged.State, sourceStates), nil
		}
	}
	return issue, doctorStateInList(issue.State, sourceStates), nil
}

func resolveDoctorDependencyBlockers(
	ctx context.Context,
	projectConnector doctorAutoPromoteConnector,
	issue connector.Issue,
) ([]doctorDependencyBlocker, error) {
	blockers := make([]doctorDependencyBlocker, 0, len(issue.BlockedBy))
	identifiers := make([]string, 0, len(issue.BlockedBy))
	seen := map[string]struct{}{}
	for _, ref := range issue.BlockedBy {
		ref.Identifier = strings.TrimSpace(ref.Identifier)
		ref.ID = strings.TrimSpace(ref.ID)
		ref.State = strings.TrimSpace(ref.State)
		blockers = append(blockers, doctorDependencyBlocker{Ref: ref})
		if ref.Identifier == "" {
			continue
		}
		key := strings.ToLower(ref.Identifier)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		identifiers = append(identifiers, ref.Identifier)
	}

	resolver, ok := projectConnector.(connector.IssueReferenceResolver)
	if !ok || len(identifiers) == 0 {
		return blockers, nil
	}
	issues, err := resolver.FetchIssueStatesByIdentifiers(ctx, identifiers)
	if err != nil {
		return nil, err
	}
	byIdentifier := make(map[string]connector.Issue, len(issues))
	for _, blocker := range issues {
		identifier := strings.ToLower(strings.TrimSpace(blocker.Identifier))
		if identifier != "" {
			byIdentifier[identifier] = blocker
		}
	}
	for index := range blockers {
		identifier := strings.ToLower(strings.TrimSpace(blockers[index].Ref.Identifier))
		blocker, ok := byIdentifier[identifier]
		if !ok {
			continue
		}
		blockers[index].Issue = blocker
		blockers[index].Resolved = true
		blockers[index].Ref.ID = doctorFirstNonBlank(blocker.ID, blockers[index].Ref.ID)
		blockers[index].Ref.Identifier = doctorFirstNonBlank(blocker.Identifier, blockers[index].Ref.Identifier)
		blockers[index].Ref.State = doctorFirstNonBlank(blocker.State, blockers[index].Ref.State)
	}
	return blockers, nil
}

func unresolvedDoctorDependencyReferences(blockers []doctorDependencyBlocker) []string {
	refs := []string{}
	for _, blocker := range blockers {
		if blocker.Resolved {
			continue
		}
		ref := doctorDependencyRefLabel(blocker.Ref)
		if ref != "" {
			refs = append(refs, ref)
		}
	}
	return refs
}

func doctorDependencyBlockersReady(
	blockers []doctorDependencyBlocker,
	cfg doctorDependencyAutoUnblockSettings,
	terminalStates []string,
) bool {
	if len(blockers) == 0 {
		return false
	}
	for _, blocker := range blockers {
		if !doctorDependencyBlockerReady(blocker, cfg, terminalStates) {
			return false
		}
	}
	return true
}

func doctorDependencyBlockerReady(
	blocker doctorDependencyBlocker,
	cfg doctorDependencyAutoUnblockSettings,
	terminalStates []string,
) bool {
	if blocker.Resolved {
		if blocker.Issue.Closed || doctorStateInList(blocker.Issue.State, terminalStates) {
			return true
		}
		if cfg.Readiness == workflowconfig.DependencyReadinessTerminalOrMerged && doctorPullRequestMerged(blocker.Issue.PullRequest) {
			return true
		}
		return false
	}
	if strings.TrimSpace(blocker.Ref.State) == "" {
		return false
	}
	return doctorStateInList(blocker.Ref.State, terminalStates)
}

func doctorPullRequestMerged(pullRequest *connector.PullRequest) bool {
	return pullRequest != nil && strings.EqualFold(strings.TrimSpace(pullRequest.State), "merged")
}

func doctorDependencyAutoUnblockDetail(
	cfg doctorDependencyAutoUnblockSettings,
	sampled int,
	diagnostics []doctorDependencyDiagnostic,
) string {
	status := "tracker.dependency_auto_unblock.enabled=false"
	if cfg.Enabled {
		status = "tracker.dependency_auto_unblock.enabled=true"
	}
	detail := fmt.Sprintf(
		"%s; sampled %d dependency waiting candidate(s) from source_states=%s",
		status,
		sampled,
		strings.Join(cfg.SourceStates, ","),
	)
	if len(diagnostics) == 0 {
		return detail + "; no stalled dependency candidates found"
	}
	parts := make([]string, 0, len(diagnostics))
	for _, diagnostic := range diagnostics {
		parts = append(parts, doctorDependencyDiagnosticDetail(diagnostic))
	}
	return detail + "; " + strings.Join(parts, "; ")
}

func doctorDependencyDiagnosticDetail(diagnostic doctorDependencyDiagnostic) string {
	return fmt.Sprintf(
		"%s: %s references %s",
		diagnostic.Code,
		doctorIssueLabel(diagnostic.Issue),
		strings.Join(diagnostic.References, ", "),
	)
}

func doctorDependencyAutoUnblockHint(diagnostics []doctorDependencyDiagnostic) string {
	codes := map[string]struct{}{}
	for _, diagnostic := range diagnostics {
		codes[diagnostic.Code] = struct{}{}
	}
	hints := []string{}
	if _, ok := codes["dependency_auto_unblock_disabled"]; ok {
		hints = append(hints, "Set tracker.dependency_auto_unblock.enabled: true and ensure source_states include the waiting Status values.")
	}
	if _, ok := codes["dependency_reference_unresolved"]; ok {
		hints = append(hints, "Fix issue content so Depends on: or Blocked by: references point to existing GitHub issues.")
	}
	if _, ok := codes["dependency_metadata_missing"]; ok {
		hints = append(hints, "Add canonical Depends on: or Blocked by: lines to the blocked issue body before leaving dependency-blocked work in Blocked.")
	}
	if _, ok := codes["dependency_ready_but_still_blocked"]; ok {
		hints = append(hints, "Check tracker.dependency_auto_unblock source_states, target_state, readiness, and GitHub Project Status mappings.")
	}
	return strings.Join(hints, " ")
}

func doctorDependencyReferenceLabels(issue connector.Issue) []string {
	refs := doctorBlockedRefLabels(issue.BlockedBy)
	if len(refs) > 0 {
		return refs
	}
	return doctorDependencyLineReferences(issue.Description)
}

func doctorBlockedRefLabels(refs []connector.BlockedRef) []string {
	labels := make([]string, 0, len(refs))
	seen := map[string]struct{}{}
	for _, ref := range refs {
		label := doctorDependencyRefLabel(ref)
		if label == "" {
			continue
		}
		key := strings.ToLower(label)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		labels = append(labels, label)
	}
	return labels
}

func doctorDependencyLineReferences(body string) []string {
	refs := []string{}
	seen := map[string]struct{}{}
	for _, line := range strings.FieldsFunc(body, func(r rune) bool {
		return r == '\n' || r == '\r'
	}) {
		ref, ok := doctorDependencyLineReference(line)
		if !ok {
			continue
		}
		key := strings.ToLower(ref)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		refs = append(refs, ref)
	}
	return refs
}

func doctorIssueWithTextDependencyRefs(issue connector.Issue) connector.Issue {
	issue.BlockedBy = mergeDoctorDependencyBlockedRefs(issue.BlockedBy, doctorDependencyTextBlockedRefs(issue))
	issue.BlockedBy = doctorDependencyBlockedRefsWithoutSelf(issue.BlockedBy, issue.Identifier)
	return issue
}

func doctorDependencyTextBlockedRefs(issue connector.Issue) []connector.BlockedRef {
	repo := doctorDependencyIssueRepo(issue.Identifier)
	refs := []connector.BlockedRef{}
	seen := map[string]struct{}{}
	appendIdentifier := func(identifier string) {
		identifier = strings.TrimSpace(identifier)
		if identifier == "" {
			return
		}
		key := strings.ToLower(identifier)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		refs = append(refs, connector.BlockedRef{Identifier: identifier})
	}
	for _, lineRef := range doctorDependencyLineReferences(issue.Description) {
		identifiers := doctorDependencyIssueIdentifiersInText(lineRef, repo)
		if len(identifiers) == 0 {
			appendIdentifier(lineRef)
			continue
		}
		for _, identifier := range identifiers {
			appendIdentifier(identifier)
		}
	}
	return refs
}

func mergeDoctorDependencyBlockedRefs(existing []connector.BlockedRef, incoming []connector.BlockedRef) []connector.BlockedRef {
	if len(existing) == 0 && len(incoming) == 0 {
		return nil
	}
	merged := make([]connector.BlockedRef, 0, len(existing)+len(incoming))
	seen := map[string]struct{}{}
	appendRefs := func(refs []connector.BlockedRef) {
		for _, ref := range refs {
			key := strings.ToLower(strings.TrimSpace(ref.Identifier))
			if key == "" {
				key = strings.ToLower(strings.TrimSpace(ref.ID))
			}
			if key == "" {
				continue
			}
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			merged = append(merged, ref)
		}
	}
	appendRefs(existing)
	appendRefs(incoming)
	return merged
}

func doctorDependencyBlockedRefsWithoutSelf(refs []connector.BlockedRef, identifier string) []connector.BlockedRef {
	self := strings.ToLower(strings.TrimSpace(identifier))
	if self == "" || len(refs) == 0 {
		return refs
	}
	filtered := refs[:0]
	for _, ref := range refs {
		if strings.ToLower(strings.TrimSpace(ref.Identifier)) == self {
			continue
		}
		filtered = append(filtered, ref)
	}
	if len(filtered) == 0 {
		return nil
	}
	return filtered
}

func doctorDependencyIssueIdentifiersInText(text string, repo string) []string {
	refs := []string{}
	seen := map[string]struct{}{}
	appendIdentifier := func(refRepo string, number string) {
		identifier := doctorDependencyBlockerIdentifier(refRepo, number, repo)
		if identifier == "" {
			return
		}
		key := strings.ToLower(identifier)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		refs = append(refs, identifier)
	}
	for _, matches := range doctorDependencyIssueURLPattern.FindAllStringSubmatch(text, -1) {
		if len(matches) == 3 {
			appendIdentifier(matches[1], matches[2])
		}
	}
	for _, matches := range doctorDependencyIssueRefPattern.FindAllStringSubmatch(text, -1) {
		if len(matches) == 3 {
			appendIdentifier(matches[1], matches[2])
		}
	}
	return refs
}

func doctorDependencyIssueRepo(identifier string) string {
	identifier = strings.TrimSpace(identifier)
	index := strings.LastIndex(identifier, "#")
	if index <= 0 {
		return ""
	}
	return strings.TrimSpace(identifier[:index])
}

func doctorDependencyBlockerIdentifier(refRepo string, number string, repo string) string {
	if strings.TrimSpace(number) == "" {
		return ""
	}
	refRepo = strings.TrimSpace(refRepo)
	if refRepo == "" {
		if repo == "" {
			return "#" + strings.TrimSpace(number)
		}
		refRepo = repo
	}
	return refRepo + "#" + strings.TrimSpace(number)
}

func doctorDependencyLineReference(line string) (string, bool) {
	return dependencyline.Match(line)
}

func doctorDependencyBlockerLabels(blockers []doctorDependencyBlocker) []string {
	labels := make([]string, 0, len(blockers))
	for _, blocker := range blockers {
		if blocker.Resolved {
			if identifier := strings.TrimSpace(blocker.Issue.Identifier); identifier != "" {
				labels = append(labels, identifier)
				continue
			}
			if id := strings.TrimSpace(blocker.Issue.ID); id != "" {
				labels = append(labels, id)
				continue
			}
		}
		if label := doctorDependencyRefLabel(blocker.Ref); label != "" {
			labels = append(labels, label)
		}
	}
	return labels
}

func doctorDependencyRefLabel(ref connector.BlockedRef) string {
	if identifier := strings.TrimSpace(ref.Identifier); identifier != "" {
		return identifier
	}
	return strings.TrimSpace(ref.ID)
}

func sameDoctorIssueIdentity(left connector.Issue, right connector.Issue) bool {
	leftID := strings.TrimSpace(left.ID)
	rightID := strings.TrimSpace(right.ID)
	if leftID != "" && rightID != "" && leftID == rightID {
		return true
	}
	leftIdentifier := strings.ToLower(strings.TrimSpace(left.Identifier))
	rightIdentifier := strings.ToLower(strings.TrimSpace(right.Identifier))
	return leftIdentifier != "" && leftIdentifier == rightIdentifier
}

func mergeDoctorDependencyIssue(left connector.Issue, right connector.Issue) connector.Issue {
	merged := left
	if strings.TrimSpace(right.ID) != "" {
		merged.ID = right.ID
	}
	if strings.TrimSpace(right.Identifier) != "" {
		merged.Identifier = right.Identifier
	}
	if strings.TrimSpace(right.Title) != "" {
		merged.Title = right.Title
	}
	if strings.TrimSpace(right.Description) != "" {
		merged.Description = right.Description
	}
	if strings.TrimSpace(right.State) != "" {
		merged.State = right.State
	}
	if strings.TrimSpace(right.URL) != "" {
		merged.URL = right.URL
	}
	if len(right.BlockedBy) > 0 {
		merged.BlockedBy = right.BlockedBy
	}
	if strings.TrimSpace(right.BlockerReason) != "" {
		merged.BlockerReason = right.BlockerReason
	}
	return merged
}

func doctorFirstNonBlank(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func doctorLinkedPullRequestNumber(issue connector.Issue) (int, bool) {
	if issue.PRNumber == nil || *issue.PRNumber <= 0 {
		return 0, false
	}
	return *issue.PRNumber, true
}

func doctorIssueLabel(issue connector.Issue) string {
	id := strings.TrimSpace(issue.ID)
	identifier := strings.TrimSpace(issue.Identifier)
	switch {
	case id != "" && identifier != "":
		return id + " (" + identifier + ")"
	case id != "":
		return id
	case identifier != "":
		return identifier
	default:
		return "sampled issue"
	}
}

func doctorStateInList(state string, states []string) bool {
	state = strings.ToLower(strings.TrimSpace(state))
	if state == "" {
		return false
	}
	for _, candidate := range states {
		if strings.ToLower(strings.TrimSpace(candidate)) == state {
			return true
		}
	}
	return false
}
