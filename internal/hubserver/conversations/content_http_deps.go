package conversations

import (
	"context"
	"encoding/json"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type ArtifactHTTPDeps struct {
	StorageAvailable bool
	Hosted           bool
	Now              func() time.Time
	APIError         func(echo.Context, error, ...any) error
	InvalidRequest   func(echo.Context, error) error
	Refused          func(echo.Context, int, string, string, any, ...any) error
	Bind             func(context.Context, hubscope.NativeScope, hubapi.NativeCommandOptions, BindArtifactServiceRequest) (json.RawMessage, error)
	PublisherToken   func(context.Context, hubscope.NativeScope, string) (string, error)
	References       func(context.Context, hubscope.NativeScope, string) ([]artifact.Reference, error)
	HostedReader     func(echo.Context, string, string) (hubapi.APICredential, error)
	Services         func(context.Context, hubscope.NativeScope) ([]artifact.Binding, error)
	ReadGrant        func(context.Context, hubscope.NativeScope, string, string, int64, time.Time) (artifact.Grant, error)
	Reference        func(context.Context, hubscope.NativeScope, string, string, int64, time.Time) (artifact.Reference, error)
	PublishReceipt   func(context.Context, hubscope.NativeScope, artifact.Reference) error
	AuthorizeUpload  func(context.Context, hubscope.NativeScope, artifact.Reservation) error
	ReadStoredGrant  func(context.Context, hubscope.NativeScope, string, artifact.ReadAuthorization) (artifactstore.ReadGrant, tracker.Actor, error)
}

type ArtifactHTTP struct{ deps ArtifactHTTPDeps }

func NewArtifactHTTP(deps ArtifactHTTPDeps) ArtifactHTTP { return ArtifactHTTP{deps: deps} }

type AttachmentHTTPDeps struct {
	StorageAvailable  bool
	APIError          func(echo.Context, error, ...any) error
	CheckEvidence     func(echo.Context, *attachment.EvidenceRequest) error
	Authorize         func(context.Context, accessstore.Reader, hubscope.NativeScope, bool) error
	QuotaLimit        func(context.Context, tenantstore.Reader) (int64, error)
	ReadTenant        func(context.Context, tracker.OrganizationID, func(tenantstore.Reader) error) error
	SharedClaims      func(echo.Context) (cloudassert.Claims, bool)
	PrepareUpload     func(echo.Context, attachment.UploadRequest) error
	RecordUpload      func(echo.Context, attachment.UploadRequest) error
	ReadMetadata      func(context.Context, hubscope.NativeScope, string) (attachment.Metadata, error)
	DeleteMetadata    func(context.Context, hubscope.NativeScope, string) error
	ReferenceMetadata func(context.Context, hubscope.NativeScope, string, attachment.SourceReference, tracker.Mutation) error
	Expired           func(echo.Context) error
	FinishDeletion    func(echo.Context) error
}

type AttachmentHTTP struct{ deps AttachmentHTTPDeps }

func NewAttachmentHTTP(deps AttachmentHTTPDeps) AttachmentHTTP { return AttachmentHTTP{deps: deps} }

type AttachmentArchiveHTTPDeps struct {
	Now                   func() time.Time
	APIError              func(echo.Context, error, ...any) error
	InvalidRequest        func(echo.Context, error) error
	ReadWorkItem          func(context.Context, hubscope.NativeScope, string) (corestore.WorkItem, eventstore.CollaborationVersion, error)
	RequireReporterTriage func(hubscope.NativeScope, tracker.NativeIssue) error
	WriteAuthority        func(context.Context, tenantstore.Writer, hubscope.NativeScope, *attachment.EvidenceRequest) (corestore.WorkItem, error)
	QuotaLimit            func(context.Context, tenantstore.Reader) (int64, error)
	ReadConversation      func(context.Context, hubscope.NativeScope, string) (ConversationRecord, error)
	Authorize             func(context.Context, accessstore.Reader, hubscope.NativeScope, bool) error
	ReadTenant            func(context.Context, tracker.OrganizationID, func(tenantstore.Reader) error) error
	WriteTenant           func(context.Context, tracker.OrganizationID, func(tenantstore.Writer) error) error
}

type AttachmentArchiveHTTP struct{ deps AttachmentArchiveHTTPDeps }

func NewAttachmentArchiveHTTP(deps AttachmentArchiveHTTPDeps) AttachmentArchiveHTTP {
	return AttachmentArchiveHTTP{deps: deps}
}

type ContentSettingsHTTPDeps struct {
	StorageAvailable bool
	APIError         func(echo.Context, error, ...any) error
	InvalidRequest   func(echo.Context, error) error
	ReadProject      func(context.Context, hubscope.NativeScope) (projectstore.Metadata, error)
	MutationFeature  func(echo.Context) string
	UpdateProject    func(context.Context, hubscope.NativeScope, hubapi.NativeCommandOptions, ContentSettingsRequest) (json.RawMessage, error)
}

type ContentSettingsHTTP struct{ deps ContentSettingsHTTPDeps }

func NewContentSettingsHTTP(deps ContentSettingsHTTPDeps) ContentSettingsHTTP {
	return ContentSettingsHTTP{deps: deps}
}
