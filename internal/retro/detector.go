package retro

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	ScopeWorkflow = "workflow"
	ScopeProduct  = "product"

	SeverityInfo     = "info"
	SeverityWarning  = "warning"
	SeverityHigh     = "high"
	SeverityCritical = "critical"

	PatternCompletedRedispatch    = "completed_work_redispatched"
	PatternSystemicBreaker        = "systemic_breaker_trip"
	PatternCeilingThenSuccess     = "session_ceiling_then_success"
	PatternReceiptBaseline        = "receipt_exceeds_baseline"
	PatternGateWaitTimeout        = "gate_wait_timeout"
	PatternInvalidWorkpadStatus   = "invalid_workpad_status"
	PatternSlowCapacityRecovery   = "slow_capacity_recovery"
	PatternFallbackOrphanRecovery = "fallback_orphan_recovery"
)

type Snapshot struct {
	Attempts    []Attempt
	Sessions    []Session
	UsageEvents []UsageEvent
	PhaseEvents []PhaseEvent
}

type Attempt struct {
	ID                   int64
	IssueID              string
	Identifier           string
	IssueURL             string
	AttemptNumber        int
	StartedAt            time.Time
	CompletedAt          time.Time
	TerminalState        string
	ErrorClass           string
	ErrorMessage         string
	Phase                string
	StatusMessage        string
	WaitReason           string
	CapacitySnapshotJSON string
}

type Session struct {
	ID                    int64
	WorkAttemptID         int64
	IssueID               string
	Identifier            string
	IssueURL              string
	StartedAt             time.Time
	CompletedAt           time.Time
	TotalTokens           int64
	FinalState            string
	OrphanRecoveryOutcome string
}

type UsageEvent struct {
	IssueID     string
	Identifier  string
	FinishedAt  time.Time
	TotalTokens int64
	Outcome     string
}

type PhaseEvent struct {
	IssueID     string
	Identifier  string
	IssueURL    string
	PhaseType   string
	PhaseName   string
	Reason      string
	Status      string
	StartedAt   time.Time
	FinishedAt  time.Time
	TotalTokens int64
}

type DetectorOptions struct {
	FallbackThreshold       int
	ReceiptBaselineMultiple float64
}

type Finding struct {
	Pattern     string
	Scope       string
	Severity    string
	Title       string
	Detail      string
	TokenDelta  int64
	Occurrences []Occurrence
	Proposal    *Proposal
}

type Occurrence struct {
	Issue  string
	At     time.Time
	Tokens int64
	Detail string
}

type Proposal struct {
	Path   string
	Change string
}

func Detect(snapshot Snapshot, options DetectorOptions) []Finding {
	if options.FallbackThreshold < 2 {
		options.FallbackThreshold = DefaultFallbackThreshold
	}
	if options.ReceiptBaselineMultiple <= 1 {
		options.ReceiptBaselineMultiple = DefaultReceiptBaselineMultiple
	}
	findings := []Finding{}
	appendResult := func(finding Finding, ok bool) {
		if ok {
			findings = append(findings, finding)
		}
	}
	appendResult(detectCompletedRedispatch(snapshot.Attempts))
	findings = append(findings, detectSystemicBreakers(snapshot.Attempts)...)
	appendResult(detectCeilingThenSuccess(snapshot.Attempts, snapshot.Sessions))
	appendResult(detectReceiptBaseline(snapshot.UsageEvents, options.ReceiptBaselineMultiple))
	appendResult(detectGateWaitTimeouts(snapshot.Attempts, snapshot.PhaseEvents))
	appendResult(detectInvalidWorkpadStatuses(snapshot.PhaseEvents))
	appendResult(detectSlowCapacityRecovery(snapshot.Attempts))
	appendResult(detectFallbackOrphanRecovery(snapshot.Sessions, options.FallbackThreshold))
	for index := range findings {
		sortOccurrences(findings[index].Occurrences)
	}
	sort.SliceStable(findings, func(i, j int) bool {
		if severityRank(findings[i].Severity) != severityRank(findings[j].Severity) {
			return severityRank(findings[i].Severity) > severityRank(findings[j].Severity)
		}
		return findings[i].Pattern < findings[j].Pattern
	})
	return findings
}

