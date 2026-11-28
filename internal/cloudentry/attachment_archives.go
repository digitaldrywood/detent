package cloudentry

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) uploadArchive(c echo.Context) error {
	c.Response().Header().Set("Cache-Control", "private, no-store")
	if s.attachments == nil && s.config.ArchiveCredentials == nil {
		return attachmentError(c, http.StatusServiceUnavailable, "archives_not_configured", "Archives not configured")
	}
	organization, err := s.readyOrganization(c.Request().Context(), c.Param("organization"))
	if err != nil || !safeID(c.Param("item")) {
		return attachmentNotFound(c)
	}
	var check attachment.ArchiveCheck
	lease := c.Request().Header.Get(attachment.ArchiveLeaseHeader)
	token := c.Request().Header.Get(attachment.ArchiveFenceHeader)
	if lease != "" || token != "" {
		value, err := strconv.ParseInt(token, 10, 64)
		fence := attachment.ArchiveFence{LeaseID: tracker.LeaseID(lease), FencingToken: tracker.FencingToken(value)}
		if err != nil || !fence.Valid() {
			return attachmentError(c, http.StatusBadRequest, "invalid_archive", "Invalid archive execution fence")
		}
		check.Fence = &fence
	}
	suffix := "/archives/" + c.Param("item")
	status, raw, err := s.attachmentCall(c, organization, http.MethodPost, suffix+"/check", check)
	if err != nil || status != http.StatusOK {
		return attachmentCallError(c, status, raw, err, false)
	}
	var authorized struct {
		Principal string `json:"principal_id"`
	}
	if json.Unmarshal(raw, &authorized) != nil || authorized.Principal == "" {
		return attachmentError(c, http.StatusBadGateway, "tenant_unavailable", "Archive authorization failed")
	}
	schema, err := strconv.Atoi(c.Request().Header.Get("X-Detent-Archive-Schema"))
	if err != nil || schema < 1 {
		return attachmentError(c, http.StatusBadRequest, "invalid_archive", "An archive schema version is required")
	}
	c.Request().Body = http.MaxBytesReader(c.Response(), c.Request().Body, attachment.MaxArchiveBytes+1)
	ref, data, err := attachment.PrepareArchive(organization.ID, schema, c.Request().Body)
	if err != nil {
		return attachmentUploadError(c, err)
	}
	if check.Fence != nil {
		ref.Key, err = check.Fence.Key(organization.ID, c.Param("project"), c.Param("item"), ref.SHA256)
		if err != nil {
			return attachmentUploadError(c, err)
		}
	}
	unlock := s.attachmentLocks.lock(organization.ID, false)
	defer unlock()
	current, err := s.readyOrganization(c.Request().Context(), organization.ID)
	if err != nil || current.Generation != organization.Generation {
		return attachmentNotFound(c)
	}
	binding, err := s.currentArchiveBinding(c.Request().Context(), organization.ID)
	if err != nil {
		return attachmentError(c, http.StatusServiceUnavailable, "archive_storage_unavailable", "Archive storage is unavailable")
	}
	ref.BindingID, ref.CustomerOwned = binding.ID, binding.Backend == "s3"
	storage, err := s.bindingStorage(c.Request().Context(), binding)
	if err != nil {
		return attachmentError(c, http.StatusBadGateway, "archive_storage_unavailable", "Archive storage is unavailable")
	}
	check.Reference = &ref
	status, raw, err = s.attachmentCall(c, organization, http.MethodPost, suffix+"/check", check)
	if err != nil || status != http.StatusOK {
		return attachmentCallError(c, status, raw, err, false)
	}
	ctx := c.Request().Context()
	if err := s.auth.audit(ctx, authorized.Principal, organization.ID, "archive_uploaded"); err != nil {
		return attachmentError(c, http.StatusServiceUnavailable, "audit_unavailable", "Archive audit is unavailable")
	}
	if err := attachment.StoreArchive(ctx, storage, organization.ID, ref, data); err != nil {
		return attachmentError(c, http.StatusBadGateway, "archive_storage_unavailable", "Archive storage is unavailable")
	}
	status, raw, err = s.attachmentCall(c, organization, http.MethodPost, suffix, attachment.ArchivePublication{ArchiveReference: ref, Fence: check.Fence})
	if err != nil || status != http.StatusCreated {
		if err == nil && status == http.StatusConflict && check.Fence != nil {
			var refusal struct {
				Details map[string]bool `json:"details"`
			}
			if json.Unmarshal(raw, &refusal) == nil && refusal.Details[attachment.ArchiveDiscardObject] {
				s.discardArchive(ctx, storage, organization.ID, authorized.Principal, ref.Key)
			}
		}
		return attachmentCallError(c, status, raw, err, false)
	}
	var stored attachment.ArchiveReference
	if json.Unmarshal(raw, &stored) != nil || stored != ref {
		return attachmentError(c, http.StatusBadGateway, "tenant_unavailable", "Archive reference is invalid")
	}
	return c.JSON(http.StatusCreated, stored)
}

