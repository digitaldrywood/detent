package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/forgeavailability"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/workspace"
)

const forgeUnavailableErrorClass = forgeavailability.Condition

// Workspace creation can fail before a worker turn when its Git read cannot
// reach the forge. Keep that failure in the existing forge retry path.
func classifyWorkspaceForgeReadFailure(err error, fallbackHost string) error {
	if err == nil || !errors.Is(err, runpkg.ErrWorkspacePreparation) {
		return err
	}
	detail := err.Error()
	var commandErr *workspace.CommandError
	operation := ""
	if errors.As(err, &commandErr) && commandErr != nil && commandErr.Command == "git" {
		for _, arg := range commandErr.Args {
			if arg == "ls-remote" || arg == "fetch" {
				operation = "git " + arg
				break
			}
		}
		detail += "\n" + commandErr.Output
	}
	if operation == "" {
		lower := strings.ToLower(detail)
		switch {
		case strings.Contains(lower, "ls-remote"):
			operation = "git ls-remote"
		case strings.Contains(lower, "git fetch"):
			operation = "git fetch"
		}
	}
	host := forgeavailability.HostFromText(detail)
	if operation == "" || host == "" {
		return err
	}
	if configured := forgeavailability.NormalizeHost(fallbackHost); configured != "" && host != configured {
		return err
	}
	if forgeavailability.GitHubCLIAuthFailure("git fetch", detail) {
		return err
	}
	class, unavailable := forgeavailability.Classify("git fetch", detail)
	if strings.Contains(strings.ToLower(detail), "permission denied (publickey)") || strings.Contains(strings.ToLower(detail), "authentication failed") {
		class, unavailable = forgeavailability.ClassTransport, true
	}
	if !unavailable {
		return err
	}
	return forgeavailability.NewError(forgeavailability.Scope{Host: host, Operation: operation}, class, err)
}

type ForgeCondition = telemetry.ForgeCondition

type forgeWaitMetadata struct {
	Host              string    `json:"host"`
	Operation         string    `json:"operation"`
	Branch            string    `json:"branch,omitempty"`
	WorkProductPushed bool      `json:"work_product_pushed,omitempty"`
	ErrorClass        string    `json:"error_class"`
	DetectedAt        time.Time `json:"detected_at"`
	NextProbeAt       time.Time `json:"next_probe_at"`
}

func (o *Orchestrator) registerForgeUnavailable(state *State, availabilityErr *forgeavailability.Error, running Running, observedAt time.Time) ForgeCondition {
	if state == nil || availabilityErr == nil {
		return ForgeCondition{}
	}
	if observedAt.IsZero() {
		observedAt = o.clockNow()
	}
	observedAt = observedAt.UTC()
	if state.ForgeUnavailable == nil {
		state.ForgeUnavailable = map[string]ForgeCondition{}
	}
	scope := availabilityErr.Scope.Normalize()
	if scope.Host == "" {
		scope.Host = forgeavailability.NormalizeHost(o.cfg.ForgeHost)
	}
	key := scope.Key()
	condition, exists := state.ForgeUnavailable[key]
	if !exists {
		condition = ForgeCondition{
			ProjectID:  strings.TrimSpace(o.cfg.Project.ID),
			Host:       scope.Host,
			DetectedAt: observedAt,
		}
	}
	condition.Operation = scope.Operation
	condition.ErrorClass = mergedForgeAvailabilityClass(condition.ErrorClass, availabilityErr.Class)
	condition.LastObservedAt = observedAt
	condition.LastError = strings.TrimSpace(availabilityErr.Error())
	condition.NextProbeAt = backendCapacityBoundedProbeAt(
		time.Time{},
		observedAt.Add(backendCapacityProbeDelayForAttempt(condition.ProbeAttempts)),
		observedAt,
	)
	state.ForgeUnavailable[key] = condition
	if !exists {
		recordStateEvent(state, telemetry.ActivityEvent{At: observedAt, Event: forgeavailability.Condition, Message: forgeUnavailableStatusMessage(condition)})
		telemetry.LogLifecycleMessage(o.logger, slog.LevelWarn, telemetry.LifecycleSafetyControl, forgeavailability.Condition, "forge unavailable", o.runningLifecycleCorrelation(running.Issue, running),
			"forge_host", condition.Host,
			"operation", condition.Operation,
			"error_class", condition.ErrorClass,
			"next_probe_at", condition.NextProbeAt,
			"error", availabilityErr,
		)
	}
	return condition
}

