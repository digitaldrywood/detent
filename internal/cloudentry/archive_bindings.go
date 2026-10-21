package cloudentry

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/cloudassert"
)

const archiveBindingSelect = `SELECT organization_id,id,revision,backend,endpoint,region,bucket,prefix,credential_reference,versioned FROM archive_storage_bindings`

func (s *Service) archiveContentSecurity(c echo.Context) string {
	policy := s.contentSecurity()
	organization := c.Param("organization")
	if !ValidOrganizationID(organization) {
		return policy
	}
	rows, err := s.registry.store.reader.QueryContext(c.Request().Context(), `SELECT DISTINCT endpoint FROM archive_storage_bindings WHERE organization_id=?`, organization)
	if err != nil {
		return policy
	}
	defer rows.Close()
	for rows.Next() {
		var endpoint string
		if rows.Scan(&endpoint) != nil {
			return s.contentSecurity()
		}
		if artifact.ValidOrigin(endpoint) && strings.HasPrefix(endpoint, "https://") {
			policy = strings.Replace(policy, "connect-src 'self'", "connect-src 'self' "+endpoint, 1)
		}
	}
	if rows.Err() != nil {
		return s.contentSecurity()
	}
	return policy
}

func scanArchiveBinding(row rowScanner) (attachment.StorageBinding, error) {
	var b attachment.StorageBinding
	err := row.Scan(&b.Organization, &b.ID, &b.Revision, &b.Backend, &b.Endpoint, &b.Region, &b.Bucket, &b.Prefix, &b.CredentialReference, &b.Versioned)
	return b, err
}

func (s *Service) ensureLegacyArchiveBinding(ctx context.Context, organization string) error {
	if s.config.Attachments == nil {
		return nil
	}
	var exists bool
	if err := s.registry.store.reader.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM archive_storage_bindings WHERE organization_id=? AND id='')`, organization).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	cfg := s.config.Attachments
	b := attachment.StorageBinding{Organization: organization, Backend: "hosted", Endpoint: cfg.Endpoint, Region: cfg.Region, Bucket: cfg.Bucket, CredentialReference: "hosted"}
	if b.Validate() != nil {
		return attachment.ErrInvalid
	}

	_, err := s.registry.store.db.ExecContext(ctx, `INSERT INTO archive_storage_bindings(organization_id,id,revision,backend,endpoint,region,bucket,prefix,credential_reference,versioned)
 SELECT ?,?,0,?,?,?,?,?,?,? WHERE EXISTS(SELECT 1 FROM organizations WHERE id=? AND state IN ('ready','deleting')) ON CONFLICT(organization_id,id) DO NOTHING`, organization, "", b.Backend, b.Endpoint, b.Region, b.Bucket, b.Prefix, b.CredentialReference, b.Versioned, organization)
	return err
}

func (s *Service) currentArchiveBinding(ctx context.Context, organization string) (attachment.StorageBinding, error) {
	if err := s.ensureLegacyArchiveBinding(ctx, organization); err != nil {
		return attachment.StorageBinding{}, err
	}
	b, err := scanArchiveBinding(s.registry.store.reader.QueryRowContext(ctx, archiveBindingSelect+` WHERE organization_id=? ORDER BY revision DESC LIMIT 1`, organization))
	if errors.Is(err, sql.ErrNoRows) {
		return attachment.StorageBinding{}, nil
	}
	return b, err
}

func (s *Service) saveArchiveBinding(ctx context.Context, b attachment.StorageBinding, expected int64) (attachment.StorageBinding, error) {
	if b.Validate() != nil {
		return b, attachment.ErrInvalid
	}
	tx, err := s.registry.store.db.BeginTx(ctx, nil)
	if err != nil {
		return b, err
	}
	defer tx.Rollback()
	var current int64
	if err := tx.QueryRowContext(ctx, `SELECT coalesce(max(revision),0) FROM archive_storage_bindings WHERE organization_id=?`, b.Organization).Scan(&current); err != nil {
		return b, err
	}
	if current != expected {
		return b, ErrOrganizationConflict
	}
	b.Revision = current + 1
	digest := sha256.Sum256([]byte(b.Organization + "\n" + strconv.FormatInt(b.Revision, 10)))
	b.ID = "asb_" + hex.EncodeToString(digest[:16])
	result, err := tx.ExecContext(ctx, `INSERT INTO archive_storage_bindings(organization_id,id,revision,backend,endpoint,region,bucket,prefix,credential_reference,versioned)
