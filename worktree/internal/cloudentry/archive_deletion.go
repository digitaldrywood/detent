package cloudentry

import (
	"context"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
)

func (s *Service) recordArchiveDeletion(ctx context.Context, organization, binding string, receipt attachment.DeletionReceipt, started bool) error {
	tx, err := s.registry.store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO archive_deletion_receipts(organization_id,binding_id,complete,truncated,attempts,updated_at) VALUES(?,?,?,?,1,?)
ON CONFLICT(organization_id,binding_id) DO UPDATE SET complete=excluded.complete,truncated=excluded.truncated,attempts=attempts+?,updated_at=excluded.updated_at`, organization, binding, receipt.Complete, receipt.Truncated, formatTime(s.config.now()), started); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM archive_deletion_failures WHERE organization_id=? AND binding_id=?`, organization, binding); err != nil {
		return err
	}
	for index, failure := range receipt.Remaining {
		if index >= attachment.DeletionReceiptLimit {
			break
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO archive_deletion_failures(organization_id,binding_id,ordinal,object_key,version_id,error_code) VALUES(?,?,?,?,?,?)`, organization, binding, index, failure.Key, failure.VersionID, failure.Code); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Service) eraseBinding(ctx context.Context, organization string, b attachment.StorageBinding) error {
	prefix, err := attachment.OrganizationPrefix(organization)
	if err != nil {
		return err
	}
	receipt := attachment.DeletionReceipt{Remaining: []attachment.DeletionFailure{{Key: b.Prefix + prefix, Code: "deletion_pending"}}}
	if err := s.recordArchiveDeletion(ctx, organization, b.ID, receipt, true); err != nil {
		return err
	}
	if err := s.auth.audit(ctx, "entry", organization, "attachment_deleted"); err != nil {
		return err
	}
	storage, err := s.bindingStorage(ctx, b)
	if err != nil {
		receipt.Remaining[0].Code = "credentials_or_endpoint_unavailable"
	} else if eraser, ok := storage.(attachment.OrganizationEraser); ok {
		receipt, err = eraser.EraseOrganization(ctx, organization)
	} else {
		err = artifact.ErrUnsupported
		receipt.Remaining[0].Code = "deletion_unsupported"
	}
	if recordErr := s.recordArchiveDeletion(ctx, organization, b.ID, receipt, false); recordErr != nil {
		return recordErr
	}
	return err
}

func (s *Service) eraseOrganizationStorage(ctx context.Context, organization string) error {
	if err := s.ensureLegacyArchiveBinding(ctx, organization); err != nil {
		return err
	}
	rows, err := s.registry.store.reader.QueryContext(ctx, archiveBindingSelect+` WHERE organization_id=? ORDER BY revision`, organization)
	if err != nil {
		return err
	}
	defer rows.Close()
	var bindings []attachment.StorageBinding
	for rows.Next() {
		b, err := scanArchiveBinding(rows)
		if err != nil {
			return err
		}
		bindings = append(bindings, b)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	pending := false
	if s.attachments != nil {
		if err := s.eraseBinding(ctx, organization, attachment.StorageBinding{ID: "hosted"}); err != nil {
			pending = true
		}
	}
	for _, b := range bindings {
		if err := s.eraseBinding(ctx, organization, b); err != nil {
			pending = true
		}
	}
	if pending {
		return artifact.ErrStorage
	}
	return nil
}

func (s *Service) archiveDeletionReceipt(c echo.Context) error {
	ctx := c.Request().Context()
	session, err := s.session(c)
	if err != nil {
		return readableNotFound(c)
	}
	organization, err := s.registry.Organization(ctx, c.Param("organization"))
	if err != nil || session.Identity.SupportActor != "" {
		return readableNotFound(c)
	}
	members, err := s.config.Provider.Memberships(ctx, session.Subject, organization.ProviderID)
	if err != nil {
		return readableNotFound(c)
	}
	owner := false
	for _, member := range members {
		if member.UserID == session.Subject && member.OrganizationID == organization.ProviderID && member.Status == "active" && member.Role.Slug == "owner" {
			owner = true
		}
	}
	if !owner {
		return readableNotFound(c)
	}
	rows, err := s.registry.store.reader.QueryContext(ctx, `SELECT r.binding_id,r.complete,r.truncated,r.attempts,r.updated_at,coalesce(f.object_key,''),coalesce(f.version_id,''),coalesce(f.error_code,''),coalesce(b.backend,''),coalesce(b.endpoint,''),coalesce(b.bucket,''),coalesce(b.prefix,'') FROM archive_deletion_receipts r LEFT JOIN archive_storage_bindings b ON b.organization_id=r.organization_id AND b.id=r.binding_id LEFT JOIN archive_deletion_failures f ON f.organization_id=r.organization_id AND f.binding_id=r.binding_id WHERE r.organization_id=? ORDER BY r.binding_id,f.ordinal`, organization.ID)
	if err != nil {
		return attachmentError(c, http.StatusServiceUnavailable, "receipt_unavailable", "Deletion receipt is unavailable")
	}
	defer rows.Close()
	type record struct {
		Binding   string `json:"binding_id"`
		Backend   string `json:"backend"`
		Endpoint  string `json:"endpoint"`
		Bucket    string `json:"bucket"`
		Prefix    string `json:"prefix"`
		Complete  bool   `json:"complete"`
		Truncated bool   `json:"truncated"`
		Attempts  int    `json:"attempts"`
		Updated   string `json:"updated_at"`
		Key       string `json:"key,omitempty"`
		Version   string `json:"version_id,omitempty"`
		Code      string `json:"code,omitempty"`
	}
	result := []record{}
	for rows.Next() {
		var row record
		if err := rows.Scan(&row.Binding, &row.Complete, &row.Truncated, &row.Attempts, &row.Updated, &row.Key, &row.Version, &row.Code, &row.Backend, &row.Endpoint, &row.Bucket, &row.Prefix); err != nil {
			return err
		}
		if row.Binding == "hosted" && s.config.Attachments != nil {
			row.Backend, row.Endpoint, row.Bucket = "hosted", s.config.Attachments.Endpoint, s.config.Attachments.Bucket
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return c.JSON(http.StatusOK, result)
}
