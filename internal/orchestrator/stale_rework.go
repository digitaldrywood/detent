package orchestrator

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type staleReworkEvidence struct {
	ChangeID        string   `json:"change_id"`
	VersionID       string   `json:"version_id"`
	HeadSHA         string   `json:"head_sha"`
	BaseSHA         string   `json:"base_sha"`
	BaseFingerprint string   `json:"base_fingerprint,omitempty"`
	Findings        []string `json:"findings,omitempty"`
}

func (o *Orchestrator) currentStaleReworkEvidence(ctx context.Context, issue connector.Issue, changeID string) *staleReworkEvidence {
	reader, ok := o.connector.(tracker.ChangeReader)
	if !ok {
		return nil
	}
	if changeID == "" {
		changes, err := reader.FetchChanges(ctx, tracker.NativeWorkItemID(issue.ID))
		if err != nil || len(changes) == 0 {
			return nil
		}
		latest := changes[0]
		for _, change := range changes[1:] {
			if change.UpdatedAt.After(latest.UpdatedAt) {
				latest = change
			}
		}
		changeID = latest.ID
	}
	detail, err := reader.FetchChange(ctx, tracker.NativeWorkItemID(issue.ID), changeID)
	if err != nil || detail.Change.ID != changeID || detail.Change.CurrentVersion == "" {
		return nil
	}
	var evidence *staleReworkEvidence
	for _, version := range detail.Versions {
		if version.ID == detail.Change.CurrentVersion && strings.TrimSpace(version.HeadSHA) != "" {
			evidence = &staleReworkEvidence{ChangeID: changeID, VersionID: version.ID, HeadSHA: version.HeadSHA, BaseSHA: version.BaseSHA}
			break
		}
	}
	if evidence == nil {
		return nil
	}
	evidence.BaseFingerprint = o.blockedCauseSignals(ctx, issue, RunModeImplement, autoPromoteReworkState, DiffStats{}).BaseFingerprint
	for _, review := range detail.Reviews {
		if review.VersionID != evidence.VersionID || review.Decision != "changes_requested" {
			continue
		}
		for _, result := range []*gate.ValidatorResult{review.Validator, review.ImplementationReview} {
			if result == nil || result.Verdict != gate.ValidatorVerdictRework {
				continue
			}
			for _, finding := range result.Findings {
				if strings.TrimSpace(finding.Path) != "" {
					evidence.Findings = append(evidence.Findings, fmt.Sprintf("review:%s:%d:%s", finding.Path, finding.Line, strings.ToLower(finding.Severity)))
				}
			}
		}
		if review.SecurityAudit != nil && review.SecurityAudit.Verdict == "rework" {
			for _, finding := range review.SecurityAudit.Run.Findings {
				if finding.Status != "resolved" && strings.TrimSpace(finding.Path) != "" {
					evidence.Findings = append(evidence.Findings, fmt.Sprintf("audit:%s:%d:%s", finding.Path, finding.Line, strings.ToLower(finding.Severity)))
				}
			}
		}
	}
	slices.Sort(evidence.Findings)
	evidence.Findings = slices.Compact(evidence.Findings)
	return evidence
}

func staleReworkEvidenceChanged(parked, current *staleReworkEvidence) bool {
	if parked == nil || current == nil || parked.ChangeID != current.ChangeID {
		return false
	}
	if parked.BaseFingerprint != "" && current.BaseFingerprint != "" && parked.BaseFingerprint != current.BaseFingerprint {
		return true
	}
	if parked.VersionID == current.VersionID {
		return false
	}
	if parked.HeadSHA != current.HeadSHA || parked.BaseSHA != "" && current.BaseSHA != "" && parked.BaseSHA != current.BaseSHA {
		return true
	}
	return slices.ContainsFunc(current.Findings, func(finding string) bool { return !slices.Contains(parked.Findings, finding) })
}