SELECT ?,?,?,?,?,?,?,?,?,? WHERE EXISTS(SELECT 1 FROM organizations WHERE id=? AND state='ready')`, b.Organization, b.ID, b.Revision, b.Backend, b.Endpoint, b.Region, b.Bucket, b.Prefix, b.CredentialReference, b.Versioned, b.Organization)
	if err != nil {
		return b, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return b, err
	}
	if count != 1 {
		return b, ErrOrganizationNotFound
	}
	return b, tx.Commit()
}

func (s *Service) bindingStorage(ctx context.Context, b attachment.StorageBinding) (attachment.ArchiveStorage, error) {
	if b.Organization == "" {
		storage, ok := s.attachments.(attachment.ArchiveStorage)
		if !ok {
			return nil, artifact.ErrStorage
		}
		return storage, nil
	}
	var secret attachment.Credentials
	if b.Backend == "hosted" {
		if s.config.Attachments == nil {
			return nil, artifact.ErrStorage
		}
		secret = attachment.Credentials{AccessKeyID: s.config.Attachments.AccessKeyID, SecretAccessKey: s.config.Attachments.SecretAccessKey}
	} else {
		if s.config.ArchiveCredentials == nil {
			return nil, artifact.ErrStorage
		}
		var err error
		secret, err = s.config.ArchiveCredentials(ctx, b.Organization, b.CredentialReference)
		if err != nil {
			return nil, artifact.ErrStorage
		}
	}
	storage, err := attachment.NewBindingStorage(ctx, b, secret, s.config.attachmentTransport)
	if err != nil {
		return nil, artifact.ErrStorage
	}
	return storage, nil
}

func (s *Service) archiveStorageForReference(ctx context.Context, organization string, ref attachment.ArchiveReference) (attachment.ArchiveStorage, error) {
	if ref.BindingID == "" {
		if err := s.ensureLegacyArchiveBinding(ctx, organization); err != nil {
			return nil, artifact.ErrStorage
		}
	}
	b, err := scanArchiveBinding(s.registry.store.reader.QueryRowContext(ctx, archiveBindingSelect+` WHERE organization_id=? AND id=?`, organization, ref.BindingID))
	if errors.Is(err, sql.ErrNoRows) && ref.BindingID == "" {
		return s.bindingStorage(ctx, attachment.StorageBinding{})
	}
	if err != nil || (b.Backend == "s3") != ref.CustomerOwned {
		return nil, artifact.ErrStorage
	}
	return s.bindingStorage(ctx, b)
}

func (s *Service) archiveStorageBinding(c echo.Context) error {
	ctx := c.Request().Context()
	organization, err := s.readyOrganization(ctx, c.Param("organization"))
	if err != nil || c.Request().Header.Get(echo.HeaderAuthorization) != "" {
		return readableNotFound(c)
	}
	claims := cloudassert.Claims{}
	if _, _, err := s.browserClaims(c, organization, &claims); err != nil || claims.Role != "owner" || claims.SupportActor != "" {
		return readableNotFound(c)
	}
	if c.Request().Method == http.MethodGet {
		binding, err := s.currentArchiveBinding(ctx, organization.ID)
		if err != nil {
			return attachmentError(c, http.StatusServiceUnavailable, "archive_storage_unavailable", "Archive storage is unavailable")
		}
		return c.JSON(http.StatusOK, binding)
	}
	if c.Request().Header.Get("X-CSRF-Token") != claims.CSRF {
		return attachmentError(c, http.StatusForbidden, "invalid_csrf", "Reload the form and try again")
	}
	var input struct {
		Backend             string `json:"backend"`
		Endpoint            string `json:"endpoint"`
		Region              string `json:"region"`
		Bucket              string `json:"bucket"`
		Prefix              string `json:"prefix"`
		CredentialReference string `json:"credential_reference"`
		ExpectedRevision    int64  `json:"expected_revision"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(c.Response(), c.Request().Body, 8192))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&input) != nil {
		return attachmentError(c, http.StatusBadRequest, "invalid_binding", "Invalid storage binding")
	}
	b := attachment.StorageBinding{Organization: organization.ID, Backend: input.Backend, Endpoint: input.Endpoint, Region: input.Region, Bucket: input.Bucket, Prefix: input.Prefix, CredentialReference: input.CredentialReference}
	if b.Backend == "hosted" && s.config.Attachments != nil {
		cfg := s.config.Attachments
		b.Endpoint, b.Region, b.Bucket, b.Prefix, b.CredentialReference = cfg.Endpoint, cfg.Region, cfg.Bucket, "", "hosted"
	}
	if b.Validate() != nil || attachment.ValidateArchiveEndpoint(ctx, b.Endpoint) != nil {
		return attachmentError(c, http.StatusBadRequest, "invalid_binding", "Invalid storage binding endpoint or location")
	}
	s.attachmentMu.Lock()
	defer s.attachmentMu.Unlock()
	if err := s.ensureLegacyArchiveBinding(ctx, organization.ID); err != nil {
		return attachmentError(c, http.StatusServiceUnavailable, "archive_storage_unavailable", "Archive storage is unavailable")
	}
	b.ID = "validating"
	storage, err := s.bindingStorage(ctx, b)
	if err != nil || storage.ConfigureArchiveReads(ctx, []string{s.config.PublicURL}) != nil {
		return attachmentError(c, http.StatusBadGateway, "archive_storage_unavailable", "Archive storage is unavailable")
	}
	versioned, err := storage.ArchiveVersioning(ctx)
	if err != nil {
		return attachmentError(c, http.StatusBadGateway, "archive_storage_unavailable", "Archive storage is unavailable")
	}
	b.Versioned = &versioned
	if err := s.auth.audit(ctx, claims.Subject, organization.ID, "archive_storage_bound"); err != nil {
		return attachmentError(c, http.StatusServiceUnavailable, "audit_unavailable", "Storage audit is unavailable")
	}
	b, err = s.saveArchiveBinding(ctx, b, input.ExpectedRevision)
	if errors.Is(err, ErrOrganizationConflict) {
		return attachmentError(c, http.StatusConflict, "revision_conflict", "Storage changed; reload and try again")
	}
	if err != nil {
		return attachmentError(c, http.StatusServiceUnavailable, "archive_storage_unavailable", "Archive storage is unavailable")
	}
	return c.JSON(http.StatusOK, b)
}