// Credential failures pause the whole project, so overlapping host failures cannot
// downgrade them before the existing write canary proves recovery.
func mergedForgeAvailabilityClass(current, observed string) string {
	if current == forgeavailability.ClassWorkerGitHubCredentialUnavailable {
		return current
	}
	return strings.TrimSpace(observed)
}

func forgeCondition(state *State, host string) (ForgeCondition, bool) {
	if state == nil || len(state.ForgeUnavailable) == 0 {
		return ForgeCondition{}, false
	}
	host = forgeavailability.NormalizeHost(host)
	if host != "" {
		condition, ok := state.ForgeUnavailable[host]
		return condition, ok
	}
	if len(state.ForgeUnavailable) != 1 {
		return ForgeCondition{}, false
	}
	for _, condition := range state.ForgeUnavailable {
		return condition, true
	}
	return ForgeCondition{}, false
}

func forgeHostForIssue(issue connector.Issue, fallback string) string {
	pullRequestURL := ""
	if issue.PullRequest != nil {
		pullRequestURL = issue.PullRequest.URL
	}
	for _, value := range []string{
		pullRequestURL,
		issue.PRRepository,
		issue.URL,
	} {
		if host := forgeavailability.HostFromText(value); host != "" {
			return host
		}
	}
	return forgeavailability.NormalizeHost(fallback)
}

func forgeAvailabilityBlocks(state *State, issue connector.Issue, retry Retry, fallbackHost string, now time.Time) bool {
	if workerGitHubCredentialAvailabilityBlocks(state, issue, retry, now) {
		return true
	}
	host := retry.ForgeHost
	if host == "" {
		host = forgeHostForIssue(issue, fallbackHost)
	}
	condition, active := forgeCondition(state, host)
	if !active || condition.ErrorClass == forgeavailability.ClassWorkerGitHubCredentialUnavailable {
		// Credential admission was decided across the whole project above.
		return false
	}
	if condition.ProbeIssueID == issue.ID {
		return false
	}
	if retry.ForgeUnavailable && condition.ProbeIssueID == "" && !now.Before(condition.NextProbeAt) {
		return false
	}
	return retry.ForgeUnavailable || mergeWorkerIssue(issue)
}

// A checked clean PR can merge through the existing API path without the Git
// read that opened this condition. Write failures still hold the merge lane.
func (p dispatchPlanner) forgeAvailabilityBlocks(state *State, issue connector.Issue, retry Retry, now time.Time) bool {
	if !forgeAvailabilityBlocks(state, issue, retry, p.cfg.ForgeHost, now) {
		return false
	}
	if retry.ForgeUnavailable || !p.readyMergeControlCandidate(state, issue) {
		return true
	}
	condition, active := forgeCondition(state, forgeHostForIssue(issue, p.cfg.ForgeHost))
	return !active || !forgeRetryReadOperation(condition.Operation) || condition.ErrorClass == forgeavailability.ClassWorkerGitHubCredentialUnavailable
}

func forgeRetryReadOperation(operation string) bool {
	operation = strings.ToLower(strings.TrimSpace(operation))
	return operation == "git fetch" || operation == "git ls-remote"
}

func workerGitHubCredentialAvailabilityBlocks(state *State, issue connector.Issue, retry Retry, now time.Time) bool {
	if state == nil {
		return false
	}
	retryHost := forgeavailability.NormalizeHost(retry.ForgeHost)
	active, canary := false, false
	for _, condition := range state.ForgeUnavailable {
		if condition.ErrorClass != forgeavailability.ClassWorkerGitHubCredentialUnavailable {
			continue
		}
		active = true
		// Credential conditions share project-wide admission: one host's write
		// canary must pass overlapping pauses, while all other workers wait.
		if condition.ProbeIssueID != "" {
			if condition.ProbeIssueID != issue.ID {
				return true
			}
			canary = true
			continue
		}
		if credentialCanaryNeedsIssue(state, condition, now) ||
			retry.ForgeUnavailable && retryHost == forgeavailability.NormalizeHost(condition.Host) && !now.Before(condition.NextProbeAt) {
			canary = true
		}
	}
	return active && (!canary || mergeWorkerIssue(issue))
}

