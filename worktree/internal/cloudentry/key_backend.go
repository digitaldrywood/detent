package cloudentry

import (
	"context"
	"database/sql"
	"errors"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/cloudentry/platformstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (r *Registry) postgresRegister(ctx context.Context, item Organization) (bool, error) {
	changed, err := r.store.db.postgres.Register(ctx, platformstore.Organization{ID: tracker.OrganizationID(item.ID), ProviderID: item.ProviderID, Name: item.Name, State: item.State, Endpoint: item.Endpoint, Generation: item.Generation})
	if errors.Is(err, platformstore.ErrOrganizationConflict) {
		return false, ErrOrganizationConflict
	}
	return changed, err
}

func (a *authStore) postgresKey(ctx context.Context, column, value string) (accessKey, error) {
	if column == "id" {
		if err := a.store.reader.QueryRowContext(ctx, "SELECT token_hash FROM access_keys WHERE id=?", value).Scan(&value); err != nil {
			return accessKey{}, err
		}
	} else if column != "token_hash" {
		return accessKey{}, errors.New("invalid key selector")
	}
	item, err := a.store.db.postgres.Key(ctx, value)
	if errors.Is(err, corestore.ErrNotFound) {
		return accessKey{}, sql.ErrNoRows
	}
	if err != nil {
		return accessKey{}, err
	}
	result := accessKey{ID: item.ID, Hash: item.Hash, Name: item.Name, Kind: item.Kind, Owner: item.OwnerSubject, OwnerEmail: item.OwnerEmail, Permission: apikey.Scope(item.Permission), AccessContext: item.AccessContext, CreatedAt: item.CreatedAt, ExpiresAt: item.ExpiresAt, RevokedAt: item.RevokedAt}
	if item.ServiceOrganizationID != nil {
		result.ServiceOrganization = string(*item.ServiceOrganizationID)
	}
	for _, org := range item.Organizations {
		context := apikey.ProjectContext{Access: org.ProjectAccess}
		for _, project := range org.Projects {
			context.Projects = append(context.Projects, string(project))
		}
		result.Organizations = append(result.Organizations, keyOrganization{OrganizationID: string(org.OrganizationID), ProjectContext: context})
	}
	if !result.valid() {
		return accessKey{}, errors.New("stored key context is invalid")
	}
	return result, nil
}

func (a *authStore) postgresCreateKey(ctx context.Context, key accessKey) error {
	item := platformstore.Key{ID: key.ID, Hash: key.Hash, Name: key.Name, Kind: key.Kind, OwnerSubject: key.Owner, OwnerEmail: key.OwnerEmail, Permission: string(key.Permission), AccessContext: key.AccessContext, CreatedAt: key.CreatedAt, ExpiresAt: key.ExpiresAt}
	if key.ServiceOrganization != "" {
		id := tracker.OrganizationID(key.ServiceOrganization)
		item.ServiceOrganizationID = &id
	}
	for _, org := range key.Organizations {
		selection := platformstore.KeyOrganization{OrganizationID: tracker.OrganizationID(org.OrganizationID), ProjectAccess: org.Access}
		for _, project := range org.Projects {
			selection.Projects = append(selection.Projects, tracker.ProjectID(project))
		}
		item.Organizations = append(item.Organizations, selection)
	}
	return a.store.db.postgres.CreateKey(ctx, item)
}

func (s *Service) keyRestrictions(ctx context.Context, id, organization string) (_ bool, _ []string, resultErr error) {
	var blocked bool
	err := s.auth.store.reader.QueryRowContext(ctx, "SELECT blocked FROM access_key_organizations WHERE key_id=? AND organization_id=?", id, organization).Scan(&blocked)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil, nil
	}
	if err != nil {
		return false, nil, err
	}
	rows, err := s.auth.store.reader.QueryContext(ctx, "SELECT project_id FROM access_key_blocked_projects WHERE key_id=? AND organization_id=? ORDER BY project_id LIMIT 201", id, organization)
	if err != nil {
		return false, nil, err
	}
	defer closeEntryRows(rows, &resultErr)
	var projects []string
	for rows.Next() {
		var project string
		if err := rows.Scan(&project); err != nil {
			return false, nil, err
		}
		projects = append(projects, project)
	}
	if len(projects) > 200 {
		return false, nil, errors.New("stored project exclusions are invalid")
	}
	return blocked, projects, rows.Err()
}
