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
	CriterionReworkFixable     = "rework_fixable"
	CriterionUnsatisfiable     = "unsatisfiable"
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
	UnverifiedClass        string   `json:"unverified_class,omitempty"`
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
			if entry.Kind != EvidenceNotVerified || entry.RestatesImplementation {
				entry.UnverifiedClass = ""
			}
			if entry.RestatesImplementation {
				entry.Kind = EvidenceNotVerified
				entry.Behavior = "Test restates the implementation instead of asserting the criterion's behavior"
				finding := Finding{Severity: "p3", Body: criterion + ": " + entry.Behavior + " (" + entry.Reference + ")"}
				if !slices.Contains(result.Findings, finding) {
					result.Findings = append(result.Findings, finding)
				}
			}
			if entry.Kind == EvidenceReceipt && slices.ContainsFunc(result.AttachmentReads, func(read AttachmentRead) bool {
				return slices.Contains(entry.ReceiptIDs, read.ReceiptID)
			}) {
				entry.Kind = EvidenceAttachment
			}
			switch entry.Kind {
			case EvidenceAttachment:
				if !entry.matchesAttachmentReads(result.AttachmentReads, result.Commands, result.HeadSHA, tree) {
					entry.Kind = EvidenceNotVerified
					entry.Behavior = "Observed attachment and matching host read receipt required"
				}
			case EvidenceReceipt:
				if !entry.normalizeReceipts(result.Commands, result.HeadSHA, tree) {
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
			if result.VersionID != "" && strings.ToLower(strings.TrimSpace(entry.UnverifiedClass)) == CriterionUnsatisfiable {
				entry.UnverifiedClass = CriterionUnsatisfiable
			} else {
				entry.UnverifiedClass = CriterionReworkFixable
			}
			if entry.Reference == "" {
				entry.Reference = "Evidence not supplied"
			}
			result.NotVerified = append(result.NotVerified, criterion)
		} else {
			entry.UnverifiedClass = ""
		}
		result.CriteriaEvidence = append(result.CriteriaEvidence, entry)
	}
}

func (result ValidatorResult) UnsatisfiableCriteria() []string {
	var criteria []string
	for _, entry := range result.CriteriaEvidence {
		if entry.Kind == EvidenceNotVerified && entry.UnverifiedClass == CriterionUnsatisfiable && slices.Contains(result.NotVerified, entry.Criterion) && !slices.Contains(criteria, entry.Criterion) {
			criteria = append(criteria, entry.Criterion)
		}
	}
	return criteria
}

func (entry *CriterionEvidence) matchesAttachmentReads(reads []AttachmentRead, commands []CommandResult, head, tree string) bool {
	if entry.Reference == "" || entry.Behavior == "" || len(entry.ReceiptIDs) == 0 || head == "" {
		return false
	}
	seen := make(map[string]bool, len(entry.ReceiptIDs))
	var commandIDs []string
	observedAttachment := false
	for _, id := range entry.ReceiptIDs {
		if strings.TrimSpace(id) == "" || seen[id] {
			return false
		}
		seen[id] = true
		found := false
		for _, read := range reads {
			if read.ReceiptID != id {
				continue
			}
			if found || read.AttachmentID == "" || read.SHA256 == "" || read.HeadSHA != head || read.TreeSHA == "" || tree != "" && read.TreeSHA != tree {
				return false
			}
			tree = read.TreeSHA
			found = true
		}
		if !found {
			commandIDs = append(commandIDs, id)
			continue
		}
		observedAttachment = true
	}
	if !observedAttachment {
		return false
	}
	if len(commandIDs) > 0 {
		commandEntry := *entry
		commandEntry.ReceiptIDs = commandIDs
		if !commandEntry.normalizeReceipts(commands, head, tree) {
			return false
		}
	}
	entry.HeadSHA = head
	entry.TreeSHA = tree
	return true
}

func (entry *CriterionEvidence) normalizeReceipts(commands []CommandResult, head, tree string) bool {
	if entry.Reference == "" || entry.Behavior == "" || head == "" {
		return false
	}
	if len(entry.ReceiptIDs) == 0 {
		if entry.HeadSHA != head || entry.TreeSHA == "" || tree != "" && entry.TreeSHA != tree {
			return false
		}
		return slices.ContainsFunc(commands, func(command CommandResult) bool {
			return entry.Reference == command.Command && command.Command != "" && command.ExitCode == 0 && command.HeadSHA == head && command.TreeSHA == entry.TreeSHA
		})
	}
	seen := make(map[string]bool, len(entry.ReceiptIDs))
	var selected CommandResult
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
			if found || command.Command == "" || command.ExitCode != 0 || command.HeadSHA != head || command.TreeSHA == "" || tree != "" && command.TreeSHA != tree {
				return false
			}
			tree = command.TreeSHA
			found = true
			selected = command
		}
		if !found {
			return false
		}
	}
	entry.HeadSHA = selected.HeadSHA
	entry.TreeSHA = selected.TreeSHA
	return true
}

func ApplyCriterionPolicy(cfg ValidatorConfig, result *ValidatorResult) {
	unsatisfiable := len(result.UnsatisfiableCriteria()) > 0
	if unsatisfiable || cfg.UnverifiedCriteria != UnverifiedCriteriaDisclose && len(result.NotVerified) > 0 {
		if normalizeValidatorVerdict(result.Verdict) != ValidatorVerdictError {
			verdict := normalizeValidatorVerdict(result.Verdict)
			result.Verdict = ValidatorVerdictRework
			if effective := effectiveValidatorConfig(cfg); unsatisfiable && verdict != ValidatorVerdictRework && !validatorHasBlockedSeverity(effective, result.Findings) && result.Score >= effective.MinScore {
				result.Verdict = ValidatorVerdictWait
			}
		}
		disclosure := "Acceptance criteria not verified: " + strings.Join(result.NotVerified, "; ")
		if !strings.Contains(result.Summary, disclosure) {
			result.Summary = strings.TrimSpace(result.Summary + "\n" + disclosure)
		}
	}
}
