package eventstore

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestChangeReviewProjectionPreservesValidHistoricalOwners(t *testing.T) {
	base := tracker.ChangeReview{ID: "review_example", VersionID: "version_example", Decision: "approved", SessionMachineID: "machine_example", Actor: tracker.Actor{Kind: "runner", PrincipalID: "runner_example"}, CreatedAt: time.Date(2026, 1, 1, 1, 2, 3, 123456789, time.UTC)}
	row := Evidence{Kind: "review", VersionID: new(base.VersionID), Decision: base.Decision, ActorKind: base.Actor.Kind, ActorPrincipalID: base.Actor.PrincipalID, CreatedAt: base.CreatedAt.Truncate(time.Microsecond)}
	for _, test := range []struct {
		name    string
		edit    func(*tracker.ChangeReview)
		kind    string
		session int64
		invalid bool
	}{
		{name: "human", kind: "human", edit: func(*tracker.ChangeReview) {}},
		{name: "validator", kind: "validator", session: 11, edit: func(v *tracker.ChangeReview) { v.Validator = &gate.ValidatorResult{SessionID: 11} }},
		{name: "implementation", kind: "implementation", session: 22, edit: func(v *tracker.ChangeReview) { v.ImplementationReview = &gate.ValidatorResult{SessionID: 22} }},
		{name: "audit", kind: "security_audit", session: 33, edit: func(v *tracker.ChangeReview) { v.SecurityAudit = &tracker.SecurityAuditReview{SessionID: 33} }},
		{name: "implementation and validator", invalid: true, edit: func(v *tracker.ChangeReview) {
			v.Validator, v.ImplementationReview = &gate.ValidatorResult{SessionID: 11}, &gate.ValidatorResult{SessionID: 22}
		}},
		{name: "audit and validator", invalid: true, edit: func(v *tracker.ChangeReview) {
			v.SecurityAudit, v.Validator = &tracker.SecurityAuditReview{SessionID: 33}, &gate.ValidatorResult{SessionID: 11}
		}},
		{name: "audit and implementation", invalid: true, edit: func(v *tracker.ChangeReview) {
			v.SecurityAudit, v.ImplementationReview = &tracker.SecurityAuditReview{SessionID: 33}, &gate.ValidatorResult{SessionID: 22}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			value := base
			test.edit(&value)
			projected, err := ProjectChangeReview(row, value)
			if test.invalid {
				if !errors.Is(err, ErrInvalidRecord) {
					t.Fatal("combined machine ownership was projected as one session")
				}
				return
			}
			if err != nil || !projected.ReviewSessionKnown || projected.ReviewKind != test.kind || projected.ReviewSessionID != test.session || projected.ReviewSessionMachine != base.SessionMachineID {
				t.Fatalf("historical owner projection = %s session=%d: %v", projected.ReviewKind, projected.ReviewSessionID, err)
			}
			raw, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := DecodeChangeReview(projected, raw)
			if err != nil {
				t.Fatal(err)
			}
			actual, err := json.Marshal(decoded)
			if err != nil || !bytes.Equal(actual, raw) {
				t.Fatal("valid historical owner payload changed during projection")
			}
		})
	}
	for _, test := range []struct {
		name string
		edit func(*tracker.ChangeReview)
	}{
		{name: "missing identity", edit: func(v *tracker.ChangeReview) { v.ID = "" }},
		{name: "version", edit: func(v *tracker.ChangeReview) { v.VersionID = "other" }},
		{name: "decision", edit: func(v *tracker.ChangeReview) { v.Decision = "changes_requested" }},
		{name: "actor", edit: func(v *tracker.ChangeReview) { v.Actor.PrincipalID = "other" }},
		{name: "time", edit: func(v *tracker.ChangeReview) { v.CreatedAt = v.CreatedAt.Add(time.Second) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			value := base
			test.edit(&value)
			raw, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := DecodeChangeReview(row, raw); !errors.Is(err, ErrInvalidRecord) {
				t.Fatal("mismatched canonical review metadata was accepted")
			}
		})
	}
}
