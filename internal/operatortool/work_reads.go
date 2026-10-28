package operatortool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/explain"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const (
	WorkList            = "work_list"
	WorkItem            = "work_item"
	WorkConfig          = "work_config"
	HealthFindings      = "health_findings"
	WorkComments        = "work_comments"
	WorkPRComments      = "work_pr_comments"
	WorkHistory         = "work_history"
	WorkVersion         = "work_version"
	WorkRelationships   = "work_relationships"
	WorkRuns            = "work_runs"
	WorkReferences      = "work_references"
	WorkExport          = "work_export"
	WorkBulkExport      = "work_bulk_export"
	BoardActivity       = "board_activity"
	BoardReceipt        = "board_receipt"
	BoardSession        = "board_session"
	BoardSessionHistory = "board_session_history"
	WorkAttemptReceipt  = "work_attempt_receipt"
	GitHubScopeTimings  = "github_scope_timings"
)

var ErrReadUnavailable = errors.New("work read service is unavailable")
var ErrProjectScopeRequired = errors.New("work item belongs to another project outside this credential's scope; use a credential with that project's scope")

type ReadUnavailableError struct {
	Err error
}

func (e *ReadUnavailableError) Error() string {
	if e.Err == nil {
		return ErrReadUnavailable.Error()
	}
	return "read work: " + e.Err.Error()
}

func (e *ReadUnavailableError) Unwrap() error {
	return e.Err
}

func (e *ReadUnavailableError) Is(target error) bool {
	return target == ErrReadUnavailable
}

const WorkListPageBytes = MaxResultBytes / 4

const WorkHistoryPageBytes = MaxResultBytes / 4

// WorkReadRequest contains application selectors only. Organization and principal
// always come from the connection. Each operation permits only its own fields.
type WorkReadRequest struct {
	ArchiveURL      bool     `json:"archive_url,omitempty"`
	View            string   `json:"view,omitempty"`
	ProjectID       string   `json:"project_id"`
	Since           string   `json:"since,omitempty"`
	Reference       string   `json:"reference,omitempty"`
	Query           string   `json:"query,omitempty"`
	CreatedAfter    string   `json:"created_after,omitempty"`
	CreatedBefore   string   `json:"created_before,omitempty"`
	UpdatedAfter    string   `json:"updated_after,omitempty"`
	Fingerprint     string   `json:"fingerprint,omitempty"`
	Open            *bool    `json:"open,omitempty"`
	Type            string   `json:"type,omitempty"`
	Verification    string   `json:"verification,omitempty"`
	State           string   `json:"state,omitempty"`
	Label           string   `json:"label,omitempty"`
	States          []string `json:"states,omitempty"`
	Labels          []string `json:"labels,omitempty"`
	CreatedBy       string   `json:"created_by,omitempty"`
	Assignee        string   `json:"assignee,omitempty"`
	Assignees       []string `json:"assignees,omitempty"`
	Priority        *int     `json:"priority,omitempty"`
	Priorities      []int    `json:"priorities,omitempty"`
	Archived        string   `json:"archived,omitempty"`
	Include         []string `json:"include,omitempty"`
	Cursor          string   `json:"cursor,omitempty"`
	Offset          int      `json:"offset,omitempty"`
	Limit           int      `json:"limit,omitempty"`
	CommentID       string   `json:"comment_id,omitempty"`
	Revision        int64    `json:"revision,omitempty"`
	AttemptID       int64    `json:"attempt_id,omitempty"`
	NativeAttemptID string   `json:"native_attempt_id,omitempty"`
	RunnerID        string   `json:"runner_id,omitempty"`
}

// WorkReader is implemented by dashboard application adapters, not transports.
// Native adapters bind the freshly resolved principal to their existing reads.
type WorkReader interface {
	ReadWork(context.Context, string, WorkReadRequest) (Result, error)
}

// WorkReadResult preserves source freshness separately from when the application
// read was made. Data is a concrete application model at every call site.
type WorkReadResult[T any] struct {
	ProjectID   string                     `json:"project_id"`
	Reference   string                     `json:"reference,omitempty"`
	URL         string                     `json:"url,omitempty"`
	GeneratedAt time.Time                  `json:"generated_at"`
	Freshness   explain.SourceState        `json:"freshness"`
	ExpiresAt   *time.Time                 `json:"expires_at,omitempty"`
	Data        T                          `json:"data"`
	Archive     *tracker.ArchiveDescriptor `json:"archive,omitempty"`
}

