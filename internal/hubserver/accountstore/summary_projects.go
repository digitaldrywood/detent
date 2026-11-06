package accountstore

import (
	"context"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type SummaryProject struct {
	ID         tracker.ProjectID
	Name, Slug string
}

type SummaryProjectReader interface {
	SummaryProjects(context.Context, string, int) ([]SummaryProject, error)
}
