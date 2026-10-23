package hubserver

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

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
	audience, err := conversationReadAudience(ctx, t.coordinator.service.store.reader, record.OrganizationID, credential)
	if err != nil {
		return operatortool.ErrAccessDenied
	}
	return t.coordinator.write(ctx, record.ID, func(ctx context.Context, tx *sql.Tx, fresh *conversationRecord, _ time.Time) error {
		if fresh.ReadAudience != "" && fresh.ReadAudience != audience {
			return fmt.Errorf("you don't have access to this conversation's stored answers: equal screen access is required: %w", operatortool.ErrAccessDenied)
		}
		if fresh.ReadAudience == audience {
			return nil
		}
		_, err := tx.ExecContext(ctx, "UPDATE conversations SET read_audience=? WHERE id=?", audience, record.ID)
		return err
	})
}
