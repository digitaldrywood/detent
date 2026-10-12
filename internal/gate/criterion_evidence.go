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
	EvidenceAttachment         = "attachment"
	EvidenceNotVerified        = "not_verified"
)

type AttachmentRead struct {
	ReceiptID    string `json:"receipt_id"`
	AttachmentID string `json:"attachment_id"`
	SHA256       string `json:"sha256"`
	ProjectID    string `json:"project_id"`
	WorkItemID   string `json:"work_item_id,omitempty"`
	CommentID    string `json:"comment_id,omitempty"`
	HeadSHA      string `json:"head_sha"`
	TreeSHA      string `json:"tree_sha"`
}

type CriterionEvidence struct {
	Criterion              string   `json:"criterion"`
	Kind                   string   `json:"kind"`
	Reference              string   `json:"reference"`
	ReceiptIDs             []string `json:"receipt_ids,omitempty"`
	HeadSHA                string   `json:"head_sha,omitempty"`
	TreeSHA                string   `json:"tree_sha,omitempty"`
	Behavior               string   `json:"behavior,omitempty"`
	RestatesImplementation bool     `json:"restates_implementation,omitempty"`
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
			case EvidenceAttachment:
				if !entry.matchesAttachmentReads(result.AttachmentReads, result.HeadSHA, tree) {
					entry.Kind = EvidenceNotVerified
					entry.Behavior = "Observed attachment and matching host read receipt required"
				}
			case EvidenceReceipt:
				if !entry.matchesReceipts(result.Commands, result.HeadSHA, tree) {
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

func (entry CriterionEvidence) matchesAttachmentReads(reads []AttachmentRead, head, tree string) bool {
	if entry.Reference == "" || entry.Behavior == "" || len(entry.ReceiptIDs) == 0 || head == "" || entry.HeadSHA != head || entry.TreeSHA == "" || tree != "" && entry.TreeSHA != tree {
		return false
	}
	seen := make(map[string]bool, len(entry.ReceiptIDs))
	for _, id := range entry.ReceiptIDs {
		if id == "" || seen[id] {
			return false
		}
		seen[id] = true
		if !slices.ContainsFunc(reads, func(read AttachmentRead) bool {
			return read.ReceiptID == id && read.AttachmentID != "" && read.SHA256 != "" && read.HeadSHA == head && read.TreeSHA == entry.TreeSHA
		}) {
			return false
		}
	}
	return true
}

func (entry CriterionEvidence) matchesReceipts(commands []CommandResult, head, tree string) bool {
	if entry.Reference == "" || entry.Behavior == "" || head == "" || entry.HeadSHA != head || entry.TreeSHA == "" || tree != "" && entry.TreeSHA != tree {
		return false
	}
	valid := func(command CommandResult) bool {
		return command.Command != "" && command.ExitCode == 0 && command.HeadSHA == head && command.TreeSHA == entry.TreeSHA
	}
	if len(entry.ReceiptIDs) == 0 {
		return slices.ContainsFunc(commands, func(command CommandResult) bool {
			return entry.Reference == command.Command && valid(command)
		})
	}
	seen := make(map[string]bool, len(entry.ReceiptIDs))
	for _, id := range entry.ReceiptIDs {
		if strings.TrimSpace(id) == "" || seen[id] {
			return false
		}
		seen[id] = true
		found := false
		for _, command := range commands {
			if command.ReceiptID != id {
				continue
			}
			if found || !valid(command) {
				return false
			}
			found = true
		}
		if !found {
			return false
		}
	}
	return true
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