func (s *Service) discardArchive(ctx context.Context, storage attachment.ArchiveStorage, organization, principal, key string) {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	err := s.auth.audit(cleanup, principal, organization, "attachment_deleted")
	if err == nil {
		err = storage.Delete(cleanup, key)
	}
	if err != nil {
		s.config.Logger.WarnContext(cleanup, "rejected archive cleanup failed", "organization", organization)
	}
}

func (s *Service) readArchive(c echo.Context) error {
	c.Response().Header().Set("Cache-Control", "private, no-store")
	organization, err := s.readyOrganization(c.Request().Context(), c.Param("organization"))
	if err != nil || !safeID(c.Param("item")) || !artifact.ValidHash(c.Param("digest"), 64) {
		return attachmentNotFound(c)
	}
	status, raw, err := s.attachmentCall(c, organization, http.MethodGet, "/archives/"+c.Param("item")+"/"+c.Param("digest")+archiveBindingQuery(c), nil)
	if err != nil || status != http.StatusOK {
		return attachmentCallError(c, status, raw, err, false)
	}
	var ref attachment.ArchiveReference
	if json.Unmarshal(raw, &ref) != nil || ref.Validate(organization.ID) != nil || ref.SHA256 != c.Param("digest") {
		return attachmentError(c, http.StatusBadGateway, "tenant_unavailable", "Archive reference is invalid")
	}
	unlock := s.attachmentLocks.lock(organization.ID, false)
	defer unlock()
	current, err := s.readyOrganization(c.Request().Context(), organization.ID)
	if err != nil || current.Generation != organization.Generation {
		return attachmentNotFound(c)
	}
	if err := s.auth.audit(c.Request().Context(), "entry", organization.ID, "archive_read"); err != nil {
		return attachmentError(c, http.StatusServiceUnavailable, "audit_unavailable", "Archive audit is unavailable")
	}
	storage, err := s.archiveStorageForReference(c.Request().Context(), organization.ID, ref)
	if err != nil {
		return attachmentError(c, http.StatusBadGateway, "archive_storage_unavailable", "Archive storage is unavailable")
	}
	access, err := storage.PresignArchive(c.Request().Context(), ref)
	if err != nil {
		return attachmentError(c, http.StatusBadGateway, "archive_storage_unavailable", "Archive storage is unavailable")
	}
	return c.JSON(http.StatusOK, access)
}

func (s *Service) registerArchiveRoutes(e *echo.Echo, attachmentBase string) {
	base := strings.TrimSuffix(attachmentBase, "/attachments") + "/work-items/:item/archives"
	e.POST(base, s.uploadArchive)
	e.GET(base+"/:digest/access", s.readArchive)
}

func archiveStorage(storage attachment.Storage) attachment.ArchiveStorage {
	result, ok := storage.(attachment.ArchiveStorage)
	if !ok {
		return nil
	}
	return result
}

func archiveBindingQuery(c echo.Context) string {
	query := url.Values{}
	if c.QueryParams().Has("binding") {
		query.Set("binding", c.QueryParam("binding"))
	}
	if c.QueryParam("section") != "" {
		query.Set("section", c.QueryParam("section"))
		query.Set("revision", c.QueryParam("revision"))
	}
	if len(query) == 0 {
		return ""
	}
	return "?" + query.Encode()
}
