package tracker

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/gate"
)

const NativeStepReceiptBytes = 256 << 10

type NativeStepAttachment struct {
	ID             string           `json:"id"`
	OrganizationID OrganizationID   `json:"organization_id"`
	ProjectID      ProjectID        `json:"project_id"`
	WorkItemID     NativeWorkItemID `json:"work_item_id"`
	SHA256         string           `json:"sha256"`
	Size           int64            `json:"size"`
}

type NativeStepReceipt struct {
	PayloadAttachment *NativeStepAttachment `json:"payload_attachment,omitempty"`
	PayloadArtifact   *artifact.Reference   `json:"payload_artifact,omitempty"`
	Step              string                `json:"step"`
	PolicyID          string                `json:"policy_id"`
	ReviewState       string                `json:"review_state"`
	ReviewEpoch       FencingToken          `json:"review_epoch,string,omitempty"`
	Source            NativeCheckpoint      `json:"source"`
	Change            NativeChangeReference `json:"change,omitzero"`
	CompletedAt       time.Time             `json:"completed_at"`
	CompletionBody    string                `json:"completion_body,omitempty"`
	FinalizedBaseSHA  string                `json:"finalized_base_sha,omitempty"`
	Validation        *gate.CommandResult   `json:"validation,omitempty"`
	Validator         *gate.ValidatorResult `json:"validator,omitempty"`
	SecurityAudit     *SecurityAuditReview  `json:"security_audit,omitempty"`
}

func NativeStepReviewState(recovery NativeRecovery) string {
	state := []string{recovery.Issue.Title, recovery.Issue.Body}
	for _, comment := range recovery.Discussion {
		if comment.Actor.Kind == "human" || comment.EditedBy != nil && comment.EditedBy.Kind == "human" {
			state = append(state, comment.ID, comment.Body, comment.CreatedAt.UTC().Format(time.RFC3339Nano), comment.UpdatedAt.UTC().Format(time.RFC3339Nano))
		}
	}
	if detail := recovery.ChangeDetail; detail != nil {
		for _, comment := range detail.Discussion {
			if comment.Actor.Kind == "human" {
				state = append(state, comment.ID, comment.Body, comment.CreatedAt.UTC().Format(time.RFC3339Nano))
			}
		}
		for _, review := range detail.Reviews {
			if review.Decision == "changes_requested" {
				state = append(state, review.ID, review.Decision, review.CreatedAt.UTC().Format(time.RFC3339Nano))
			}
		}
	}
	var key strings.Builder
	for _, value := range state {
		key.WriteString(strconv.Itoa(len(value)))
		key.WriteByte(':')
		key.WriteString(value)
	}
	digest := sha256.Sum256([]byte(key.String()))
	return hex.EncodeToString(digest[:])
}

func NativeStepReviewEpoch(recovery NativeRecovery) FencingToken {
	var epoch FencingToken
	for _, attempt := range recovery.Attempts {
		for _, receipt := range attempt.Steps {
			epoch = max(epoch, receipt.ReviewEpoch)
		}
		if attempt.Runtime != nil && attempt.Runtime.Validation != nil && attempt.Runtime.Validation.ExitCode != 0 {
			epoch = max(epoch, attempt.FencingToken)
		}
	}
	return epoch
}

func (r NativeStepReceipt) Matches(expected NativeStepReceipt) bool {
	return r.Step == expected.Step && r.PolicyID == expected.PolicyID && r.ReviewState == expected.ReviewState && r.ReviewEpoch == expected.ReviewEpoch &&
		r.Change == expected.Change && r.Source.HeadSHA != "" && r.Source.HeadSHA == expected.Source.HeadSHA &&
		r.Source.WorkspaceDigest != "" && r.Source.WorkspaceDigest == expected.Source.WorkspaceDigest
}
