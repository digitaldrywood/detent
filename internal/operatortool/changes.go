package operatortool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/changerequest"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const (
	ListChanges               = "list_changes"
	GetChange                 = "get_change"
	GetChangeVersion          = "get_change_version"
	GetChangeReviewPolicy     = "get_change_review_policy"
	ChangeViewedFiles         = "change_viewed_files"
	ArtifactServices          = "artifact_services"
	ArtifactReferences        = "artifact_references"
	GetArtifactReference      = "get_artifact_reference"
	ArtifactAccess            = "artifact_access"
	CreateChange              = "create_change"
	PublishChangeVersion      = "publish_change_version"
	DiscussChange             = "discuss_change"
	ReviewChange              = "review_change"
	ViewChangeFile            = "view_change_file"
	ApproveChangeReviewPolicy = "approve_change_review_policy"
	BindArtifactService       = "bind_artifact_service"
	GetAttemptDiff            = "get_attempt_diff"
	GetWorkItemDiff           = "get_work_item_diff"
	ListWorkItemPullRequests  = "list_work_item_pull_requests"
	ArtifactLibrary           = "artifact_library"
	GetNativeRun              = "get_native_run"
)

var ErrServiceUnavailable = errors.New("change or artifact service is unavailable")

// ChangeArguments is the typed application selector. Each tool allows only its
// declared fields; credentials, producer provenance and leases are never input.
type ChangeArguments struct {
	ProjectID         string                           `json:"project_id"`
	RequestID         string                           `json:"request_id,omitempty"`
	ItemID            string                           `json:"work_item_id,omitempty"`
	LinkedIssues      []tracker.NativeWorkItemID       `json:"linked_issues,omitempty"`
	ChangeID          string                           `json:"change_id,omitempty"`
	VersionID         string                           `json:"version_id,omitempty"`
	ExpectedVersionID *string                          `json:"expected_version_id,omitempty"`
	BaseSHA           string                           `json:"base_sha,omitempty"`
	HeadSHA           string                           `json:"head_sha,omitempty"`
	MergeBaseSHA      string                           `json:"merge_base_sha,omitempty"`
	Repository        string                           `json:"repository,omitempty"`
	Code              *tracker.ChangeArtifact          `json:"code,omitempty"`
	Artifacts         []tracker.ChangeArtifact         `json:"artifacts,omitempty"`
	PolicyID          string                           `json:"policy_id,omitempty"`
	External          *tracker.ChangeExternalReference `json:"external,omitempty"`
	ExpectedRevision  int64                            `json:"expected_revision,omitempty"`
	ArtifactID        string                           `json:"artifact_id,omitempty"`
	Revision          int64                            `json:"revision,omitempty"`
	SHA256            string                           `json:"sha256,omitempty"`
	Limit             int                              `json:"limit,omitempty"`
	Offset            int                              `json:"offset,omitempty"`
	Title             string                           `json:"title,omitempty"`
	Body              string                           `json:"body,omitempty"`
	Decision          string                           `json:"decision,omitempty"`
	Bundle            *tracker.ChangeReviewBundle      `json:"bundle,omitempty"`
	FileSHA256        string                           `json:"file_sha256,omitempty"`
	Viewed            bool                             `json:"viewed,omitempty"`
	ExpectedPolicyID  string                           `json:"expected_review_policy_id,omitempty"`
	Policy            *tracker.ChangeReviewPolicy      `json:"policy,omitempty"`
	Binding           *artifact.Binding                `json:"binding,omitempty"`
	AttemptID         string                           `json:"attempt_id,omitempty"`
	Sequence          int64                            `json:"sequence,omitempty"`
	Source            string                           `json:"source,omitempty"`
	Kind              string                           `json:"kind,omitempty"`
	Status            string                           `json:"status,omitempty"`
}