func Qualifies(finding Finding, minOccurrences int, singleOccurrenceSeverity string) bool {
	if minOccurrences < 2 {
		minOccurrences = DefaultMinOccurrences
	}
	if len(finding.Occurrences) >= minOccurrences {
		return true
	}
	return len(finding.Occurrences) == 1 && severityRank(finding.Severity) >= severityRank(singleOccurrenceSeverity)
}

func Fingerprint(projectID string, finding Finding) string {
	raw := strings.ToLower(strings.TrimSpace(projectID)) + "\x00" + finding.Scope + "\x00" + finding.Pattern
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func severityRank(severity string) int {
	switch strings.ToLower(strings.TrimSpace(severity)) {
	case SeverityInfo:
		return 1
	case SeverityWarning, "warn":
		return 2
	case SeverityHigh:
		return 3
	case SeverityCritical:
		return 4
	default:
		return 0
	}
}

func detectCompletedRedispatch(attempts []Attempt) (Finding, bool) {
	groups := attemptsByIssue(attempts)
	occurrences := []Occurrence{}
	for _, group := range groups {
		slices.SortFunc(group, compareAttemptTime)
		completed := false
		for _, attempt := range group {
			if completed {
				occurrences = append(occurrences, attemptOccurrence(attempt, "dispatched after a successful completed attempt"))
			}
			if completedWork(attempt) {
				completed = true
			}
		}
	}
	return Finding{
		Pattern:     PatternCompletedRedispatch,
		Scope:       ScopeProduct,
		Severity:    SeverityCritical,
		Title:       "Stop re-dispatching completed work",
		Detail:      "Work received another agent dispatch after a successful completion.",
		Occurrences: occurrences,
	}, len(occurrences) > 0
}

func detectSystemicBreakers(attempts []Attempt) []Finding {
	byClass := map[string][]Occurrence{}
	issues := map[string]map[string]struct{}{}
	for _, attempt := range attempts {
		class := systemicErrorClass(attempt)
		if class == "" {
			continue
		}
		byClass[class] = append(byClass[class], attemptOccurrence(attempt, strings.TrimSpace(attempt.ErrorMessage)))
		if issues[class] == nil {
			issues[class] = map[string]struct{}{}
		}
		issues[class][attemptIssue(attempt)] = struct{}{}
	}
	classes := make([]string, 0, len(byClass))
	for class := range byClass {
		classes = append(classes, class)
	}
	sort.Strings(classes)
	findings := make([]Finding, 0, len(classes))
	for _, class := range classes {
		occurrences := byClass[class]
		severity := SeverityHigh
		if class == "quota" && len(issues[class]) >= 2 {
			severity = SeverityCritical
		}
		findings = append(findings, Finding{
			Pattern:     PatternSystemicBreaker + ":" + class,
			Scope:       ScopeProduct,
			Severity:    severity,
			Title:       "Handle systemic " + class + " breaker trips",
			Detail:      fmt.Sprintf("The same systemic %s failure class affected %d issue(s).", class, len(issues[class])),
			Occurrences: occurrences,
		})
	}
	return findings
}

func detectCeilingThenSuccess(attempts []Attempt, sessions []Session) (Finding, bool) {
	sessionTokens := map[int64]int64{}
	for _, session := range sessions {
		if session.WorkAttemptID > 0 && session.TotalTokens > sessionTokens[session.WorkAttemptID] {
			sessionTokens[session.WorkAttemptID] = session.TotalTokens
		}
	}
	groups := attemptsByIssue(attempts)
	occurrences := []Occurrence{}
	var tokenDelta int64
	for _, group := range groups {
		slices.SortFunc(group, compareAttemptTime)
		for index, attempt := range group {
			if !containsAnyFold(attempt.ErrorClass+" "+attempt.ErrorMessage, "session token ceiling", "max_session_tokens", "context multiplier", "max_session_context_multiplier") {
				continue
			}
			if !laterSuccess(group[index+1:]) {
				continue
			}
			tokens := sessionTokens[attempt.ID]
			tokenDelta += tokens
			occurrences = append(occurrences, Occurrence{Issue: attemptIssue(attempt), At: attempt.CompletedAt, Tokens: tokens, Detail: strings.TrimSpace(attempt.ErrorMessage)})
		}
	}
	return Finding{
		Pattern:     PatternCeilingThenSuccess,
		Scope:       ScopeWorkflow,
		Severity:    SeverityHigh,
		Title:       "Recalibrate session ceilings that kill viable work",
		Detail:      "Sessions stopped by token guards later succeeded for the same unchanged work item.",
		TokenDelta:  tokenDelta,
		Occurrences: occurrences,
		Proposal: &Proposal{
			Path:   "agent.max_session_context_multiplier",
			Change: "Remove or raise the context multiplier and set an intentional absolute agent.max_session_tokens ceiling after human review.",
		},
	}, len(occurrences) > 0
}

func detectReceiptBaseline(events []UsageEvent, multiple float64) (Finding, bool) {
	totals := map[string]int64{}
	latest := map[string]time.Time{}
	for _, event := range events {
		key := usageIssue(event)
		if key == "" {
			continue
		}
		totals[key] += event.TotalTokens
		if event.FinishedAt.After(latest[key]) {
			latest[key] = event.FinishedAt
		}
	}
	values := make([]int64, 0, len(totals))
	for _, total := range totals {
		if total > 0 {
			values = append(values, total)
		}
	}
	if len(values) < 2 {
		return Finding{}, false
	}
	slices.Sort(values)
	baseline := values[(len(values)-1)/2]
	if baseline <= 0 {
		return Finding{}, false
	}
	occurrences := []Occurrence{}
	var tokenDelta int64
	for issue, total := range totals {
		if float64(total) < float64(baseline)*multiple {
			continue
		}
		delta := total - baseline
		tokenDelta += delta
		occurrences = append(occurrences, Occurrence{Issue: issue, At: latest[issue], Tokens: total, Detail: fmt.Sprintf("receipt %d tokens; baseline %d; multiple %.2fx", total, baseline, float64(total)/float64(baseline))})
	}
	return Finding{
		Pattern:     PatternReceiptBaseline,
		Scope:       ScopeWorkflow,
		Severity:    SeverityHigh,
		Title:       "Reduce issue receipts above the project baseline",
		Detail:      fmt.Sprintf("Issue token receipts exceeded %.1fx the project median baseline.", multiple),
		TokenDelta:  tokenDelta,
		Occurrences: occurrences,
		Proposal:    &Proposal{Path: "agent", Change: "Review effort defaults, gate settings, and workflow instructions for the cited issue shapes."},
	}, len(occurrences) > 0
}

func detectGateWaitTimeouts(attempts []Attempt, phases []PhaseEvent) (Finding, bool) {
	occurrences := []Occurrence{}
	for _, attempt := range attempts {
		text := strings.Join([]string{attempt.ErrorClass, attempt.ErrorMessage, attempt.WaitReason}, " ")
		if containsAnyFold(text, "gate_wait_timeout", "gate wait timeout", "gate-wait timeout") {
			occurrences = append(occurrences, attemptOccurrence(attempt, strings.TrimSpace(text)))
		}
	}
	for _, event := range phases {
		text := strings.Join([]string{event.PhaseName, event.Reason, event.Status}, " ")
		if containsAnyFold(text, "gate_wait_timeout", "gate wait timeout", "gate-wait timeout") {
			occurrences = append(occurrences, Occurrence{Issue: phaseIssue(event), At: phaseTime(event), Tokens: event.TotalTokens, Detail: strings.TrimSpace(text)})
		}
	}
	return Finding{
		Pattern:     PatternGateWaitTimeout,
		Scope:       ScopeWorkflow,
		Severity:    SeverityHigh,
		Title:       "Tune recurring gate-wait timeouts",
		Detail:      "The project repeatedly exhausted its configured gate-wait window.",
		Occurrences: dedupeOccurrences(occurrences),
		Proposal:    &Proposal{Path: "agent.auto_promote.gate_wait_timeout_seconds", Change: "Increase the gate-wait timeout or narrow the required checks after human review."},
	}, len(occurrences) > 0
}

func detectInvalidWorkpadStatuses(phases []PhaseEvent) (Finding, bool) {
	occurrences := []Occurrence{}
	for _, event := range phases {
		if !strings.EqualFold(strings.TrimSpace(event.Reason), "workpad_status_invalid") {
			continue
		}
		occurrences = append(occurrences, Occurrence{Issue: phaseIssue(event), At: phaseTime(event), Tokens: event.TotalTokens, Detail: "workpad_status_invalid"})
	}
	return Finding{
		Pattern:     PatternInvalidWorkpadStatus,
		Scope:       ScopeWorkflow,
		Severity:    SeverityHigh,
		Title:       "Prevent invalid workpad statuses",
		Detail:      "Agents emitted workpad statuses outside the configured detent-status contract.",
		Occurrences: occurrences,
		Proposal:    &Proposal{Path: "WORKFLOW.md", Change: "State that detent-status.status accepts only in_progress, blocked, or complete."},
	}, len(occurrences) > 0
}

func detectSlowCapacityRecovery(attempts []Attempt) (Finding, bool) {
	ordered := append([]Attempt(nil), attempts...)
	slices.SortFunc(ordered, compareAttemptTime)
	occurrences := []Occurrence{}
	for index, attempt := range ordered {
		if !containsAnyFold(attempt.ErrorClass+" "+attempt.ErrorMessage, "backend_capacity", "quota", "usage limit", "resource_exhausted") {
			continue
		}
		resetAt, ok := capacityResetAt(attempt.CapacitySnapshotJSON)
		if !ok {
			continue
		}
		recoveredAt := nextSuccessfulStart(ordered[index+1:])
		if recoveredAt.IsZero() || !recoveredAt.After(resetAt) {
			continue
		}
		delay := recoveredAt.Sub(resetAt)
		occurrences = append(occurrences, Occurrence{Issue: attemptIssue(attempt), At: recoveredAt, Detail: "recovered " + delay.Round(time.Second).String() + " after reset window"})
	}
	return Finding{
		Pattern:     PatternSlowCapacityRecovery,
		Scope:       ScopeProduct,
		Severity:    SeverityHigh,
		Title:       "Recover capacity promptly after reset windows",
		Detail:      "Backend capacity remained unavailable after its recorded reset window.",
		Occurrences: occurrences,
	}, len(occurrences) > 0
}

func detectFallbackOrphanRecovery(sessions []Session, threshold int) (Finding, bool) {
	candidates := make([]Session, 0, len(sessions))
	for _, session := range sessions {
		if strings.TrimSpace(session.OrphanRecoveryOutcome) != "" && sessionIssue(session) != "" {
			candidates = append(candidates, session)
		}
	}
	slices.SortFunc(candidates, func(a, b Session) int {
		if result := a.StartedAt.Compare(b.StartedAt); result != 0 {
			return result
		}
		return cmp.Compare(a.ID, b.ID)
	})
	occurrences := []Occurrence{}
	consecutive := []Session{}
	for _, session := range candidates {
		if strings.EqualFold(strings.TrimSpace(session.OrphanRecoveryOutcome), "fresh") {
			consecutive = append(consecutive, session)
			if len(consecutive) == threshold {
				for _, failed := range consecutive {
					occurrences = append(occurrences, fallbackOccurrence(failed, threshold))
				}
			} else if len(consecutive) > threshold {
				occurrences = append(occurrences, fallbackOccurrence(session, threshold))
			}
			continue
		}
		consecutive = nil
	}
	return Finding{
		Pattern:     PatternFallbackOrphanRecovery,
		Scope:       ScopeProduct,
		Severity:    SeverityHigh,
		Title:       "Restore orphan session reattachment",
		Detail:      fmt.Sprintf("At least %d consecutive orphan recoveries fell back to fresh sessions.", threshold),
		Occurrences: occurrences,
	}, len(occurrences) > 0
}

func fallbackOccurrence(session Session, threshold int) Occurrence {
	at := session.CompletedAt
	if at.IsZero() {
		at = session.StartedAt
	}
	return Occurrence{
		Issue:  sessionIssue(session),
		At:     at,
		Tokens: session.TotalTokens,
		Detail: strconv.Itoa(threshold) + " consecutive orphan recoveries started fresh",
	}
}

func attemptsByIssue(attempts []Attempt) map[string][]Attempt {
	groups := map[string][]Attempt{}
	for _, attempt := range attempts {
		key := attemptIssue(attempt)
		if key != "" {
			groups[key] = append(groups[key], attempt)
		}
	}
	return groups
}

func compareAttemptTime(a, b Attempt) int {
	if result := a.StartedAt.Compare(b.StartedAt); result != 0 {
		return result
	}
	return cmp.Compare(a.ID, b.ID)
}

func attemptOccurrence(attempt Attempt, detail string) Occurrence {
	at := attempt.CompletedAt
	if at.IsZero() {
		at = attempt.StartedAt
	}
	return Occurrence{Issue: attemptIssue(attempt), At: at, Detail: strings.TrimSpace(detail)}
}

func attemptIssue(attempt Attempt) string {
	return firstNonBlank(attempt.Identifier, attempt.IssueID, attempt.IssueURL)
}

func sessionIssue(session Session) string {
	return firstNonBlank(session.Identifier, session.IssueID, session.IssueURL)
}

func usageIssue(event UsageEvent) string {
	return firstNonBlank(event.Identifier, event.IssueID)
}

func phaseIssue(event PhaseEvent) string {
	return firstNonBlank(event.Identifier, event.IssueID, event.IssueURL)
}

func phaseTime(event PhaseEvent) time.Time {
	if !event.FinishedAt.IsZero() {
		return event.FinishedAt
	}
	return event.StartedAt
}

func successful(state string) bool {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "success", "completed", "complete", "done", "merged":
		return true
	default:
		return false
	}
}