// A retained project condition can outlive its original issue. Reuse the next
// eligible worker as the canary only when no existing forge retry can serve it.
func credentialCanaryNeedsIssue(state *State, condition ForgeCondition, now time.Time) bool {
	if condition.ErrorClass != forgeavailability.ClassWorkerGitHubCredentialUnavailable || condition.ProbeIssueID != "" || now.Before(condition.NextProbeAt) {
		return false
	}
	for _, retry := range state.Retry {
		if retry.ForgeUnavailable && !mergeWorkerIssue(retry.Issue) && forgeavailability.NormalizeHost(retry.ForgeHost) == forgeavailability.NormalizeHost(condition.Host) {
			return false
		}
	}
	return true
}

func reserveCredentialCanaryForDispatch(state *State, issueID string, now time.Time) {
	for _, condition := range state.ForgeUnavailable {
		if condition.ErrorClass == forgeavailability.ClassWorkerGitHubCredentialUnavailable && condition.ProbeIssueID != "" {
			return
		}
	}
	for _, key := range sortedKeys(state.ForgeUnavailable) {
		condition := state.ForgeUnavailable[key]
		if credentialCanaryNeedsIssue(state, condition, now) {
			reserveForgeAvailabilityProbe(state, issueID, Retry{ForgeUnavailable: true, ForgeHost: condition.Host}, now)
			return
		}
	}
}

func reserveForgeAvailabilityProbe(state *State, issueID string, retry Retry, now time.Time) (string, bool) {
	if state == nil || !retry.ForgeUnavailable {
		return "", false
	}
	condition, active := forgeCondition(state, retry.ForgeHost)
	if !active || condition.ProbeIssueID != "" || now.Before(condition.NextProbeAt) {
		return "", false
	}
	condition.ProbeIssueID = issueID
	condition.ProbeAttempts++
	condition.LastProbeAt = now.UTC()
	condition.LastProbeResult = "in_progress"
	condition.LastProbeDetail = "forge write canary started"
	condition.NextProbeAt = time.Time{}
	state.ForgeUnavailable[forgeavailability.NormalizeHost(condition.Host)] = condition
	return condition.Host, true
}

func releaseForgeAvailabilityProbe(state *State, issueID string, result string, detail string, now time.Time) {
	if state == nil {
		return
	}
	for key, condition := range state.ForgeUnavailable {
		if condition.ProbeIssueID != issueID {
			continue
		}
		condition.ProbeIssueID = ""
		condition.LastProbeAt = now.UTC()
		condition.LastProbeResult = strings.TrimSpace(result)
		condition.LastProbeDetail = strings.TrimSpace(detail)
		if condition.NextProbeAt.IsZero() {
			condition.NextProbeAt = now.Add(backendCapacityProbeDelayForAttempt(condition.ProbeAttempts)).UTC()
		}
		state.ForgeUnavailable[key] = condition
	}
}

func reservedForgeProbeHost(state *State, issueID string) string {
	if state == nil {
		return ""
	}
	for _, condition := range state.ForgeUnavailable {
		if condition.ProbeIssueID == issueID {
			return condition.Host
		}
	}
	return ""
}

