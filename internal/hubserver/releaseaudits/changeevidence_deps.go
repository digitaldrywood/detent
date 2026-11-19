package releaseaudits

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnersession"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/labstack/echo/v4"
)

type ChangeEvidenceDeps struct {
	Repository                    tenantstore.Repository
	Clock                         func() time.Time
	AuthorizeAccessScope          func(context.Context, accessstore.Reader, hubscope.NativeScope, bool) error
	RunDetailStore                func() attachment.DetailStore
	NativeAPIError                func(echo.Context, error) error
	InvalidAPIRequest             func(echo.Context, error) error
	RunnerSessionFence            func(context.Context, tenantstore.Reader, hubscope.NativeScope, runnersession.Mutation, string, bool) (leasestore.Attempt, policystore.Revision, error)
	ValidateNativeDetailReference func(context.Context, attachment.DetailStore, tracker.OrganizationID, attachment.ArchiveReference, []byte) error
}

type ChangeEvidence struct {
	deps ChangeEvidenceDeps
}

func NewChangeEvidence(deps ChangeEvidenceDeps) *ChangeEvidence {
	return &ChangeEvidence{deps: deps}
}
