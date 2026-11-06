package actionstore

import (
	"context"
	"strconv"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

type Record struct {
	Action  workspacesession.Action
	Command attachment.ArchiveReference
}

type Reader interface {
	ProjectActions(context.Context, tracker.ProjectID) ([]Record, error)
	ProjectAction(context.Context, tracker.ProjectID, string) (Record, error)
}

type Writer interface {
	Reader
	CreateProjectAction(context.Context, Record) error
	UpdateProjectAction(context.Context, Record, int64) error
	DeleteProjectAction(context.Context, tracker.ProjectID, string) error
}

func ArchiveOwnerID(project tracker.ProjectID, action string) string {
	return strconv.Itoa(len(project)) + ":" + string(project) + action
}
