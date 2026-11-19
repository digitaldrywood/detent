package hubserver

import (
	"fmt"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/tracker"
)

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
