package operatortool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
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
	DiscussChange             = "discuss_change"
	ReviewChange              = "review_change"
	ViewChangeFile            = "view_change_file"
	ApproveChangeReviewPolicy = "approve_change_review_policy"
)

var ErrServiceUnavailable = errors.New("change or artifact service is unavailable")

// ChangeArguments is the typed application selector. Each tool allows only its
// declared fields; credentials, producer provenance and leases are never input.
type ChangeArguments struct {
	ProjectID        string                      `json:"project_id"`
	RequestID        string                      `json:"request_id,omitempty"`
	ItemID           string                      `json:"work_item_id,omitempty"`
	ChangeID         string                      `json:"change_id,omitempty"`
	VersionID        string                      `json:"version_id,omitempty"`
	ExpectedRevision int64                       `json:"expected_revision,omitempty"`
	ArtifactID       string                      `json:"artifact_id,omitempty"`
	Revision         int64                       `json:"revision,omitempty"`
	SHA256           string                      `json:"sha256,omitempty"`
	Limit            int                         `json:"limit,omitempty"`
	Offset           int                         `json:"offset,omitempty"`
	Title            string                      `json:"title,omitempty"`
	Body             string                      `json:"body,omitempty"`
	Decision         string                      `json:"decision,omitempty"`
	Bundle           *tracker.ChangeReviewBundle `json:"bundle,omitempty"`
	FileSHA256       string                      `json:"file_sha256,omitempty"`
	Viewed           bool                        `json:"viewed,omitempty"`
	ExpectedPolicyID string                      `json:"expected_review_policy_id,omitempty"`
	Policy           *tracker.ChangeReviewPolicy `json:"policy,omitempty"`
}

// ChangeResult contains bounded application data, never rendered HTML.
type ChangeResult struct {
	OrganizationID string                      `json:"organization_id"`
	ProjectID      string                      `json:"project_id"`
	WorkItemID     string                      `json:"work_item_id,omitempty"`
	ChangeID       string                      `json:"change_id,omitempty"`
	URL            string                      `json:"url,omitempty"`
	GeneratedAt    time.Time                   `json:"generated_at"`
	Freshness      string                      `json:"freshness"`
	NextOffset     *int                        `json:"next_offset,omitempty"`
	Changes        []tracker.ChangeRequest     `json:"changes,omitempty"`
	Detail         *tracker.ChangeDetail       `json:"detail,omitempty"`
	Version        *tracker.ChangeVersion      `json:"version,omitempty"`
	Policy         *tracker.ChangeReviewPolicy `json:"policy,omitempty"`
	Viewed         []tracker.ChangeViewedFile  `json:"viewed_files,omitempty"`
	Services       []artifact.Binding          `json:"services,omitempty"`
	Artifacts      []artifact.Reference        `json:"artifacts,omitempty"`
	Access         *ArtifactDownload           `json:"access,omitempty"`
	Receipt        json.RawMessage             `json:"receipt,omitempty"`
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
	for _, k := range []string{"project_id", "work_item_id", "change_id", "version_id", "artifact_id", "expected_review_policy_id"} {
		props[k] = str()
	}
	props["request_id"] = json.RawMessage(`{"type":"string","minLength":1,"maxLength":128}`)
	props["title"] = json.RawMessage(`{"type":"string","minLength":1,"maxLength":256}`)
	props["body"] = json.RawMessage(`{"type":"string","maxLength":32768}`)
	props["decision"] = json.RawMessage(`{"type":"string","enum":["approved","changes_requested","commented"]}`)
	for _, k := range []string{"expected_revision", "revision"} {
		props[k] = json.RawMessage(`{"type":"integer","minimum":1,"maximum":9007199254740991}`)
	}
	props["limit"] = json.RawMessage(`{"type":"integer","minimum":1,"maximum":200}`)
	props["offset"] = json.RawMessage(`{"type":"integer","minimum":0,"maximum":10000}`)
	props["viewed"] = json.RawMessage(`{"type":"boolean"}`)
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
		schema, _ := json.Marshal(struct {
			Type       string                     `json:"type"`
			Required   []string                   `json:"required"`
			Properties map[string]json.RawMessage `json:"properties"`
			Additional bool                       `json:"additionalProperties"`
		}{"object", req, fields, false})
		definitions = append(definitions, Definition{Name: name, Description: description, InputSchema: schema, Annotations: Annotations{ReadOnly: read, Destructive: material, Idempotent: true, OpenWorld: true}, Meta: ToolMetadata{Toolset: "changes_artifacts"}})
	}
	add(ListChanges, "List Change Requests linked to an owned work item.", "work_item_id", "limit offset", true, false)
	add(GetChange, "Read Change Request versions, discussion, reviews, checks and landing evidence.", "work_item_id change_id", "", true, false)
	add(GetChangeVersion, "Read one immutable version with code/diff and artifact references.", "work_item_id change_id version_id", "", true, false)
	add(GetChangeReviewPolicy, "Read the project's approved review policy.", "", "", true, false)
	add(ChangeViewedFiles, "Read this principal's viewed-file digests for a version.", "work_item_id change_id version_id", "limit offset", true, false)
	add(ArtifactServices, "Read installed artifact services without publisher credentials.", "", "", true, false)
	add(ArtifactReferences, "Read artifact receipts and availability for an owned work item.", "work_item_id", "limit offset", true, false)
	add(GetArtifactReference, "Read the exact immutable artifact receipt, including historical revisions.", "work_item_id artifact_id revision", "", true, false)
	add(ArtifactAccess, "Authorize an exact artifact revision for download/export through existing manifest/object endpoints. Token expires within one minute; send it as Bearer, never in a URL. Replays reauthorize and mint fresh ephemeral access.", "work_item_id artifact_id revision sha256 request_id", "", false, false)
	add(CreateChange, "Create a Change Request using the existing application command.", "work_item_id title request_id", "body", false, false)
	add(DiscussChange, "Discuss a change through its application command.", "work_item_id change_id body request_id", "version_id", false, false)
	add(ReviewChange, "Review the current immutable bundle. Approval/requests for changes require real operator confirmation; comments run directly.", "work_item_id change_id version_id expected_revision decision bundle request_id", "body", false, true)
	add(ViewChangeFile, "Record a viewed-file digest for an immutable review bundle.", "work_item_id change_id version_id bundle file_sha256 viewed request_id", "", false, false)
	add(ApproveChangeReviewPolicy, "Approve the project review policy using current administrator authority and operator confirmation.", "policy request_id", "expected_review_policy_id", false, true)
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
		if !ok || string(v) == "null" || string(v) == `""` {
			return args, ErrInvalidArguments
		}
	}
	for _, v := range []string{args.ProjectID, args.ItemID, args.ChangeID, args.VersionID, args.ArtifactID, args.ExpectedPolicyID} {
		if len(v) > 256 || strings.TrimSpace(v) != v || strings.ContainsAny(v, "/\\?#%") {
			return args, ErrInvalidArguments
		}
	}
	if args.ProjectID == "" || len(args.RequestID) > 128 || strings.TrimSpace(args.RequestID) != args.RequestID || len(args.Title) > 256 || len(args.Body) > 32768 || args.Limit < 0 || args.Limit > 200 || args.Offset < 0 || args.Offset > 10000 {
		return args, ErrInvalidArguments
	}
	for _, key := range []string{"revision", "expected_revision"} {
		if _, ok := fields[key]; ok {
			v := args.Revision
			if key == "expected_revision" {
				v = args.ExpectedRevision
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
