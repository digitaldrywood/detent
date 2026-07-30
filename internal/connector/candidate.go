package connector

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

const DefaultCandidatePageSize = 100

var (
	ErrCandidateSelectorUnsupported = errors.New("candidate selector is not supported")
	ErrInvalidCandidateRequest      = errors.New("candidate request is invalid")
)

type CandidateSelector string

const (
	CandidateSelectorStates CandidateSelector = "states"
	CandidateSelectorLabels CandidateSelector = "labels"
)

type CandidateCapabilities struct {
	Selectors []CandidateSelector `json:"selectors" yaml:"selectors"`
}

func (c CandidateCapabilities) Supports(selector CandidateSelector) bool {
	for _, candidate := range c.Selectors {
		if candidate == selector {
			return true
		}
	}
	return false
}

func CandidateCapabilitiesFor(backend Backend, statusSource string) CandidateCapabilities {
	switch backend {
	case BackendGitHub:
		switch strings.ToLower(strings.TrimSpace(statusSource)) {
		case "", "project_v2":
			return statesCandidateCapabilities()
		case "issue_field", "label":
			return statesAndLabelsCandidateCapabilities()
		default:
			return CandidateCapabilities{}
		}
	case BackendGitHubLocal, BackendLocalSQLite, BackendMemory:
		return statesAndLabelsCandidateCapabilities()
	default:
		return CandidateCapabilities{}
	}
}

type CandidateRequest struct {
	Selector CandidateSelector `json:"selector" yaml:"selector"`
	States   []string          `json:"states,omitempty" yaml:"states,omitempty"`
	Labels   []string          `json:"labels,omitempty" yaml:"labels,omitempty"`
	Limit    int               `json:"limit" yaml:"limit"`
	PageSize int               `json:"page_size,omitempty" yaml:"page_size,omitempty"`
}

func (r CandidateRequest) Validate(capabilities CandidateCapabilities) error {
	if !capabilities.Supports(r.Selector) {
		return fmt.Errorf("%w: %s", ErrCandidateSelectorUnsupported, r.Selector)
	}
	if r.Limit <= 0 {
		return fmt.Errorf("%w: limit must be greater than zero", ErrInvalidCandidateRequest)
	}
	if r.PageSize < 0 {
		return fmt.Errorf("%w: page size must not be negative", ErrInvalidCandidateRequest)
	}
	switch r.Selector {
	case CandidateSelectorStates:
		if len(normalizedCandidateStates(r.States)) == 0 {
			return fmt.Errorf("%w: states selector requires at least one state", ErrInvalidCandidateRequest)
		}
	case CandidateSelectorLabels:
		if len(normalizedCandidateLabels(r.Labels)) == 0 {
			return fmt.Errorf("%w: labels selector requires at least one label", ErrInvalidCandidateRequest)
		}
	default:
		return fmt.Errorf("%w: %s", ErrCandidateSelectorUnsupported, r.Selector)
	}
	return nil
}

func (r CandidateRequest) EffectivePageSize() int {
	if r.PageSize > 0 {
		return r.PageSize
	}
	return DefaultCandidatePageSize
}

func (r CandidateRequest) ProbeLimit() int {
	if r.Limit == int(^uint(0)>>1) {
		return r.Limit
	}
	return r.Limit + 1
}

type CandidateResult struct {
	Issues    []Issue `json:"issues" yaml:"issues"`
	PagesRead int     `json:"pages_read" yaml:"pages_read"`
	Truncated bool    `json:"truncated" yaml:"truncated"`
}

type CandidateReader interface {
	CandidateCapabilities() CandidateCapabilities
	ReadCandidates(context.Context, CandidateRequest) (CandidateResult, error)
}

func NewCandidateResult(issues []Issue, request CandidateRequest, pagesRead int, incomplete bool) CandidateResult {
	issues = append([]Issue(nil), issues...)
	SortCandidateIssues(issues)
	truncated := incomplete || len(issues) > request.Limit
	if len(issues) > request.Limit {
		issues = issues[:request.Limit]
	}
	return CandidateResult{
		Issues:    issues,
		PagesRead: pagesRead,
		Truncated: truncated,
	}
}

func SortCandidateIssues(issues []Issue) {
	sort.SliceStable(issues, func(i, j int) bool {
		left := issues[i]
		right := issues[j]
		if left.CreatedAt != nil && right.CreatedAt != nil && !left.CreatedAt.Equal(*right.CreatedAt) {
			return left.CreatedAt.Before(*right.CreatedAt)
		}
		if left.CreatedAt != nil && right.CreatedAt == nil {
			return true
		}
		if left.CreatedAt == nil && right.CreatedAt != nil {
			return false
		}
		leftIdentifier := strings.TrimSpace(left.Identifier)
		rightIdentifier := strings.TrimSpace(right.Identifier)
		if comparison := strings.Compare(strings.ToLower(leftIdentifier), strings.ToLower(rightIdentifier)); comparison != 0 {
			return comparison < 0
		}
		if leftIdentifier != rightIdentifier {
			return leftIdentifier < rightIdentifier
		}
		leftID := strings.TrimSpace(left.ID)
		rightID := strings.TrimSpace(right.ID)
		if leftID != rightID {
			return leftID < rightID
		}
		if left.Number != right.Number {
			return left.Number < right.Number
		}
		leftURL := strings.TrimSpace(left.URL)
		rightURL := strings.TrimSpace(right.URL)
		if leftURL != rightURL {
			return leftURL < rightURL
		}
		return strings.TrimSpace(left.Title) < strings.TrimSpace(right.Title)
	})
}

func statesCandidateCapabilities() CandidateCapabilities {
	return CandidateCapabilities{Selectors: []CandidateSelector{CandidateSelectorStates}}
}

func statesAndLabelsCandidateCapabilities() CandidateCapabilities {
	return CandidateCapabilities{Selectors: []CandidateSelector{CandidateSelectorStates, CandidateSelectorLabels}}
}

func normalizedCandidateStates(states []string) []string {
	return normalizedCandidateValues(states)
}

func normalizedCandidateLabels(labels []string) []string {
	return normalizedCandidateValues(labels)
}

func normalizedCandidateValues(values []string) []string {
	normalized := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		key := strings.ToLower(value)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		normalized = append(normalized, value)
	}
	return normalized
}
