package platformstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/tenantauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func validKey(k Key) bool {
	if k.ID == "" || k.Hash == "" || k.OwnerSubject == "" || k.Name == "" || len(k.Name) > 128 || !apikey.ValidScope(apikey.Scope(k.Permission)) || len(k.Organizations) > 200 || k.Kind != "personal" && k.Kind != "service" {
		return false
	}
	seen := map[tracker.OrganizationID]bool{}
	for _, o := range k.Organizations {
		projects := make([]string, len(o.Projects))
		for i, p := range o.Projects {
			projects[i] = string(p)
		}
		if o.OrganizationID == "" || seen[o.OrganizationID] || !(apikey.ProjectContext{Access: o.ProjectAccess, Projects: projects}).Valid() {
			return false
		}
		seen[o.OrganizationID] = true
	}
	if k.Kind == "service" && (len(k.Organizations) != 1 || k.ServiceOrganizationID == nil || *k.ServiceOrganizationID != k.Organizations[0].OrganizationID) {
		return false
	}
	if k.Kind == "personal" && k.ServiceOrganizationID != nil {
		return false
	}
	switch k.AccessContext {
	case "global":
		return k.Kind == "personal"
	case "selected":
		return len(k.Organizations) > 0
	case "project":
		return len(k.Organizations) == 1 && k.Organizations[0].ProjectAccess == "project"
	default:
		return false
	}
}