func EncodeResult(value any) (Result, error) { return encodeResult(value) }

func WorkReadCatalog() []Definition {
	var definitions []Definition
	for _, spec := range []struct{ name, description, fields string }{
		{WorkList, "List or search work items in one authorized project. Native projects support ranked stemmed/prefix OR search with matched snippets, RFC3339 created_after (inclusive), created_before (exclusive), updated_after (inclusive), type (bug or change_request), verification (pending lists Done issues awaiting the filer's verify_fix; verified lists confirmed fixes), created_by (principal ID or me for a human caller), archived selection, OR selections within each filter, and workspace/work projections. Explicit include=[\"summary\"] returns only lane-scan fields and the latest transition reason. Combine created_by=me with verification=pending for the caller's issues to verify. GitHub snapshots refuse unsupported native selectors.", "query created_after created_before updated_after fingerprint open type verification state label states labels created_by assignee assignees priority priorities archived include cursor offset limit"},
		{HealthFindings, "Read project-scoped health findings and the detector last tick, with bounded pagination.", "state since cursor limit"},
		{WorkConfig, "Read configured project lanes, priorities, available labels and effective Reporter filing templates by issue type.", ""},
		{WorkItem, "Read a work item's detail and source freshness. For native retained evidence use view=recovery, then follow data.next_cursor with the same view and reference. Each response contains one base64 JSON record fragment with section, record_id, original version_id, byte offset, total_bytes and SHA-256; section=complete ends traversal. This includes full review text, command output, frozen historical policy and discussion omitted from bounded recovery or get_change. Decode fragments individually; do not assemble the whole history.", "reference view cursor"},
		{WorkComments, "Read a bounded page of work-item comments with edit revisions. Native pages may return fewer than limit items to fit the byte budget; follow next_cursor until absent. Native projects require cursor paging; offset is supported only for GitHub snapshots. A single comment too large for a page requires work_bulk_export.", "reference cursor offset limit"},
		{WorkPRComments, "Read a bounded page of the work item's linked PR discussion through the existing forge reader. The application selects the repository and PR; deployments without a PR discussion reader are unavailable.", "reference offset limit"},
		{WorkHistory, "Read durable application history, identifying the tracker or workflow source; separate from recent_activity's live snapshot.", "reference cursor offset limit"},
		{WorkVersion, "Read a saved native issue or comment revision.", "reference comment_id revision"},
		{WorkRelationships, "Read authorized dependencies, relationships and external references.", "reference"},
		{WorkRuns, "Read bounded work-item run summaries. Full native runtime detail is available through work_attempt_receipt, explain_item and board_session_history.", "reference cursor offset limit"},
		{WorkReferences, "Read PR, change, diff and artifact identifiers and links; detail is owned by the review/artifact tools.", "reference cursor offset limit"},
		{WorkBulkExport, "Export complete stored work items and comments in one authorized native project. Cursor pages default to 500 items with a 16 MiB byte budget; total and truncated describe continuation. Reads never refresh linked sources.", "states open labels archived cursor limit"},
		{WorkExport, "Export the native work-item application resource as JSON.", "reference"},
		{BoardActivity, "Read the board activity model, with durable and live event provenance.", "reference cursor limit"},
		{BoardReceipt, "Read a work item's recorded receipt and runtime evidence; efficiency requires its application owner.", "reference"},
		{BoardSession, "Read the current or latest work-item session.", "reference"},
		{BoardSessionHistory, "Read a bounded page of the work item's persisted session rollout or native instruction activity, including coverage limits.", "reference attempt_id native_attempt_id offset limit"},
		{WorkAttemptReceipt, "Read an attempt receipt and bounded local/scheduled check comparisons owned by this work item; select a local attempt_id or a native_attempt_id.", "reference attempt_id native_attempt_id"},
		{GitHubScopeTimings, "Read bounded recorded GitHub HTTP and inclusive token timings for one native runner attempt. Reads make no upstream requests; HTTP sums may exceed wall and token time can contain installation HTTP. Unknown boundaries remain unavailable.", "reference native_attempt_id runner_id"},
	} {
		properties := map[string]any{"project_id": map[string]any{"type": "string", "minLength": 1, "maxLength": 256}}
		required := []string{"project_id"}
		switch spec.name {
		case WorkHistory, WorkRuns, WorkVersion, WorkAttemptReceipt, BoardSession, BoardSessionHistory, WorkItem:
			spec.fields += " archive_url"
		}
		for field := range strings.FieldsSeq(spec.fields) {
			switch field {
			case "type":
				properties[field] = map[string]any{"type": "string", "enum": []string{"bug", "change_request"}}
			case "verification":
				properties[field] = map[string]any{"type": "string", "enum": []string{"pending", "verified"}}
			case "open", "archive_url":
				properties[field] = map[string]any{"type": "boolean"}
			case "limit":
				properties[field] = map[string]any{"type": "integer", "minimum": 1, "maximum": MaxItemLimit}
			case "offset":
				properties[field] = map[string]any{"type": "integer", "minimum": 0, "maximum": 1000000}
			case "states", "labels", "assignees":
				properties[field] = map[string]any{"type": "array", "maxItems": 32, "items": map[string]any{"type": "string", "minLength": 1, "maxLength": 256}}
			case "priority":
				properties[field] = map[string]any{"type": "integer", "minimum": 0, "maximum": 3}
			case "priorities":
				properties[field] = map[string]any{"type": "array", "maxItems": 32, "items": map[string]any{"type": "integer", "minimum": 0, "maximum": 3}}
			case "archived":
				properties[field] = map[string]any{"type": "string", "enum": []string{"true", "false", "all"}}
			case "view":
				properties[field] = map[string]any{"type": "string", "enum": []string{"recovery"}}
			case "include":
				properties[field] = map[string]any{"type": "array", "maxItems": 3, "items": map[string]any{"type": "string", "enum": []string{"workspace", "work", "summary"}}}
			case "revision", "attempt_id":
				properties[field] = map[string]any{"type": "integer", "minimum": 1}
				if field != "attempt_id" {
					required = append(required, field)
				}
			default:
				bound := 256
				if field == "cursor" {
					bound = 4096
				}
				property := map[string]any{"type": "string", "maxLength": bound}
				properties[field] = property
				if field == "reference" || spec.name == GitHubScopeTimings && (field == "native_attempt_id" || field == "runner_id") {
					required = append(required, field)
					property["minLength"] = 1
				}
			}
		}
		if spec.name == WorkBulkExport {
			properties["limit"] = map[string]any{"type": "integer", "minimum": 1, "maximum": WorkBulkExportMaxItems}
		}
		if spec.name == HealthFindings {
			properties["state"] = map[string]any{"type": "string", "enum": []string{"open", "resolved"}}
			properties["limit"] = map[string]any{"type": "integer", "minimum": 1, "maximum": 100}
		}
		shape := map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
		if spec.name == WorkAttemptReceipt {
			shape["oneOf"] = []map[string]any{{"required": []string{"attempt_id"}}, {"required": []string{"native_attempt_id"}}}
		}
		schema, _ := json.Marshal(shape) //nolint:errcheck // The schema contains only JSON primitives, slices and maps.
		d := definition(spec.name, spec.description, string(schema))
		d.Meta = toolset(BoardState)
		d.Annotations.OpenWorld = true
		definitions = append(definitions, d)
	}
	return definitions
}

