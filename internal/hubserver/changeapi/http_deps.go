package changeapi

import (
	"context"
	"encoding/json"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type HTTPDeps struct {
	RunDetailContext                func(ctx context.Context) context.Context
	StorageArchiveContent           func() entryauthority.ContentHandler
	AuthorizeAccessScope            func(ctx context.Context, r accessstore.Reader, scope hubscope.NativeScope, write bool) error
	AuthorizeWorkItemRecordScope    func(ctx context.Context, r accessstore.Reader, scope hubscope.NativeScope) error
	ReviewChangeRecordCommand       func(ctx context.Context, scope hubscope.NativeScope, item, changeID, versionID, operation string, request tracker.ReviewChange) (json.RawMessage, error)
	Repository                      tenantstore.Repository
	Clock                           func() time.Time
	NativeAPIError                  func(c echo.Context, err error, attributes ...any) error
	InvalidAPIRequest               func(c echo.Context, err error) error
	RequireNativeScope              func(roles ...hubapi.APIScope) echo.MiddlewareFunc
	RequireInstanceAdmin            func() echo.MiddlewareFunc
	RequireOnboardingAdmin          func() echo.MiddlewareFunc
	RunDetailStore                  func() attachment.DetailStore
	ChangeReviewPolicyRecord        func(ctx context.Context, scope hubscope.NativeScope) (tracker.ChangeReviewPolicy, error)
	ApproveChangeReviewPolicyRecord func(c echo.Context, request tracker.ApproveChangeReviewPolicy) error
	ListChangeRecords               func(ctx context.Context, scope hubscope.NativeScope, item string) ([]tracker.ChangeRequest, error)
	CreateChangeRecord              func(c echo.Context, request tracker.CreateChange) error
	SelectedChangeDetail            func(ctx context.Context, scope hubscope.NativeScope, item, id string, now time.Time) (tracker.ChangeDetail, error)
	ReadBoundedChangeRecord         func(ctx context.Context, scope hubscope.NativeScope, args operatortool.ChangeArguments, result operatortool.ChangeResult) (operatortool.ChangeResult, error)
	ChangeRecordEvidenceMutation    func(c echo.Context, kind, versionID, sourceKey string, value any) error
	ChangeCheckRecordMutation       func(c echo.Context, request tracker.SubmitChangeCheck) error
	SameNativeClaimReviewPolicy     func(left eventstore.ReviewPolicy, right eventstore.ReviewPolicy) bool
	NativeArchiveJSON               func(c echo.Context, status int, value any) error
	ChangeCheckRequest              func(c echo.Context) bool
	HostedOrganization              func() (string, bool)
	ChangeResult                    func(hubscope.NativeScope, operatortool.ChangeArguments) operatortool.ChangeResult
}

type HTTP struct {
	deps HTTPDeps
}

func NewHTTP(deps HTTPDeps) *HTTP {
	return &HTTP{deps: deps}
}
