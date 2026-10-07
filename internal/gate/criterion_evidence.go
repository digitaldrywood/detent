package gate

import (
	"slices"
	"strings"
)

const (
	UnverifiedCriteriaRework   = "rework"
	UnverifiedCriteriaDisclose = "pass-with-disclosure"
	EvidenceReceipt            = "receipt"
	EvidenceTest               = "test"
	EvidenceNotVerified        = "not_verified"
)

type CriterionEvidence struct {
	Criterion              string `json:"criterion"`
	Kind                   string `json:"kind"`
	Reference              string `json:"reference"`
	HeadSHA                string `json:"head_sha,omitempty"`
	TreeSHA                string `json:"tree_sha,omitempty"`
	Behavior               string `json:"behavior,omitempty"`
	RestatesImplementation bool   `json:"restates_implementation,omitempty"`
}

func NormalizeCriterionEvidence(result *ValidatorResult, criteria []string, tree string) {
	entries := make(map[string][]CriterionEvidence, len(result.CriteriaEvidence))
	for _, entry := range result.CriteriaEvidence {
		key := strings.TrimSpace(entry.Criterion)
		entries[key] = append(entries[key], entry)
	}
	result.CriteriaEvidence = nil
	result.NotVerified = nil
	for _, criterion := range criteria {
		entry := CriterionEvidence{Criterion: criterion, Kind: EvidenceNotVerified, Reference: "Evidence not supplied"}
		if matches := entries[criterion]; len(matches) == 1 {
			entry = matches[0]
			entry.Criterion = criterion
			entry.Kind = strings.ToLower(strings.TrimSpace(entry.Kind))
			entry.Reference = strings.TrimSpace(entry.Reference)
			entry.Behavior = strings.TrimSpace(entry.Behavior)
			if entry.RestatesImplementation {
				entry.Kind = EvidenceNotVerified
				entry.Behavior = "Test restates the implementation instead of asserting the criterion's behavior"
				finding := Finding{Severity: "p3", Body: criterion + ": " + entry.Behavior + " (" + entry.Reference + ")"}
				if !slices.Contains(result.Findings, finding) {
					result.Findings = append(result.Findings, finding)
				}
			}
			switch entry.Kind {
			case EvidenceReceipt:
				valid := false
				for _, command := range result.Commands {
					if entry.Reference != "" && entry.Behavior != "" && result.HeadSHA != "" && entry.Reference == command.Command && command.ExitCode == 0 && entry.HeadSHA == result.HeadSHA && command.HeadSHA == result.HeadSHA && entry.TreeSHA != "" && command.TreeSHA == entry.TreeSHA && (tree == "" || tree == command.TreeSHA) {
						valid = true
					}
				}
				if !valid {
					entry.Kind = EvidenceNotVerified
					entry.Behavior = "Missing, failed, or mismatched host command receipt"
				}
			case EvidenceTest:
				if entry.Reference == "" || entry.Behavior == "" {
					entry.Kind = EvidenceNotVerified
					entry.Behavior = "Named test and independent behavioral assertion required"
				}
			case EvidenceNotVerified:
			default:
				entry.Kind = EvidenceNotVerified
				entry.Behavior = "Unsupported evidence kind"
			}
		}
		if entry.Kind == EvidenceNotVerified {
			if entry.Reference == "" {
				entry.Reference = "Evidence not supplied"
			}
			result.NotVerified = append(result.NotVerified, criterion)
		}
		result.CriteriaEvidence = append(result.CriteriaEvidence, entry)
	}
}

func ApplyCriterionPolicy(cfg ValidatorConfig, result *ValidatorResult) {
	if cfg.UnverifiedCriteria != UnverifiedCriteriaDisclose && len(result.NotVerified) > 0 {
		if normalizeValidatorVerdict(result.Verdict) != ValidatorVerdictError {
			result.Verdict = ValidatorVerdictRework
		}
		disclosure := "Acceptance criteria not verified: " + strings.Join(result.NotVerified, "; ")
		if !strings.Contains(result.Summary, disclosure) {
			result.Summary = strings.TrimSpace(result.Summary + "\n" + disclosure)
		}
	}
}