func IsWorkRead(name string) bool {
	for _, d := range WorkReadCatalog() {
		if d.Name == name {
			return true
		}
	}
	return false
}

func invalidWorkRead(message string) error {
	return &RequestError{Code: "invalid_request", Message: message}
}

func DecodeWorkRead(name string, raw json.RawMessage) (WorkReadRequest, error) {
	var request WorkReadRequest
	if err := decodeArguments(raw, &request); err != nil {
		return request, err
	}
	var definition Definition
	for _, d := range WorkReadCatalog() {
		if d.Name == name {
			definition = d
			break
		}
	}
	if definition.Name == "" {
		return request, ErrUnknownTool
	}
	var schema argumentSchema
	if err := json.Unmarshal(definition.InputSchema, &schema); err != nil {
		return request, ErrInvalidArguments
	}
	var value any
	if json.Unmarshal(raw, &value) != nil || !schema.accepts(value) {
		return request, invalidWorkRead("arguments do not match the " + name + " input schema")
	}
	var fields map[string]json.RawMessage
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &fields); err != nil {
			return request, invalidWorkRead("arguments must be a JSON object")
		}
	}
	for field, value := range fields {
		if string(value) == "null" {
			return request, invalidWorkRead(field + " must not be null; omit it instead")
		}
		if _, ok := schema.Properties[field]; !ok {
			return request, invalidWorkRead(field + " is not a " + name + " argument")
		}
	}
	for _, field := range schema.Required {
		if _, ok := fields[field]; !ok {
			return request, invalidWorkRead(field + " is required")
		}
	}
	if name == WorkItem && request.Cursor != "" && request.View != "recovery" {
		return request, invalidWorkRead("work_item cursor requires view=recovery")
	}
	maximum := MaxItemLimit
	if name == WorkBulkExport {
		maximum = WorkBulkExportMaxItems
	}
	for _, date := range []string{request.CreatedAfter, request.CreatedBefore, request.UpdatedAfter} {
		if date != "" {
			if _, err := time.Parse(time.RFC3339Nano, date); err != nil {
				return request, invalidWorkRead("date filters must be RFC3339 timestamps")
			}
		}
	}
	request.ProjectID = strings.TrimSpace(request.ProjectID)
	request.Reference = strings.TrimSpace(request.Reference)
	if request.ProjectID == "" || len(request.ProjectID) > 256 || len(request.Reference) > 256 || len(request.Query) > 256 || len(request.CreatedBy) > 256 || len(request.Fingerprint) > 256 || len(request.State) > 256 || len(request.Label) > 256 || len(request.CommentID) > 256 || len(request.Cursor) > 4096 || request.Offset < 0 || request.Offset > 1000000 || request.Limit < 0 || request.Limit > maximum {
		return request, invalidWorkRead(fmt.Sprintf("project_id is required, text filters are at most 256 bytes, cursor at most 4096 bytes, offset 0 to 1000000 and limit 1 to %d", maximum))
	}
	for _, field := range schema.Required {
		if field == "reference" && request.Reference == "" || field == "revision" && request.Revision <= 0 || field == "attempt_id" && request.AttemptID <= 0 {
			return request, invalidWorkRead(field + " is required")
		}
	}
	if rawLimit, ok := fields["limit"]; ok && (string(rawLimit) == "null" || request.Limit == 0) {
		return request, invalidWorkRead(fmt.Sprintf("limit must be 1 to %d", maximum))
	}
	if _, present := fields["attempt_id"]; present && request.AttemptID <= 0 {
		return request, invalidWorkRead("attempt_id must be positive")
	}
	for _, filter := range []struct {
		single   string
		multiple []string
	}{{request.State, request.States}, {request.Label, request.Labels}, {request.Assignee, request.Assignees}} {
		count := len(filter.multiple)
		if filter.single != "" {
			count++
		}
		if count > 32 {
			return request, invalidWorkRead("at most 32 values are allowed per filter")
		}
	}
	if request.Priority != nil && len(request.Priorities) == 32 {
		return request, invalidWorkRead("at most 32 values are allowed per filter")
	}
	if slices.Contains(request.Include, "work") && slices.Contains(request.Include, "summary") {
		return request, invalidWorkRead("include accepts summary or work, not both")
	}
	if name == HealthFindings && request.Since != "" {
		if _, err := time.Parse(time.RFC3339Nano, request.Since); err != nil {
			return request, invalidWorkRead("since must be an RFC3339 timestamp")
		}
	}
	if name == WorkBulkExport {
		if request.Limit == 0 {
			request.Limit = WorkBulkExportMaxItems
		}
	} else {
		request.Limit = itemLimit(request.Limit)
	}
	if name == GitHubScopeTimings && (request.NativeAttemptID == "" || request.RunnerID == "") {
		return request, invalidWorkRead("native_attempt_id and runner_id are required")
	}
	if len(request.NativeAttemptID) > 128 || request.NativeAttemptID != "" && !strings.HasPrefix(request.NativeAttemptID, "attempt_") || request.AttemptID != 0 && request.NativeAttemptID != "" || name == WorkAttemptReceipt && request.AttemptID <= 0 && request.NativeAttemptID == "" {
		return request, invalidWorkRead("give exactly one of attempt_id or native_attempt_id; native_attempt_id starts with attempt_")
	}
	return request, nil
}

