package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/backendcapacity"
	"github.com/digitaldrywood/detent/internal/claudecode"
	"github.com/digitaldrywood/detent/internal/codex"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/dependencyline"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/workpad"
)

var (
	doctorDependencyIssueURLPattern = regexp.MustCompile(`https://github\.com/([^/\s]+/[^/\s]+)/(?:issues|pull)/(\d+)`)
	doctorDependencyIssueRefPattern = regexp.MustCompile(`(?:^|[\s(,;:])([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)?#(\d+)\b`)
)

const (
	doctorDependencyAutoUnblockSampleLimit = 5
)

type doctorBlockedRecoveryCandidateDiagnostic struct {
	IssueID             string `json:"issue_id,omitempty"`
	IssueIdentifier     string `json:"issue_identifier,omitempty"`
	IssueURL            string `json:"issue_url,omitempty"`
	PRNumber            int    `json:"pr_number,omitempty"`
	PRURL               string `json:"pr_url,omitempty"`
	PRHeadSHA           string `json:"pr_head_sha,omitempty"`
	TargetState         string `json:"target_state,omitempty"`
	Action              string `json:"action,omitempty"`
	Reason              string `json:"reason"`
	Detail              string `json:"detail,omitempty"`
	Remedy              string `json:"remedy,omitempty"`
	NeedsHumanAttention bool   `json:"needs_human_attention,omitempty"`
}

