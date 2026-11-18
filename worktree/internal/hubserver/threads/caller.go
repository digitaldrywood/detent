package threads

import (
	"context"
	"strings"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type CallerDeps struct {
	Item           string
	Actor          *tracker.Actor
	Authorize      func(context.Context, accessstore.Reader, tracker.ProjectID, string, bool) error
	RequireItem    func(context.Context, tenantstore.Reader, tracker.ProjectID, string, string) error
	ReadAttachment func(context.Context, tenantstore.Reader, tracker.ProjectID, string, string) error
}

func NewCaller(a *hubauth.Authenticator, input hubauth.Input, deps CallerDeps) Caller {
	credential := input.Credential
	actor := hubauth.Actor(credential, input.Organization)
	if deps.Actor != nil {
		actor = *deps.Actor
	}
	caller := Caller{
		Organization: input.Organization, Project: input.Project, Item: deps.Item,
		Principal: hubauth.Identity(credential, string(input.Organization)).PrincipalID, ActorPrincipal: actor.PrincipalID,
		CredentialID: credential.ID, Name: credential.Name, Agent: credential.Agent, HostedRole: credential.HostedRole,
		Hosted: credential.Hosted != nil, EntryKey: credential.EntryKey != nil,
		Personal:      credential.HostedKeyKind == "personal" || credential.EntryKey != nil && credential.EntryKey.Kind == "personal",
		AllProjects:   credential.Hosted == nil && credential.Scope == hubauth.Admin && !credential.NativeOnly,
		ProjectFilter: hubauth.ProjectFilter(credential, "", "", 0),
		Authority: func(project tracker.ProjectID, item string) accessstore.Authority {
			bound := input
			bound.Project = project
			return hubauth.Authority(bound, false)
		},
		Authorize: deps.Authorize, RequireItem: deps.RequireItem, ReadAttachment: deps.ReadAttachment,
	}
	if caller.Authorize == nil {
		caller.Authorize = func(ctx context.Context, reader accessstore.Reader, project tracker.ProjectID, item string, write bool) error {
			bound := input
			bound.Project = project
			return a.Scope(bound).Authorize(ctx, reader, write)
		}
	}
	if caller.RequireItem == nil {
		caller.RequireItem = func(ctx context.Context, reader tenantstore.Reader, project tracker.ProjectID, item, id string) error {
			row, err := reader.WorkItem(ctx, project, tracker.NativeWorkItemID(id))
			if err != nil {
				return err
			}
			if credential.HostedRole == "reporter" && (actor.PrincipalID == "" || hubapi.StringValue(row.ActorPrincipalID) != actor.PrincipalID) {
				return corestore.ErrNotFound
			}
			return nil
		}
	}
	if credential.Hosted != nil {
		caller.HostedName = strings.TrimSpace(credential.Hosted.FirstName + " " + credential.Hosted.LastName)
		caller.HostedSubject = credential.Hosted.Subject
	}
	return caller
}
