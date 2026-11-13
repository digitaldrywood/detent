package hubserver

import (
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type changeMutationPreparationKey struct{}

type changeMutationPreparation struct {
	change           tracker.ChangeRequest
	version          tracker.ChangeVersion
	publication      *preparedChangeVersion
	quality          *preparedQualityEscape
	review           tracker.ReviewChange
	reviewFields     []cloudContentField
	record           tracker.ChangeReview
	storedReview     tracker.ChangeReview
	reviewRaw        string
	reviewSlots      map[string][]attributionSlot
	sessionMachineID tracker.MachineID
	snapshot         changeMutationSnapshot
	sourceAttempt    string
	lease            tracker.LeaseID
	promote          bool
	evaluatedAt      time.Time
	promotionUntil   time.Time
	outputs          map[string]attachment.ArchiveReference
}

type changeMutationSnapshot struct {
	change, policy, rules, sourceRun, currentRun     string
	issueRevision, contentRevision, evidenceSequence int64
}