func completedWork(attempt Attempt) bool {
	if !successful(attempt.TerminalState) {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(attempt.Phase), "waiting") ||
		containsAnyFold(attempt.StatusMessage, "terminal state", "gate-wait", "gate wait")
}

func laterSuccess(attempts []Attempt) bool {
	return !nextSuccessfulStart(attempts).IsZero()
}

func nextSuccessfulStart(attempts []Attempt) time.Time {
	for _, attempt := range attempts {
		if successful(attempt.TerminalState) {
			return attempt.StartedAt
		}
	}
	return time.Time{}
}

func systemicErrorClass(attempt Attempt) string {
	text := strings.ToLower(strings.Join([]string{attempt.ErrorClass, attempt.ErrorMessage}, " "))
	switch {
	case containsAnyFold(text, "backend_capacity", "quota", "usage limit", "resource_exhausted", "rate limit"):
		return "quota"
	case containsAnyFold(text, "model_not_found", "invalid model", "backend config", "configuration", "authentication", "unauthorized", "permission denied"):
		return "backend-configuration"
	default:
		return ""
	}
}

func containsAnyFold(value string, needles ...string) bool {
	value = strings.ToLower(value)
	for _, needle := range needles {
		if strings.Contains(value, strings.ToLower(needle)) {
			return true
		}
	}
	return false
}

