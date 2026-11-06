package platformstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/lib/pq"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type TenantKeySnapshot struct {
	Organization Organization
	Key          Key
	Authority    apikey.KeyAuthority
	Resource     string
}

func (r *Repository) TenantKey(ctx context.Context, id string, organization tracker.OrganizationID) (TenantKeySnapshot, error) {
	select {
	case r.serving <- struct{}{}:
	case <-ctx.Done():
		return TenantKeySnapshot{}, ctx.Err()
	}
	defer func() { <-r.serving }()
	var result TenantKeySnapshot
	source := `SELECT id,token_hash,kind,owner_subject,owner_email,permission,access_context,service_organization_id,expires_at,'' AS resource FROM platform.access_keys WHERE id=$1 AND revoked_at IS NULL`
	oauth := strings.HasPrefix(id, "oauth_grant_")
	if oauth {
		source = `SELECT id,access_hash AS token_hash,'personal' AS kind,owner_subject,owner_email,permission,'selected' AS access_context,NULL::text AS service_organization_id,access_expires_at AS expires_at,resource FROM platform.oauth_grants WHERE id=$1 AND organization_id=$2 AND revoked_at IS NULL AND access_hash IS NOT NULL AND access_expires_at IS NOT NULL AND (SELECT count(*) FROM (SELECT 1 FROM platform.oauth_grants WHERE id=$1 LIMIT 2) duplicate)=1`
	}
	var projects, blocked pq.StringArray
	var access string
	err := r.pool.QueryRowContext(ctx, `WITH k AS (`+source+`)
 SELECT k.token_hash,k.kind,k.owner_subject,k.owner_email,k.permission,k.expires_at,k.resource,
 tenant.provider_id,tenant.slug,tenant.endpoint,tenant.generation,
 coalesce(o.project_access,'all'),
 ARRAY(SELECT project_id FROM platform.access_key_projects WHERE organization_id=$2 AND key_id=$1 ORDER BY project_id LIMIT 201),
 ARRAY(SELECT project_id FROM platform.access_key_blocked_projects WHERE organization_id=$2 AND key_id=$1 ORDER BY project_id LIMIT 201)
 FROM k JOIN platform.organizations tenant ON tenant.id=$2 AND tenant.managed AND tenant.state='ready'
 LEFT JOIN platform.access_key_organizations o ON o.organization_id=$2 AND o.key_id=$1
 LEFT JOIN platform.organization_key_policy p ON p.organization_id=$2
 WHERE (k.expires_at IS NULL OR k.expires_at>$3)
 AND ($4 OR (k.kind='personal' AND k.access_context='global') OR o.selected)
 AND (k.kind='personal' OR k.service_organization_id=$2)
 AND NOT coalesce(o.blocked,false)
 AND (k.kind<>'personal' OR (coalesce(p.personal_keys,'allowed')<>'blocked' AND (coalesce(p.personal_keys,'allowed')<>'approval' OR o.approved_at IS NOT NULL)))`,
		id, organization, r.now(), oauth).Scan(&result.Key.Hash, &result.Key.Kind, &result.Key.OwnerSubject, &result.Key.OwnerEmail, &result.Key.Permission, &result.Key.ExpiresAt, &result.Resource,
		&result.Organization.ProviderID, &result.Organization.Slug, &result.Organization.Endpoint, &result.Organization.Generation, &access, &projects, &blocked)
	if errors.Is(err, sql.ErrNoRows) {
		return TenantKeySnapshot{}, ErrNoSession
	}
	if err != nil {
		return TenantKeySnapshot{}, mapError(ctx, err)
	}
	if len(projects) > 200 || len(blocked) > 200 {
		return TenantKeySnapshot{}, ErrInvalid
	}
	result.Key.ID = id
	result.Organization.ID, result.Organization.Managed, result.Organization.State = organization, true, "ready"
	sum := sha256.Sum256([]byte("detent-entry-coordinator-credential-v1\x00" + result.Key.Hash))
	result.Authority = apikey.KeyAuthority{ID: id, CredentialBinding: hex.EncodeToString(sum[:]), Kind: result.Key.Kind, Permission: apikey.Scope(result.Key.Permission), ProjectContext: apikey.ProjectContext{Access: access}, BlockedProjects: blocked}
	if access != "all" {
		result.Authority.Projects = projects
	}
	if !apikey.ValidScope(result.Authority.Permission) || !result.Authority.ProjectContext.Valid() {
		return TenantKeySnapshot{}, ErrInvalid
	}
	return result, nil
}
