package cloudentry

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"time"
)

var (
	ErrOrganizationConflict = errors.New("organization conflicts with an existing registry record")
	ErrOrganizationNotFound = errors.New("organization is not registered")
)

type Organization struct {
	ID             string    `json:"id"`
	ProviderID     string    `json:"provider_organization_id"`
	Slug           string    `json:"slug"`
	Name           string    `json:"name"`
	State          string    `json:"state"`
	Endpoint       string    `json:"endpoint"`
	Generation     int64     `json:"generation"`
	Managed        bool      `json:"managed"`
	CheckoutPrice  string    `json:"-"`
	CreatorSubject string    `json:"-"`
	CreatorEmail   string    `json:"-"`
	Step           string    `json:"step,omitempty"`
	Attempts       int       `json:"attempts,omitempty"`
	NextAttemptAt  string    `json:"-"`
	ErrorCode      string    `json:"error_code,omitempty"`
	ErrorDetail    string    `json:"error_detail,omitempty"`
	UpdatedAt      time.Time `json:"updated_at"`
}

func ValidOrganizationID(id string) bool {
	if !strings.HasPrefix(id, "org_") || len(id) > 64 {
		return false
	}
	return safeID(id)
}

func safeID(value string) bool {
	return value != "" && len(value) <= 128 && strings.IndexFunc(value, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' && r != '-'
	}) == -1
}

func ValidEndpoint(endpoint string) bool {
	path, ok := strings.CutPrefix(endpoint, "unix:")
	return ok && filepath.IsAbs(path) && filepath.Clean(path) == path
}

func (o Organization) validate() error {
	if !ValidOrganizationID(o.ID) || !safeID(o.ProviderID) || strings.TrimSpace(o.Name) == "" || len(o.Name) > 120 || o.Generation < 1 || !ValidEndpoint(o.Endpoint) {
		return errors.New("organization ID, provider organization, name, endpoint and generation are required and must be valid")
	}
	if o.State != "ready" && o.State != "disabled" {
		return errors.New("organization state must be ready or disabled")
	}
	return nil
}

type Registry struct {
	store *store
	now   func() time.Time
}

func (r *Registry) Close() error {
	return r.store.Close()
}

func (r *Registry) Register(ctx context.Context, organization Organization) (bool, error) {
	return r.postgresRegister(ctx, organization)
}

const organizationSelect = "SELECT slug,id,provider_id,name,state,endpoint,generation,managed,creator_subject,creator_email,step,attempts,next_attempt_at,error_code,error_detail,checkout_price,updated_at FROM organizations"

type rowScanner interface {
	Scan(...any) error
}

func scanOrganization(row rowScanner) (Organization, error) {
	var organization Organization
	var updated string
	if err := row.Scan(&organization.Slug, &organization.ID, &organization.ProviderID, &organization.Name, &organization.State, &organization.Endpoint, &organization.Generation, &organization.Managed,
		&organization.CreatorSubject, &organization.CreatorEmail, &organization.Step, &organization.Attempts, entryOptionalTime{&organization.NextAttemptAt}, &organization.ErrorCode, &organization.ErrorDetail, &organization.CheckoutPrice, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Organization{}, ErrOrganizationNotFound
		}
		return Organization{}, err
	}
	var err error
	organization.UpdatedAt, err = parseTime(updated)
	return organization, err
}

func (r *Registry) Organization(ctx context.Context, id string) (Organization, error) {
	if !ValidOrganizationID(id) {
		return Organization{}, ErrOrganizationNotFound
	}
	return scanOrganization(r.store.db.QueryRowContext(ctx, organizationSelect+" WHERE id = ?", id))
}

func (r *Registry) ByProvider(ctx context.Context, providerID string) (Organization, error) {
	if !safeID(providerID) {
		return Organization{}, ErrOrganizationNotFound
	}
	return scanOrganization(r.store.db.QueryRowContext(ctx, organizationSelect+" WHERE provider_id = ?", providerID))
}

func (r *Registry) List(ctx context.Context) (_ []Organization, resultErr error) {
	rows, err := r.store.db.QueryContext(ctx, organizationSelect+" ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer closeEntryRows(rows, &resultErr)
	var result []Organization
	for rows.Next() {
		organization, err := scanOrganization(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, organization)
	}
	return result, rows.Err()
}