type doctorDependencyAutoUnblockSettings struct {
	Enabled      bool
	Source       string
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

func checkDoctorBlockedRecovery(ctx context.Context, id string, cfg workflowconfig.Config, storePath string, deps doctorDeps) doctorCheck {
	name := "Project " + id + " blocked recovery"
	if !doctorTrackerUsesGitHubReads(cfg.Tracker.Kind) {
		return doctorCheck{
			Name:   name,
			Status: doctorOK,
			Detail: "live blocked recovery diagnostics skipped for " + cfg.Tracker.Kind + " tracker",
		}
	}

	deps = deps.withDefaults()
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

	var timelineStore doctorTelemetryStore
	timelineDetail := ""
	if strings.TrimSpace(storePath) != "" {
		timelineStore, err = deps.openSQLiteReadOnly(ctx, storePath)
		if err != nil {
			timelineDetail = "durable recovery metadata unavailable: " + err.Error()
		}
	}

	check := checkDoctorBlockedRecoveryLive(ctx, name, projectConnector, cfg, time.Now(), timelineStore)
	if timelineDetail != "" {
		check.Detail += "; " + timelineDetail
	}
	if timelineStore != nil {
		if err := timelineStore.Close(); err != nil && check.Status != doctorFail {
			check.Status = doctorWarn
			check.Detail += "; recovery metadata store close failed: " + err.Error()
		}
	}
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
	cfg workflowconfig.Config,
	now time.Time,
	timelineStore doctorTelemetryStore,
) doctorCheck {
	recoveryConfig := orchestrator.BlockedRecoveryConfig{
		Enabled:      cfg.Tracker.BlockedRecovery.Enabled,
		SourceStates: append([]string(nil), cfg.Tracker.BlockedRecovery.SourceStates...),
		TargetState:  cfg.Tracker.BlockedRecovery.TargetState,
		ReasonCodes:  append([]string(nil), cfg.Tracker.BlockedRecovery.ReasonCodes...),
	}
	sourceStates := recoveryConfig.SourceStates
	if len(sourceStates) == 0 {
		sourceStates = []string{"Blocked"}
	}
	if !doctorStateInList("Blocked", sourceStates) {
		sourceStates = append([]string{"Blocked"}, sourceStates...)
	}
	targetState := strings.TrimSpace(recoveryConfig.TargetState)
	if targetState == "" {
		targetState = "Rework"
	}
	statusStates := append(append([]string(nil), sourceStates...), targetState)
	if verifier, ok := projectConnector.(doctorStatusOptionVerifier); ok {
		if err := verifier.VerifyStatusOptions(ctx, statusStates); err != nil {
			return doctorCheck{
				Name:   name,
				Status: doctorFail,
				Detail: fmt.Sprintf("status option verification failed: %v", err),
				Hint:   fmt.Sprintf("Ensure %s resolve through tracker.state_map to existing GitHub Project Status options.", strings.Join(statusStates, " and ")),
			}
		}
	}

	issues, err := fetchDoctorBlockedRecoveryIssues(ctx, projectConnector, sourceStates)
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
		decision := orchestrator.EvaluateBlockedRecoveryWithConfig(issue, recoveryConfig)
		if decision.Action != orchestrator.BlockedRecoveryActionRework {
			continue
		}
		candidates = append(candidates, doctorBlockedRecoveryCandidateDiagnosticFromIssue(issue, decision))
	}
	held := []doctorBlockedRecoveryCandidateDiagnostic{}
	heldIssues := map[string]struct{}{}
	for _, issue := range issues {
		reason := orchestrator.BlockedRecoveryHumanHoldReason(issue, cfg.Agent.AutoPromote.OptoutLabel)
		if reason == "" {
			continue
		}
		detail := "runtime recovery requires human attention"
		if issue.WorkpadSignal != nil && issue.WorkpadSignal.Invalid != nil {
			detail = strings.TrimSpace(issue.WorkpadSignal.Invalid.Message)
		}
		held = append(held, doctorBlockedRecoveryCandidateDiagnostic{
			IssueID:             strings.TrimSpace(issue.ID),
			IssueIdentifier:     strings.TrimSpace(issue.Identifier),
			IssueURL:            strings.TrimSpace(issue.URL),
			Action:              "hold",
			Reason:              reason,
			Detail:              detail,
			Remedy:              orchestrator.BlockedRecoveryOperatorRemedy(issue, reason),
			NeedsHumanAttention: true,
		})
		heldIssues[doctorBlockedRecoveryIdentityKey(issue)] = struct{}{}
	}
	capacity := doctorBlockedCapacityDiagnostics(ctx, projectConnector, cfg, issues, now)
	capacityIssues := doctorParkedCapacityIssueSet(capacity)
	withoutRecovery := []doctorBlockedRecoveryCandidateDiagnostic{}
	for _, issue := range issues {
		if _, isHeld := heldIssues[doctorBlockedRecoveryIdentityKey(issue)]; isHeld {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(issue.State), "Blocked") ||
			doctorBlockedIssueHasRecoveryPredicate(ctx, issue, recoveryConfig, timelineStore, capacityIssues) {
			continue
		}
		withoutRecovery = append(withoutRecovery, doctorBlockedRecoveryCandidateDiagnostic{
			IssueID:         strings.TrimSpace(issue.ID),
			IssueIdentifier: strings.TrimSpace(issue.Identifier),
			IssueURL:        strings.TrimSpace(issue.URL),
			Reason:          "no_recovery_predicate",
			Detail:          "Blocked issue has no dependency relation, human owner, or durable recovery predicate",
		})
	}

	detail := fmt.Sprintf("scanned %d blocked-recovery source candidate(s)", len(issues))
	if len(candidates) == 0 && len(held) == 0 && len(capacity) == 0 && len(withoutRecovery) == 0 {
		return doctorCheck{
			Name:   name,
			Status: doctorOK,
			Detail: detail + "; no structured blocked-recovery condition matches found",
		}
	}
	if len(candidates) > 0 {
		detail += "; " + doctorBlockedRecoveryCandidateSummaries(candidates)
	}
	if len(held) > 0 {
		detail += fmt.Sprintf(
			"; %d permanently-held blocked recovery issue(s) need human attention: %s",
			len(held),
			strings.Join(doctorBlockedRecoveryIssueLabels(held), ", "),
		)
	}
	if len(capacity) > 0 {
		detail += fmt.Sprintf(
			"; %d issue(s) parked by provider capacity exhaustion: %s",
			len(capacity),
			strings.Join(doctorParkedCapacityIssueLabels(capacity), ", "),
		)
	}
	if len(withoutRecovery) > 0 {
		detail += fmt.Sprintf(
			"; %d relation-less Blocked issue(s) have no recovery predicate: %s",
			len(withoutRecovery),
			strings.Join(doctorBlockedRecoveryIssueLabels(withoutRecovery), ", "),
		)
	}
	hint := fmt.Sprintf("PR-condition matches are diagnostic only; runtime still requires the latest durable source-lane entry reason before recovery to %s. Detent separately recovers quota-parked issues after provider capacity returns.", targetState)
	if len(held) > 0 {
		hint += " Apply the remedy reported for each permanently-held recovery."
	}
	if len(withoutRecovery) > 0 {
		hint += " Add a native dependency, structured human_action, or durable owner and recovery predicate before parking the issue in Blocked."
	}
	return doctorCheck{
		Name:                      name,
		Status:                    doctorWarn,
		Detail:                    detail,
		Hint:                      hint,
		BlockedRecoveryCandidates: candidates,
		PermanentlyHeldRecoveries: held,
		BlockedWithoutRecovery:    withoutRecovery,
		BackendCapacity:           capacity,
	}
}

