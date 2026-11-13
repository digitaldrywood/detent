package hubserver

import (
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
)

const duplicateChallengeLifetime = time.Hour

type duplicateChallenge struct {
	PossibleDuplicates []hubscope.PossibleDuplicate `json:"possible_duplicates"`
	ChallengeToken     string                       `json:"challenge_token"`
}
