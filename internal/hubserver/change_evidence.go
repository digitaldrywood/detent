package hubserver

import (
	"fmt"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) reviewChange(c echo.Context) error {
	var request tracker.ReviewChange
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	return s.changeRecordEvidenceMutation(c, "review", c.Param("version"), "", request)
}

func (s *Service) submitChangeCheck(c echo.Context) error {
	var request tracker.SubmitChangeCheck
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	if s.config.Hosted != nil && nativeRequestScope(c).credential.Runner.RunnerID == "" && request.Source != "independent" {
		return s.nativeAPIError(c, hubapi.NativeNotFound())
	}
	if s.config.TenantRepository == nil {
		return s.nativeAPIError(c, hubapi.NativeWorkItemStorageUnavailable())
	}
	return s.changeCheckRecordMutation(c, request)
}

func (s *Service) discussChange(c echo.Context) error {
	var request tracker.DiscussChange
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	return s.changeRecordEvidenceMutation(c, "discussion", request.VersionID, "", request)
}

func changeReviewContentFields(request tracker.ReviewChange) []cloudContentField {
	fields := []cloudContentField{{name: "body", value: request.Body}}
	for _, result := range []struct {
		name  string
		value *gate.ValidatorResult
	}{{"validator", request.Validator}, {"implementation_review", request.ImplementationReview}} {
		if result.value == nil {
			continue
		}
		for i, evidence := range result.value.CriteriaEvidence {
			fields = append(fields, cloudContentField{name: fmt.Sprintf("%s.criteria_evidence[%d].reference", result.name, i), value: evidence.Reference})
		}
	}
	return fields
}
