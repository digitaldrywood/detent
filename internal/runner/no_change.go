package runner

import (
	"context"
	"fmt"
	"strings"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type NoChangeExecution interface {
	PrepareNoChange(context.Context, string) (string, bool, error)
}

func noChangeReviewIssue(issue connector.Issue, completion *tracker.NativeNoChangeCompletion) connector.Issue {
	if completion == nil {
		return issue
	}
	issue.Description += fmt.Sprintf("\n\n## No change needed evidence\n\nThe worker claims acceptance already holds without a source change. Independently verify every criterion on current base %s. The worker reported SHA %s; evidence for any other SHA is historical and must be re-verified on the pinned current base. Reject with concrete implementation feedback when acceptance is not demonstrated. No Change or landing will follow accepted evidence.\n\n%s", completion.VerifiedSHA, completion.ReportedSHA, completion.Evidence)
	return issue
}

func (c *NativeChange) NoChangeAccepted(cfg gate.Config) bool {
	if c == nil || c.NoChange == nil || c.Changed || c.ChangeID != "" || c.VersionID != "" || c.Landing != nil || c.Files != 0 || c.Error != "" || c.VersionError != "" {
		return false
	}
	n := c.NoChange
	if n.VerifiedSHA == "" || n.VersionID() == "" || strings.TrimSpace(n.Evidence) == "" || n.PolicyID == "" || n.Repository == "" || c.HeadSHA != n.VerifiedSHA || c.BaseSHA != n.VerifiedSHA {
		return false
	}
	cfg = gate.Effective(cfg)
	if cfg.Run != "" {
		commands := []*gate.CommandResult{n.Validation}
		for _, result := range []*gate.ValidatorResult{c.ImplementationReview, c.Validator} {
			if result != nil {
				for i := range result.Commands {
					commands = append(commands, &result.Commands[i])
				}
			}
		}
		passed := false
		for _, command := range commands {
			if command != nil && command.Command == cfg.Run && command.ExitCode == 0 && command.HeadSHA == n.VerifiedSHA && command.TreeSHA != "" {
				passed = true
			}
		}
		if !passed {
			return false
		}
	}
	accept := func(result *gate.ValidatorResult, required bool) bool {
		if !required {
			return true
		}
		if result == nil || !result.Submitted || result.SessionID <= 0 || result.VersionID != n.VersionID() || result.HeadSHA != n.VerifiedSHA || result.BaseSHA != n.VerifiedSHA || result.Repository != n.Repository || result.Verdict != gate.ValidatorVerdictPass {
			return false
		}
		validator := cfg.Validator
		validator.Enabled = true
		_, stopped := gate.EvaluateValidator(validator, *result)
		return !stopped
	}
	if !accept(c.ImplementationReview, gate.AutomatedReviewMode(cfg) == gate.AutomatedReviewRequired) || !accept(c.Validator, cfg.Validator.Enabled) {
		return false
	}
	if cfg.SecurityAudit.Enabled {
		audit := c.SecurityAudit
		if audit == nil || audit.Landing || audit.VersionID != n.VersionID() || audit.Verdict != "pass" || audit.SessionID <= 0 || audit.SessionID != audit.Run.ID {
			return false
		}
		key := securityaudit.Key{ProjectID: audit.Run.ProjectID, Repository: n.Repository, BaseSHA: n.VerifiedSHA, HeadSHA: n.VerifiedSHA}
		if !securityaudit.Evaluate(audit.Run, nil, key, "", cfg.SecurityAudit.BlockOn).Allowed {
			return false
		}
	}
	return true
}
