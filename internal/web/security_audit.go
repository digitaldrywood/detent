package web

import (
	"errors"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/store"
)

type securityAuditDispositionRequest struct {
	Repository  string `json:"repository" form:"repository"`
	PullRequest int    `json:"pull_request" form:"pull_request"`
	BaseSHA     string `json:"base_sha" form:"base_sha"`
	HeadSHA     string `json:"head_sha" form:"head_sha"`
	FindingID   string `json:"finding_id" form:"finding_id"`
	Status      string `json:"status" form:"status"`
	Evidence    string `json:"evidence" form:"evidence"`
	Confirm     bool   `json:"confirm" form:"confirm"`
}

func (s *Server) apiSecurityAuditDisposition(c echo.Context) error {
	var payload securityAuditDispositionRequest
	if err := c.Bind(&payload); err != nil {
		return c.JSON(http.StatusUnprocessableEntity, errorResponse("invalid_request", "Request body must be valid JSON or form data"))
	}
	if !payload.Confirm {
		return c.JSON(http.StatusPreconditionRequired, errorResponse("confirmation_required", "Confirm the false-positive disposition with confirm=true"))
	}

	projectID := strings.TrimSpace(c.Param("project_id"))
	if _, ok := s.registry.Get(project.ID(projectID)); !ok {
		return c.JSON(http.StatusNotFound, errorResponse("project_not_found", "project not found"))
	}
	payload.Repository = strings.TrimSpace(payload.Repository)
	payload.BaseSHA = strings.TrimSpace(payload.BaseSHA)
	payload.HeadSHA = strings.TrimSpace(payload.HeadSHA)
	payload.FindingID = strings.TrimSpace(payload.FindingID)
	payload.Status = strings.ToLower(strings.TrimSpace(payload.Status))
	payload.Evidence = strings.TrimSpace(payload.Evidence)
	if payload.Repository == "" || payload.PullRequest <= 0 || payload.BaseSHA == "" || payload.HeadSHA == "" || payload.FindingID == "" || payload.Evidence == "" {
		return c.JSON(http.StatusUnprocessableEntity, errorResponse("invalid_disposition", "repository, pull_request, base_sha, head_sha, finding_id, and evidence are required"))
	}
	if payload.Status != securityaudit.DispositionFalsePositive {
		return c.JSON(http.StatusUnprocessableEntity, errorResponse("invalid_disposition", "status must be false_positive"))
	}
	if len(payload.Evidence) > securityaudit.MaxDispositionEvidenceBytes {
		return c.JSON(http.StatusUnprocessableEntity, errorResponse("invalid_disposition", "evidence is too large"))
	}

	key := securityaudit.Key{
		ProjectID:  projectID,
		Repository: payload.Repository,
		PRNumber:   payload.PullRequest,
		BaseSHA:    payload.BaseSHA,
		HeadSHA:    payload.HeadSHA,
	}
	run, err := s.store.LatestSecurityAuditRun(c.Request().Context(), key)
	if errors.Is(err, store.ErrNotFound) {
		return c.JSON(http.StatusNotFound, errorResponse("audit_not_found", "trusted exact-head security audit not found"))
	}
	if err != nil {
		s.logger.Warn("security audit disposition lookup failed", "project_id", projectID, "error", err)
		return c.JSON(http.StatusServiceUnavailable, errorResponse("audit_lookup_failed", "security audit lookup failed"))
	}
	serviceIdentity := securityaudit.ServiceIdentity(projectID)
	evaluation := securityaudit.Evaluate(run, nil, key, serviceIdentity, []string{"p1", "p2", "p3"})
	if evaluation.Reason != securityaudit.ReasonUnresolvedFindings {
		return c.JSON(http.StatusConflict, errorResponse("audit_not_disposable", "security audit is not a trusted successful run with unresolved findings"))
	}
	found := false
	for _, finding := range run.Findings {
		if strings.TrimSpace(finding.ID) == payload.FindingID {
			found = true
			break
		}
	}
	if !found {
		return c.JSON(http.StatusNotFound, errorResponse("finding_not_found", "finding does not belong to the exact-head audit run"))
	}

	disposition, err := s.store.RecordSecurityAuditDisposition(c.Request().Context(), securityaudit.Disposition{
		AuditRunID:      run.ID,
		FindingID:       payload.FindingID,
		Status:          securityaudit.DispositionFalsePositive,
		Evidence:        payload.Evidence,
		ServiceIdentity: serviceIdentity,
		RecordedAt:      s.now().UTC(),
	})
	if err != nil {
		s.logger.Warn("security audit disposition persistence failed", "project_id", projectID, "audit_run_id", run.ID, "finding_id", payload.FindingID, "error", err)
		return c.JSON(http.StatusServiceUnavailable, errorResponse("disposition_failed", "security audit disposition could not be recorded"))
	}
	s.logger.Info("security audit finding disposition recorded", "project_id", projectID, "audit_run_id", run.ID, "finding_id", payload.FindingID, "service_identity", serviceIdentity)
	return c.JSON(http.StatusCreated, disposition)
}
