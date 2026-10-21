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

<<<<<<< HEAD
func (s *Service) archiveContentSecurity(c echo.Context) string {
	policy := s.contentSecurity()
	organization := c.Param("organization")
	if !ValidOrganizationID(organization) {
		return policy
	}
	rows, err := s.registry.store.reader.QueryContext(c.Request().Context(), `SELECT DISTINCT endpoint FROM archive_storage_bindings WHERE organization_id=?`, organization)
=======
const maxArchiveOrigins = 16
const maxArchiveOriginBytes = 4096

func archiveBindingOrigins(ctx context.Context, q queryer, organization, candidate string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `WITH RECURSIVE origins(endpoint) AS (
 SELECT (SELECT endpoint FROM archive_storage_bindings WHERE organization_id=? ORDER BY endpoint COLLATE NOCASE LIMIT 1)
 UNION ALL
 SELECT (SELECT endpoint FROM archive_storage_bindings WHERE organization_id=? AND endpoint>origins.endpoint COLLATE NOCASE ORDER BY endpoint COLLATE NOCASE LIMIT 1)
 FROM origins WHERE endpoint IS NOT NULL LIMIT ?
) SELECT substr(endpoint,1,?) FROM origins WHERE endpoint IS NOT NULL`, organization, organization, maxArchiveOrigins+1, maxArchiveOriginBytes+1)
>>>>>>> 7283634fb9 (fix: complete prj_6d4919bebd73446798e6cd807feda10e#1533)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
<<<<<<< HEAD
	for rows.Next() {
		var endpoint string
		if rows.Scan(&endpoint) != nil {
			return s.contentSecurity()
=======
	origins := make([]string, 0, maxArchiveOrigins)
	total := 0
	add := func(endpoint string) error {
		endpoint = strings.ToLower(endpoint)
		if !artifact.ValidOrigin(endpoint) || !strings.HasPrefix(endpoint, "https://") {
			return attachment.ErrInvalid
>>>>>>> 7283634fb9 (fix: complete prj_6d4919bebd73446798e6cd807feda10e#1533)
		}
		for _, origin := range origins {
			if origin == endpoint {
				return nil
			}
		}
		total += len(endpoint) + 1
		if len(origins) == maxArchiveOrigins || total > maxArchiveOriginBytes {
			return attachment.ErrInvalid
		}
		origins = append(origins, endpoint)
		return nil
	}
	for count := 0; rows.Next(); count++ {
		if count == maxArchiveOrigins {
			return nil, attachment.ErrInvalid
		}
		var endpoint string
		if err := rows.Scan(&endpoint); err != nil {
			return nil, err
		}
		if err := add(endpoint); err != nil {
			return nil, err
		}
	}
<<<<<<< HEAD
	if rows.Err() != nil {
		return s.contentSecurity()
=======
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if candidate != "" {
		if err := add(candidate); err != nil {
			return nil, err
		}
	}
	return origins, nil
}

func (s *Service) archiveContentSecurity(c echo.Context) string {
	organization := c.Param("organization")
	if !ValidOrganizationID(organization) {
		return contentSecurity
>>>>>>> 7283634fb9 (fix: complete prj_6d4919bebd73446798e6cd807feda10e#1533)
	}
	origins, err := archiveBindingOrigins(c.Request().Context(), s.registry.store.reader, organization, "")
	if err != nil || len(origins) == 0 {
		return contentSecurity
	}
	return strings.Replace(contentSecurity, "connect-src 'self'", "connect-src 'self' "+strings.Join(origins, " "), 1)
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

	tx, err := s.registry.store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := archiveBindingOrigins(ctx, tx, organization, b.Endpoint); err != nil {
		return err
	}

	_, err = tx.ExecContext(ctx, `INSERT INTO archive_storage_bindings(organization_id,id,revision,backend,endpoint,region,bucket,prefix,credential_reference,versioned)
 SELECT ?,?,0,?,?,?,?,?,?,? WHERE EXISTS(SELECT 1 FROM organizations WHERE id=? AND state IN ('ready','deleting')) ON CONFLICT(organization_id,id) DO NOTHING`, organization, "", b.Backend, b.Endpoint, b.Region, b.Bucket, b.Prefix, b.CredentialReference, b.Versioned, organization)
	if err != nil {
		return err
	}
	return tx.Commit()
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
	if _, err := archiveBindingOrigins(ctx, tx, b.Organization, b.Endpoint); err != nil {
		return b, err
	}
	b.Endpoint = strings.ToLower(b.Endpoint)

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
	unlock := s.attachmentLocks.lock(organization.ID, true)
	defer unlock()
	if err := s.ensureLegacyArchiveBinding(ctx, organization.ID); err != nil {
		return attachmentError(c, http.StatusServiceUnavailable, "archive_storage_unavailable", "Archive storage is unavailable")
	}
	if err := s.auth.audit(ctx, claims.Subject, organization.ID, "archive_storage_bound"); err != nil {
		return attachmentError(c, http.StatusServiceUnavailable, "audit_unavailable", "Storage audit is unavailable")
	}
	b, err = s.saveArchiveBinding(ctx, b, input.ExpectedRevision)
	if errors.Is(err, attachment.ErrInvalid) {
		return attachmentError(c, http.StatusBadRequest, "invalid_binding", "Archive storage origins exceed the supported limit")
	}
	if errors.Is(err, ErrOrganizationConflict) {
		return attachmentError(c, http.StatusConflict, "revision_conflict", "Storage changed; reload and try again")
	}
	if err != nil {
		return attachmentError(c, http.StatusServiceUnavailable, "archive_storage_unavailable", "Archive storage is unavailable")
	}
	return c.JSON(http.StatusOK, b)
}