// ChangeResult contains bounded application data, never rendered HTML.
type ChangeResult struct {
	ValidationAudit *tracker.ValidationAudit    `json:"validation_audit,omitempty"`
	OrganizationID  string                      `json:"organization_id"`
	ProjectID       string                      `json:"project_id"`
	WorkItemID      string                      `json:"work_item_id,omitempty"`
	WorkItemState   string                      `json:"work_item_state,omitempty"`
	ChangeID        string                      `json:"change_id,omitempty"`
	URL             string                      `json:"url,omitempty"`
	GeneratedAt     time.Time                   `json:"generated_at"`
	Freshness       string                      `json:"freshness"`
	NextOffset      *int                        `json:"next_offset,omitempty"`
	Changes         []tracker.ChangeRequest     `json:"changes,omitempty"`
	Detail          *tracker.ChangeDetail       `json:"detail,omitempty"`
	Version         *tracker.ChangeVersion      `json:"version,omitempty"`
	Policy          *tracker.ChangeReviewPolicy `json:"policy,omitempty"`
	Viewed          []tracker.ChangeViewedFile  `json:"viewed_files,omitempty"`
	Services        []artifact.Binding          `json:"services,omitempty"`
	Artifacts       []artifact.Reference        `json:"artifacts,omitempty"`
	Access          *ArtifactDownload           `json:"access,omitempty"`
	Receipt         json.RawMessage             `json:"receipt,omitempty"`
	Diff            *tracker.AttemptDiff        `json:"diff"`
	PullRequests    []tracker.PullRequestView   `json:"pull_requests,omitempty"`
	Library         []ArtifactLibraryRow        `json:"library,omitempty"`
	Attempt         *tracker.NativeAttempt      `json:"attempt,omitempty"`
}

