package tenantauth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/pgtransport"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type PlatformKeyRequest struct {
	Organization   tracker.OrganizationID
	KeyID          string
	CredentialHash string
	Managed        bool
	OAuthExchange  bool
}

type PlatformKey struct {
	Authority    apikey.KeyAuthority
	Hash         string
	OwnerSubject string
	OwnerEmail   string
	ExpiresAt    *time.Time
	Resource     string
	ProviderID   string
	Slug         string
	Endpoint     string
	Generation   int64
}

func ReadPlatformKey(ctx context.Context, reader pgtransport.Reader, input PlatformKeyRequest, now time.Time) (PlatformKey, error) {
	if input.Organization == "" || (input.KeyID == "") == (input.CredentialHash == "") || input.OAuthExchange && !strings.HasPrefix(input.KeyID, "oauth_grant_") {
		return PlatformKey{}, platformDenied("key_required", "A valid key is required")
	}
	selector, value := "id", input.KeyID
	if input.CredentialHash != "" {
		selector, value = "token_hash", input.CredentialHash
	}
	source := `SELECT id,token_hash,name,kind,owner_subject,owner_email,permission,access_context,service_organization_id,expires_at,revoked_at,'' AS resource FROM platform.access_keys WHERE ` + selector + `=$1 AND NOT $4`
	oauth := strings.HasPrefix(input.KeyID, "oauth_grant_")
	if oauth {
		source = `SELECT g.id,coalesce(g.access_hash,'') AS token_hash,c.name,'personal' AS kind,g.owner_subject,g.owner_email,g.permission,'selected' AS access_context,NULL::text AS service_organization_id,
 CASE WHEN $4 THEN NULL::timestamptz ELSE g.access_expires_at END AS expires_at,g.revoked_at,g.resource
 FROM platform.oauth_grants g JOIN platform.oauth_clients c ON c.id=g.client_id
 WHERE g.id=$1 AND g.organization_id=$2 AND ($4 OR (g.access_hash IS NOT NULL AND g.access_expires_at IS NOT NULL))
 AND (SELECT count(*) FROM (SELECT 1 FROM platform.oauth_grants WHERE id=$1 LIMIT 2) duplicate)=1`
	}
	var result PlatformKey
	var name, accessContext, policy string
	var serviceOrganization *tracker.OrganizationID
	var revoked *time.Time
	var selected, blocked, approved bool
	var selections int
	err := reader.QueryRow(ctx, `WITH k AS (`+source+`)
 SELECT k.id,k.token_hash,k.name,k.kind,k.owner_subject,k.owner_email,k.permission,k.access_context,k.service_organization_id,k.expires_at,k.revoked_at,k.resource,
 tenant.provider_id,tenant.slug,tenant.endpoint,tenant.generation,
 coalesce(o.project_access,'all'),coalesce(o.selected,false),coalesce(o.blocked,false),o.approved_at IS NOT NULL,coalesce(p.personal_keys,'allowed'),
 ARRAY(SELECT project_id FROM platform.access_key_projects WHERE organization_id=$2 AND key_id=k.id ORDER BY project_id LIMIT 201),
 ARRAY(SELECT project_id FROM platform.access_key_blocked_projects WHERE organization_id=$2 AND key_id=k.id ORDER BY project_id LIMIT 201),
 (SELECT count(*) FROM (SELECT 1 FROM platform.access_key_organizations WHERE key_id=k.id AND selected LIMIT 201) selections)
 FROM k JOIN platform.organizations tenant ON tenant.id=$2 AND tenant.state='ready' AND (NOT $3 OR tenant.managed)
 LEFT JOIN platform.access_key_organizations o ON o.organization_id=$2 AND o.key_id=k.id
 LEFT JOIN platform.organization_key_policy p ON p.organization_id=$2`,
		value, input.Organization, input.Managed, input.OAuthExchange).Scan(
		&result.Authority.ID, &result.Hash, &name, &result.Authority.Kind, &result.OwnerSubject, &result.OwnerEmail, &result.Authority.Permission, &accessContext, &serviceOrganization, &result.ExpiresAt, &revoked, &result.Resource,
		&result.ProviderID, &result.Slug, &result.Endpoint, &result.Generation,
		&result.Authority.Access, &selected, &blocked, &approved, &policy, &result.Authority.Projects, &result.Authority.BlockedProjects, &selections)
	if errors.Is(err, pgx.ErrNoRows) {
		return PlatformKey{}, platformDenied("key_organization_denied", "The organization is unavailable to this key")
	}
	if err != nil {
		return PlatformKey{}, unavailable(err)
	}
	if revoked != nil || result.ExpiresAt != nil && !result.ExpiresAt.After(now) {
		return PlatformKey{}, platformDenied("key_inactive", "The key is expired or revoked")
	}
	if !oauth && !(result.Authority.Kind == "personal" && accessContext == "global") && !selected || result.Authority.Kind == "service" && (serviceOrganization == nil || *serviceOrganization != input.Organization || selections != 1) {
		return PlatformKey{}, platformDenied("key_organization_denied", "The organization is outside this key's access context")
	}
	validContext := accessContext == "global" && result.Authority.Kind == "personal" || accessContext == "selected" && (oauth || selections > 0 && selections <= 200) || accessContext == "project" && selections == 1 && result.Authority.Access == "project"
	if !validContext || name == "" || len(name) > 128 || result.OwnerSubject == "" || result.Hash == "" && !input.OAuthExchange || !result.Authority.Valid() || len(result.Authority.BlockedProjects) > 0 && !(apikey.ProjectContext{Access: "selected", Projects: result.Authority.BlockedProjects}).Valid() || result.Authority.Kind == "personal" && serviceOrganization != nil {
		return PlatformKey{}, platformDenied("key_context_invalid", "The stored key context is invalid")
	}

	if blocked {
		return PlatformKey{}, platformDenied("organization_key_blocked", "An organization administrator blocked this key")
	}
	if result.Authority.Kind == "personal" {
		switch policy {
		case "allowed":
		case "approval":
			if !approved {
				return PlatformKey{}, platformDenied("organization_key_policy_denied", "This personal key requires organization approval")
			}
		default:
			return PlatformKey{}, platformDenied("organization_key_policy_denied", "This organization blocks personal keys")
		}
	}
	sum := sha256.Sum256([]byte("detent-entry-coordinator-credential-v1\x00" + result.Hash))
	result.Authority.CredentialBinding = hex.EncodeToString(sum[:])
	return result, nil
}

func platformDenied(code, message string) error {
	return errors.Join(ErrDenied, &apikey.Refusal{Code: code, Message: message})
}
