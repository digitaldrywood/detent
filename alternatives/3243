package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func savedViewRecord(v savedWorkView, org tracker.OrganizationID) viewstore.Record {
	r := viewstore.Record{OrganizationID: org, ID: v.ID, OwnerID: v.OwnerID, Name: v.Name, Scope: v.Scope, Favorite: v.Favorite, State: viewstore.State(v.State), ProjectIDs: []tracker.ProjectID{}}
	if v.ProjectID != nil {
		r.ProjectID = new(tracker.ProjectID(*v.ProjectID))
	}
	for _, id := range v.ProjectIDs {
		r.ProjectIDs = append(r.ProjectIDs, tracker.ProjectID(id))
	}
	return r
}

func savedViewResponse(r viewstore.Record, user string) savedWorkView {
	v := savedWorkView{CanEdit: r.OwnerID == user, ID: r.ID, OwnerID: r.OwnerID, Name: r.Name, Scope: r.Scope, Favorite: r.Favorite, State: savedWorkState(r.State), ProjectIDs: []string{}}
	if r.ProjectID != nil {
		v.ProjectID = new(string(*r.ProjectID))
	}
	for _, id := range r.ProjectIDs {
		v.ProjectIDs = append(v.ProjectIDs, string(id))
	}
	return v
}

func (s *Service) viewRecordScope(c echo.Context) (nativeScope, error) {
	credential, _, err := s.appCredential(c)
	if err != nil {
		return nativeScope{}, err
	}
	if c.Param("organization") != s.appOrganizationID() {
		return nativeScope{}, nativeNotFound()
	}
	return nativeScope{organization: tracker.OrganizationID(s.appOrganizationID()), project: tracker.ProjectID(c.Param("project")), credential: credential}, nil
}

func (s *Service) workViewPreferenceRecord(c echo.Context) error {
	scope, err := s.viewRecordScope(c)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	ctx := c.Request().Context()
	user := appSubject(scope.credential)
	value := workViewPreference{}
	if c.Request().Method == http.MethodGet {
		err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
			if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
				return err
			}
			query, err := r.WorkViewPreference(ctx, user, string(scope.project))
			if errors.Is(err, corestore.ErrNotFound) {
				return nil
			}
			if err != nil {
				return err
			}
			value.Query = &query
			return nil
		})
	} else {
		if c.Request().Method == http.MethodPut {
			if err := decodeAPIJSON(c, &value); err != nil {
				return s.invalidAPIRequest(c, err)
			}
			if value.Query == nil || len(*value.Query) > 8192 {
				return s.nativeAPIError(c, nativePublicInvalid("A view query of at most 8192 bytes is required"))
			}
			if _, err := url.ParseQuery(*value.Query); err != nil {
				return s.nativeAPIError(c, nativePublicInvalid("The view query is invalid"))
			}
		}
		err = s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
			if err := s.authorizeAccessScope(ctx, w, scope, false); err != nil {
				return err
			}
			return w.SetWorkViewPreference(ctx, user, string(scope.project), value.Query)
		})
	}
	if err != nil {
		return s.nativeAPIError(c, projectRecordError(err))
	}
	return c.JSON(http.StatusOK, value)
}

func (s *Service) savedWorkViewsRecord(c echo.Context) error {
	scope, err := s.viewRecordScope(c)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	ctx := c.Request().Context()
	user, id := appSubject(scope.credential), c.Param("view")
	selection := viewstore.Selection{UserID: user, EnforceProjectGrants: s.config.Hosted != nil}
	if c.Request().Method == http.MethodGet {
		return s.readSavedWorkViewsRecord(c, scope, selection, id)
	}
	favorite := strings.HasSuffix(c.Path(), "/favorite")
	var v savedWorkView
	if c.Request().Method != http.MethodDelete {
		if err := decodeAPIJSON(c, &v); err != nil {
			return s.invalidAPIRequest(c, err)
		}
		v.Name = strings.TrimSpace(v.Name)
		if !favorite && !validSavedWorkView(v) {
			return s.nativeAPIError(c, nativePublicInvalid("The view name, scope or display state is invalid"))
		}
	}
	create := id == ""
	if create {
		id = newNativeID("view")
	}
	v.ID, v.OwnerID = id, user
	var result viewstore.Record
	err = s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := s.authorizeAccessScope(ctx, w, scope, false); err != nil {
			return err
		}
		switch {
		case c.Request().Method == http.MethodDelete:
			return w.DeleteSavedWorkView(ctx, selection, id)
		case favorite:
			if err := w.SetSavedWorkViewFavorite(ctx, selection, id, v.Favorite); err != nil {
				return err
			}
		default:
			for _, project := range v.ProjectIDs {
				projectScope := scope
				projectScope.project = tracker.ProjectID(project)
				if err := s.authorizeAccessScope(ctx, w, projectScope, false); err != nil {
					return err
				}
			}
			if err := w.PutSavedWorkView(ctx, selection, savedViewRecord(v, scope.organization), create); err != nil {
				return err
			}
		}
		var err error
		result, err = w.SavedWorkView(ctx, selection, id)
		return err
	})
	if errors.Is(err, viewstore.ErrOwnerLimit) {
		err = nativePublicInvalid("The saved view limit for this owner has been reached")
	}
	if errors.Is(err, viewstore.ErrOrganizationLimit) {
		err = nativePublicInvalid("The saved view limit for this organization has been reached")
	}
	if err != nil {
		return s.nativeAPIError(c, projectRecordError(err))
	}
	if c.Request().Method == http.MethodDelete {
		return c.NoContent(http.StatusNoContent)
	}
	return c.JSON(http.StatusOK, savedViewResponse(result, user))
}

func (s *Service) readSavedWorkViewsRecord(c echo.Context, scope nativeScope, selection viewstore.Selection, id string) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	params := c.QueryParams()
	if id == "" {
		for key, values := range params {
			if key != "cursor" || len(values) != 1 || len(values[0]) > 100 {
				return s.nativeAPIError(c, nativePublicInvalid("Invalid saved views cursor"))
			}
		}
	}
	page := savedWorkViewsPage{Views: []savedWorkView{}}
	var single savedWorkView
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		ids := []string{id}
		if id == "" {
			var err error
			ids, err = r.SavedWorkViewIDs(ctx, selection, params.Get("cursor"), maxSavedWorkViewsPageSize+1)
			if err != nil {
				return err
			}
		}
		for _, key := range ids {
			if len(page.Views) == maxSavedWorkViewsPageSize {
				page.NextCursor = page.Views[len(page.Views)-1].ID
				break
			}
			record, err := r.SavedWorkView(ctx, selection, key)
			if err != nil {
				return err
			}
			v := savedViewResponse(record, selection.UserID)
			if id != "" {
				single = v
				return nil
			}
			candidate := savedWorkViewsPage{Views: append(page.Views, v), NextCursor: key}
			encoded, err := json.Marshal(candidate)
			if err != nil {
				return err
			}
			if len(encoded)+1 > maxSavedWorkViewsPageBytes {
				if len(page.Views) == 0 {
					return nativePublicInvalid("Saved view exceeds the page size limit")
				}
				page.NextCursor = page.Views[len(page.Views)-1].ID
				break
			}
			page.Views = candidate.Views
		}
		return nil
	})
	if err != nil {
		return s.nativeAPIError(c, projectRecordError(err))
	}
	if id != "" {
		return c.JSON(http.StatusOK, single)
	}
	return c.JSON(http.StatusOK, page)
}
