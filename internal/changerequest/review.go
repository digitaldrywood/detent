package changerequest

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func PolicyID(rules tracker.ChangeReviewPolicy) string {
	rules.ID = ""
	data, err := json.Marshal(rules)
	if err != nil {
		return ""
	}
	return "review_" + policy.Digest(data)
}

func ValidHash(value string, size int) bool {
	if len(value) != size || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func ValidReference(value string) bool {
	if len(value) > 2048 || strings.ContainsAny(value, "\r\n\t") {
		return false
	}
	u, err := url.Parse(value)
	if err != nil || u == nil {
		return false
	}
	return slices.Contains([]string{"https", "s3", "gs"}, u.Scheme) && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == ""
}

func ValidateArtifacts(artifacts []tracker.ChangeArtifact) error {
	if len(artifacts) > 64 {
		return errors.New("at most 64 artifact references are permitted")
	}
	for _, artifact := range artifacts {
		if !slices.Contains([]string{"code", "manifest", "diff", "test", "log", "checkpoint", "artifact"}, artifact.Kind) || !ValidReference(artifact.URI) || !ValidHash(artifact.SHA256, 64) || !slices.Contains([]string{"unverified", "available", "missing", "inaccessible"}, artifact.Availability) {
			return errors.New("artifacts require a typed location without credentials, SHA-256 digest, and availability")
		}
	}
	return nil
}

func ValidatePolicy(rules tracker.ChangeReviewPolicy, approved policy.Descriptor) error {
	if rules.PolicyID != approved.ID || len(rules.RequiredChecks) > 256 || len(rules.RequiredChecks) < approved.Gates.RequiredChecks {
		return errors.New("review policy must match the approved repository policy and required check floor")
	}
	if approved.Gates.Kind == "human_review" && !rules.RequireReview {
		return errors.New("repository human review cannot be disabled")
	}
	names := map[string]bool{}
	independent := 0
	for _, check := range rules.RequiredChecks {
		if strings.TrimSpace(check.Name) == "" || len(check.Name) > 128 || names[check.Name] || check.PrincipalID == "" || len(check.PrincipalID) > 128 || strings.TrimSpace(check.WorkflowID) == "" || len(check.WorkflowID) > 256 || !ValidHash(check.WorkflowSHA256, 64) || !slices.Contains([]string{"customer", "independent"}, check.Source) || check.MaxAgeSeconds < 60 || check.MaxAgeSeconds > 7*24*60*60 {
			return errors.New("checks require unique names, pinned principals and workflows, explicit source, and freshness from 60 seconds to 7 days")
		}
		names[check.Name] = true
		if check.Source == "independent" {
			independent++
		}
	}
	if approved.Gates.Validator && independent == 0 {
		return errors.New("repository validator gate requires an independent check")
	}
	return nil
}

func ValidateResult(version tracker.ChangeVersion, result tracker.ChangeCheckResult, principal string, worker bool) error {
	if result.HeadSHA != version.HeadSHA || result.RunID != version.RunID || result.PolicyID != version.PolicyID || result.ConfigDigest != version.Policy.ConfigDigest || result.CompletedAt.Before(version.CreatedAt) {
		return errors.New("CI result does not match the immutable head, run, policy, and configuration")
	}
	if !slices.Contains([]string{"success", "failure", "cancelled", "skipped"}, result.Conclusion) {
		return errors.New("CI result requires a terminal conclusion")
	}
	for _, check := range version.Checks {
		if check.CheckRunID != result.CheckRunID {
			continue
		}
		if check.PrincipalID != principal || check.WorkflowID != result.WorkflowID || check.WorkflowSHA256 != result.WorkflowSHA256 || check.Source != result.Source || worker && check.Source == "independent" {
			return errors.New("CI source, authenticated principal, or workflow does not match the expected check")
		}
		if len(result.Evidence) == 0 {
			return errors.New("CI results require evidence references")
		}
		return ValidateArtifacts(result.Evidence)
	}
	return errors.New("CI check run was not requested for this version")
}

func Summarize(detail tracker.ChangeDetail, policyID, reviewPolicyID string, now time.Time) tracker.ChangeSummary {
	summary := tracker.ChangeSummary{NativeReview: "pending", ExternalReview: "not_linked", Checks: "missing", Status: "draft", Messages: []string{}}
	var current *tracker.ChangeVersion
	for i := range detail.Versions {
		if detail.Versions[i].ID == detail.Change.CurrentVersion {
			current = &detail.Versions[i]
			break
		}
	}
	if current == nil {
		summary.Messages = append(summary.Messages, "No immutable version has been published.")
		return summary
	}
	summary.Status = "needs_evidence"
	if current.External != nil {
		summary.ExternalReview = "external_gate"
		summary.Messages = append(summary.Messages, "GitHub required reviews and protected merge checks are evaluated by the repository integration.")
	}
	if !current.ReviewPolicy.RequireReview {
		summary.NativeReview = "not_required"
	}
	latest := map[string]string{}
	staleApproval := false
	for _, review := range detail.Reviews {
		if review.VersionID == current.ID && review.Decision != "commented" {
			latest[review.Actor.PrincipalID] = review.Decision
		} else if review.Decision == "approved" {
			staleApproval = true
		}
	}
	for _, decision := range latest {
		if decision == "approved" && summary.NativeReview == "pending" {
			summary.NativeReview = "approved"
		}
		if decision == "changes_requested" {
			summary.NativeReview = "changes_requested"
		}
	}
	if staleApproval && summary.NativeReview == "pending" {
		summary.NativeReview = "stale"
		summary.Messages = append(summary.Messages, "Approval belongs to an older version. Review the current version.")
	}
	if len(current.Checks) > 0 {
		summary.Checks = "success"
	} else if current.ReviewPolicy.ID != "" && len(current.ReviewPolicy.RequiredChecks) == 0 {
		summary.Checks = "not_required"
	}
	for _, expected := range current.Checks {
		found := false
		for _, result := range detail.Checks {
			if result.VersionID != current.ID || result.CheckRunID != expected.CheckRunID {
				continue
			}
			found = true
			if result.Conclusion != "success" {
				summary.Checks = "failure"
			} else if !now.Before(result.CompletedAt.Add(time.Duration(expected.MaxAgeSeconds)*time.Second)) || result.ReceivedAt.After(now) {
				summary.Checks = "stale"
			}
			for _, artifact := range result.Evidence {
				if artifact.Availability != "available" {
					summary.Checks = "missing"
				}
			}
		}
		if !found {
			summary.Checks = "missing"
		}
	}
	checksSatisfied := slices.Contains([]string{"success", "not_required"}, summary.Checks)
	if !checksSatisfied {
		summary.Messages = append(summary.Messages, "Current version checks are "+summary.Checks+". Missing or stale evidence does not pass review.")
	}
	if checksSatisfied && slices.Contains([]string{"approved", "not_required"}, summary.NativeReview) {
		summary.Status = "reviewed"
	}
	if policyID != current.PolicyID || reviewPolicyID != current.ReviewPolicy.ID {
		summary.Status = "stale_policy"
		summary.Messages = append(summary.Messages, "Approved repository or review policy changed. Publish a new version with the current policy.")
	}
	return summary
}