func (r WorkReadRequest) NativeWorkQuery() url.Values {
	params := url.Values{"limit": {strconv.Itoa(r.Limit)}}
	if r.Cursor != "" {
		params.Set("cursor", r.Cursor)
	}
	for _, filter := range []struct {
		name, single string
		values       []string
	}{
		{"type", r.Type, nil},
		{"verification", r.Verification, nil},
		{"state", r.State, r.States},
		{"label", r.Label, r.Labels},
		{"assignee", r.Assignee, r.Assignees},
	} {
		if filter.single != "" {
			params.Add(filter.name, filter.single)
		}
		for _, value := range filter.values {
			params.Add(filter.name, value)
		}
	}
	if r.Priority != nil {
		params.Add("priority", strconv.Itoa(*r.Priority))
	}
	for _, value := range r.Priorities {
		params.Add("priority", strconv.Itoa(value))
	}
	if r.Open != nil {
		params.Set("open", strconv.FormatBool(*r.Open))
	}
	if r.Fingerprint != "" {
		params.Set("fingerprint", r.Fingerprint)
	}
	for name, value := range map[string]string{"created_after": r.CreatedAfter, "created_before": r.CreatedBefore, "updated_after": r.UpdatedAfter} {
		if value != "" {
			params.Set(name, value)
		}
	}
	if r.Query != "" {
		params.Set("q", r.Query)
	}
	if r.CreatedBy != "" {
		params.Set("created_by", r.CreatedBy)
	}
	if r.Archived != "" {
		params.Set("archived", r.Archived)
	}
	include := r.Include
	if len(include) == 0 {
		include = []string{"summary"}
	}
	params.Set("include", strings.Join(include, ","))
	return params
}

