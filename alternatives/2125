package hubapi

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Facts struct {
	Organization tracker.OrganizationID
	Project      tracker.ProjectID
	Actor        tracker.Actor
	CredentialID string
	HostedRole   string
	Admin        bool
	Runner       runnerauth.Identity
}

type ProjectAuthorizer func(ctx context.Context, reader accessstore.Reader, project tracker.ProjectID, write bool) error

type Scope struct {
	Facts
	authorize ProjectAuthorizer
}

func NewScope(facts Facts, authorize func(context.Context, accessstore.Reader, bool) error) Scope {
	if authorize == nil {
		return Scope{Facts: facts}
	}
	bound := facts.Project
	return Scope{Facts: facts, authorize: func(ctx context.Context, reader accessstore.Reader, project tracker.ProjectID, write bool) error {
		if project != bound {
			return accessstore.ErrDenied
		}
		return authorize(ctx, reader, write)
	}}
}

func NewProjectScope(facts Facts, authorize ProjectAuthorizer) Scope {
	return Scope{Facts: facts, authorize: authorize}
}

func (s Scope) Authorize(ctx context.Context, reader accessstore.Reader, write bool) error {
	if s.authorize == nil || reader == nil {
		return accessstore.ErrDenied
	}
	return s.authorize(ctx, reader, s.Project, write)
}

func (s Scope) ForProject(project tracker.ProjectID) Scope {
	s.Project = project
	return s
}
