package hubserver

import (
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func validateNativeAdmissionContext(c tracker.NativeAdmissionContext) error {
	if err := c.Validate(); err != nil {
		return hubapi.NativeInvalid(err.Error())
	}
	return nil
}