func doctorBlockedRecoveryIdentityKey(issue connector.Issue) string {
	return strings.ToLower(strings.TrimSpace(doctorFirstNonBlank(issue.ID, issue.Identifier, issue.URL)))
}

func doctorParkedCapacityIssueLabels(diagnostics []doctorBackendCapacityDiagnostic) []string {
	labels := []string{}
	for _, diagnostic := range diagnostics {
		for _, issue := range diagnostic.ParkedIssues {
			if !slices.Contains(labels, issue) {
				labels = append(labels, issue)
			}
		}
	}
	return labels
}

func doctorParkedCapacityIssueSet(diagnostics []doctorBackendCapacityDiagnostic) map[string]struct{} {
	labels := doctorParkedCapacityIssueLabels(diagnostics)
	set := make(map[string]struct{}, len(labels))
	for _, label := range labels {
		set[label] = struct{}{}
	}
	return set
}

func doctorBlockedIssueHasRecoveryPredicate(
	ctx context.Context,
	issue connector.Issue,
	cfg orchestrator.BlockedRecoveryConfig,
	timelineStore doctorTelemetryStore,
	capacityIssues map[string]struct{},
) bool {
	decision := orchestrator.EvaluateBlockedRecovery(issue)
	if decision.Reason == orchestrator.BlockedRecoveryReasonHumanBlocker ||
		decision.Reason == orchestrator.BlockedRecoveryReasonDependencyBlocker {
		return true
	}
	if signal := issue.WorkpadSignal; signal != nil {
		if strings.TrimSpace(signal.HumanAction) != "" {
			return true
		}
		if signal.Invalid == nil && signal.Source == workpad.SourceStructured && len(signal.Blockers) > 0 {
			return true
		}
		if cfg.Enabled &&
			signal.Invalid == nil &&
			signal.Source == workpad.SourceStructured &&
			strings.EqualFold(strings.TrimSpace(signal.Status), workpad.StatusBlocked) &&
			doctorBlockedRecoveryReasonAllowed(signal.ReasonCode, cfg.ReasonCodes) {
			return true
		}
	}
	if _, ok := capacityIssues[doctorIssueLabel(issue)]; ok {
		return true
	}
	return doctorBlockedRecoveryTimelinePredicate(ctx, timelineStore, issue)
}

func doctorBlockedRecoveryReasonAllowed(reason string, allowed []string) bool {
	normalize := func(value string) string {
		value = strings.ToLower(strings.TrimSpace(value))
		value = strings.NewReplacer("-", "_", " ", "_").Replace(value)
		if value == "merge_conflicts" {
			return "merge_conflict"
		}
		return value
	}
	reason = normalize(reason)
	if reason == "" {
		return false
	}
	if len(allowed) == 0 {
		allowed = []string{"merge_conflict", "stale_base", "missing_current_head_ci"}
	}
	for _, candidate := range allowed {
		if normalize(candidate) == reason {
			return true
		}
	}
	return false
}

func doctorBlockedRecoveryTimelinePredicate(
	ctx context.Context,
	timelineStore doctorTelemetryStore,
	issue connector.Issue,
) bool {
	if timelineStore == nil {
		return false
	}
	var phaseName string
	var enteredAtRaw string
	var metadataJSON string
	err := timelineStore.QueryRowContext(ctx, `
SELECT phase_name, started_at, metadata_json
FROM workflow_phase_events
WHERE (issue_id = ? OR identifier = ? OR issue_url = ?)
  AND phase_type = 'lane'
  AND status = 'entered'
ORDER BY started_at DESC, id DESC
LIMIT 1`,
		doctorBlockedRecoveryIdentity(issue.ID),
		doctorBlockedRecoveryIdentity(issue.Identifier),
		doctorBlockedRecoveryIdentity(issue.URL),
	).Scan(&phaseName, &enteredAtRaw, &metadataJSON)
	if err != nil {
		return false
	}
	enteredAt, err := time.Parse(time.RFC3339Nano, enteredAtRaw)
	if err != nil {
		return false
	}
	return orchestrator.BlockedIssueHasCurrentRecoveryPredicate(issue, phaseName, enteredAt, metadataJSON)
}