func (o *Orchestrator) handleForgeUnavailableCompletion(ctx context.Context, state *State, event runpkg.Completion, running Running) bool {
	availabilityErr, unavailable := forgeavailability.As(event.Err)
	if !unavailable || availabilityErr == nil {
		condition, active := forgeCondition(state, running.ForgeProbeHost)
		if strings.TrimSpace(running.ForgeProbeHost) == "" || !active ||
			condition.ErrorClass != forgeavailability.ClassWorkerGitHubCredentialUnavailable || event.Result.ForgeWriteCompleted {
			return false
		}
		// An inconclusive credential canary remains the same durable forge wait.
		// Completing it as an ordinary failure loses the canary retry on restart.
		cause := event.Err
		if cause == nil {
			cause = errors.New("credential write canary completed without a successful forge write")
		}
		availabilityErr = forgeavailability.NewError(forgeavailability.Scope{
			Host: condition.Host, Operation: condition.Operation,
		}, condition.ErrorClass, cause)
	}
	// Completion releases the worker's reservation even when the error names
	// another host. The unresolved conditions remain available for later probes.
	releaseForgeAvailabilityProbe(state, running.Issue.ID, "failed", availabilityErr.Error(), event.CompletedAt)
	condition := o.registerForgeUnavailable(state, availabilityErr, running, event.CompletedAt)
	forgeRetry := forgeRetryFromCompletion(event, running, condition)
	o.releaseTerminalAttemptClaim(ctx, state, running.Issue, event.CompletedAt)
	message := strings.TrimSpace(availabilityErr.Error())
	o.persistForgeAvailabilityWait(ctx, state, running, event.CompletedAt, condition, forgeRetry, message)
	if workspaceIssueTerminal(running.Issue, o.cfg.TerminalStates) {
		return true
	}
	if state.FailureBreaker.Active() && state.FailureBreaker.CanaryIssueID == running.Issue.ID {
		o.deferProjectFailureBreakerCanary(state, running.Issue.ID, event.CompletedAt, condition.NextProbeAt.Sub(event.CompletedAt))
	}
	state.Retry[running.Issue.ID] = Retry{
		Issue:            cloneIssue(running.Issue),
		Attempt:          running.Attempt,
		DueAt:            condition.NextProbeAt,
		Error:            message,
		WorkerHost:       running.WorkerHost,
		ForgeUnavailable: true,
		ForgeHost:        condition.Host,
		ForgeRetry:       forgeRetry,
	}
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      event.CompletedAt.UTC(),
		Event:   "forge_unavailable_attempt_waiting",
		Message: "waiting to retry the forge write for " + issueLabel(running.Issue) + " without tripping a failure breaker",
	})
	return true
}

func (o *Orchestrator) persistForgeAvailabilityWait(
	ctx context.Context,
	state *State,
	running Running,
	completedAt time.Time,
	condition ForgeCondition,
	forgeRetry *runpkg.ForgeRetry,
	message string,
) bool {
	if forgeRetry == nil {
		return false
	}
	return o.completeDurableWorkAttemptWithMetadata(
		ctx,
		state,
		running,
		completedAt,
		store.WorkAttemptTerminalCapacity,
		forgeUnavailableErrorClass,
		message,
		"waiting",
		message,
		forgeAvailabilityWaitMetadata(condition, forgeRetry),
	)
}

func (o *Orchestrator) persistLegacyForgeAvailabilityWait(
	ctx context.Context,
	running Running,
	condition ForgeCondition,
	forgeRetry *runpkg.ForgeRetry,
	message string,
	existingMetadataJSON string,
) bool {
	updater, ok := o.workAttempts.(store.TerminalWorkAttemptStore)
	if !ok || forgeRetry == nil || running.WorkAttemptID <= 0 {
		return false
	}
	err := updater.UpdateTerminalWorkAttemptWait(ctx, store.WorkAttemptTerminalWaitUpdate{
		AttemptID:          running.WorkAttemptID,
		ExpectedErrorClass: deliverableConfigurationFailureCause,
		TerminalState:      store.WorkAttemptTerminalCapacity,
		ErrorClass:         forgeUnavailableErrorClass,
		ErrorMessage:       message,
		Phase:              "waiting",
		StatusMessage:      message,
		WorkerMetadataJSON: mergeLegacyForgeAvailabilityWaitMetadata(existingMetadataJSON, running, condition, forgeRetry),
	})
	if err != nil {
		if o.logger != nil {
			o.logger.Warn("persist legacy forge wait failed", "attempt_id", running.WorkAttemptID, "issue_id", running.Issue.ID, "error", err)
		}
		return false
	}
	return true
}

func mergeLegacyForgeAvailabilityWaitMetadata(existing string, running Running, condition ForgeCondition, forgeRetry *runpkg.ForgeRetry) string {
	metadata := map[string]any{}
	if json.Unmarshal([]byte(strings.TrimSpace(existing)), &metadata) != nil || metadata == nil {
		return runningWorkAttemptMetadataJSON(running, forgeAvailabilityWaitMetadata(condition, forgeRetry))
	}
	for key, value := range forgeAvailabilityWaitMetadata(condition, forgeRetry) {
		metadata[key] = value
	}
	return marshalWorkAttemptJSON(metadata)
}

