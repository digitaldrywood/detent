package changeapi

import (
	"encoding/json"

	"github.com/digitaldrywood/detent/internal/gate"

	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func AppendBoundedChangeEvidence(detail *tracker.ChangeDetail, row eventstore.Evidence, raw []byte, boundReview func(*gate.ValidatorResult) *gate.ValidatorResult) error {
	switch row.Kind {
	case "review":
		var value tracker.ChangeReview
		if json.Unmarshal(raw, &value) != nil || value.ID == "" || value.VersionID != hubapi.ValueOrEmpty(row.VersionID) || value.Decision != row.Decision || value.Actor.Kind != row.ActorKind || value.Actor.PrincipalID != row.ActorPrincipalID || !sameTypedEvidenceTime(value.CreatedAt, row.CreatedAt) {
			return eventstore.ErrInvalidRecord
		}
		if row.ReviewKind != "" {
			kind, landing := "human", false
			switch {
			case value.SecurityAudit != nil:
				kind, landing = "security_audit", value.SecurityAudit.Landing
			case value.ImplementationReview != nil:
				kind = "implementation"
			case value.Validator != nil:
				kind = "validator"
			}
			if row.ReviewKind != kind || row.ReviewLanding != landing {
				return eventstore.ErrInvalidRecord
			}
		}
		value.Validator = boundReview(value.Validator)
		value.ImplementationReview = boundReview(value.ImplementationReview)
		detail.Reviews = append(detail.Reviews, value)
	case "check":
		var value tracker.ChangeCheck
		if json.Unmarshal(raw, &value) != nil || value.VersionID != hubapi.ValueOrEmpty(row.VersionID) || value.CheckRunID != hubapi.ValueOrEmpty(row.SourceKey) || value.Actor.Kind != row.ActorKind || value.Actor.PrincipalID != row.ActorPrincipalID || !sameTypedEvidenceTime(value.ReceivedAt, row.CreatedAt) {
			return eventstore.ErrInvalidRecord
		}
		detail.Checks = append(detail.Checks, value)
	case "discussion":
		var value tracker.ChangeDiscussion
		if json.Unmarshal(raw, &value) != nil || value.ID == "" || value.VersionID != hubapi.ValueOrEmpty(row.VersionID) || value.Actor.Kind != row.ActorKind || value.Actor.PrincipalID != row.ActorPrincipalID || !sameTypedEvidenceTime(value.CreatedAt, row.CreatedAt) {
			return eventstore.ErrInvalidRecord
		}
		detail.Discussion = append(detail.Discussion, value)
	default:
		return eventstore.ErrInvalidRecord
	}
	return nil
}
