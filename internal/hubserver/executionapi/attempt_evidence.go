package executionapi

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type EvidenceDeps struct {
	Repository    tenantstore.Repository
	Now           func() time.Time
	PlansEnabled  func() bool
	ValidateLease func(context.Context, hubscope.NativeScope, tracker.LeaseID, tracker.FencingToken) (runnerauth.Runner, error)
	StaleLease    func(error, string) error
}

func (d EvidenceDeps) CheckAttachmentEvidence(c echo.Context, input *attachment.EvidenceRequest) (resultErr error) {
	if c.Param("attempt") == "" {
		if input != nil {
			return hubapi.NativePublicInvalid("Evidence requires its attempt endpoint")
		}
		return nil
	}
	if input == nil || input.AttemptID != c.Param("attempt") || strings.TrimSpace(input.Caption) == "" || len(input.Caption) > 1024 || !utf8.ValidString(input.Caption) || strings.ContainsAny(input.Caption, "\r\n\x00") || input.IdempotencyKey == "" || input.LeaseID == "" || input.FencingToken <= 0 {
		return hubapi.NativePublicInvalid("Invalid attempt evidence")
	}
	return d.CheckAttachmentEvidenceTenant(c.Request().Context(), hubscope.NativeRequestScope(c), *input)
}

func EscapeEvidenceCaption(caption string) string {
	escape := strings.NewReplacer("\\", "\\\\", "`", "\\`", "*", "\\*", "_", "\\_", "[", "\\[", "]", "\\]", "<", "&lt;", ">", "&gt;", "#", "\\#", "!", "\\!")
	return escape.Replace(caption)
}