func forgeAvailabilityWaitMetadata(condition ForgeCondition, forgeRetry *runpkg.ForgeRetry) map[string]any {
	return map[string]any{"forge_wait": forgeWaitMetadata{
		Host:              condition.Host,
		Operation:         forgeRetry.Operation,
		Branch:            forgeRetry.Branch,
		WorkProductPushed: forgeRetry.WorkProductPushed,
		ErrorClass:        condition.ErrorClass,
		DetectedAt:        condition.DetectedAt,
		NextProbeAt:       condition.NextProbeAt,
	}}
}

func (o *Orchestrator) recoverForgeAvailabilityWaits(ctx context.Context, state *State, attempts []store.WorkAttempt, now time.Time) {
	if state == nil || len(attempts) == 0 {
		return
	}
	latest := latestStoreTerminalAttemptsByIssue(attempts)
	waits := make(map[string]forgeWaitMetadata, len(latest))
	issueIDs := make([]string, 0, len(latest))
	for issueID, attempt := range latest {
		metadata, ok := forgeWaitMetadataFromAttempt(attempt)
		if !ok {
			continue
		}
		waits[issueID] = metadata
		issueIDs = append(issueIDs, issueID)
	}
	if len(waits) == 0 {
		return
	}
	issuesByID, validated := o.validateForgeWaitIssues(ctx, issueIDs)
	for issueID, metadata := range waits {
		attempt := latest[issueID]
		issue := forgeWaitIssueFromAttempt(attempt)
		if validated {
			var ok bool
			issue, ok = issuesByID[issueID]
			if !ok || !stateIn(issue.State, o.cfg.ActiveStates) || workspaceIssueTerminal(issue, o.cfg.TerminalStates) {
				if metadata.ErrorClass == forgeavailability.ClassWorkerGitHubCredentialUnavailable {
					o.restoreForgeAvailabilityCondition(state, attempt, metadata, now)
				}
				continue
			}
		}
		o.restoreForgeAvailabilityWait(state, issue, attempt, metadata, now)
	}
}

func latestStoreTerminalAttemptsByIssue(attempts []store.WorkAttempt) map[string]store.WorkAttempt {
	latest := make(map[string]store.WorkAttempt)
	for _, attempt := range attempts {
		if attempt.Status != store.WorkAttemptStatusTerminal {
			continue
		}
		issueID := strings.TrimSpace(attempt.IssueID)
		if issueID == "" {
			continue
		}
		current, ok := latest[issueID]
		if ok && (attempt.CompletedAt.Before(current.CompletedAt) || attempt.CompletedAt.Equal(current.CompletedAt) && attempt.ID <= current.ID) {
			continue
		}
		latest[issueID] = attempt
	}
	return latest
}

func forgeWaitMetadataFromAttempt(attempt store.WorkAttempt) (forgeWaitMetadata, bool) {
	if attempt.TerminalState != store.WorkAttemptTerminalCapacity || strings.TrimSpace(attempt.ErrorClass) != forgeUnavailableErrorClass {
		return forgeWaitMetadata{}, false
	}
	var metadata struct {
		ForgeWait forgeWaitMetadata `json:"forge_wait"`
	}
	if json.Unmarshal([]byte(attempt.WorkerMetadataJSON), &metadata) != nil {
		return forgeWaitMetadata{}, false
	}
	metadata.ForgeWait.Host = forgeavailability.NormalizeHost(metadata.ForgeWait.Host)
	metadata.ForgeWait.Operation = strings.TrimSpace(metadata.ForgeWait.Operation)
	metadata.ForgeWait.Branch = strings.TrimSpace(metadata.ForgeWait.Branch)
	metadata.ForgeWait.ErrorClass = strings.TrimSpace(metadata.ForgeWait.ErrorClass)
	if metadata.ForgeWait.Host == "" || !forgeavailability.WriteOperation(metadata.ForgeWait.Operation) || !validForgeAvailabilityClass(metadata.ForgeWait.ErrorClass) {
		return forgeWaitMetadata{}, false
	}
	return metadata.ForgeWait, true
}

func validForgeAvailabilityClass(class string) bool {
	switch strings.TrimSpace(class) {
	case forgeavailability.ClassServer, forgeavailability.ClassTimeout, forgeavailability.ClassTransport,
		forgeavailability.ClassWorkerGitHubCredentialUnavailable:
		return true
	default:
		return false
	}
}

