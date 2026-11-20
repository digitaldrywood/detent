package progresscheck

import (
	"errors"
	"strings"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const MaxChecklistEntries = 64
const maxChecklistTextBytes = 32 << 10

func FreezeChecklist(body string, cfg config.Config) (tracker.ExecutionChecklist, error) {
	contract, err := cfg.PolicyIssueContract()
	if err != nil {
		return tracker.ExecutionChecklist{}, err
	}
	gateConfig := gate.Effective(cfg.Gate)
	checklist := tracker.ExecutionChecklist{
		Revision: ChecklistRevision(body, cfg.Policy.ID),
		PolicyID: cfg.Policy.ID,
		Checks:   []tracker.ExecutionCheck{},
	}
	bytes := 0
	add := func(kind tracker.ExecutionCheckKind, text, digest string, status tracker.ExecutionCheckStatus) {
		bytes += len(text)
		checklist.Checks = append(checklist.Checks, tracker.ExecutionCheck{Ordinal: len(checklist.Checks) + 1, Kind: kind, Text: text, CheckDigest: digest, Status: status})
	}
	for _, criterion := range contract.AcceptanceCriteria(body) {
		add(tracker.CheckAcceptance, criterion, "", tracker.CheckNeedsReview)
	}
	for _, criterion := range config.VerificationCriteria(body) {
		if gateConfig.Run != "" && referencesCheck(criterion, gateConfig.Run) {
			continue
		}
		add(tracker.CheckVerification, criterion, "", tracker.CheckNeedsReview)
	}
	if gateConfig.Run != "" {
		add(tracker.CheckProjectGate, "Project command gate", policy.Digest([]byte(gateConfig.Run)), tracker.CheckUnverified)
	}
	for _, check := range gateConfig.RequiredStatusChecks {
		add(tracker.CheckStatusGate, check, policy.Digest([]byte(check)), tracker.CheckUnverified)
	}
	if gateConfig.Validator.Enabled {
		add(tracker.CheckIndependentReview, "Independent acceptance review", "", tracker.CheckUnverified)
	}
	if gateConfig.SecurityAudit.Enabled {
		add(tracker.CheckSecurityAudit, "Independent security audit", "", tracker.CheckUnverified)
	}
	if len(checklist.Checks) > MaxChecklistEntries || bytes > maxChecklistTextBytes {
		return tracker.ExecutionChecklist{}, errors.New("issue checklist exceeds the execution evidence limit")
	}
	return checklist, nil
}

func ChecklistRevision(body, policyID string) string {
	return policy.Digest([]byte(policyID + "\x00" + body))
}

func referencesCheck(criterion, command string) bool {
	if strings.TrimSpace(criterion) == command {
		return true
	}
	fields := strings.Split(criterion, "`")
	for i := 1; i+1 < len(fields); i += 2 {
		if fields[i] == command {
			return true
		}
	}
	return false
}
