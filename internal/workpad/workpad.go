package workpad

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	SourceStructured   = "structured"
	SourceProse        = "prose"
	SourceProseSection = "prose_section"
	SourceProsePhrase  = "prose_phrase"

	StatusInProgress = "in_progress"
	StatusBlocked    = "blocked"
	StatusComplete   = "complete"

	FieldCompletionKind     = "completion_kind"
	FieldCompletionEvidence = "completion_evidence"
	CompletionOperational   = "operational"

	BlockerOwnerOrchestrator = "orchestrator"
	BlockerOwnerHuman        = "human"

	PredicateIssueState        = "issue_state"
	PredicatePullRequestState  = "pull_request_state"
	PredicateCheckPresence     = "check_presence"
	PredicateBudgetCapacity    = "budget_capacity"
	PredicateConfigFingerprint = "config_fingerprint"
)

var refPattern = regexp.MustCompile(`^(?:([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+))?#([1-9][0-9]*)$`)

type Signal struct {
	Source      string            `json:"source,omitempty" yaml:"source,omitempty"`
	CommentURL  string            `json:"comment_url,omitempty" yaml:"comment_url,omitempty"`
	Status      string            `json:"status,omitempty" yaml:"status,omitempty"`
	ReasonCode  string            `json:"reason_code,omitempty" yaml:"reason_code,omitempty"`
	Blockers    []Blocker         `json:"blockers,omitempty" yaml:"blockers,omitempty"`
	HumanAction string            `json:"human_action,omitempty" yaml:"human_action,omitempty"`
	RecordedAt  *time.Time        `json:"recorded_at,omitempty" yaml:"recorded_at,omitempty"`
	Fields      map[string]string `json:"fields,omitempty" yaml:"fields,omitempty"`
	Invalid     *Invalid          `json:"invalid,omitempty" yaml:"invalid,omitempty"`
}

type Blocker struct {
	Ref             string     `json:"ref,omitempty" yaml:"ref,omitempty"`
	Identifier      string     `json:"identifier,omitempty" yaml:"identifier,omitempty"`
	Reason          string     `json:"reason,omitempty" yaml:"reason,omitempty"`
	Owner           string     `json:"owner,omitempty" yaml:"owner,omitempty"`
	Predicate       *Predicate `json:"predicate,omitempty" yaml:"predicate,omitempty"`
	ExpiresAt       *time.Time `json:"expires_at,omitempty" yaml:"expires_at,omitempty"`
	RecheckInterval string     `json:"recheck_interval,omitempty" yaml:"recheck_interval,omitempty"`
	Unverifiable    bool       `json:"unverifiable,omitempty" yaml:"unverifiable,omitempty"`
}

type Predicate struct {
	Type        string   `json:"type,omitempty" yaml:"type,omitempty"`
	Ref         string   `json:"ref,omitempty" yaml:"ref,omitempty"`
	Identifier  string   `json:"identifier,omitempty" yaml:"identifier,omitempty"`
	States      []string `json:"states,omitempty" yaml:"states,omitempty"`
	Check       string   `json:"check,omitempty" yaml:"check,omitempty"`
	Present     *bool    `json:"present,omitempty" yaml:"present,omitempty"`
	Scope       string   `json:"scope,omitempty" yaml:"scope,omitempty"`
	Resource    string   `json:"resource,omitempty" yaml:"resource,omitempty"`
	Condition   string   `json:"condition,omitempty" yaml:"condition,omitempty"`
	Fingerprint string   `json:"fingerprint,omitempty" yaml:"fingerprint,omitempty"`
}

type Invalid struct {
	Hash    string `json:"hash,omitempty" yaml:"hash,omitempty"`
	Message string `json:"message,omitempty" yaml:"message,omitempty"`
	Content string `json:"content,omitempty" yaml:"content,omitempty"`
}

type statusBlockYAML struct {
	Schema      int               `yaml:"schema"`
	Status      string            `yaml:"status"`
	ReasonCode  string            `yaml:"reason_code"`
	Blockers    []blockerYAML     `yaml:"blockers"`
	HumanAction *string           `yaml:"human_action"`
	Fields      map[string]string `yaml:"fields"`
}

