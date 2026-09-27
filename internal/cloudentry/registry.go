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
	Name           string    `json:"name"`
	State          string    `json:"state"`
	Endpoint       string    `json:"endpoint"`
	Generation     int64     `json:"generation"`
	Managed        bool      `json:"managed"`
	CreatorSubject string    `json:"-"`
	CreatorEmail   string    `json:"-"`
	Step           string    `json:"step,omitempty"`
	Attempts       int       `json:"attempts,omitempty"`
	NextAttemptAt  string    `json:"-"`
	ErrorCode      string    `json:"error_code,omitempty"`
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

func OpenRegistry(ctx context.Context, path string) (*Registry, error) {
	s, err := openStore(ctx, path, registryApplicationID, "registry")
	if err != nil {
		return nil, err
	}
	return &Registry{store: s, now: time.Now}, nil
}

func (r *Registry) Close() error {
	return r.store.Close()
}

func (r *Registry) Register(ctx context.Context, organization Organization) (bool, error) {
	if organization.State == "" {
		organization.State = "ready"
	}
	if err := organization.validate(); err != nil {
		return false, err
	}
	tx, err := r.store.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	existing, err := scanOrganization(tx.QueryRowContext(ctx, organizationSelect+" WHERE id = ? OR provider_id = ?", organization.ID, organization.ProviderID))
	now := formatTime(r.now())
	switch {
	case errors.Is(err, ErrOrganizationNotFound):
		if _, err := tx.ExecContext(ctx, "INSERT INTO organizations(id,provider_id,name,state,endpoint,generation,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?)",
			organization.ID, organization.ProviderID, organization.Name, organization.State, organization.Endpoint, organization.Generation, now, now); err != nil {
			return false, err
		}
	case err != nil:
		return false, err
	case existing.ID != organization.ID || existing.ProviderID != organization.ProviderID || organization.Generation < existing.Generation || existing.Managed || existing.State == "deleted":
		return false, ErrOrganizationConflict
	case existing.Name == organization.Name && existing.State == organization.State && existing.Endpoint == organization.Endpoint && existing.Generation == organization.Generation:
		return false, nil
	case existing.Generation == organization.Generation && existing.Endpoint != organization.Endpoint:
		return false, ErrOrganizationConflict
	default:
		if _, err := tx.ExecContext(ctx, "UPDATE organizations SET name = ?, state = ?, endpoint = ?, generation = ?, updated_at = ? WHERE id = ?",
			organization.Name, organization.State, organization.Endpoint, organization.Generation, now, organization.ID); err != nil {
			return false, err
		}
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO organization_events(organization_id,event,generation,recorded_at) VALUES (?,?,?,?)", organization.ID, "registered_"+organization.State, organization.Generation, now); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

const organizationSelect = "SELECT id,provider_id,name,state,endpoint,generation,managed,creator_subject,creator_email,step,attempts,next_attempt_at,error_code,updated_at FROM organizations"

type rowScanner interface {
	Scan(...any) error
}

func scanOrganization(row rowScanner) (Organization, error) {
	var organization Organization
	var updated string
	if err := row.Scan(&organization.ID, &organization.ProviderID, &organization.Name, &organization.State, &organization.Endpoint, &organization.Generation, &organization.Managed,
		&organization.CreatorSubject, &organization.CreatorEmail, &organization.Step, &organization.Attempts, &organization.NextAttemptAt, &organization.ErrorCode, &updated); err != nil {
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

func (r *Registry) List(ctx context.Context) ([]Organization, error) {
	rows, err := r.store.db.QueryContext(ctx, organizationSelect+" ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
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
