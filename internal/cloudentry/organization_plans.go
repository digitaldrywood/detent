package cloudentry

import (
	"context"
	"database/sql"
	"net/url"

	"github.com/digitaldrywood/detent/internal/web/templates"
)

type organizationQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func (s *Service) freeSlotUsed(ctx context.Context, q organizationQuerier, session accountSession) (bool, error) {
	memberships, err := s.config.Provider.Memberships(ctx, session.Subject, "")
	if err != nil {
		return false, err
	}
	owners := make(map[string]bool)
	for _, membership := range memberships {
		if membership.UserID == session.Subject && membership.Status == "active" && membership.Role.Slug == "owner" {
			owners[membership.OrganizationID] = true
		}
	}
	rows, err := q.QueryContext(ctx, organizationSelect+" WHERE state != 'deleted'")
	if err != nil {
		return false, err
	}
	defer rows.Close()
	var organizations []Organization
	for rows.Next() {
		organization, err := scanOrganization(rows)
		if err != nil {
			return false, err
		}
		organizations = append(organizations, organization)
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	for _, organization := range organizations {
		if organization.State == "requested" || organization.State == "allocating" || organization.State == "failed" {
			if organization.CreatorSubject == session.Subject && organization.CheckoutPrice == "" {
				return true, nil
			}
			continue
		}
		if !owners[organization.ProviderID] {
			continue
		}
		state, err := s.tenantBillingState(ctx, organization, false)
		if err != nil {
			return false, err
		}
		if state.FreeOrganization {
			return true, nil
		}
	}
	return false, nil
}

func (s *Service) creationPriceAllowed(id string) bool {
	if s.config.Allocation == nil || s.config.Billing == nil {
		return false
	}
	for _, price := range s.config.Allocation.Prices {
		if price.PriceID == id && price.Plan.ID != "free" && price.Plan.ID != "pilot_free" && price.Plan.ID != "comp_team" {
			return true
		}
	}
	return false
}

func (s *Service) creationPlans(ctx context.Context, session accountSession) (bool, []templates.HostedBillingPrice, error) {
	if s.config.Allocation == nil || s.entitlementAdministrator(ctx, session) {
		return false, nil, nil
	}
	used, err := s.freeSlotUsed(ctx, s.registry.store.db, session)
	if err != nil {
		return false, nil, err
	}
	prices := []templates.HostedBillingPrice{}
	for _, price := range s.config.Allocation.Prices {
		if s.creationPriceAllowed(price.PriceID) {
			prices = append(prices, templates.HostedBillingPrice{ID: price.PriceID, Label: price.Label})
		}
	}
	return used, prices, nil
}

func (s *Service) creationDestination(organization Organization) string {
	if organization.CheckoutPrice != "" {
		return "/organizations/" + organization.ID + "/organization/billing?checkout_price=" + url.QueryEscape(organization.CheckoutPrice)
	}
	return s.organizationHome(organization.ID)
}