type blockerYAML struct {
	Ref             string         `yaml:"ref"`
	Reason          string         `yaml:"reason"`
	Owner           string         `yaml:"owner"`
	Predicate       *predicateYAML `yaml:"predicate"`
	ExpiresAt       string         `yaml:"expires_at"`
	RecheckInterval string         `yaml:"recheck_interval"`
}

type predicateYAML struct {
	Type        string   `yaml:"type"`
	Kind        string   `yaml:"kind"`
	Ref         string   `yaml:"ref"`
	State       string   `yaml:"state"`
	States      []string `yaml:"states"`
	Check       string   `yaml:"check"`
	Present     *bool    `yaml:"present"`
	Scope       string   `yaml:"scope"`
	Resource    string   `yaml:"resource"`
	Condition   string   `yaml:"condition"`
	Fingerprint string   `yaml:"fingerprint"`
}

func SignalFromComment(body string, commentURL string, repo string) (*Signal, bool) {
	content, ok := LastStatusBlock(body)
	if !ok {
		return nil, false
	}

	block, err := ParseStatusBlock(content, repo)
	if err != nil {
		return &Signal{
			Source:     SourceStructured,
			CommentURL: strings.TrimSpace(commentURL),
			Invalid: &Invalid{
				Hash:    ContentHash(content),
				Message: err.Error(),
				Content: content,
			},
		}, true
	}
	block.CommentURL = strings.TrimSpace(commentURL)
	return block, true
}

func LastStatusBlock(body string) (string, bool) {
	var last string
	found := false
	inFence := false
	fenceChar := byte(0)
	fenceLen := 0
	lines := []string{}

	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if !inFence {
			char, length, ok := statusFenceOpening(trimmed)
			if !ok {
				continue
			}
			inFence = true
			fenceChar = char
			fenceLen = length
			lines = lines[:0]
			continue
		}
		if statusFenceClosing(trimmed, fenceChar, fenceLen) {
			last = strings.Join(lines, "\n")
			found = true
			inFence = false
			continue
		}
		lines = append(lines, line)
	}

	return last, found
}

