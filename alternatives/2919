package postgres_test

import (
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestSavedWorkViewsRetainTenantAndOwnerScope(t *testing.T) {
	repository, _ := repositoryFor(t, template.Database(t), smallPool())
	selection := viewstore.Selection{UserID: "first"}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedTenantAccess(t, repository, org)
		for _, id := range []string{"view_first", "view_second"} {
			v := viewstore.Record{OrganizationID: org, ID: id, OwnerID: "first", Name: string(org) + id, Scope: "personal", ProjectIDs: []tracker.ProjectID{}, Favorite: true, State: viewstore.State{View: "board", Tab: "active", Sort: "default", CompletedWindow: "24h", Label: []string{"one", "two"}}}
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				if err := w.PutSavedWorkView(t.Context(), selection, v, true); err != nil {
					return err
				}
				return w.SetWorkViewPreference(t.Context(), "first", id, new(string(org)+id))
			}); err != nil {
				t.Fatal(err)
			}
		}
		if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			ids, err := r.SavedWorkViewIDs(t.Context(), selection, "", 1)
			if err != nil || !reflect.DeepEqual(ids, []string{"view_first"}) {
				t.Fatalf("first page=%v error=%v", ids, err)
			}
			ids, err = r.SavedWorkViewIDs(t.Context(), selection, ids[0], 1)
			if err != nil || !reflect.DeepEqual(ids, []string{"view_second"}) {
				t.Fatalf("second page=%v error=%v", ids, err)
			}
			for _, id := range []string{"view_first", "view_second"} {
				v, err := r.SavedWorkView(t.Context(), selection, id)
				if err != nil {
					return err
				}
				if v.OrganizationID != org || v.Name != string(org)+id || !v.Favorite || v.ProjectID != nil || v.State.Lanes != nil || !reflect.DeepEqual(v.State.Label, []string{"one", "two"}) {
					t.Fatalf("stored view=%+v", v)
				}
				query, err := r.WorkViewPreference(t.Context(), "first", id)
				if err != nil || query != string(org)+id {
					t.Fatalf("preference=%v error=%v", query, err)
				}
				if _, err := r.SavedWorkView(t.Context(), viewstore.Selection{UserID: "second"}, id); !errors.Is(err, corestore.ErrNotFound) {
					t.Fatalf("foreign owner read=%v", err)
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name  string
		state viewstore.State
	}{
		{"combined filter limit", viewstore.State{Label: slices.Repeat([]string{"label"}, 200), Assignee: slices.Repeat([]string{"member"}, 57)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := viewstore.Record{OrganizationID: "org_first", ID: "view_oversized", OwnerID: "first", Name: "oversized", Scope: "personal", ProjectIDs: []tracker.ProjectID{}, State: tc.state}
			v.State.View, v.State.Tab, v.State.Sort, v.State.CompletedWindow = "board", "active", "default", "24h"
			err := repository.WriteTenant(t.Context(), "org_first", corestore.Serving, func(w tenantstore.Writer) error {
				return w.PutSavedWorkView(t.Context(), selection, v, true)
			})
			if !errors.Is(err, viewstore.ErrInvalidView) {
				t.Fatalf("oversized filter error = %v", err)
			}
		})
	}
	rollback := errors.New("rollback view mutation")
	err := repository.WriteTenant(t.Context(), "org_first", corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.DeleteSavedWorkView(t.Context(), selection, "view_first"); err != nil {
			return err
		}
		if err := w.SetWorkViewPreference(t.Context(), "first", "view_first", nil); err != nil {
			return err
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatal(err)
	}
	if err := repository.ReadTenant(t.Context(), "org_first", corestore.Serving, func(r tenantstore.Reader) error {
		v, err := r.SavedWorkView(t.Context(), selection, "view_first")
		if err != nil || !v.Favorite || len(v.State.Label) != 2 {
			t.Fatalf("rollback lost view children: %+v error=%v", v, err)
		}
		q, err := r.WorkViewPreference(t.Context(), "first", "view_first")
		if err != nil || q != "org_firstview_first" {
			t.Fatalf("rollback lost preference: %v error=%v", q, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
