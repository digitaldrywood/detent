package workitem

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/url"
	"strings"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
)

type ErrorCode string

const (
	CodeMissingTitle         ErrorCode = "missing_title"
	CodeMissingDescription   ErrorCode = "missing_description"
	CodeInvalidState         ErrorCode = "invalid_state"
	CodeInvalidFields        ErrorCode = "invalid_fields"
	CodeDuplicateIdentifier  ErrorCode = "duplicate_identifier"
	CodeUnsupportedTracker   ErrorCode = "unsupported_tracker"
	CodeConnectorUnavailable ErrorCode = "connector_unavailable"
	CodeIdentifierGeneration ErrorCode = "identifier_generation_failed"
	CodeSubmissionFailed     ErrorCode = "submission_failed"
)

type Error struct {
	Code    ErrorCode
	Message string
	Err     error
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if strings.TrimSpace(e.Message) != "" {
		return e.Message
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	return string(e.Code)
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

type Request struct {
	Title         string                 `json:"title"`
	Description   string                 `json:"description"`
	State         string                 `json:"state,omitempty"`
	Labels        []string               `json:"labels,omitempty"`
	Fields        map[string]string      `json:"fields,omitempty"`
	Priority      *int                   `json:"priority,omitempty"`
	ModelOverride string                 `json:"model_override,omitempty"`
	Deliverable   *connector.Deliverable `json:"deliverable,omitempty"`
	Identifier    string                 `json:"identifier,omitempty"`
}

type Response struct {
	ID         string `json:"id"`
	Identifier string `json:"identifier"`
	Number     int    `json:"number,omitempty"`
	URL        string `json:"url"`
}

type Target struct {
	ProjectID           string
	Workflow            workflowconfig.Config
	Connector           connector.Connector
	DashboardURL        string
	Now                 func() time.Time
	IdentifierGenerator func() (string, error)
}

func Create(ctx context.Context, target Target, req Request) (Response, error) {
	title := strings.TrimSpace(req.Title)
	if title == "" {
		return Response{}, &Error{Code: CodeMissingTitle, Message: "title is required"}
	}
	description := strings.TrimSpace(req.Description)
	if description == "" {
		return Response{}, &Error{Code: CodeMissingDescription, Message: "description is required"}
	}
	state, err := configuredState(target.Workflow, req.State)
	if err != nil {
		return Response{}, err
	}
	labels := trimNonBlank(req.Labels)
	fields, err := cleanFields(req.Fields)
	if err != nil {
		return Response{}, err
	}
	if target.Connector == nil {
		return Response{}, &Error{Code: CodeConnectorUnavailable, Message: "connector is unavailable"}
	}
	upserter, ok := target.Connector.(connector.IssueUpserter)
	if !ok {
		return Response{}, &Error{Code: CodeUnsupportedTracker, Message: "tracker kind does not support runtime work-item creation"}
	}

	identifier, err := submissionIdentifier(ctx, target, req.Identifier)
	if err != nil {
		return Response{}, err
	}
	itemURL := WorkItemURL(target.DashboardURL, target.ProjectID, identifier)
	now := target.now().UTC()
	issue := connector.NewIssue()
	issue.ID = identifier
	issue.Identifier = identifier
	issue.Title = title
	issue.Description = description
	issue.State = state
	issue.Labels = labels
	issue.Fields = fields
	issue.Priority = req.Priority
	issue.URL = itemURL
	issue.ModelOverride = strings.TrimSpace(req.ModelOverride)
	issue.Deliverable = cloneDeliverable(req.Deliverable)
	issue.CreatedAt = &now
	issue.UpdatedAt = &now
	issue.StageUpdatedAt = &now

	if err := upserter.UpsertIssues(ctx, []connector.Issue{issue}); err != nil {
		return Response{}, &Error{Code: CodeSubmissionFailed, Message: "create work item failed", Err: err}
	}
	if created, ok, err := createdIssue(ctx, target.Connector, issue.Identifier); err != nil {
		return Response{}, &Error{Code: CodeSubmissionFailed, Message: "read created work item failed", Err: err}
	} else if ok {
		issue = created
	}
	return Response{ID: issue.ID, Identifier: issue.Identifier, Number: issue.Number, URL: issue.URL}, nil
}

func createdIssue(ctx context.Context, projectConnector connector.Connector, identifier string) (connector.Issue, bool, error) {
	resolver, ok := projectConnector.(connector.IssueReferenceResolver)
	if !ok {
		return connector.Issue{}, false, nil
	}
	issues, err := resolver.FetchIssueStatesByIdentifiers(ctx, []string{identifier})
	if err != nil {
		return connector.Issue{}, false, err
	}
	for _, issue := range issues {
		if strings.EqualFold(strings.TrimSpace(issue.Identifier), strings.TrimSpace(identifier)) {
			return issue, true, nil
		}
	}
	return connector.Issue{}, false, nil
}

func WorkItemURL(base string, projectID string, identifier string) string {
	projectID = strings.TrimSpace(projectID)
	identifier = strings.TrimSpace(identifier)
	path := "/kanban"
	if projectID != "" {
		path = "/projects/" + url.PathEscape(projectID) + "/kanban"
	}
	if projectID != "" && identifier != "" {
		path = "/projects/" + url.PathEscape(projectID) + "/issues/" + url.PathEscape(identifier)
	}
	base = strings.TrimSpace(base)
	if base == "" {
		return path
	}
	parsed, err := url.Parse(base)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return path
	}
	decodedPath, err := url.PathUnescape(path)
	if err != nil {
		return path
	}
	parsed.RawPath = path
	parsed.Path = decodedPath
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}

func configuredState(cfg workflowconfig.Config, requested string) (string, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		for _, state := range cfg.Tracker.ActiveStates {
			if state = strings.TrimSpace(state); state != "" {
				return state, nil
			}
		}
		return "", &Error{Code: CodeInvalidState, Message: "state is required because tracker.active_states is empty"}
	}
	for _, state := range configuredStates(cfg) {
		if strings.EqualFold(strings.TrimSpace(state), requested) {
			return strings.TrimSpace(state), nil
		}
	}
	return "", &Error{Code: CodeInvalidState, Message: "state must be one of the configured tracker states"}
}