func ParseStatusBlock(content string, repo string) (*Signal, error) {
	var raw statusBlockYAML
	decoder := yaml.NewDecoder(strings.NewReader(content))
	decoder.KnownFields(true)
	if err := decoder.Decode(&raw); err != nil {
		return nil, fmt.Errorf("parse detent-status YAML: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return nil, errors.New("parse detent-status YAML: multiple YAML documents are not supported")
	} else if !errors.Is(err, io.EOF) {
		return nil, errors.New("parse detent-status YAML: multiple YAML documents are not supported")
	}

	problems := []string{}
	if raw.Schema != 1 {
		problems = append(problems, "schema must be 1")
	}
	raw.Status = strings.TrimSpace(raw.Status)
	switch raw.Status {
	case StatusInProgress, StatusBlocked, StatusComplete:
	default:
		if raw.Status == "" {
			problems = append(problems, "status must be one of in_progress, blocked, complete")
		} else {
			problems = append(problems, fmt.Sprintf("status %q must be one of in_progress, blocked, complete", raw.Status))
		}
	}

	humanAction := ""
	if raw.HumanAction != nil {
		humanAction = strings.TrimSpace(*raw.HumanAction)
	}
	reasonCode := normalizeReasonCode(raw.ReasonCode)
	blockers := make([]Blocker, 0, len(raw.Blockers))
	for index, blocker := range raw.Blockers {
		ref := strings.TrimSpace(blocker.Ref)
		reason := strings.TrimSpace(blocker.Reason)
		owner := normalizeToken(blocker.Owner)
		predicate, predicateProblems := normalizePredicate(blocker.Predicate, ref, repo)
		for _, problem := range predicateProblems {
			problems = append(problems, fmt.Sprintf("blockers[%d].%s", index, problem))
		}
		identifier := ""
		if ref != "" {
			parsed, err := ParseRef(ref, repo)
			if err != nil {
				problems = append(problems, fmt.Sprintf("blockers[%d].ref %q must be #N or owner/repo#N", index, ref))
			} else {
				identifier = parsed
			}
		}
		if predicate == nil && ref != "" {
			predicate = &Predicate{Type: PredicateIssueState, Ref: ref, Identifier: identifier}
		}
		if owner == "" {
			owner = BlockerOwnerHuman
			if predicate != nil {
				owner = BlockerOwnerOrchestrator
			}
		}
		if owner != BlockerOwnerOrchestrator && owner != BlockerOwnerHuman {
			problems = append(problems, fmt.Sprintf("blockers[%d].owner %q must be orchestrator or human", index, strings.TrimSpace(blocker.Owner)))
		}
		expiresAt, expiryProblem := parseExpiry(blocker.ExpiresAt)
		if expiryProblem != "" {
			problems = append(problems, fmt.Sprintf("blockers[%d].expires_at %s", index, expiryProblem))
		}
		recheckInterval, intervalProblem := parseRecheckInterval(blocker.RecheckInterval, predicate != nil)
		if intervalProblem != "" {
			problems = append(problems, fmt.Sprintf("blockers[%d].recheck_interval %s", index, intervalProblem))
		}
		if predicate == nil && reason == "" {
			problems = append(problems, fmt.Sprintf("blockers[%d] requires a ref, predicate, or reason", index))
		}
		blockers = append(blockers, Blocker{
			Ref:             ref,
			Identifier:      identifier,
			Reason:          reason,
			Owner:           owner,
			Predicate:       predicate,
			ExpiresAt:       expiresAt,
			RecheckInterval: recheckInterval,
			Unverifiable:    predicate == nil,
		})
	}
	if raw.Status == StatusBlocked && len(blockers) == 0 && humanAction == "" && reasonCode == "" {
		problems = append(problems, "status blocked requires at least one blocker ref, human_action, or reason_code")
	}
	if raw.Status != StatusBlocked && reasonCode != "" {
		problems = append(problems, "reason_code is only valid when status is blocked")
	}
	fields := make(map[string]string, len(raw.Fields))
	fieldNames := make([]string, 0, len(raw.Fields))
	for name := range raw.Fields {
		fieldNames = append(fieldNames, name)
	}
	sort.Strings(fieldNames)
	for _, name := range fieldNames {
		value := strings.TrimSpace(raw.Fields[name])
		name = strings.TrimSpace(name)
		if name == "" {
			problems = append(problems, "fields must not contain a blank field name")
			continue
		}
		if value == "" {
			problems = append(problems, fmt.Sprintf("fields[%q] must not be blank", name))
			continue
		}
		fields[name] = value
	}
	if len(fields) == 0 {
		fields = nil
	}
	if len(problems) > 0 {
		return nil, errors.New(strings.Join(problems, "; "))
	}

	return &Signal{
		Source:      SourceStructured,
		Status:      raw.Status,
		ReasonCode:  reasonCode,
		Blockers:    blockers,
		HumanAction: humanAction,
		Fields:      fields,
	}, nil
}

func normalizePredicate(raw *predicateYAML, blockerRef string, repo string) (*Predicate, []string) {
	if raw == nil {
		return nil, nil
	}
	problems := []string{}
	predicateType := normalizePredicateType(raw.Type)
	kind := normalizePredicateType(raw.Kind)
	if predicateType == "" {
		predicateType = kind
	} else if kind != "" && kind != predicateType {
		problems = append(problems, "predicate type and kind must match")
	}
	ref := strings.TrimSpace(raw.Ref)
	if ref == "" {
		ref = strings.TrimSpace(blockerRef)
	}
	identifier := ""
	if ref != "" {
		parsed, err := ParseRef(ref, repo)
		if err != nil {
			problems = append(problems, fmt.Sprintf("predicate.ref %q must be #N or owner/repo#N", ref))
		} else {
			identifier = parsed
		}
	}
	states := append([]string(nil), raw.States...)
	if state := strings.TrimSpace(raw.State); state != "" {
		states = append(states, state)
	}
	for index := range states {
		states[index] = normalizeToken(states[index])
	}
	states = uniqueNonBlank(states)
	predicate := &Predicate{
		Type:        predicateType,
		Ref:         ref,
		Identifier:  identifier,
		States:      states,
		Check:       strings.TrimSpace(raw.Check),
		Present:     cloneBool(raw.Present),
		Scope:       normalizeToken(raw.Scope),
		Resource:    strings.TrimSpace(raw.Resource),
		Condition:   normalizeToken(raw.Condition),
		Fingerprint: strings.TrimSpace(raw.Fingerprint),
	}
	switch predicate.Type {
	case PredicateIssueState:
		if predicate.Identifier == "" {
			problems = append(problems, "predicate.ref is required for issue_state")
		}
	case PredicatePullRequestState:
		if len(predicate.States) == 0 {
			problems = append(problems, "predicate.state or predicate.states is required for pull_request_state")
		}
	case PredicateCheckPresence:
		if predicate.Check == "" {
			problems = append(problems, "predicate.check is required for check_presence")
		}
		if predicate.Present == nil {
			problems = append(problems, "predicate.present is required for check_presence")
		}
	case PredicateBudgetCapacity:
		switch predicate.Scope {
		case "daily", "daily_budget":
			predicate.Scope = "daily_budget"
		case "issue", "issue_budget":
			predicate.Scope = "issue_budget"
		case "global", "global_capacity":
			predicate.Scope = "global_capacity"
		case "backend", "backend_capacity":
			predicate.Scope = "backend_capacity"
		default:
			problems = append(problems, "predicate.scope must be daily_budget, issue_budget, global_capacity, or backend_capacity")
		}
		if predicate.Condition == "" {
			predicate.Condition = "exhausted"
		}
		switch predicate.Condition {
		case "exhausted", "full", "unavailable":
			predicate.Condition = "exhausted"
		case "available":
		default:
			problems = append(problems, "predicate.condition must be exhausted or available")
		}
	case PredicateConfigFingerprint:
		if predicate.Fingerprint == "" {
			problems = append(problems, "predicate.fingerprint is required for config_fingerprint")
		}
	default:
		problems = append(problems, fmt.Sprintf("predicate.type %q must be issue_state, pull_request_state, check_presence, budget_capacity, or config_fingerprint", predicate.Type))
	}
	return predicate, problems
}

func normalizePredicateType(value string) string {
	switch normalizeToken(value) {
	case "issue_ref", "issue_ref_state":
		return PredicateIssueState
	case "pr_state":
		return PredicatePullRequestState
	case "check_present":
		return PredicateCheckPresence
	case "budget_condition", "capacity_condition":
		return PredicateBudgetCapacity
	case "config":
		return PredicateConfigFingerprint
	default:
		return normalizeToken(value)
	}
}

func parseExpiry(value string) (*time.Time, string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, ""
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return nil, "must be an RFC3339 timestamp"
	}
	parsed = parsed.UTC()
	return &parsed, ""
}