func (o *Orchestrator) classifyWorkerGitHubCredentialUnavailable(err error, running Running) error {
	if err == nil {
		return nil
	}
	var deliverableErr *runpkg.DeliverableCommandError
	if !errors.As(err, &deliverableErr) || deliverableErr == nil {
		return err
	}
	if cliErr := runpkg.WorkerGitHubCLIAuthError(err); cliErr != nil {
		return cliErr
	}
	detail := strings.TrimSpace(deliverableErr.Message + "\n" + deliverableErr.Body)
	if !deliverableErr.ApprovalDenied && !forgeavailability.WorkerGitHubCredentialUnavailable(detail) {
		return err
	}
	operation := strings.TrimSpace(deliverableErr.Operation)
	if deliverableErr.OperationClass == "push" && !strings.Contains(strings.ToLower(operation), "git push") {
		operation = "git push"
	}
	if !forgeavailability.WriteOperation(operation) {
		return err
	}
	scope := forgeavailability.Scope{Operation: operation}
	if availabilityErr, ok := forgeavailability.As(err); ok && availabilityErr != nil {
		if availabilityErr.Class == forgeavailability.ClassWorkerGitHubCredentialUnavailable {
			return err
		}
		scope = availabilityErr.Scope
		if strings.TrimSpace(scope.Operation) == "" {
			scope.Operation = operation
		}
	}
	if strings.TrimSpace(scope.Host) == "" {
		scope.Host = o.cfg.ForgeHost
		if scope.Host == "" {
			scope.Host = forgeavailability.HostFromText(deliverableErr.Arguments)
		}
	}
	if strings.TrimSpace(scope.Host) == "" {
		scope.Host = forgeHostForIssue(running.Issue, o.cfg.ForgeHost)
	}
	return forgeavailability.NewError(scope, forgeavailability.ClassWorkerGitHubCredentialUnavailable, err)
}

func workerGitHubCredentialUnavailableError(err error) bool {
	availabilityErr, ok := forgeavailability.As(err)
	return ok && availabilityErr != nil && availabilityErr.Class == forgeavailability.ClassWorkerGitHubCredentialUnavailable
}

func (o *Orchestrator) validateForgeWaitIssues(ctx context.Context, issueIDs []string) (map[string]connector.Issue, bool) {
	if o == nil || o.connector == nil || len(issueIDs) == 0 {
		return nil, false
	}
	issues, err := o.connector.FetchIssueStatesByIDs(ctx, issueIDs)
	if err != nil {
		if o.logger != nil {
			o.logger.Warn("forge wait issue validation failed; preserving durable waits", "issue_ids", issueIDs, "error", err)
		}
		return nil, false
	}
	byID := make(map[string]connector.Issue, len(issues))
	for _, issue := range issues {
		if issueID := strings.TrimSpace(issue.ID); issueID != "" {
			byID[issueID] = issue
		}
	}
	return byID, true
}

func forgeWaitIssueFromAttempt(attempt store.WorkAttempt) connector.Issue {
	issue := connector.NewIssue()
	issue.ID = strings.TrimSpace(attempt.IssueID)
	issue.Identifier = strings.TrimSpace(attempt.Identifier)
	issue.URL = strings.TrimSpace(attempt.IssueURL)
	issue.State = strings.TrimSpace(attempt.Lane)
	issue.PRRepository = strings.TrimSpace(attempt.Repo)
	if attempt.PRNumber != nil && *attempt.PRNumber > 0 {
		number := int(*attempt.PRNumber)
		issue.PRNumber = &number
	}
	return issue
}

func (o *Orchestrator) restoreForgeAvailabilityCondition(state *State, attempt store.WorkAttempt, metadata forgeWaitMetadata, now time.Time) ForgeCondition {
	detectedAt := metadata.DetectedAt
	if detectedAt.IsZero() {
		detectedAt = attempt.CompletedAt
	}
	nextProbeAt := metadata.NextProbeAt
	if nextProbeAt.IsZero() || nextProbeAt.Before(now) {
		nextProbeAt = now
	}
	host := forgeavailability.NormalizeHost(metadata.Host)
	condition, exists := state.ForgeUnavailable[host]
	if !exists {
		condition = ForgeCondition{ProjectID: strings.TrimSpace(o.cfg.Project.ID), Host: host, DetectedAt: detectedAt.UTC()}
	}
	if condition.DetectedAt.IsZero() || detectedAt.Before(condition.DetectedAt) {
		condition.DetectedAt = detectedAt.UTC()
	}
	condition.Operation = metadata.Operation
	condition.ErrorClass = mergedForgeAvailabilityClass(condition.ErrorClass, metadata.ErrorClass)
	condition.LastObservedAt = attempt.CompletedAt.UTC()
	condition.LastError = strings.TrimSpace(attempt.ErrorMessage)
	if condition.NextProbeAt.IsZero() || nextProbeAt.Before(condition.NextProbeAt) {
		condition.NextProbeAt = nextProbeAt.UTC()
	}
	state.ForgeUnavailable[host] = condition
	return condition
}

