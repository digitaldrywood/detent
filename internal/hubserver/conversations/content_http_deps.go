package conversations

import (
	"context"
	"encoding/json"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
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