// ArtifactLibraryRow is the business data from the existing library read,
// without UI state, local database paths or arbitrary producer metadata.
type ArtifactLibraryRow struct {
	ID               string    `json:"id"`
	ProjectID        string    `json:"project_id"`
	Kind             string    `json:"kind"`
	Title            string    `json:"title"`
	State            string    `json:"state"`
	ValidationStatus string    `json:"validation_status"`
	ReviewURL        string    `json:"review_url,omitempty"`
	SourceURL        string    `json:"source_url,omitempty"`
	ArtifactPath     string    `json:"artifact_path,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// ArtifactDownload authorizes existing manifest/object endpoints for one exact
// revision. The token is returned only here, never in URLs or retry receipts.
type ArtifactDownload struct {
	Grant               artifact.Grant `json:"grant"`
	ManifestURL         string         `json:"manifest_url"`
	ObjectURLTemplate   string         `json:"object_url_template"`
	AuthorizationScheme string         `json:"authorization_scheme"`
}

func DownloadResult(grant artifact.Grant) (ArtifactDownload, error) {
	if !artifact.ValidOrigin(grant.Origin) || !artifact.ValidID(grant.ArtifactID, "artifact") || grant.Revision < 1 || !artifact.ValidHash(grant.SHA256, 64) || grant.Token == "" || len(grant.Token) > 4096 || grant.ExpiresAt.IsZero() {
		return ArtifactDownload{}, ErrServiceUnavailable
	}
	base := fmt.Sprintf("%s/v1/artifacts/%s/manifests/%d", grant.Origin, grant.ArtifactID, grant.Revision)
	return ArtifactDownload{Grant: grant, ManifestURL: base, ObjectURLTemplate: base + "/objects/{object_id}", AuthorizationScheme: "Bearer"}, nil
}

func ChangeCatalog() []Definition {
	str := func() json.RawMessage { return json.RawMessage(`{"type":"string","minLength":1,"maxLength":256}`) }
	props := map[string]json.RawMessage{}
	for _, k := range []string{"project_id", "work_item_id", "change_id", "version_id", "artifact_id", "expected_review_policy_id", "attempt_id", "kind", "status"} {
		props[k] = str()
	}
	props["request_id"] = json.RawMessage(`{"type":"string","minLength":1,"maxLength":128}`)
	props["expected_version_id"] = json.RawMessage(`{"type":"string","maxLength":256}`)
	props["policy_id"] = str()
	props["repository"] = json.RawMessage(`{"type":"string","minLength":1,"maxLength":2048}`)
	for _, k := range []string{"base_sha", "head_sha", "merge_base_sha"} {
		props[k] = json.RawMessage(`{"type":"string","pattern":"^([0-9a-f]{40}|[0-9a-f]{64})$"}`)
	}
	artifactSchema := json.RawMessage(`{"type":"object","required":["kind","uri","sha256","availability"],"properties":{"kind":{"type":"string","enum":["code","manifest","diff","test","log","checkpoint","artifact"]},"uri":{"type":"string","minLength":1,"maxLength":2048},"sha256":{"type":"string","pattern":"^[0-9a-f]{64}$"},"availability":{"type":"string","enum":["unverified","available","missing","inaccessible"]}},"additionalProperties":false}`)
	props["code"] = json.RawMessage(`{"type":"object","required":["kind","uri","sha256","availability"],"properties":{"kind":{"type":"string","enum":["code"]},"uri":{"type":"string","minLength":1,"maxLength":2048},"sha256":{"type":"string","pattern":"^[0-9a-f]{64}$"},"availability":{"type":"string","enum":["unverified","available","missing","inaccessible"]}},"additionalProperties":false}`)
	props["artifacts"] = json.RawMessage(`{"type":"array","maxItems":63,"items":` + string(artifactSchema) + `}`)
	props["external"] = json.RawMessage(`{"type":"object","required":["provider","id","url"],"properties":{"provider":{"type":"string","enum":["github"]},"id":{"type":"string","minLength":1,"maxLength":128},"url":{"type":"string","minLength":1,"maxLength":2048}},"additionalProperties":false}`)
	props["title"] = json.RawMessage(`{"type":"string","minLength":1,"maxLength":512}`)
	props["body"] = json.RawMessage(`{"type":"string","maxLength":32768}`)
	props["linked_issues"] = json.RawMessage(`{"type":"array","maxItems":32,"items":{"type":"string","pattern":"^wi_[A-Za-z0-9_]+$","maxLength":256}}`)
	props["decision"] = json.RawMessage(`{"type":"string","enum":["approved","changes_requested","commented"]}`)
	for _, k := range []string{"expected_revision", "revision", "sequence"} {
		props[k] = json.RawMessage(`{"type":"integer","minimum":1,"maximum":9007199254740991}`)
	}
	props["limit"] = json.RawMessage(`{"type":"integer","minimum":1,"maximum":200}`)
	props["offset"] = json.RawMessage(`{"type":"integer","minimum":0,"maximum":10000}`)
	props["viewed"] = json.RawMessage(`{"type":"boolean"}`)
	props["source"] = json.RawMessage(`{"type":"string","enum":["attempt","workspace"]}`)
	props["binding"] = json.RawMessage(`{"type":"object","required":["service_id","origin","mode","hosted_opt_in","publisher_token_id"],"properties":{"service_id":{"type":"string","minLength":1,"maxLength":256},"origin":{"type":"string","minLength":1,"maxLength":2048},"mode":{"type":"string","enum":["customer","hosted"]},"hosted_opt_in":{"type":"boolean"},"publisher_token_id":{"type":"string","minLength":1,"maxLength":256}},"additionalProperties":false}`)
	for _, k := range []string{"sha256", "file_sha256"} {
		props[k] = json.RawMessage(`{"type":"string","pattern":"^[0-9a-f]{64}$"}`)
	}
	props["bundle"] = json.RawMessage(`{"type":"object","required":["artifact_id","revision","sha256","head_sha"],"properties":{"artifact_id":{"type":"string","minLength":1,"maxLength":256},"revision":{"type":"integer","minimum":1,"maximum":9007199254740991},"sha256":{"type":"string","pattern":"^[0-9a-f]{64}$"},"head_sha":{"type":"string","pattern":"^([0-9a-f]{40}|[0-9a-f]{64})$"}},"additionalProperties":false}`)
	props["policy"] = json.RawMessage(`{"type":"object","required":["policy_id","require_review","required_checks"],"properties":{"review_policy_id":{"type":"string","maxLength":256},"policy_id":{"type":"string","minLength":1,"maxLength":256},"require_review":{"type":"boolean"},"required_checks":{"type":"array","maxItems":256,"items":{"type":"object","required":["name","principal_id","workflow_id","workflow_sha256","source","max_age_seconds"],"properties":{"name":{"type":"string","minLength":1,"maxLength":256},"principal_id":{"type":"string","minLength":1,"maxLength":256},"workflow_id":{"type":"string","minLength":1,"maxLength":256},"workflow_sha256":{"type":"string","pattern":"^[0-9a-f]{64}$"},"source":{"type":"string","enum":["customer","independent"]},"max_age_seconds":{"type":"integer","minimum":60,"maximum":604800}},"additionalProperties":false}}},"additionalProperties":false}`)
	var definitions []Definition
	add := func(name, description, required, optional string, read, material bool) {
		fields := map[string]json.RawMessage{"project_id": props["project_id"]}
		req := []string{"project_id"}
		for _, k := range strings.Fields(required) {
			fields[k] = props[k]
			req = append(req, k)
		}
		for _, k := range strings.Fields(optional) {
			fields[k] = props[k]
		}
		schema, _ := json.Marshal(struct { //nolint:errcheck // Properties are fixed, valid JSON schema fragments.
			Type       string                     `json:"type"`
			Required   []string                   `json:"required"`
			Properties map[string]json.RawMessage `json:"properties"`
			Additional bool                       `json:"additionalProperties"`
		}{"object", req, fields, false})
		definitions = append(definitions, Definition{Name: name, Description: description, InputSchema: schema, Annotations: Annotations{ReadOnly: read, Destructive: material, Idempotent: true, OpenWorld: name != ArtifactLibrary}, Meta: ToolMetadata{Toolset: "changes_artifacts"}})
	}
	add(ListChanges, "List Change Requests linked to an owned work item.", "work_item_id", "limit offset", true, false)
	add(GetChange, "Read Change Request versions, discussion, reviews, checks, landing evidence and bounded local/scheduled check comparisons.", "work_item_id change_id", "", true, false)
	add(GetChangeVersion, "Read one immutable version with code/diff and artifact references.", "work_item_id change_id version_id", "", true, false)
	add(GetChangeReviewPolicy, "Read the project's approved review policy.", "", "", true, false)
	add(ChangeViewedFiles, "Read this principal's viewed-file digests for a version.", "work_item_id change_id version_id", "limit offset", true, false)
	add(ArtifactServices, "Read installed artifact services without publisher credentials.", "", "", true, false)
	add(ArtifactReferences, "Read artifact receipts and availability for an owned work item.", "work_item_id", "limit offset", true, false)
	add(GetArtifactReference, "Read the exact immutable artifact receipt, including historical revisions.", "work_item_id artifact_id revision", "", true, false)
	add(ArtifactAccess, "Authorize an exact artifact revision for download/export through existing manifest/object endpoints. Token expires within one minute; send it as Bearer, never in a URL. Replays reauthorize and mint fresh ephemeral access.", "work_item_id artifact_id revision sha256 request_id", "", false, false)
	add(CreateChange, "Create a Change Request using the existing application command, optionally linking other work items owned by this project.", "work_item_id title request_id", "body linked_issues", false, false)
	add(PublishChangeVersion, "Publish a genuine operator-authored immutable version without a Run/Attempt. Use an empty expected_version_id for first publication. Reuses approved policy, artifact validation and promotion; returns the stable published version and live current Change/lane. Unverified artifacts remain unverified; external PR references do not bypass repository protection.", "work_item_id change_id expected_version_id base_sha head_sha merge_base_sha repository code policy_id request_id", "artifacts external", false, false)
	add(DiscussChange, "Discuss a change through its application command.", "work_item_id change_id body request_id", "version_id", false, false)
	add(ReviewChange, "Review the current immutable bundle. Decisions execute using current review authority.", "work_item_id change_id version_id expected_revision decision bundle request_id", "body", false, true)
	add(ViewChangeFile, "Record a viewed-file digest for an immutable review bundle.", "work_item_id change_id version_id bundle file_sha256 viewed request_id", "", false, false)
	add(ApproveChangeReviewPolicy, "Approve the project review policy using current administrator authority.", "policy request_id", "expected_review_policy_id", false, true)
	add(BindArtifactService, "Bind an artifact service to this project using existing administrator policy. Publisher identity is an existing granted credential ID, never a token.", "binding request_id", "", false, true)
	add(GetAttemptDiff, "Read stored files and patches for an attempt owned by this work item. Freshness describes the read; diff.created_at, producer and generation identify the stored artifact. This read does not capture current worktree edits.", "work_item_id attempt_id", "sequence source limit offset", true, false)
	add(GetWorkItemDiff, "Read stored files and patches for this work item's latest diff, or diff:null when no stored generation exists for the selected source. Freshness describes the read; diff.created_at, producer and generation identify the stored artifact. This read does not capture current worktree edits.", "work_item_id", "source limit offset", true, false)
	add(ListWorkItemPullRequests, "Read the existing PR panel with source observation times, checks, reviews and mergeability.", "work_item_id", "limit offset", true, false)
	add(ArtifactLibrary, "Read a bounded page of this project's artifact library and review links.", "", "kind status limit offset", true, false)
	add(GetNativeRun, "Read an owned native attempt, its linked changes and artifact receipts.", "work_item_id attempt_id", "", true, false)
	return definitions
}

func DecodeChangeArguments(name string, raw json.RawMessage) (ChangeArguments, error) {
	var args ChangeArguments
	definition, ok := ChangeDefinition(name)
	if !ok {
		return args, ErrUnknownTool
	}
	var fields map[string]json.RawMessage
	var schema struct {
		Required   []string                   `json:"required"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if DecodeArguments(raw, &args) != nil || json.Unmarshal(raw, &fields) != nil || fields == nil || json.Unmarshal(definition.InputSchema, &schema) != nil {
		return args, ErrInvalidArguments
	}
	for k := range fields {
		if _, ok := schema.Properties[k]; !ok {
			return args, ErrInvalidArguments
		}
	}
	for _, k := range schema.Required {
		v, ok := fields[k]
		if !ok || string(v) == "null" || string(v) == `""` && k != "expected_version_id" {
			return args, ErrInvalidArguments
		}
	}
	for _, v := range []string{args.ProjectID, args.ItemID, args.ChangeID, args.VersionID, args.ArtifactID, args.ExpectedPolicyID, args.AttemptID} {
		if len(v) > 256 || strings.TrimSpace(v) != v || strings.ContainsAny(v, "/\\?#%") {
			return args, ErrInvalidArguments
		}
	}
	if args.ProjectID == "" || len(args.RequestID) > 128 || strings.TrimSpace(args.RequestID) != args.RequestID || len(args.Title) > 512 || len(args.Body) > 32768 || args.Limit < 0 || args.Limit > 200 || args.Offset < 0 || args.Offset > 10000 {
		return args, ErrInvalidArguments
	}
	if len(args.LinkedIssues) > 32 || string(fields["linked_issues"]) == "null" {
		return args, ErrInvalidArguments
	}
	for _, id := range args.LinkedIssues {
		if len(id) > 256 || !strings.HasPrefix(string(id), "wi_") || len(id) <= 3 || strings.ContainsAny(string(id), "/\\?#% \t\n") {
			return args, ErrInvalidArguments
		}
		for _, char := range string(id)[3:] {
			if char != '_' && (char < '0' || char > '9') && (char < 'A' || char > 'Z') && (char < 'a' || char > 'z') {
				return args, ErrInvalidArguments
			}
		}
	}
	if name == PublishChangeVersion {
		expected := args.ExpectedVersionID
		if expected == nil || len(*expected) > 256 || strings.TrimSpace(*expected) != *expected || strings.ContainsAny(*expected, "/\\?#%") || args.PolicyID == "" || len(args.PolicyID) > 256 || strings.TrimSpace(args.PolicyID) != args.PolicyID {
			return args, ErrInvalidArguments
		}
		for _, commit := range []string{args.BaseSHA, args.HeadSHA, args.MergeBaseSHA} {
			if !changerequest.ValidHash(commit, 40) && !changerequest.ValidHash(commit, 64) {
				return args, ErrInvalidArguments
			}
		}
		if !changerequest.ValidReference(args.Repository) || args.Code == nil || args.Code.Kind != "code" || changerequest.ValidateArtifacts(append([]tracker.ChangeArtifact{*args.Code}, args.Artifacts...)) != nil {
			return args, ErrInvalidArguments
		}
		if external := args.External; external != nil && (external.Provider != "github" || external.ID == "" || len(external.ID) > 128 || !changerequest.ValidReference(external.URL)) {
			return args, ErrInvalidArguments
		}
	}
	for _, key := range []string{"revision", "expected_revision", "sequence"} {
		if _, ok := fields[key]; ok {
			v := args.Revision
			if key == "expected_revision" {
				v = args.ExpectedRevision
			}
			if key == "sequence" {
				v = args.Sequence
			}
			if v < 1 || v > 9007199254740991 {
				return args, ErrInvalidArguments
			}
		}
	}
	if _, ok := fields["limit"]; ok && args.Limit < 1 {
		return args, ErrInvalidArguments
	}
	if args.Decision != "" && args.Decision != "approved" && args.Decision != "changes_requested" && args.Decision != "commented" {
		return args, ErrInvalidArguments
	}
	if len(args.Kind) > 256 || len(args.Status) > 256 || args.Source != "" && args.Source != tracker.DiffSourceAttempt && args.Source != tracker.DiffSourceWorkspace {
		return args, ErrInvalidArguments
	}
	if b := args.Binding; b != nil {
		if !artifact.ValidID(b.ServiceID, "service") || !artifact.ValidOrigin(b.Origin) || len(b.Origin) > 2048 || b.Mode != "customer" && b.Mode != "hosted" || b.Mode == "hosted" && !b.HostedOptIn || b.PublisherTokenID == "" || len(b.PublisherTokenID) > 256 {
			return args, ErrInvalidArguments
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) != nil {
			return args, ErrInvalidArguments
		}
		var bindingFields map[string]json.RawMessage
		if json.Unmarshal(fields["binding"], &bindingFields) != nil || bindingFields["hosted_opt_in"] == nil || string(bindingFields["hosted_opt_in"]) == "null" {
			return args, ErrInvalidArguments
		}
	}
	if args.SHA256 != "" && !artifact.ValidHash(args.SHA256, 64) || args.FileSHA256 != "" && !artifact.ValidHash(args.FileSHA256, 64) {
		return args, ErrInvalidArguments
	}
	if b := args.Bundle; b != nil {
		if !artifact.ValidID(b.ArtifactID, "artifact") || b.Revision < 1 || b.Revision > 9007199254740991 || !artifact.ValidHash(b.SHA256, 64) || !artifact.ValidHash(b.HeadSHA, 40) && !artifact.ValidHash(b.HeadSHA, 64) {
			return args, ErrInvalidArguments
		}
	}
	if p := args.Policy; p != nil {
		var policyFields map[string]json.RawMessage
		if json.Unmarshal(fields["policy"], &policyFields) != nil {
			return args, ErrInvalidArguments
		}
		for _, key := range []string{"policy_id", "require_review", "required_checks"} {
			value, ok := policyFields[key]
			if !ok || string(value) == "null" {
				return args, ErrInvalidArguments
			}
		}

		if len(p.RequiredChecks) > 256 || len(p.ID) > 256 || p.PolicyID == "" || len(p.PolicyID) > 256 {
			return args, ErrInvalidArguments
		}
		for _, c := range p.RequiredChecks {
			if c.Name == "" || len(c.Name) > 256 || c.PrincipalID == "" || len(c.PrincipalID) > 256 || c.WorkflowID == "" || len(c.WorkflowID) > 256 || !artifact.ValidHash(c.WorkflowSHA256, 64) || c.MaxAgeSeconds < 60 || c.MaxAgeSeconds > 604800 || c.Source != "customer" && c.Source != "independent" {
				return args, ErrInvalidArguments
			}
		}
	}
	return args, nil
}

