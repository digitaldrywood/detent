package cloudentry

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/digitaldrywood/detent/internal/cloudentry/attachmententry"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
)

func (s *Service) uploadArchive(c echo.Context) error {
	c.Response().Header().Set("Cache-Control", "private, no-store")
	if s.attachments == nil && s.config.ArchiveCredentials == nil {
		return attachmententry.Error(c, http.StatusServiceUnavailable, "archives_not_configured", "Archives not configured")
	}
	organization, err := s.readyOrganization(c.Request().Context(), c.Param("organization"))
	if err != nil || !safeID(c.Param("item")) {
		return attachmententry.NotFound(c)
	}
	suffix := "/archives/" + c.Param("item")
	status, raw, err := s.attachmentCall(c, organization, http.MethodPost, suffix+"/check", struct{}{})
	if err != nil || status != http.StatusOK {
		return attachmententry.CallError(c, status, raw, err, false)
	}
	var authorized struct {
		Principal string `json:"principal_id"`
	}
	if json.Unmarshal(raw, &authorized) != nil || authorized.Principal == "" {
		return attachmententry.Error(c, http.StatusBadGateway, "tenant_unavailable", "Archive authorization failed")
	}
	schema, err := strconv.Atoi(c.Request().Header.Get("X-Detent-Archive-Schema"))
	if err != nil || schema < 1 {
		return attachmententry.Error(c, http.StatusBadRequest, "invalid_archive", "An archive schema version is required")
	}
	c.Request().Body = http.MaxBytesReader(c.Response(), c.Request().Body, attachment.MaxArchiveBytes+1)
	ref, data, err := attachment.PrepareArchive(organization.ID, schema, c.Request().Body)
	if err != nil {
		return attachmententry.UploadError(c, err)
	}
	unlock := s.attachmentLocks.lock(organization.ID, false)
	defer unlock()
	current, err := s.readyOrganization(c.Request().Context(), organization.ID)
	if err != nil || current.Generation != organization.Generation {
		return attachmententry.NotFound(c)
	}
	binding, err := s.currentArchiveBinding(c.Request().Context(), organization.ID)
	if err != nil {
		return attachmententry.Error(c, http.StatusServiceUnavailable, "archive_storage_unavailable", "Archive storage is unavailable")
	}
	ref.BindingID, ref.CustomerOwned = binding.ID, binding.Backend == "s3"
	storage, err := s.bindingStorage(c.Request().Context(), binding)
	if err != nil {
		return attachmententry.Error(c, http.StatusBadGateway, "archive_storage_unavailable", "Archive storage is unavailable")
	}
	status, raw, err = s.attachmentCall(c, organization, http.MethodPost, suffix+"/check", struct {
		Reference attachment.ArchiveReference `json:"reference"`
	}{ref})
	if err != nil || status != http.StatusOK {
		return attachmententry.CallError(c, status, raw, err, false)
	}
	ctx := c.Request().Context()
	if err := s.auth.audit(ctx, authorized.Principal, organization.ID, "archive_uploaded"); err != nil {
		return attachmententry.Error(c, http.StatusServiceUnavailable, "audit_unavailable", "Archive audit is unavailable")
	}
	if err := attachment.StoreArchive(ctx, storage, organization.ID, ref, data); err != nil {
		return attachmententry.Error(c, http.StatusBadGateway, "archive_storage_unavailable", "Archive storage is unavailable")
	}
	current, err = s.readyOrganization(ctx, organization.ID)
	if err != nil || current.Generation != organization.Generation {
		return attachmententry.NotFound(c)
	}
	status, raw, err = s.attachmentCall(c, organization, http.MethodPost, suffix, ref)
	if err != nil || status != http.StatusCreated {
		return attachmententry.CallError(c, status, raw, err, false)
	}
	var stored attachment.ArchiveReference
	if json.Unmarshal(raw, &stored) != nil || stored != ref {
		return attachmententry.Error(c, http.StatusBadGateway, "tenant_unavailable", "Archive reference is invalid")
	}
	return c.JSON(http.StatusCreated, stored)
}

func (s *Service) readArchive(c echo.Context) error {
	c.Response().Header().Set("Cache-Control", "private, no-store")
	organization, err := s.readyOrganization(c.Request().Context(), c.Param("organization"))
	if err != nil || !safeID(c.Param("item")) || !artifact.ValidHash(c.Param("digest"), 64) {
		return attachmententry.NotFound(c)
	}
	status, raw, err := s.attachmentCall(c, organization, http.MethodGet, "/archives/"+c.Param("item")+"/"+c.Param("digest")+archiveBindingQuery(c), nil)
	if err != nil || status != http.StatusOK {
		return attachmententry.CallError(c, status, raw, err, false)
	}
	var ref attachment.ArchiveReference
	if json.Unmarshal(raw, &ref) != nil || ref.Validate(organization.ID) != nil || ref.SHA256 != c.Param("digest") {
		return attachmententry.Error(c, http.StatusBadGateway, "tenant_unavailable", "Archive reference is invalid")
	}
	unlock := s.attachmentLocks.lock(organization.ID, false)
	defer unlock()
	current, err := s.readyOrganization(c.Request().Context(), organization.ID)
	if err != nil || current.Generation != organization.Generation {
		return attachmententry.NotFound(c)
	}
	if err := s.auth.audit(c.Request().Context(), "entry", organization.ID, "archive_read"); err != nil {
		return attachmententry.Error(c, http.StatusServiceUnavailable, "audit_unavailable", "Archive audit is unavailable")
	}
	storage, err := s.archiveStorageForReference(c.Request().Context(), organization.ID, ref)
	if err != nil {
		return attachmententry.Error(c, http.StatusBadGateway, "archive_storage_unavailable", "Archive storage is unavailable")
	}
	access, err := storage.PresignArchive(c.Request().Context(), ref)
	if err != nil {
		return attachmententry.Error(c, http.StatusBadGateway, "archive_storage_unavailable", "Archive storage is unavailable")
	}
	return c.JSON(http.StatusOK, access)
}

func (s *Service) registerArchiveRoutes(e *echo.Echo, attachmentBase string) {
	base := strings.TrimSuffix(attachmentBase, "/attachments") + "/work-items/:item/archives"
	e.POST(base, s.uploadArchive)
	e.GET(base+"/:digest/access", s.readArchive)
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
