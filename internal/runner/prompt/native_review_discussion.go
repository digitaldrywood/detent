package prompt

import (
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func NativeReviewDiscussionForVersion(issue []tracker.NativeComment, change []tracker.ChangeDiscussion, version string) NativeReviewDiscussion {
	discussion := NativeReviewDiscussion{Issue: issue}
	for _, comment := range change {
		if comment.VersionID == "" || comment.VersionID == version {
			discussion.Change = append(discussion.Change, comment)
		}
	}
	return discussion
}

func (d NativeReviewDiscussion) HumanFeedbackSince(reviewed time.Time) bool {
	human := func(actor tracker.Actor) bool { return actor.Kind == "human" && actor.PrincipalID != "" }
	for _, comment := range d.Issue {
		if human(comment.Actor) && !comment.CreatedAt.Before(reviewed) || comment.EditedBy != nil && human(*comment.EditedBy) && !comment.UpdatedAt.Before(reviewed) {
			return true
		}
	}
	for _, comment := range d.Change {
		if human(comment.Actor) && !comment.CreatedAt.Before(reviewed) {
			return true
		}
	}
	return false
}
