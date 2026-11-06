package platformstore

import (
	"context"
	"errors"
	"path/filepath"
	"strings"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const organizationColumns = `id,provider_id,slug,name,state,endpoint,generation,managed,creator_subject,creator_email,step,attempts,next_attempt_at,error_code,error_detail,checkout_price,created_at,updated_at`

type scanner interface{ Scan(...any) error }

func scanOrganization(row scanner) (Organization, error) {
	var o Organization
	err := row.Scan(&o.ID, &o.ProviderID, &o.Slug, &o.Name, &o.State, &o.Endpoint, &o.Generation, &o.Managed, &o.CreatorSubject, &o.CreatorEmail, &o.Step, &o.Attempts, &o.NextAttemptAt, &o.ErrorCode, &o.ErrorDetail, &o.CheckoutPrice, &o.CreatedAt, &o.UpdatedAt)
	return o, err
}

func validID(id string) bool {
	return id != "" && len(id) <= 128 && strings.IndexFunc(id, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' && r != '-'
	}) == -1
}

func (r *Repository) Register(ctx context.Context, o Organization) (bool, error) {
	if o.State == "" {
		o.State = "ready"
	}
	endpoint, ok := strings.CutPrefix(o.Endpoint, "unix:")
	if !strings.HasPrefix(string(o.ID), "org_") || len(o.ID) > 64 || !validID(string(o.ID)) || !validID(o.ProviderID) || strings.TrimSpace(o.Name) == "" || len(o.Name) > 120 || o.Generation < 1 || !ok || !filepath.IsAbs(endpoint) || filepath.Clean(endpoint) != endpoint || o.State != "ready" && o.State != "disabled" {
		return false, ErrInvalid
	}
	changed := false
	err := r.transaction(ctx, corestore.Serving, func(tx pgx.Tx) error {

		now := r.now()
		inserted, err := tx.Exec(ctx, `INSERT INTO platform.organizations(id,provider_id,name,state,endpoint,generation,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$7) ON CONFLICT DO NOTHING`, o.ID, o.ProviderID, o.Name, o.State, o.Endpoint, o.Generation, now)
		if err != nil {
			return err
		}
		count := inserted.RowsAffected()
		if err != nil {
			return err
		}
		if count == 0 {
			current, err := scanOrganization(tx.QueryRow(ctx, `SELECT `+organizationColumns+` FROM platform.organizations WHERE id=$1 FOR UPDATE`, o.ID))
			switch {
			case errors.Is(err, pgx.ErrNoRows):
				return ErrOrganizationConflict
			case err != nil:
				return err
			case current.ProviderID != o.ProviderID || o.Generation < current.Generation || current.Managed || current.State == "deleted":
				return ErrOrganizationConflict
			case current.Name == o.Name && current.State == o.State && current.Endpoint == o.Endpoint && current.Generation == o.Generation:
				return nil
			case current.Generation == o.Generation && current.Endpoint != o.Endpoint:
				return ErrOrganizationConflict
			}
			if _, err := tx.Exec(ctx, `UPDATE platform.organizations SET name=$1,state=$2,endpoint=$3,generation=$4,updated_at=$5 WHERE id=$6`, o.Name, o.State, o.Endpoint, o.Generation, now, o.ID); err != nil {
				return err
			}
		}

		_, err = tx.Exec(ctx, `INSERT INTO platform.organization_events(organization_id,event,generation,recorded_at) VALUES($1,$2,$3,$4)`, o.ID, "registered_"+o.State, o.Generation, now)
		changed = err == nil
		return err
	})
	if errors.Is(err, corestore.ErrUniqueViolation) {
		return false, ErrOrganizationConflict
	}
	return changed && err == nil, err
}

func (r *Repository) Organization(ctx context.Context, id tracker.OrganizationID) (Organization, error) {
	var o Organization
	err := r.read(ctx, corestore.Serving, func(tx pgquery.DBTX) error {
		var err error
		o, err = scanOrganization(tx.QueryRow(ctx, `SELECT `+organizationColumns+` FROM platform.organizations WHERE id=$1`, id))
		return err
	})
	return o, err
}

func (r *Repository) ByProvider(ctx context.Context, provider string) (Organization, error) {
	if !validID(provider) {
		return Organization{}, corestore.ErrNotFound
	}
	var o Organization
	err := r.read(ctx, corestore.Serving, func(tx pgquery.DBTX) error {
		var err error
		o, err = scanOrganization(tx.QueryRow(ctx, `SELECT `+organizationColumns+` FROM platform.organizations WHERE provider_id=$1 AND provider_id<>''`, provider))
		return err
	})
	return o, err
}

func (r *Repository) Organizations(ctx context.Context, after tracker.OrganizationID, limit int) ([]Organization, error) {
	if limit < 1 || limit > corestore.MaxPageSize {
		return nil, ErrInvalid
	}
	result := []Organization{}
	err := r.read(ctx, corestore.Serving, func(tx pgquery.DBTX) error {
		rows, err := tx.Query(ctx, `SELECT `+organizationColumns+` FROM platform.organizations WHERE id>$1 ORDER BY id LIMIT $2`, after, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			o, err := scanOrganization(rows)
			if err != nil {
				return err
			}
			result = append(result, o)
		}
		return rows.Err()
	})
	return result, err
}

func (r *Repository) Member(ctx context.Context, email string) (Member, error) {
	var m Member
	err := r.read(ctx, corestore.Serving, func(tx pgquery.DBTX) error {
		return tx.QueryRow(ctx, `SELECT email,role,added_by,added_at,updated_at FROM platform.platform_members WHERE email=$1`, strings.ToLower(strings.TrimSpace(email))).Scan(&m.Email, &m.Role, &m.AddedBy, &m.AddedAt, &m.UpdatedAt)
	})
	return m, err
}