func ChangeDefinition(name string) (Definition, bool) {
	for _, d := range ChangeCatalog() {
		if d.Name == name {
			return d, true
		}
	}
	return Definition{}, false
}

func BoundedChangeResult(value ChangeResult) (Result, error) {
	raw, err := json.Marshal(value)
	if err != nil || len(raw) > MaxResultBytes {
		return Result{}, ErrServiceUnavailable
	}
	return Result{Content: raw}, nil
}

// ChangeApplication is the deployment's existing application service, bound to
// current connection authority. HTTP and MCP delegate to the same commands.
type ChangeApplication interface {
	ReadChange(context.Context, string, ChangeArguments) (ChangeResult, error)
	MutateChange(context.Context, string, ChangeArguments) (ChangeResult, error)
}

func ChangePage[T any](items []T, args ChangeArguments) ([]T, *int) {
	if items == nil {
		return []T{}, nil
	}
	limit := args.Limit
	if limit == 0 {
		limit = 100
	}
	start := min(args.Offset, len(items))
	end := min(start+limit, len(items))
	var next *int
	if end < len(items) {
		next = &end
	}
	return append([]T{}, items[start:end]...), next
}

// ChangeDiffPage retains source totals/identity while bounding patch text on a
// transport page. Omitted patch bytes are explicitly marked truncated.
func ChangeDiffPage(diff *tracker.AttemptDiff, args ChangeArguments) (*tracker.AttemptDiff, *int) {
	if diff == nil {
		return nil, nil
	}
	value := *diff
	page, next := ChangePage(diff.Files, args)
	files := make([]tracker.AttemptDiffFile, 0, len(page))
	budget := MaxResultBytes / 2
	for _, file := range page {
		encoded, _ := json.Marshal(file) //nolint:errcheck // AttemptDiffFile contains only strings, integers and booleans.
		if len(encoded) > budget && file.Patch != "" {
			file.Patch = ""
			file.Truncated = true
			encoded, _ = json.Marshal(file) //nolint:errcheck // AttemptDiffFile contains only strings, integers and booleans.
		}
		if len(encoded) > budget {
			offset := args.Offset + len(files)
			next = &offset
			break
		}
		budget -= len(encoded) + 1
		value.Truncated = value.Truncated || file.Truncated
		files = append(files, file)
	}
	value.Files = files
	return &value, next
}