func (o *Orchestrator) restoreForgeAvailabilityWait(state *State, issue connector.Issue, attempt store.WorkAttempt, metadata forgeWaitMetadata, now time.Time) {
	condition := o.restoreForgeAvailabilityCondition(state, attempt, metadata, now)
	host := condition.Host
	nextProbeAt := condition.NextProbeAt
	state.Retry[issue.ID] = Retry{
		Issue:            cloneIssue(issue),
		Attempt:          attempt.AttemptNumber,
		DueAt:            nextProbeAt.UTC(),
		Error:            strings.TrimSpace(attempt.ErrorMessage),
		WorkerHost:       strings.TrimSpace(attempt.WorkerHost),
		ForgeUnavailable: true,
		ForgeHost:        host,
		ForgeRetry: &runpkg.ForgeRetry{
			Host:              host,
			Operation:         metadata.Operation,
			Branch:            metadata.Branch,
			WorkProductPushed: metadata.WorkProductPushed,
		},
	}
	recordStateEvent(state, telemetry.ActivityEvent{At: now.UTC(), Event: "forge_unavailable_wait_restored", Message: "restored durable forge wait for " + issueLabel(issue)})
}

func forgeRetryFromCompletion(event runpkg.Completion, running Running, condition ForgeCondition) *runpkg.ForgeRetry {
	retry := &runpkg.ForgeRetry{
		Host:              condition.Host,
		Operation:         condition.Operation,
		Branch:            strings.TrimSpace(event.Result.WorkspaceBranch),
		WorkProductPushed: running.WorkProductPushed || event.Result.PullRequestHeadPushed || event.Result.PullRequestUpdated,
	}
	if retry.Branch == "" {
		retry.Branch = strings.TrimSpace(running.Issue.BranchName)
	}
	var deliverableErr *runpkg.DeliverableCommandError
	if errors.As(event.Err, &deliverableErr) && deliverableErr != nil {
		retry.Arguments = strings.TrimSpace(deliverableErr.Arguments)
		if operation := strings.TrimSpace(deliverableErr.Operation); operation != "" {
			retry.Operation = operation
		}
	}
	var recoveryErr *runpkg.DeliverableRecoveryError
	if errors.As(event.Err, &recoveryErr) && recoveryErr != nil && strings.TrimSpace(recoveryErr.Branch) != "" {
		retry.Branch = strings.TrimSpace(recoveryErr.Branch)
	}
	return retry
}

func (o *Orchestrator) finishForgeAvailabilityProbe(state *State, event runpkg.Completion, running Running) {
	if state == nil || strings.TrimSpace(running.ForgeProbeHost) == "" {
		return
	}
	condition, active := forgeCondition(state, running.ForgeProbeHost)
	credentialProbe := active && condition.ErrorClass == forgeavailability.ClassWorkerGitHubCredentialUnavailable
	if event.Result.ForgeWriteCompleted || !credentialProbe && forgeWriteReachedRemote(event.Err) {
		o.completeForgeAvailabilityRecovery(state, running.ForgeProbeHost, event.CompletedAt, "canary")
		return
	}
	releaseForgeAvailabilityProbe(state, running.Issue.ID, "inconclusive", errorString(event.Err), event.CompletedAt)
}

func forgeWriteReachedRemote(err error) bool {
	var deliverableErr *runpkg.DeliverableCommandError
	if !errors.As(err, &deliverableErr) || deliverableErr == nil {
		return false
	}
	operation := deliverableErr.Operation
	if deliverableErr.OperationClass == "push" {
		operation = "git push"
	}
	return forgeavailability.ProvesReachability(operation, deliverableErr.Error())
}