func doctorBlockedRecoveryIdentity(value string) sql.NullString {
	value = strings.TrimSpace(value)
	return sql.NullString{String: value, Valid: value != ""}
}

func doctorBlockedRecoveryIssueLabels(diagnostics []doctorBlockedRecoveryCandidateDiagnostic) []string {
	labels := make([]string, 0, len(diagnostics))
	for _, diagnostic := range diagnostics {
		labels = append(labels, doctorBlockedRecoveryIssueLabel(diagnostic))
	}
	return labels
}

func doctorBlockedCapacityDiagnostics(
	ctx context.Context,
	projectConnector doctorAutoPromoteConnector,
	cfg workflowconfig.Config,
	issues []connector.Issue,
	now time.Time,
) []doctorBackendCapacityDiagnostic {
	diagnostics := []doctorBackendCapacityDiagnostic{}
	var reader connector.IssueCommentReader
	if candidate, ok := projectConnector.(connector.IssueCommentReader); ok {
		reader = candidate
	}
	for _, issue := range issues {
		comments := issue.Comments
		if len(comments) == 0 && reader != nil {
			fetched, err := reader.FetchIssueComments(ctx, issue)
			if err != nil {
				continue
			}
			comments = fetched
		}
		for index := len(comments) - 1; index >= 0; index-- {
			if !orchestrator.IsLegacyFailureBreakerComment(comments[index].Body) {
				continue
			}
			backend, details, ok := doctorClassifyCapacityComment(cfg, comments[index].Body, now)
			if !ok {
				continue
			}
			detectedAt := time.Time{}
			if comments[index].CreatedAt != nil {
				detectedAt = comments[index].CreatedAt.UTC()
			}
			diagnostic := doctorBackendCapacityDiagnostic{
				BackendID:      backend.ID,
				BackendKind:    backend.Kind,
				Provider:       doctorBackendProvider(backend),
				DetectedAt:     detectedAt,
				LastObservedAt: detectedAt,
				Active:         details.ResetAt == nil || details.ResetAt.After(now),
				ParkedIssues:   []string{doctorIssueLabel(issue)},
			}
			if details.ResetAt != nil {
				resetAt := details.ResetAt.UTC()
				diagnostic.ResetAt = &resetAt
				diagnostic.ResumeAt = resetAt
			}
			diagnostics = append(diagnostics, diagnostic)
			break
		}
	}
	return diagnostics
}

func doctorClassifyCapacityComment(
	cfg workflowconfig.Config,
	body string,
	now time.Time,
) (workflowconfig.AgentBackend, backendcapacity.Details, bool) {
	err := errors.New(strings.TrimSpace(body))
	for _, backend := range cfg.AgentBackendConfigs() {
		scope := backendcapacity.Scope{
			BackendID:   backend.ID,
			BackendKind: backend.Kind,
			Provider:    doctorBackendProvider(backend),
		}
		if !scope.Hosted() {
			continue
		}
		var details backendcapacity.Details
		var ok bool
		switch backend.Kind {
		case workflowconfig.AgentBackendCodex:
			details, ok = codex.ClassifyCapacityError(err, nil, now)
		case workflowconfig.AgentBackendClaudeCode:
			details, ok = claudecode.ClassifyCapacityError(err, nil, now)
		}
		if ok {
			return backend, details, true
		}
	}
	return workflowconfig.AgentBackend{}, backendcapacity.Details{}, false
}

func doctorBackendProvider(backend workflowconfig.AgentBackend) string {
	provider := strings.TrimSpace(backend.Provider)
	if provider == "" && backend.Kind == workflowconfig.AgentBackendCodex {
		provider = strings.TrimSpace(backend.CodexOptions().ModelProvider)
	}
	return provider
}