type NativeWorkPage struct {
	tracker.Page[NativeItem]
	Total int                `json:"total"`
	Work  *NativeWorkSummary `json:"work,omitempty"`
}

type NativeWorkSummary struct {
	Items     []NativeItem             `json:"items"`
	Lanes     []tracker.NativeWorkLane `json:"lanes"`
	Truncated bool                     `json:"truncated"`
	AsOf      time.Time                `json:"as_of"`
}

func NativeWorkPageView(projectID string, page tracker.NativeIssuePage) NativeWorkPage {
	result := NativeWorkPage{Page: NativeItemPage(projectID, page.Page), Total: page.Total}
	if page.Work != nil {
		items := NativeItemPage(projectID, tracker.Page[tracker.NativeIssue]{Items: page.Work.Items})
		result.Work = &NativeWorkSummary{Items: items.Items, Lanes: page.Work.Lanes, Truncated: page.Work.Truncated, AsOf: page.Work.AsOf}
	}
	return result
}

func (e *Executor) readWork(ctx context.Context, call Call) (Result, error) {
	request, err := DecodeWorkRead(call.Name, call.Arguments)
	if err != nil {
		return Result{}, err
	}
	reader := e.workReads
	if authority, ok := ctx.Value(authorityKey{}).(Authority); ok && authority.WorkReads != nil {
		reader = authority.WorkReads
	}
	if reader == nil {
		return Result{}, ErrReadUnavailable
	}
	result, err := reader.ReadWork(ctx, call.Name, request)
	if err != nil {
		var ambiguous *explain.AmbiguousIdentityError
		var unavailable *ReadUnavailableError
		if errors.Is(err, ErrResultTooLarge) || errors.Is(err, ErrAccessDenied) || errors.Is(err, ErrInvalidArguments) || errors.Is(err, explain.ErrNotFound) || errors.Is(err, ErrProjectScopeRequired) || errors.As(err, &unavailable) || errors.As(err, &ambiguous) {
			return Result{}, err
		}
		return Result{}, &ReadUnavailableError{Err: fmt.Errorf("load snapshot: %w", err)}
	}
	limit := ResultByteLimit(call.Name)
	if len(result.Content) > limit {
		return Result{}, &ResultTooLargeError{SizeBytes: len(result.Content), LimitBytes: limit}
	}
	return result, nil
}

type WorkReference struct {
	Kind         string     `json:"kind"`
	State        string     `json:"state,omitempty"`
	ID           string     `json:"id"`
	VersionID    string     `json:"version_id,omitempty"`
	URL          string     `json:"url,omitempty"`
	Availability string     `json:"availability,omitempty"`
	ExpiresAt    *time.Time `json:"expires_at,omitempty"`
	ObservedAt   *time.Time `json:"observed_at,omitempty"`
	ContentKind  string     `json:"content_kind,omitempty"`
	ServiceID    string     `json:"service_id,omitempty"`
	Revision     int64      `json:"revision,omitempty"`
	SHA256       string     `json:"sha256,omitempty"`
	RunID        string     `json:"run_id,omitempty"`
	AttemptID    string     `json:"attempt_id,omitempty"`
}