func (r *Repository) CreateKey(ctx context.Context, k Key) error {
	if !validKey(k) {
		return ErrInvalid
	}
	return r.transaction(ctx, corestore.Serving, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO platform.access_keys(id,token_hash,name,kind,owner_subject,owner_email,permission,access_context,service_organization_id,created_at,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, k.ID, k.Hash, k.Name, k.Kind, k.OwnerSubject, k.OwnerEmail, k.Permission, k.AccessContext, k.ServiceOrganizationID, r.now(), k.ExpiresAt)
		if err != nil {
			return err
		}
		for _, o := range k.Organizations {
			if _, err := tx.Exec(ctx, `INSERT INTO platform.access_key_organizations(organization_id,key_id,project_access,selected) VALUES($1,$2,$3,true)`, o.OrganizationID, k.ID, o.ProjectAccess); err != nil {
				return err
			}
			for _, project := range o.Projects {
				if _, err := tx.Exec(ctx, `INSERT INTO platform.access_key_projects(organization_id,key_id,project_id) VALUES($1,$2,$3)`, o.OrganizationID, k.ID, project); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func (r *Repository) KeyAuthority(ctx context.Context, hash string, organization tracker.OrganizationID, project tracker.ProjectID) (apikey.KeyAuthority, error) {
	var authority apikey.KeyAuthority
	err := r.read(ctx, corestore.Serving, func(tx pgquery.DBTX) error {
		var k Key
		var blocked, approved bool
		var policy, access string
		err := tx.QueryRow(ctx, `SELECT k.id,k.kind,k.permission,k.access_context,coalesce(o.project_access,'all'),coalesce(o.blocked,false),o.approved_at IS NOT NULL,coalesce(p.personal_keys,'allowed')
 FROM platform.access_keys k
 JOIN platform.organizations tenant ON tenant.id=$2 AND tenant.state='ready'
 LEFT JOIN platform.access_key_organizations o ON o.key_id=k.id AND o.organization_id=$2
 LEFT JOIN platform.organization_key_policy p ON p.organization_id=$2
 WHERE k.token_hash=$1 AND k.revoked_at IS NULL AND (k.expires_at IS NULL OR k.expires_at>$3)
 AND ((k.kind='personal' AND k.access_context='global') OR o.selected)
 AND (k.kind='personal' OR k.service_organization_id=$2)`, hash, organization, r.now()).Scan(&k.ID, &k.Kind, &k.Permission, &k.AccessContext, &access, &blocked, &approved, &policy)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNoSession
		}
		if err != nil {
			return err
		}
		if blocked || k.Kind == "personal" && (policy == "blocked" || policy == "approval" && !approved) {
			return ErrNoSession
		}
		sum := sha256.Sum256([]byte("detent-entry-coordinator-credential-v1\x00" + hash))
		authority = apikey.KeyAuthority{ID: k.ID, CredentialBinding: hex.EncodeToString(sum[:]), Kind: k.Kind, Permission: apikey.Scope(k.Permission), ProjectContext: apikey.ProjectContext{Access: access}}
		rows, err := tx.Query(ctx, `SELECT project_id,false AS blocked FROM platform.access_key_projects WHERE organization_id=$1 AND key_id=$2 UNION ALL SELECT project_id,true AS blocked FROM platform.access_key_blocked_projects WHERE organization_id=$1 AND key_id=$2 ORDER BY project_id LIMIT 401`, organization, k.ID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			var blocked bool
			if err := rows.Scan(&id, &blocked); err != nil {
				return err
			}
			if blocked {
				authority.BlockedProjects = append(authority.BlockedProjects, id)
			} else if access != "all" {
				authority.Projects = append(authority.Projects, id)
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if !authority.Allows(string(project)) {
			return ErrNoSession
		}
		return nil
	})
	if err != nil {
		return apikey.KeyAuthority{}, err
	}
	return authority, nil
}

func (r *Repository) SetPersonalKeyPolicy(ctx context.Context, organization tracker.OrganizationID, policy string) error {
	if organization == "" || policy != "allowed" && policy != "approval" && policy != "blocked" {
		return ErrInvalid
	}
	return r.transaction(ctx, corestore.Serving, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO platform.organization_key_policy(organization_id,personal_keys) VALUES($1,$2) ON CONFLICT(organization_id) DO UPDATE SET personal_keys=excluded.personal_keys`, organization, policy)
		return err
	})
}

func (r *Repository) ApproveKey(ctx context.Context, organization tracker.OrganizationID, id string) error {
	return r.transaction(ctx, corestore.Serving, func(tx pgx.Tx) error {
		var kind, access string
		err := tx.QueryRow(ctx, `SELECT kind,access_context FROM platform.access_keys WHERE id=$1 AND revoked_at IS NULL FOR UPDATE`, id).Scan(&kind, &access)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNoSession
		}
		if err != nil {
			return err
		}
		if kind != "personal" {
			return ErrInvalid
		}
		if access == "global" {
			_, err = tx.Exec(ctx, `INSERT INTO platform.access_key_organizations(organization_id,key_id,approved_at) VALUES($1,$2,$3) ON CONFLICT(organization_id,key_id) DO UPDATE SET approved_at=excluded.approved_at`, organization, id, r.now())
			return err
		}
		result, err := tx.Exec(ctx, `UPDATE platform.access_key_organizations SET approved_at=$1 WHERE organization_id=$2 AND key_id=$3`, r.now(), organization, id)
		if err != nil {
			return err
		}
		n := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return ErrNoSession
		}
		return nil
	})
}

func (r *Repository) BlockKey(ctx context.Context, organization tracker.OrganizationID, id string, project tracker.ProjectID) error {
	if organization == "" || id == "" || strings.TrimSpace(string(project)) != string(project) {
		return ErrInvalid
	}
	return r.transaction(ctx, corestore.Serving, func(tx pgx.Tx) error {
		var access string
		err := tx.QueryRow(ctx, `SELECT access_context FROM platform.access_keys WHERE id=$1 FOR UPDATE`, id).Scan(&access)
		if err != nil {
			return err
		}
		if access == "global" {
			if _, err := tx.Exec(ctx, `INSERT INTO platform.access_key_organizations(organization_id,key_id) VALUES($1,$2) ON CONFLICT DO NOTHING`, organization, id); err != nil {
				return err
			}
		}
		if project == "" {
			_, err = tx.Exec(ctx, `UPDATE platform.access_key_organizations SET blocked=true WHERE organization_id=$1 AND key_id=$2`, organization, id)
		} else {
			_, err = tx.Exec(ctx, `INSERT INTO platform.access_key_blocked_projects(organization_id,key_id,project_id) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, organization, id, project)
		}
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO platform.access_key_notifications(owner_subject,key_id,organization_id,project_id,created_at) SELECT k.owner_subject,k.id,$1,$2,$3 FROM platform.access_keys k JOIN platform.access_key_organizations o ON o.key_id=k.id AND o.organization_id=$1 WHERE k.id=$4`, organization, project, r.now(), id)
		return err
	})
}

func (r *Repository) RevokeKey(ctx context.Context, owner, id string) error {
	return r.transaction(ctx, corestore.Serving, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE platform.access_keys SET revoked_at=$1 WHERE owner_subject=$2 AND id=$3 AND revoked_at IS NULL`, r.now(), owner, id)
		return err
	})
}

func (r *Repository) Key(ctx context.Context, hash string) (Key, error) {
	var k Key
	err := r.read(ctx, corestore.Serving, func(reader pgquery.DBTX) error {
		rows, err := reader.Query(ctx, `SELECT k.id,k.token_hash,k.name,k.kind,k.owner_subject,k.owner_email,k.permission,k.access_context,k.service_organization_id,k.created_at,k.expires_at,k.revoked_at,
 o.organization_id,o.project_access,
 ARRAY(SELECT project_id FROM platform.access_key_projects p WHERE p.organization_id=o.organization_id AND p.key_id=k.id ORDER BY project_id LIMIT 201)
 FROM platform.access_keys k
 LEFT JOIN LATERAL (SELECT organization_id,project_access FROM platform.access_key_organizations WHERE key_id=k.id AND selected ORDER BY organization_id LIMIT 201) o ON true
 WHERE k.token_hash=$1 ORDER BY o.organization_id`, hash)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var organization, access *string
			var projects []tracker.ProjectID
			if err := rows.Scan(&k.ID, &k.Hash, &k.Name, &k.Kind, &k.OwnerSubject, &k.OwnerEmail, &k.Permission, &k.AccessContext, &k.ServiceOrganizationID, &k.CreatedAt, &k.ExpiresAt, &k.RevokedAt, &organization, &access, &projects); err != nil {
				return err
			}
			if organization != nil && access != nil {
				k.Organizations = append(k.Organizations, KeyOrganization{OrganizationID: tracker.OrganizationID(*organization), ProjectAccess: *access, Projects: projects})
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if k.ID == "" {
			return pgx.ErrNoRows
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, corestore.ErrNotFound) {
			return Key{}, err
		}
		return Key{}, fmt.Errorf("read platform key: %w", errors.Join(tenantauth.ErrUnavailable, err))
	}
	if !validKey(k) {
		return Key{}, errors.Join(tenantauth.ErrDenied, &apikey.Refusal{Code: "key_context_invalid", Message: "The stored key context is invalid"})
	}
	return k, nil
}