func configuredStates(cfg workflowconfig.Config) []string {
	out := []string{}
	out = append(out, cfg.Tracker.ActiveStates...)
	out = append(out, cfg.Tracker.ObservedStates...)
	out = append(out, cfg.Tracker.TerminalStates...)
	return out
}

func submissionIdentifier(ctx context.Context, target Target, requested string) (string, error) {
	requested = strings.TrimSpace(requested)
	if requested != "" {
		if err := ensureIdentifierAvailable(ctx, target.Connector, requested); err != nil {
			return "", err
		}
		return requested, nil
	}
	for range 3 {
		identifier, err := target.identifier()
		if err != nil {
			return "", &Error{Code: CodeIdentifierGeneration, Message: "generate work item identifier failed", Err: err}
		}
		identifier = strings.TrimSpace(identifier)
		if identifier == "" {
			return "", &Error{Code: CodeIdentifierGeneration, Message: "generate work item identifier failed"}
		}
		if err := ensureIdentifierAvailable(ctx, target.Connector, identifier); err != nil {
			var itemErr *Error
			if errors.As(err, &itemErr) && itemErr.Code == CodeDuplicateIdentifier {
				continue
			}
			return "", err
		}
		return identifier, nil
	}
	return "", &Error{Code: CodeDuplicateIdentifier, Message: "identifier already exists"}
}

func ensureIdentifierAvailable(ctx context.Context, projectConnector connector.Connector, identifier string) error {
	resolver, ok := projectConnector.(connector.IssueReferenceResolver)
	if !ok {
		return nil
	}
	issues, err := resolver.FetchIssueStatesByIdentifiers(ctx, []string{identifier})
	if err != nil {
		return &Error{Code: CodeSubmissionFailed, Message: "check duplicate identifier failed", Err: err}
	}
	for _, issue := range issues {
		if strings.EqualFold(strings.TrimSpace(issue.Identifier), identifier) {
			return &Error{Code: CodeDuplicateIdentifier, Message: "identifier already exists"}
		}
	}
	return nil
}

func (target Target) now() time.Time {
	if target.Now != nil {
		return target.Now()
	}
	return time.Now()
}

func (target Target) identifier() (string, error) {
	if target.IdentifierGenerator != nil {
		return target.IdentifierGenerator()
	}
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return "wi-" + hex.EncodeToString(raw[:]), nil
}

func trimNonBlank(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			out = append(out, value)
		}
	}
	return out
}

func cleanFields(fields map[string]string) (map[string]string, error) {
	if fields == nil {
		return map[string]string{}, nil
	}
	out := make(map[string]string, len(fields))
	for key, value := range fields {
		key = strings.TrimSpace(key)
		if key == "" {
			return nil, &Error{Code: CodeInvalidFields, Message: "fields keys must not be blank"}
		}
		out[key] = strings.TrimSpace(value)
	}
	return out, nil
}

func cloneDeliverable(deliverable *connector.Deliverable) *connector.Deliverable {
	if deliverable == nil {
		return nil
	}
	out := *deliverable
	out.Kind = strings.TrimSpace(out.Kind)
	out.Path = strings.TrimSpace(out.Path)
	out.ReviewURL = strings.TrimSpace(out.ReviewURL)
	out.ValidationStatus = strings.TrimSpace(out.ValidationStatus)
	out.ExternalID = strings.TrimSpace(out.ExternalID)
	if len(deliverable.Metadata) > 0 {
		out.Metadata = make(map[string]string, len(deliverable.Metadata))
		for key, value := range deliverable.Metadata {
			key = strings.TrimSpace(key)
			if key != "" {
				out.Metadata[key] = strings.TrimSpace(value)
			}
		}
	}
	return &out
}
