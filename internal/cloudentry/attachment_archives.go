package cloudentry

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
)

func (s *Service) uploadArchive(c echo.Context) error {
	c.Response().Header().Set("Cache-Control", "private, no-store")
	storage, ok := s.attachments.(attachment.ArchiveStorage)
	if !ok {
		return attachmentError(c, http.StatusServiceUnavailable, "archives_not_configured", "Archives not configured")
	}
	organization, err := s.readyOrganization(c.Request().Context(), c.Param("organization"))
	if err != nil || !safeID(c.Param("item")) {
		return attachmentNotFound(c)
	}
	suffix := "/archives/" + c.Param("item")
	status, raw, err := s.attachmentCall(c, organization, http.MethodPost, suffix+"/check", struct{}{})
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
	s.attachmentMu.RLock()
	defer s.attachmentMu.RUnlock()
	current, err := s.readyOrganization(c.Request().Context(), organization.ID)
	if err != nil || current.Generation != organization.Generation {
		return attachmentNotFound(c)
	}
	status, raw, err = s.attachmentCall(c, organization, http.MethodPost, suffix+"/check", struct {
		Reference attachment.ArchiveReference `json:"reference"`
	}{ref})
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
	status, raw, err = s.attachmentCall(c, organization, http.MethodPost, suffix, ref)
	if err != nil || status != http.StatusCreated {
		return attachmentCallError(c, status, raw, err, false)
	}
	var stored attachment.ArchiveReference
	if json.Unmarshal(raw, &stored) != nil || stored != ref {
		return attachmentError(c, http.StatusBadGateway, "tenant_unavailable", "Archive reference is invalid")
	}
	return c.JSON(http.StatusCreated, stored)
}

func (s *Service) readArchive(c echo.Context) error {
	c.Response().Header().Set("Cache-Control", "private, no-store")
	storage, ok := s.attachments.(attachment.ArchiveStorage)
	if !ok {
		return attachmentNotFound(c)
	}
	organization, err := s.readyOrganization(c.Request().Context(), c.Param("organization"))
	if err != nil || !safeID(c.Param("item")) || !artifact.ValidHash(c.Param("digest"), 64) {
		return attachmentNotFound(c)
	}
	suffix := "/archives/" + c.Param("item") + "/" + c.Param("digest")
	if c.QueryParam("section") != "" {
		suffix += "?" + url.Values{"section": {c.QueryParam("section")}, "revision": {c.QueryParam("revision")}}.Encode()
	}
	status, raw, err := s.attachmentCall(c, organization, http.MethodGet, suffix, nil)
	if err != nil || status != http.StatusOK {
		return attachmentCallError(c, status, raw, err, false)
	}
	var ref attachment.ArchiveReference
	if json.Unmarshal(raw, &ref) != nil || ref.Validate(organization.ID) != nil || ref.SHA256 != c.Param("digest") {
		return attachmentError(c, http.StatusBadGateway, "tenant_unavailable", "Archive reference is invalid")
	}
	s.attachmentMu.RLock()
	defer s.attachmentMu.RUnlock()
	current, err := s.readyOrganization(c.Request().Context(), organization.ID)
	if err != nil || current.Generation != organization.Generation {
		return attachmentNotFound(c)
	}
	if err := s.auth.audit(c.Request().Context(), "entry", organization.ID, "archive_read"); err != nil {
		return attachmentError(c, http.StatusServiceUnavailable, "audit_unavailable", "Archive audit is unavailable")
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