func fetchDoctorBlockedRecoveryIssues(
	ctx context.Context,
	projectConnector doctorAutoPromoteConnector,
	sourceStates []string,
) ([]connector.Issue, error) {
	return projectConnector.FetchIssuesByStates(ctx, sourceStates)
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
		summary := "pr_condition_match_pending_timeline_authorization: " + doctorBlockedRecoveryIssueLabel(candidate)
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

	capabilities := doctorDependencyCapabilities(projectConnector)
	detail := doctorDependencyAutoUnblockDetail(dependencyCfg, len(issues), diagnostics, capabilities)
	if len(diagnostics) == 0 {
		return doctorCheck{
			Name:                   name,
			Status:                 doctorOK,
			Detail:                 detail,
			DependencyCapabilities: capabilities,
		}
	}
	return doctorCheck{
		Name:                   name,
		Status:                 doctorWarn,
		Detail:                 detail,
		Hint:                   doctorDependencyAutoUnblockHint(diagnostics),
		DependencyCapabilities: capabilities,
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
		Source:       doctorDependencySource(cfg.Dependencies.Source),
		SourceStates: sourceStates,
		TargetState:  targetState,
		Readiness:    readiness,
	}
}

func doctorDependencySource(source string) string {
	switch strings.ToLower(strings.TrimSpace(source)) {
	case workflowconfig.DependencySourceNativeOnly:
		return workflowconfig.DependencySourceNativeOnly
	default:
		return workflowconfig.DependencySourceMerged
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
		hydrated, ok, err := hydrateDoctorDependencyIssue(ctx, projectConnector, issue, cfg)
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
		if doctorDependencyRefsProseOnly(hydrated.BlockedBy) {
			diagnostics = append(diagnostics, doctorDependencyDiagnostic{
				Code:       "dependency_prose_only",
				Issue:      hydrated,
				References: references,
			})
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
			code := "dependency_ready_but_still_blocked"
			if doctorDependencyBlockersTerminal(blockers, terminalStates) {
				code = "dependency_terminal_but_still_blocked"
			}
			diagnostics = append(diagnostics, doctorDependencyDiagnostic{
				Code:       code,
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
	cfg doctorDependencyAutoUnblockSettings,
) (connector.Issue, bool, error) {
	issue = doctorIssueWithDependencyRefs(issue, cfg.Source)
	if strings.TrimSpace(issue.Identifier) == "" {
		return issue, doctorStateInList(issue.State, cfg.SourceStates), nil
	}
	resolver, ok := projectConnector.(connector.IssueReferenceResolver)
	if !ok {
		return issue, doctorStateInList(issue.State, cfg.SourceStates), nil
	}
	issues, err := resolver.FetchIssueStatesByIdentifiers(ctx, []string{issue.Identifier})
	if err != nil {
		return connector.Issue{}, false, err
	}
	for _, hydrated := range issues {
		if sameDoctorIssueIdentity(issue, hydrated) {
			previousBlockedBy := append([]connector.BlockedRef(nil), issue.BlockedBy...)
			merged := mergeDoctorDependencyIssue(issue, hydrated)
			merged.BlockedBy = mergeDoctorDependencyBlockedRefs(merged.BlockedBy, previousBlockedBy)
			merged = doctorIssueWithDependencyRefs(merged, cfg.Source)
			return merged, doctorStateInList(merged.State, cfg.SourceStates), nil
		}
	}
	return issue, doctorStateInList(issue.State, cfg.SourceStates), nil
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

func doctorDependencyBlockersTerminal(blockers []doctorDependencyBlocker, terminalStates []string) bool {
	if len(blockers) == 0 {
		return false
	}
	for _, blocker := range blockers {
		if blocker.Resolved {
			if !blocker.Issue.Closed && !doctorStateInList(blocker.Issue.State, terminalStates) {
				return false
			}
			continue
		}
		if !doctorStateInList(blocker.Ref.State, terminalStates) {
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
	capabilities []connector.DependencyCapability,
) string {
	status := "tracker.dependency_auto_unblock.enabled=false"
	if cfg.Enabled {
		status = "tracker.dependency_auto_unblock.enabled=true"
	}
	detail := fmt.Sprintf(
		"%s; dependencies.source=%s; sampled %d dependency waiting candidate(s) from source_states=%s",
		status,
		cfg.Source,
		sampled,
		strings.Join(cfg.SourceStates, ","),
	)
	if capabilityDetail := doctorDependencyCapabilityDetail(capabilities); capabilityDetail != "" {
		detail += "; " + capabilityDetail
	}
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
	if _, ok := codes["dependency_prose_only"]; ok {
		hints = append(hints, "Create native GitHub issue dependencies for prose-only blockers; keep Depends on: or Blocked by: body lines only as a compatibility fallback during migration.")
	}
	if _, ok := codes["dependency_ready_but_still_blocked"]; ok {
		hints = append(hints, "Check tracker.dependency_auto_unblock source_states, target_state, readiness, and GitHub Project Status mappings.")
	}
	if _, ok := codes["dependency_terminal_but_still_blocked"]; ok {
		hints = append(hints, "All dependency blockers are terminal but the issue has not transitioned; inspect dependency auto-unblock decision logs for a consumed-signature latch.")
	}
	return strings.Join(hints, " ")
}

func doctorDependencyCapabilities(projectConnector doctorAutoPromoteConnector) []connector.DependencyCapability {
	reporter, ok := projectConnector.(connector.DependencyCapabilityReporter)
	if !ok {
		return nil
	}
	capabilities := reporter.DependencyCapabilities()
	if len(capabilities) == 0 {
		return nil
	}
	return append([]connector.DependencyCapability(nil), capabilities...)
}

func doctorDependencyCapabilityDetail(capabilities []connector.DependencyCapability) string {
	if len(capabilities) == 0 {
		return ""
	}
	parts := make([]string, 0, len(capabilities))
	for _, capability := range capabilities {
		part := "dependency_capability"
		if repository := strings.TrimSpace(capability.Repository); repository != "" {
			part += " repository=" + repository
		}
		if status := strings.TrimSpace(capability.NativeBlockedBy); status != "" {
			part += " native_blocked_by=" + status
		}
		if source := strings.TrimSpace(capability.Source); source != "" {
			part += " source=" + source
		}
		if detail := strings.TrimSpace(capability.Detail); detail != "" {
			part += " detail=" + detail
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, "; ")
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

func doctorIssueWithDependencyRefs(issue connector.Issue, source string) connector.Issue {
	if doctorDependencySource(source) == workflowconfig.DependencySourceNativeOnly {
		issue.BlockedBy = doctorDependencyBlockedRefsWithoutSelf(issue.BlockedBy, issue.Identifier)
		return issue
	}
	return doctorIssueWithTextDependencyRefs(issue)
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
		refs = append(refs, connector.BlockedRef{Identifier: identifier, Source: connector.BlockedRefSourceProse})
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
			if strings.TrimSpace(ref.Source) == "" {
				ref.Source = connector.BlockedRefSourceProse
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

func doctorDependencyRefsProseOnly(refs []connector.BlockedRef) bool {
	if len(refs) == 0 {
		return false
	}
	for _, ref := range refs {
		if doctorDependencyRefSource(ref) != connector.BlockedRefSourceProse {
			return false
		}
	}
	return true
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
		source := doctorDependencyRefSource(blocker.Ref)
		if blocker.Resolved {
			if identifier := strings.TrimSpace(blocker.Issue.Identifier); identifier != "" {
				labels = append(labels, identifier+" source="+source)
				continue
			}
			if id := strings.TrimSpace(blocker.Issue.ID); id != "" {
				labels = append(labels, id+" source="+source)
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
	label := doctorDependencyRefBaseLabel(ref)
	if label == "" {
		return ""
	}
	return label + " source=" + doctorDependencyRefSource(ref)
}

func doctorDependencyRefBaseLabel(ref connector.BlockedRef) string {
	if identifier := strings.TrimSpace(ref.Identifier); identifier != "" {
		return identifier
	}
	return strings.TrimSpace(ref.ID)
}

func doctorDependencyRefSource(ref connector.BlockedRef) string {
	switch strings.TrimSpace(ref.Source) {
	case connector.BlockedRefSourceNative:
		return connector.BlockedRefSourceNative
	case connector.BlockedRefSourceProse:
		return connector.BlockedRefSourceProse
	default:
		return connector.BlockedRefSourceProse
	}
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
