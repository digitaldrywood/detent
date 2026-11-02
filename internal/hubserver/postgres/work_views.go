package postgres

import (
	"context"
	"errors"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) WorkViewPreference(ctx context.Context, user, scope string) (string, error) {
	value, err := s.queries.WorkViewPreference(ctx, pgquery.WorkViewPreferenceParams{OrganizationID: s.organization, UserID: user, Scope: scope})
	return value, mapError(ctx, err)
}

func (s *session) SetWorkViewPreference(ctx context.Context, user, scope string, value *string) error {
	if value == nil {
		return mapError(ctx, s.queries.DeleteWorkViewPreference(ctx, pgquery.DeleteWorkViewPreferenceParams{OrganizationID: s.organization, UserID: user, Scope: scope}))
	}
	return mapError(ctx, s.queries.PutWorkViewPreference(ctx, pgquery.PutWorkViewPreferenceParams{OrganizationID: s.organization, UserID: user, Scope: scope, ViewQuery: *value}))
}

func (s *session) SavedWorkViewIDs(ctx context.Context, selection viewstore.Selection, cursor string, limit int) ([]string, error) {
	if err := checkPage(limit); err != nil {
		return nil, err
	}
	ids, err := s.queries.SavedWorkViewIDs(ctx, pgquery.SavedWorkViewIDsParams{OrganizationID: s.organization, UserID: selection.UserID, EnforceGrants: selection.EnforceProjectGrants, Cursor: cursor, PageLimit: int32(limit)})
	return ids, mapError(ctx, err)
}

func (s *session) SavedWorkView(ctx context.Context, selection viewstore.Selection, id string) (viewstore.Record, error) {
	row, err := s.queries.SavedWorkView(ctx, pgquery.SavedWorkViewParams{OrganizationID: s.organization, ID: id, UserID: selection.UserID, EnforceGrants: selection.EnforceProjectGrants})
	if err != nil {
		return viewstore.Record{}, mapError(ctx, err)
	}
	v := viewstore.Record{OrganizationID: row.OrganizationID, ID: row.ID, OwnerID: row.OwnerID, Name: row.Name, Scope: row.Scope, ProjectID: row.ProjectID, Favorite: row.Favorite, State: viewstore.State{View: row.Layout, Tab: row.Tab, Q: row.Search, Sort: row.Sort, CompletedWindow: row.CompletedWindow, CompletedFrom: row.CompletedFrom, CompletedTo: row.CompletedTo, Archived: row.Archived, CreatedBy: row.CreatedBy, State: []string{}, Type: []string{}, Label: []string{}, Assignee: []string{}, Priority: []string{}, Collapsed: []string{}}}
	v.ProjectIDs = make([]tracker.ProjectID, len(row.ProjectIds))
	for i, project := range row.ProjectIds {
		v.ProjectIDs[i] = tracker.ProjectID(project)
	}
	v.State.State, v.State.Type, v.State.Label = row.StateValues, row.TypeValues, row.LabelValues
	v.State.Assignee, v.State.Priority, v.State.Collapsed = row.AssigneeValues, row.PriorityValues, row.CollapsedValues
	if !row.AllLanes {
		v.State.Lanes = row.LanesValues
	}
	return v, nil
}

