package intake

import (
	"context"
	"errors"

	"github.com/digitaldrywood/detent/internal/issueorigin"
)

type OccurrenceCommenter interface {
	CreateComment(context.Context, string, string) error
}

func CommentOccurrence(ctx context.Context, store any, issueID, body string) error {
	commenter, ok := store.(OccurrenceCommenter)
	if !ok {
		return errors.New("issue store cannot comment on duplicate occurrences")
	}
	return commenter.CreateComment(ctx, issueID, issueorigin.Occurrence(body))
}
