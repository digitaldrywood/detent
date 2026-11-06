package hubserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func conversationReadAudience(ctx context.Context, query nativeQueryer, organization tracker.OrganizationID, credential apiCredential) (string, error) {
	if credential.Hosted == nil {
		raw := sha256.Sum256([]byte(string(organization) + ":" + credential.ID + ":" + string(credential.Scope)))
		return hex.EncodeToString(raw[:]), nil
	}
	var role string
	if err := query.QueryRowContext(ctx, "SELECT role FROM hosted_members WHERE user_id=? AND active=1", credential.Hosted.Subject).Scan(&role); err != nil {
		return "", err
	}
	role = lesserHostedRole(credential.HostedRole, role)
	type grant struct {
		Project       string
		Write, Runner bool
	}
	rows, err := query.QueryContext(ctx, "SELECT project_id,can_write,manage_runner FROM hosted_project_grants WHERE organization_id=? AND user_id=? ORDER BY project_id", organization, credential.Hosted.Subject)
	if err != nil {
		return "", err
	}
	defer func() { _ = rows.Close() }()
	grants := []grant{}
	for rows.Next() {
		var g grant
		if err := rows.Scan(&g.Project, &g.Write, &g.Runner); err != nil {
			return "", err
		}
		grants = append(grants, g)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	raw, err := json.Marshal(struct {
		Organization tracker.OrganizationID
		Role         string
		Scope        apiScope
		KeyScope     string
		SupportActor string
		Grants       []grant
	}{organization, role, credential.Scope, string(credential.HostedKeyScope), credential.Hosted.SupportActor, grants})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}

func (t *coordinatorToolset) restrictReadAudience(ctx context.Context, record conversationRecord, credential apiCredential) error {
	var audience string
	err := t.coordinator.service.server.config.TenantRepository.ReadTenant(ctx, record.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		audience, err = conversationReadAudienceRecord(ctx, r, record.OrganizationID, credential)
		return err
	})
	if err != nil {
		return operatortool.ErrAccessDenied
	}
	return t.coordinator.write(ctx, record.ID, func(ctx context.Context, w tenantstore.Writer, fresh *conversationRecord, now time.Time) error {
		if fresh.ReadAudience != "" && fresh.ReadAudience != audience {
			return fmt.Errorf("equal screen access is required: %w", operatortool.ErrAccessDenied)
		}
		if fresh.ReadAudience == audience {
			return nil
		}
		fresh.ReadAudience = audience
		return t.coordinator.saveConversation(ctx, w, fresh, now)
	}, func(ctx context.Context, p *conversationPublicationPlan) error {
		if p.record.ReadAudience == audience {
			return nil
		}
		p.record.ReadAudience = audience
		return p.saveConversation(ctx)
	})
}

func conversationReadAudienceRecord(ctx context.Context, reader tenantstore.Reader, organization tracker.OrganizationID, credential apiCredential) (string, error) {
	if credential.Hosted == nil {
		raw := sha256.Sum256([]byte(string(organization) + ":" + credential.ID + ":" + string(credential.Scope)))
		return hex.EncodeToString(raw[:]), nil
	}
	member, err := reader.AccessMember(ctx, credential.Hosted.Subject)
	if err != nil {
		return "", err
	}
	if !member.Active {
		return "", operatortool.ErrAccessDenied
	}
	type grant struct {
		Project       string
		Write, Runner bool
	}
	grants := []grant{}
	var after tracker.ProjectID
	for {
		rows, err := reader.AccessProjectGrants(ctx, member.UserID, after, 100)
		if err != nil {
			return "", err
		}
		for _, row := range rows {
			grants = append(grants, grant{string(row.ProjectID), row.CanWrite, row.ManageRunner})
		}
		if len(rows) < 100 {
			break
		}
		after = rows[len(rows)-1].ProjectID
	}
	raw, err := json.Marshal(struct {
		Organization tracker.OrganizationID
		Role         string
		Scope        apiScope
		KeyScope     string
		SupportActor string
		Grants       []grant
	}{organization, lesserHostedRole(credential.HostedRole, member.Role), credential.Scope, string(credential.HostedKeyScope), credential.Hosted.SupportActor, grants})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}