func (s *session) PutSavedWorkView(ctx context.Context, selection viewstore.Selection, v viewstore.Record, create bool) error {
	if v.OrganizationID != s.organization {
		return corestore.ErrTenantMismatch
	}
	if selection.UserID != v.OwnerID {
		return corestore.ErrNotFound
	}
	if v.ProjectIDs == nil || len(v.ProjectIDs) > 100 {
		return viewstore.ErrInvalidView
	}
	valueCount := 0
	for _, values := range [][]string{v.State.State, v.State.Type, v.State.Label, v.State.Assignee, v.State.Priority, v.State.Lanes, v.State.Collapsed} {
		valueCount += len(values)
		for _, value := range values {
			if len(value) == 0 || len(value) > 512 {
				return viewstore.ErrInvalidView
			}
		}
	}
	if valueCount > 256 || len(v.State.Q) > 8192 {
		return viewstore.ErrInvalidView
	}
	var ownerSlot, organizationSlot int32
	if create {
		var err error
		ownerSlot, err = s.queries.SavedWorkViewOwnerSlot(ctx, pgquery.SavedWorkViewOwnerSlotParams{OrganizationID: s.organization, OwnerID: v.OwnerID})
		if errors.Is(mapError(ctx, err), corestore.ErrNotFound) {
			return viewstore.ErrOwnerLimit
		}
		if err != nil {
			return mapError(ctx, err)
		}
		organizationSlot, err = s.queries.SavedWorkViewOrganizationSlot(ctx, s.organization)
		if errors.Is(mapError(ctx, err), corestore.ErrNotFound) {
			return viewstore.ErrOrganizationLimit
		}
		if err != nil {
			return mapError(ctx, err)
		}
	} else {
		current, err := s.queries.LockSavedWorkView(ctx, pgquery.LockSavedWorkViewParams{OrganizationID: s.organization, ID: v.ID})
		if err != nil {
			return mapError(ctx, err)
		}
		if current.OwnerID != selection.UserID {
			return corestore.ErrNotFound
		}
		if _, err := s.SavedWorkView(ctx, selection, v.ID); err != nil {
			return err
		}
		ownerSlot, organizationSlot = current.OwnerSlot, current.OrganizationSlot
	}
	changed, err := s.queries.PutSavedWorkView(ctx, pgquery.PutSavedWorkViewParams{OrganizationID: s.organization, ID: v.ID, OwnerID: v.OwnerID, Name: v.Name, Scope: v.Scope, ProjectID: v.ProjectID, Layout: v.State.View, Tab: v.State.Tab, Search: v.State.Q, Sort: v.State.Sort, CompletedWindow: v.State.CompletedWindow, CompletedFrom: v.State.CompletedFrom, CompletedTo: v.State.CompletedTo, Archived: v.State.Archived, CreatedBy: v.State.CreatedBy, AllLanes: v.State.Lanes == nil, OwnerSlot: ownerSlot, OrganizationSlot: organizationSlot, Creating: create})
	if err != nil {
		return mapError(ctx, err)
	}
	if changed != 1 {
		return corestore.ErrRevisionConflict
	}
	if err := s.queries.DeleteSavedWorkViewValues(ctx, pgquery.DeleteSavedWorkViewValuesParams{OrganizationID: s.organization, ViewID: v.ID}); err != nil {
		return mapError(ctx, err)
	}
	for kind, values := range map[string][]string{"state": v.State.State, "type": v.State.Type, "label": v.State.Label, "assignee": v.State.Assignee, "priority": v.State.Priority, "lanes": v.State.Lanes, "collapsed": v.State.Collapsed} {
		for position, value := range values {
			if err := s.queries.PutSavedWorkViewValue(ctx, pgquery.PutSavedWorkViewValueParams{OrganizationID: s.organization, ViewID: v.ID, Kind: kind, Position: int32(position), Value: value}); err != nil {
				return mapError(ctx, err)
			}
		}
	}
	if err := s.queries.DeleteSavedWorkViewProjects(ctx, pgquery.DeleteSavedWorkViewProjectsParams{OrganizationID: s.organization, ViewID: v.ID}); err != nil {
		return mapError(ctx, err)
	}
	for position, project := range v.ProjectIDs {
		if err := s.queries.PutSavedWorkViewProject(ctx, pgquery.PutSavedWorkViewProjectParams{OrganizationID: s.organization, ViewID: v.ID, ProjectID: project, Position: int32(position)}); err != nil {
			return mapError(ctx, err)
		}
	}
	return s.SetSavedWorkViewFavorite(ctx, selection, v.ID, v.Favorite)
}

func (s *session) DeleteSavedWorkView(ctx context.Context, selection viewstore.Selection, id string) error {
	if _, err := s.queries.LockSavedWorkView(ctx, pgquery.LockSavedWorkViewParams{OrganizationID: s.organization, ID: id}); err != nil {
		return mapError(ctx, err)
	}
	if _, err := s.SavedWorkView(ctx, selection, id); err != nil {
		return err
	}
	changed, err := s.queries.DeleteSavedWorkView(ctx, pgquery.DeleteSavedWorkViewParams{OrganizationID: s.organization, ID: id, OwnerID: selection.UserID})
	if err != nil {
		return mapError(ctx, err)
	}
	if changed != 1 {
		return corestore.ErrNotFound
	}
	return nil
}

func (s *session) SetSavedWorkViewFavorite(ctx context.Context, selection viewstore.Selection, id string, favorite bool) error {
	if _, err := s.queries.LockSavedWorkView(ctx, pgquery.LockSavedWorkViewParams{OrganizationID: s.organization, ID: id}); err != nil {
		return mapError(ctx, err)
	}
	if _, err := s.SavedWorkView(ctx, selection, id); err != nil {
		return err
	}
	if favorite {
		return mapError(ctx, s.queries.PutSavedWorkViewFavorite(ctx, pgquery.PutSavedWorkViewFavoriteParams{OrganizationID: s.organization, ViewID: id, UserID: selection.UserID}))
	}
	return mapError(ctx, s.queries.DeleteSavedWorkViewFavorite(ctx, pgquery.DeleteSavedWorkViewFavoriteParams{OrganizationID: s.organization, ViewID: id, UserID: selection.UserID}))
}
