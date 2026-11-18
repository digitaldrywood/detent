package hubrecords

import (
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func ValidateNativeEditContent(title, body string, labels, previousLabels, assignees, previousAssignees []string, priority *int) error {
	if len(labels) > 100 || len(assignees) > 100 {
		return hubapi.NativePublicInvalid("At most 100 labels or assignees are supported")
	}
	added := func(values, previous []string) []string {
		return slices.DeleteFunc(slices.Clone(values), func(value string) bool { return slices.Contains(previous, value) })
	}
	return ValidateNativeContent(title, body, added(labels, previousLabels), added(assignees, previousAssignees), priority)
}

func ValidateNativeContent(title, body string, labels, assignees []string, priority *int) error {
	if strings.TrimSpace(title) == "" || len(title) > 500 || len(body) > 256<<10 {
		return hubapi.NativePublicInvalid("Title must contain 1 to 500 bytes and body at most 256 KiB")
	}
	if priority != nil && (*priority < 0 || *priority > 3) {
		return hubapi.NativePublicInvalid("Priority must be between 0 and 3")
	}
	for _, values := range [][]string{labels, assignees} {
		if len(values) > 100 {
			return hubapi.NativePublicInvalid("At most 100 labels or assignees are supported")
		}
		for _, value := range values {
			if strings.TrimSpace(value) == "" || len(value) > 200 {
				return hubapi.NativePublicInvalid("Labels and assignees must contain 1 to 200 bytes")
			}
		}
	}
	return nil
}

func ValidateNativeIssueType(value string, required bool) error {
	if value == "" && !required || value == "bug" || value == "change_request" {
		return nil
	}
	return &hubapi.NativeError{Code: "invalid_request", Message: "type must be bug or change_request", Status: http.StatusUnprocessableEntity, PublicMessage: true}
}

func RequireNativeEdit(issue tracker.NativeIssue, expected tracker.Revision) error {
	if issue.Profile != "native" {
		return hubapi.NativePublicInvalid("Compatibility project content is externally owned")
	}
	if expected <= 0 {
		return hubapi.NativePublicInvalid("Expected revision must be positive")
	}
	if expected != issue.Revision {
		return hubapi.NativeConflict(issue.Revision)
	}
	return nil
}

func ValidateNativeWorkflowTransition(states []tracker.NativeState, current, target string, scope hubapi.APIScope) error {
	allowed := false
	for _, state := range states {
		if state.Name == target && state.OperatorOnly && scope == hubapi.APIScopeWorker {
			return hubapi.NativePublicInvalid("Workflow target requires an operator")
		}
		if state.Name == current && slices.Contains(state.Transitions, target) {
			allowed = true
		}
	}
	if !allowed {
		return &hubapi.NativeError{Code: "transition_not_allowed", Message: "Workflow transition is not allowed", Status: http.StatusUnprocessableEntity, PublicMessage: true}
	}
	return nil
}

func ValidateNativeIssueQuery(params url.Values) error {
	single := url.Values{}
	for key, values := range params {
		switch key {
		case "type":
			if len(values) > 2 {
				return hubapi.NativePublicInvalid("type must be bug or change_request")
			}
			for _, value := range values {
				if err := ValidateNativeIssueType(value, true); err != nil {
					return err
				}
			}
		case "verification":
			if len(values) > 2 {
				return NativeVerificationFilterInvalid()
			}
			for _, value := range values {
				if value != tracker.VerificationPending && value != tracker.VerificationVerified {
					return NativeVerificationFilterInvalid()
				}
			}
		case "state", "label", "assignee", "priority":
			if len(values) > 32 || len(strings.Join(values, "\x00")) > 4096 {
				return hubapi.NativePublicInvalid("Query contains an unsupported field or value")
			}
			if key == "label" || key == "assignee" {
				for _, value := range values {
					if len(value) > viewstore.MaxFilterValueBytes {
						return hubapi.NativePublicInvalid("Label and assignee filters accept values up to 512 bytes")
					}
				}
			}
		default:
			single[key] = values
		}
	}
	return hubapi.ValidateNativeQuery(single, "include", "archived", "q", "open", "fingerprint", "completed_window", "completed_from", "completed_to", "sort", "created_after", "created_before", "updated_after", "created_by", "fields")
}

func NativeVerificationFilterInvalid() error {
	return &hubapi.NativeError{Code: "invalid_request", Message: "verification must be pending or verified", Status: http.StatusUnprocessableEntity, PublicMessage: true}
}

func ValidateNativeStates(states []tracker.NativeState) error {
	if err := policy.ValidateStates(states); err != nil {
		return hubapi.NativeWorkflowInvalid(err.Error())
	}
	return nil
}
