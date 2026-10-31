package runner

import (
	"context"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type NativeNoChangeExecution interface {
	NoChangeResult(context.Context) (*NativeNoChange, error)
}

type NativeNoChange struct {
	VerifiedSHA string
	CurrentBase bool
	Validation  *gate.CommandResult
	Review      *gate.ValidatorResult
	Validator   *gate.ValidatorResult
	Audit       *tracker.SecurityAuditReview
}

func (n *NativeNoChange) Accepted(p policy.Descriptor, command string) bool {
	if n == nil || !n.CurrentBase || n.VerifiedSHA == "" || p.Gates.RequiredChecks > 0 || p.Gates.HumanReview || p.Gates.Kind != "" && p.Gates.Kind != gate.KindCommand {
		return false
	}
	accepted := func(r *gate.ValidatorResult) bool {
		return r != nil && r.Submitted && r.Verdict == gate.ValidatorVerdictPass && r.BaseSHA == n.VerifiedSHA && r.HeadSHA == n.VerifiedSHA
	}
	if command != "" && (n.Validation == nil || n.Validation.Command != command || n.Validation.ExitCode != 0 || n.Validation.HeadSHA != n.VerifiedSHA) {
		return false
	}
	if p.Gates.AutomatedReview == gate.AutomatedReviewRequired && !accepted(n.Review) || p.Gates.Validator && !accepted(n.Validator) {
		return false
	}
	return !p.Gates.SecurityAudit || n.Audit != nil && n.Audit.Verdict == "pass" && n.Audit.Run.BaseSHA == n.VerifiedSHA && n.Audit.Run.HeadSHA == n.VerifiedSHA
}