func capacityResetAt(raw string) (time.Time, bool) {
	var value any
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &value); err != nil {
		return time.Time{}, false
	}
	return findResetAt(value)
}

func findResetAt(value any) (time.Time, bool) {
	switch typed := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, wanted := range []string{"resetat", "resumeat"} {
			for _, key := range keys {
				normalized := strings.NewReplacer("_", "", "-", "").Replace(strings.ToLower(key))
				if normalized != wanted {
					continue
				}
				candidate := typed[key]
				if parsed, ok := parseJSONTime(candidate); ok {
					return parsed, true
				}
			}
		}
		for _, key := range keys {
			candidate := typed[key]
			if parsed, ok := findResetAt(candidate); ok {
				return parsed, true
			}
		}
	case []any:
		for _, candidate := range typed {
			if parsed, ok := findResetAt(candidate); ok {
				return parsed, true
			}
		}
	}
	return time.Time{}, false
}

func parseJSONTime(value any) (time.Time, bool) {
	raw, ok := value.(string)
	if !ok {
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339Nano, raw)
	return parsed, err == nil
}

func dedupeOccurrences(values []Occurrence) []Occurrence {
	seen := map[string]struct{}{}
	out := make([]Occurrence, 0, len(values))
	for _, occurrence := range values {
		key := occurrence.Issue + "\x00" + occurrence.At.UTC().Format(time.RFC3339Nano) + "\x00" + occurrence.Detail
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, occurrence)
	}
	return out
}

func sortOccurrences(values []Occurrence) {
	sort.SliceStable(values, func(i, j int) bool {
		if !values[i].At.Equal(values[j].At) {
			return values[i].At.Before(values[j].At)
		}
		return values[i].Issue < values[j].Issue
	})
}

func firstNonBlank(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
