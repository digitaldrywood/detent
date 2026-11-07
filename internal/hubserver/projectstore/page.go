package projectstore

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const PagePartLimit = 4096

type Page struct {
	Body  string
	Model config.ModelSelection
}

type PageCandidate struct {
	Record    Record
	Models    policystore.OrganizationModels
	ProjectID tracker.ProjectID
	Ready     bool
	SourceAt  time.Time
}

type PageReader interface {
	ProjectPage(context.Context, Record) (Page, bool, error)
}

type PageMaintainer interface {
	NextProjectPage(context.Context, tracker.ProjectID) (PageCandidate, error)
	PublishProjectPage(context.Context, PageCandidate, Page) (bool, error)
}

type ModelPart struct {
	Kind            string
	Name            string
	Parent          int64
	Edge            string
	Model           *string
	Effort          *string
	Level           *string
	Duration        *int
	Tokens          *int64
	IssueComplexity *bool
	Enabled         *bool
	Preset          *string
	NormalModel     *string
	ComplexModel    *string
	DefaultLevel    *string
	Unavailable     *string
	BackendKinds    []string
	FallbackOrder   []string
	LevelsPresent   bool
	StagesPresent   bool
	RulesPresent    bool
	Disabled        bool
	Roles           []string
	Efforts         []string
	Assignees       []string
	Authors         []string
	Priorities      []int
	Include         []string
	Exclude         []string
	FieldNames      []string
	FieldValues     []string
}
