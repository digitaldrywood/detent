package hubserver

import (
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

const duplicateChallengeLifetime = time.Hour

type possibleDuplicate struct {
	revision   tracker.Revision
	terminal   bool
	WorkItemID tracker.NativeWorkItemID `json:"work_item_id"`
	Identifier string                   `json:"identifier"`
	Number     int                      `json:"number"`
	Title      string                   `json:"title"`
	State      string                   `json:"state"`
	WebURL     string                   `json:"web_url"`
	Why        string                   `json:"why"`
}

type duplicateChallenge struct {
	PossibleDuplicates []possibleDuplicate `json:"possible_duplicates"`
	ChallengeToken     string              `json:"challenge_token"`
}

type nativeDuplicateSearch struct {
	latestNumber int
	matches      []possibleDuplicate
	skipped      bool
}