func (o *Orchestrator) completeForgeAvailabilityRecovery(state *State, host string, recoveredAt time.Time, source string) {
	if state == nil {
		return
	}
	host = forgeavailability.NormalizeHost(host)
	condition, ok := state.ForgeUnavailable[host]
	if !ok {
		return
	}
	if recoveredAt.IsZero() {
		recoveredAt = o.clockNow()
	}
	delete(state.ForgeUnavailable, host)
	releaseForgeUnavailableRetries(state, host, recoveredAt)
	o.activateDispatchRecovery(state, dispatchRecoveryForgeUnavailable, forgeUnavailableStatusMessage(condition), recoveredAt, "")
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      recoveredAt.UTC(),
		Event:   "forge_availability_recovered",
		Message: "forge " + condition.Host + " recovered via " + source,
	})
	telemetry.LogLifecycleMessage(o.logger, slog.LevelInfo, telemetry.LifecycleSafetyControl, "forge_availability_recovered", "forge availability recovered", telemetry.LifecycleCorrelation{ProjectID: o.cfg.Project.ID},
		"forge_host", condition.Host,
		"detected_at", condition.DetectedAt,
		"recovered_at", recoveredAt,
		"source", source,
	)
}

func releaseForgeUnavailableRetries(state *State, host string, releasedAt time.Time) {
	for issueID, retry := range state.Retry {
		if !retry.ForgeUnavailable || forgeavailability.NormalizeHost(retry.ForgeHost) != host {
			continue
		}
		retry.DueAt = releasedAt
		retry.ForgeUnavailable = false
		state.Retry[issueID] = retry
	}
}

func forgeUnavailableStatusMessage(condition ForgeCondition) string {
	host := strings.TrimSpace(condition.Host)
	if host == "" {
		host = "configured"
	}
	message := fmt.Sprintf("forge %s unavailable (%s/%s)", host, forgeavailability.Condition, condition.ErrorClass)
	if !condition.NextProbeAt.IsZero() {
		message += "; next write canary at " + condition.NextProbeAt.UTC().Format(time.RFC3339)
	}
	return message
}

func (o *Orchestrator) clearForgeAvailability(state *State, host string, clearedAt time.Time) []ForgeCondition {
	if state == nil || len(state.ForgeUnavailable) == 0 {
		return nil
	}
	if clearedAt.IsZero() {
		clearedAt = o.clockNow()
	}
	host = forgeavailability.NormalizeHost(host)
	cleared := make([]ForgeCondition, 0, len(state.ForgeUnavailable))
	for _, key := range sortedKeys(state.ForgeUnavailable) {
		condition := state.ForgeUnavailable[key]
		if host != "" && forgeavailability.NormalizeHost(condition.Host) != host {
			continue
		}
		if condition.ErrorClass == forgeavailability.ClassWorkerGitHubCredentialUnavailable {
			// The existing operator action requests the write canary; it cannot
			// establish credential recovery independently of that canary.
			if condition.ProbeIssueID == "" {
				condition.NextProbeAt = clearedAt.UTC()
				state.ForgeUnavailable[key] = condition
				for issueID, retry := range state.Retry {
					if retry.ForgeUnavailable && forgeavailability.NormalizeHost(retry.ForgeHost) == key {
						retry.DueAt = clearedAt.UTC()
						state.Retry[issueID] = retry
					}
				}
			}
			continue
		}
		condition.LastProbeAt = clearedAt.UTC()
		condition.LastProbeResult = "operator_cleared"
		condition.LastProbeDetail = "operator cleared the recorded condition"
		condition.NextProbeAt = time.Time{}
		delete(state.ForgeUnavailable, key)
		releaseForgeUnavailableRetries(state, forgeavailability.NormalizeHost(condition.Host), clearedAt)
		o.activateDispatchRecovery(state, dispatchRecoveryForgeUnavailable, forgeUnavailableStatusMessage(condition), clearedAt, "")
		cleared = append(cleared, condition)
	}
	return cleared
}

func (o *Orchestrator) ClearForgeAvailability(ctx context.Context, host string) ([]ForgeCondition, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	request := forgeClearRequest{host: forgeavailability.NormalizeHost(host), at: o.clockNow(), reply: make(chan forgeClearReply, 1)}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-o.done:
		return nil, ErrStopped
	case o.forgeClearRequests <- request:
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-o.done:
		return nil, ErrStopped
	case reply := <-request.reply:
		return reply.cleared, nil
	}
}