func ArtifactWorkReference(projectID, reference string, ref artifact.Reference) WorkReference {
	return WorkReference{Kind: "artifact", ID: ref.ArtifactID, VersionID: ref.VersionID, URL: WorkItemURL(projectID, reference), ContentKind: ref.Kind, ServiceID: ref.ServiceID, Revision: ref.Revision, SHA256: ref.SHA256, RunID: ref.RunID, AttemptID: ref.AttemptID, Availability: ref.Availability, ExpiresAt: &ref.ExpiresAt, ObservedAt: &ref.ObservedAt}
}

type ReadPage[T any] struct {
	Items      []T  `json:"items"`
	Offset     int  `json:"offset"`
	NextOffset *int `json:"next_offset,omitempty"`
}

type HistoryPage[T any] struct {
	ReadPage[T]
	Source string `json:"source"`
}

func OffsetPage[T any](items []T, offset, limit int) ReadPage[T] {
	start := min(offset, len(items))
	end := min(start+limit, len(items))
	page := ReadPage[T]{Items: append([]T{}, items[start:end]...), Offset: offset}
	if end < len(items) {
		page.NextOffset = &end
	}
	return page
}

func WorkItemURL(projectID, reference string) string {
	if reference == "" {
		return ""
	}
	return "/projects/" + url.PathEscape(projectID) + "/issues/" + url.PathEscape(reference)
}

type NativeItem struct {
	ValidationAudit *tracker.ValidationAudit `json:"validation_audit,omitempty"`
	tracker.NativeIssue
	Identifier string `json:"identifier"`
	URL        string `json:"url"`
}

func NativeItemView(projectID string, issue tracker.NativeIssue) NativeItem {
	return NativeItem{NativeIssue: issue, Identifier: string(issue.ProjectID) + "#" + strconv.Itoa(issue.Number), URL: WorkItemURL(projectID, string(issue.WorkItemID))}
}
func NativeItemPage(projectID string, page tracker.Page[tracker.NativeIssue]) tracker.Page[NativeItem] {
	out := tracker.Page[NativeItem]{Items: []NativeItem{}, NextCursor: page.NextCursor}
	for _, issue := range page.Items {
		out.Items = append(out.Items, NativeItemView(projectID, NativeListIssue(issue)))
	}
	return out
}

func NativeListIssue(issue tracker.NativeIssue) tracker.NativeIssue {
	issue.Body = ""
	issue.LinkedSource = nil
	issue.Provenance = nil
	issue.ExternalReferences = nil
	issue.Change = nil
	issue.OmittedFields = slices.Clone(issue.OmittedFields)
	for _, field := range []string{"body", "linked_source", "provenance", "external_references", "change"} {
		if !slices.Contains(issue.OmittedFields, field) {
			issue.OmittedFields = append(issue.OmittedFields, field)
		}
	}
	return issue
}

func BoundNativeListIssue(issue tracker.NativeIssue) (tracker.NativeIssue, error) {
	issue = NativeListIssue(issue)
	raw, err := json.Marshal(issue)
	if err != nil {
		return tracker.NativeIssue{}, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return tracker.NativeIssue{}, err
	}
	fields["latest_transition_reason"], err = json.Marshal(issue.LatestTransitionReason)
	if err != nil {
		return tracker.NativeIssue{}, err
	}
	var names []string
	for name := range fields {
		if !slices.Contains([]string{"organization_id", "project_id", "work_item_id", "number", "revision", "profile", "omitted_fields"}, name) {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	slices.SortStableFunc(names, func(a, b string) int { return len(fields[b]) - len(fields[a]) })
	for index := 0; ; index++ {
		fields["omitted_fields"], err = json.Marshal(issue.OmittedFields)
		if err != nil {
			return tracker.NativeIssue{}, err
		}
		raw, err = json.Marshal(fields)
		if err != nil {
			return tracker.NativeIssue{}, err
		}
		if len(raw) <= WorkListPageBytes/4 {
			var bounded tracker.NativeIssue
			if err := json.Unmarshal(raw, &bounded); err != nil {
				return tracker.NativeIssue{}, err
			}
			if reason, ok := fields["latest_transition_reason"]; ok {
				if err := json.Unmarshal(reason, &bounded.LatestTransitionReason); err != nil {
					return tracker.NativeIssue{}, err
				}
			}
			return bounded, nil
		}
		if index == len(names) {
			return tracker.NativeIssue{}, ErrResultTooLarge
		}
		name := names[index]
		delete(fields, name)
		if !slices.Contains(issue.OmittedFields, name) {
			issue.OmittedFields = append(issue.OmittedFields, name)
		}
	}
}
