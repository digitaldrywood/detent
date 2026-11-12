package hubserver

import (
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/tracker"
)

// landingLane is the lane a reviewed Change Request waits in for the runner
// that lands it. It is dispatchable so the runner claims the item, and it is
// named rather than inferred so a workflow without one keeps its reviewed
// changes in review, where a person lands them by hand.
const landingLane = "Merging"

// landingTarget is the lane an approved review moves the primary issue to:
// the landing lane, when the issue's current lane may move there. It reports
// "" when the workflow has no such move.
func landingTarget(project tracker.NativeProject, current string) string {
	for _, state := range project.States {
		if state.Name != current {
			continue
		}
		for _, name := range state.Transitions {
			for _, target := range project.States {
				if target.Name == name && strings.EqualFold(target.Name, landingLane) && target.Dispatchable && !target.Terminal && !target.OperatorOnly {
					return target.Name
				}
			}
		}
	}
	return ""
}

// terminalTarget is the lane a landed issue finishes in: the first terminal
// lane the current lane may move to.
func terminalTarget(project tracker.NativeProject, current string) string {
	for _, state := range project.States {
		if state.Name != current {
			continue
		}
		for _, name := range state.Transitions {
			for _, target := range project.States {
				if target.Name == name && target.Terminal {
					return target.Name
				}
			}
		}
	}
	return ""
}

// reworkTarget is the lane a request for changes moves the primary issue
// to: a dispatchable, non-terminal lane the current lane may move to, where
// the runner picks the item up again. "In Progress" is preferred when the
// workflow has it, so the item resumes rather than restarts.
func reworkTarget(project tracker.NativeProject, current string) string {
	first := ""
	for _, state := range project.States {
		if state.Name != current {
			continue
		}
		for _, name := range state.Transitions {
			for _, target := range project.States {
				if target.Name != name || !target.Dispatchable || target.Terminal || target.OperatorOnly {
					continue
				}
				if strings.EqualFold(target.Name, "In Progress") {
					return target.Name
				}
				if first == "" {
					first = target.Name
				}
			}
		}
	}
	return first
}

// maxNativeCommentBytes is the comment body limit createNativeComment
// enforces; the generated header counts against it.
const maxNativeCommentBytes = 64 << 10

// reviewInstructions is the comment a request for changes leaves on the
// issue: a header naming the change and version, then the review text, cut
// at a character boundary so the whole comment stays within the limit a
// comment may be edited under.
func reviewInstructions(change tracker.ChangeRequest, review tracker.ChangeReview) string {
	body := "Changes requested on Change Request " + change.ID
	if review.VersionID != "" {
		body += " (version " + review.VersionID + ")"
	}
	body += "."
	text := strings.TrimSpace(review.Body)
	if text == "" {
		return body
	}
	body += "\n\n"
	room := maxNativeCommentBytes - len(body)
	if len(text) > room {
		cut := room
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		text = text[:cut]
	}
	return body + text
}

func (s *Service) landChange(c echo.Context) error {
	return s.landChangeRecord(c)
}

func nativeChangeLandingReady(state string, detail *tracker.ChangeDetail) bool {
	if !strings.EqualFold(strings.TrimSpace(state), "Merging") {
		return true
	}
	return detail != nil && detail.Change.CurrentLanding() == nil && detail.Change.CurrentVersion != "" && detail.Summary.Status == "reviewed"
}

func nativeChangeLandingClaimable(state string, detail *tracker.ChangeDetail) bool {
	if nativeChangeLandingReady(state, detail) {
		return true
	}
	return detail != nil && detail.Change.CurrentLanding() == nil && detail.Change.CurrentVersion != "" && detail.Summary.Status == "needs_evidence" &&
		slices.Contains([]string{"approved", "not_required", "stale"}, detail.Summary.NativeReview) &&
		slices.Contains([]string{"success", "not_required", "stale"}, detail.Summary.Checks)
}
