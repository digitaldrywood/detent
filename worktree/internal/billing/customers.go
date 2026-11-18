package billing

import (
	"context"
	"errors"
	"net/http"
	"net/url"
)

var ErrCustomerConflict = errors.New("more than one Stripe customer names this organization; repair the binding before billing")

type CustomerRequest struct {
	AccountID      string
	OrganizationID string
	IdempotencyKey string
}

type CustomerProvider interface {
	EnsureCustomer(context.Context, CustomerRequest) (string, error)
	CustomerOrganization(context.Context, string) (string, error)
	ChargeCustomer(context.Context, string) (string, error)
}

type stripeCustomer struct {
	ID       string            `json:"id"`
	Livemode *bool             `json:"livemode"`
	Deleted  bool              `json:"deleted"`
	Metadata map[string]string `json:"metadata"`
}

func (s *stripeProvider) customerValid(customer stripeCustomer, organization string) bool {
	return validID(customer.ID, "cus_") && s.mode(customer.Livemode) && !customer.Deleted && customer.Metadata["detent_organization_id"] == organization
}

func (s *stripeProvider) customerCreatedBy(customer stripeCustomer, request CustomerRequest) bool {
	return s.customerValid(customer, request.OrganizationID) && customer.Metadata["detent_creation_key"] == request.IdempotencyKey
}

func (s *stripeProvider) EnsureCustomer(ctx context.Context, request CustomerRequest) (string, error) {
	if !validID(request.AccountID, "acct_") || !validID(request.OrganizationID, "org_") || request.IdempotencyKey == "" || len(request.IdempotencyKey) > 255 {
		return "", errors.New("stripe customer request is invalid")
	}
	var account struct{ ID string }
	if err := s.request(ctx, http.MethodGet, "account", "", nil, &account); err != nil {
		return "", err
	}
	if account.ID != request.AccountID {
		return "", errors.New("stripe account does not match the configured billing account")
	}
	var found struct {
		Data []stripeCustomer `json:"data"`
	}
	query := url.Values{"query": {"metadata['detent_organization_id']:'" + request.OrganizationID + "'"}, "limit": {"2"}}
	if err := s.request(ctx, http.MethodGet, "customers/search", "", query, &found); err != nil {
		return "", err
	}
	switch len(found.Data) {
	case 0:
	case 1:
		if !s.customerCreatedBy(found.Data[0], request) {
			return "", ErrCustomerConflict
		}
		return found.Data[0].ID, nil
	default:
		return "", ErrCustomerConflict
	}
	var created stripeCustomer
	form := url.Values{"metadata[detent_organization_id]": {request.OrganizationID}, "metadata[detent_creation_key]": {request.IdempotencyKey}, "description": {"Detent organization " + request.OrganizationID}}
	if err := s.request(ctx, http.MethodPost, "customers", request.IdempotencyKey, form, &created); err != nil {
		return "", err
	}
	if !s.customerCreatedBy(created, request) {
		return "", errors.New("stripe returned a customer for another organization or mode")
	}
	return created.ID, nil
}

func (s *stripeProvider) CustomerOrganization(ctx context.Context, id string) (string, error) {
	if !validID(id, "cus_") {
		return "", errors.New("stripe customer ID is invalid")
	}
	var customer stripeCustomer
	if err := s.request(ctx, http.MethodGet, "customers/"+id, "", nil, &customer); err != nil {
		return "", err
	}
	organization := customer.Metadata["detent_organization_id"]
	if !validID(organization, "org_") || !s.customerValid(customer, organization) {
		return "", errors.New("stripe customer is not a Detent organization customer in this mode")
	}
	return organization, nil
}

func (s *stripeProvider) ChargeCustomer(ctx context.Context, id string) (string, error) {
	if !validID(id, "ch_") {
		return "", errors.New("stripe charge ID is invalid")
	}
	var charge struct {
		ID       string `json:"id"`
		Customer string `json:"customer"`
		Livemode *bool  `json:"livemode"`
	}
	if err := s.request(ctx, http.MethodGet, "charges/"+id, "", nil, &charge); err != nil {
		return "", err
	}
	if charge.ID != id || !s.mode(charge.Livemode) || !validID(charge.Customer, "cus_") {
		return "", errors.New("stripe charge has no customer in this mode")
	}
	return charge.Customer, nil
}