func parseRecheckInterval(value string, typed bool) (string, string) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" && typed {
		return "tick", ""
	}
	if value == "" || value == "tick" {
		return value, ""
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		return "", "must be tick or a positive Go duration"
	}
	return duration.String(), ""
}

func normalizeToken(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.ReplaceAll(value, "-", "_")
	return strings.Join(strings.Fields(value), "_")
}

func uniqueNonBlank(values []string) []string {
	out := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func cloneBool(value *bool) *bool {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func ParseRef(ref string, repo string) (string, error) {
	ref = strings.TrimSpace(ref)
	matches := refPattern.FindStringSubmatch(ref)
	if len(matches) != 3 {
		return "", fmt.Errorf("invalid ref %q", ref)
	}
	if matches[1] != "" {
		return matches[1] + "#" + matches[2], nil
	}
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return "#" + matches[2], nil
	}
	return repo + "#" + matches[2], nil
}

func Reason(signal *Signal) string {
	if signal == nil {
		return ""
	}
	parts := make([]string, 0, len(signal.Blockers)+1)
	if reasonCode := normalizeReasonCode(signal.ReasonCode); reasonCode != "" {
		parts = append(parts, reasonCode)
	}
	if humanAction := strings.TrimSpace(signal.HumanAction); humanAction != "" {
		parts = append(parts, humanAction)
	}
	for _, blocker := range signal.Blockers {
		ref := strings.TrimSpace(blocker.Identifier)
		if ref == "" {
			ref = strings.TrimSpace(blocker.Ref)
		}
		if ref == "" && blocker.Predicate != nil {
			ref = blocker.Predicate.Type
		}
		if ref == "" {
			if reason := strings.TrimSpace(blocker.Reason); reason != "" {
				parts = append(parts, reason)
			}
			continue
		}
		if reason := strings.TrimSpace(blocker.Reason); reason != "" {
			parts = append(parts, ref+": "+reason)
			continue
		}
		parts = append(parts, ref)
	}
	return strings.Join(parts, "; ")
}

func OperationalCompletion(signal *Signal) (string, bool) {
	if signal == nil || signal.Invalid != nil || signal.Source != SourceStructured ||
		strings.TrimSpace(signal.Status) != StatusComplete ||
		strings.TrimSpace(signal.HumanAction) != "" || len(signal.Blockers) > 0 {
		return "", false
	}
	if normalizeToken(signal.Fields[FieldCompletionKind]) != CompletionOperational {
		return "", false
	}
	evidence := strings.TrimSpace(signal.Fields[FieldCompletionEvidence])
	return evidence, evidence != ""
}

func normalizeReasonCode(reasonCode string) string {
	reasonCode = strings.ToLower(strings.TrimSpace(reasonCode))
	reasonCode = strings.ReplaceAll(reasonCode, "-", "_")
	return strings.Join(strings.Fields(reasonCode), "_")
}

func CloneSignal(signal *Signal) *Signal {
	if signal == nil {
		return nil
	}
	cloned := *signal
	cloned.Blockers = make([]Blocker, len(signal.Blockers))
	for index, blocker := range signal.Blockers {
		cloned.Blockers[index] = blocker
		if blocker.Predicate != nil {
			predicate := *blocker.Predicate
			predicate.States = append([]string(nil), blocker.Predicate.States...)
			predicate.Present = cloneBool(blocker.Predicate.Present)
			cloned.Blockers[index].Predicate = &predicate
		}
		if blocker.ExpiresAt != nil {
			expiresAt := *blocker.ExpiresAt
			cloned.Blockers[index].ExpiresAt = &expiresAt
		}
	}
	if signal.RecordedAt != nil {
		recordedAt := *signal.RecordedAt
		cloned.RecordedAt = &recordedAt
	}
	if signal.Fields != nil {
		cloned.Fields = make(map[string]string, len(signal.Fields))
		for name, value := range signal.Fields {
			cloned.Fields[name] = value
		}
	}
	if signal.Invalid != nil {
		invalid := *signal.Invalid
		cloned.Invalid = &invalid
	}
	return &cloned
}

func ContentHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])[:16]
}

func statusFenceOpening(line string) (byte, int, bool) {
	if len(line) < 3 || (line[0] != '`' && line[0] != '~') {
		return 0, 0, false
	}
	char := line[0]
	length := 0
	for length < len(line) && line[length] == char {
		length++
	}
	if length < 3 {
		return 0, 0, false
	}
	fields := strings.Fields(strings.TrimSpace(line[length:]))
	if len(fields) == 0 || fields[0] != "detent-status" {
		return 0, 0, false
	}
	return char, length, true
}

func statusFenceClosing(line string, char byte, length int) bool {
	if len(line) < length {
		return false
	}
	index := 0
	for index < len(line) && line[index] == char {
		index++
	}
	return index >= length && strings.TrimSpace(line[index:]) == ""
}
