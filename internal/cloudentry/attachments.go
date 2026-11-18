package cloudentry

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/cloudentry/attachmententry"
	"github.com/digitaldrywood/detent/internal/cloudentry/webhooks"
	"github.com/digitaldrywood/detent/internal/hubmaintenance"
)

func (s *Service) attachmentHandlers() *attachmententry.Service {
	return &attachmententry.Service{
		Lookup:         s.registry.Organization,
		ArchiveStorage: s.archiveStorageForReference,
		Storage:        func() attachment.Storage { return s.attachments },
		Ready:          s.readyOrganization,
		Call:           s.attachmentCall,
		ServiceCall:    s.serviceCall,
		ServiceRequest: s.serviceRequest,
		Audit:          s.auth.audit,
		Lock:           s.attachmentLocks.lock,
		Now:            s.config.Now,
		Logger:         s.config.Logger,
		StateDir:       s.config.StateDir,
		Echo:           s.echo,
	}
}

func (s *Service) attachmentCall(c echo.Context, organization Organization, method, suffix string, payload any) (int, []byte, error) {
	if c.Param("project") != "" && !safeID(c.Param("project")) {
		return http.StatusNotFound, nil, nil
	}
	path := "/api/v2/organizations/" + organization.ID + "/projects/" + c.Param("project") + "/attachment-metadata" + suffix
	if c.Param("project") == "" {
		if method != http.MethodGet {
			return http.StatusNotFound, nil, nil
		}
		path = "/api/v2/organizations/" + organization.ID + "/attachment-metadata" + suffix
		if suffix == "/current-logo" {
			path = "/api/v2/organizations/" + organization.ID + "/logo-metadata"
		}
	}
	if c.Param("attempt") != "" && method == http.MethodPost {
		if !safeID(c.Param("attempt")) {
			return http.StatusNotFound, nil, nil
		}
		path = "/api/v2/organizations/" + organization.ID + "/projects/" + c.Param("project") + "/attempts/" + c.Param("attempt") + "/evidence" + suffix
	}
	var body []byte
	var err error
	if payload != nil {
		body, err = json.Marshal(payload)
		if err != nil {
			return 0, nil, err
		}
	}
	claims, err := s.claims(organization, cloudassert.KindMachine, method, path, body)
	if err != nil {
		return 0, nil, err
	}
	var bearer, csrf, keyHash string
	if header := c.Request().Header.Get(echo.HeaderAuthorization); header != "" {
		var ok bool
		bearer, ok = strings.CutPrefix(header, "Bearer ")
		if !ok || bearer == "" {
			return http.StatusNotFound, nil, nil
		}
		keyHash, err = keyBearer(c)
		if err != nil {
			return http.StatusUnauthorized, nil, nil
		}
	} else if connection, ok := c.Request().Context().Value(accessKeyContext{}).(accessKeyConnection); ok {
		keyHash = connection.hash
	} else {
		status, _, err := s.browserClaims(c, organization, &claims)
		if err != nil {
			return status, nil, nil
		}
		if method != http.MethodGet {
			csrf = c.Request().Header.Get("X-CSRF-Token")
			if csrf == "" || subtle.ConstantTimeCompare([]byte(csrf), []byte(claims.CSRF)) != 1 {
				return http.StatusForbidden, nil, nil
			}
		}
	}
	if keyHash != "" {
		key, keyErr := s.activeAccessKey(c.Request().Context(), keyHash)
		if keyErr != nil && entryAuthStatus(keyErr, http.StatusUnauthorized) == http.StatusServiceUnavailable {
			return 0, nil, keyErr
		}
		if keyErr != nil && !errors.Is(keyErr, sql.ErrNoRows) {
			var refusal *apikey.Refusal
			if errors.As(keyErr, &refusal) {
				raw, err := json.Marshal(refusal)
				return http.StatusForbidden, raw, err
			}
			return 0, nil, keyErr
		}
		if keyErr == nil {
			claims, err = s.keyClaims(c.Request().Context(), key, organization, method, path, body)
			if err != nil && entryAuthStatus(err, http.StatusForbidden) == http.StatusServiceUnavailable {
				return 0, nil, err
			}
			if err != nil {
				var refusal *apikey.Refusal
				if errors.As(err, &refusal) {
					raw, err := json.Marshal(refusal)
					return http.StatusForbidden, raw, err
				}
				return 0, nil, err
			}
			if err := s.recordKeyUse(c.Request().Context(), key.ID, organization.ID); err != nil {
				return 0, nil, err
			}
			bearer = ""
		}
	}
	return s.signedCallCSRF(c.Request().Context(), organization, claims, body, bearer, csrf)
}

func (s *Service) sweepAttachments(ctx context.Context) {
	if s.attachments == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	now := s.config.Now()
	s.sweepAttachmentMetadata(ctx, now)
	if !s.sweepingWebhookPayloads && s.attachmentObjectCursor == "" && now.Before(s.attachmentObjectSweepAt) {
		return
	}
	prefix := "orgs/"
	if s.sweepingWebhookPayloads {
		prefix = attachment.GitHubWebhookPrefix
	}
	next, err := s.attachments.WalkPage(ctx, prefix, s.attachmentObjectCursor, 32, func(object attachment.Object) error {
		if object.CreatedAt.After(now.Add(-attachment.OrphanTTL)) {
			return nil
		}
		if s.sweepingWebhookPayloads {
			if err := webhooks.SweepReleasedPayload(ctx, s.attachments, s.registry.store.db.postgres.GitHubIngressReleased, object.Key); err != nil {
				s.config.Logger.Warn("webhook payload orphan check failed", "error", err)
			}
			return ctx.Err()
		}
		id, key, ok := strings.Cut(strings.TrimPrefix(object.Key, "orgs/"), "/")
		if !ok || !ValidOrganizationID(id) {
			return nil
		}
		for _, scope := range attachmentObjectScopes {
			if !strings.HasPrefix(key, scope.prefix) {
				continue
			}
			org, err := s.readyOrganization(ctx, id)
			if errors.Is(err, ErrOrganizationNotFound) {
				return nil
			}
			if err == nil {
				err = s.attachmentHandlers().SweepOrphan(ctx, org, scope.prefix, scope.endpoint, object)
			}
			if err != nil {
				s.config.Logger.Warn("attachment orphan check failed", "organization", id, "error", err)
			}
			break
		}
		return ctx.Err()
	})
	if err != nil {
		s.config.Logger.Warn("attachment orphan listing failed", "error", err)
		return
	}
	if !s.sweepingWebhookPayloads && s.attachmentObjectCursor == "" {
		s.attachmentObjectSweepAt = now.Add(time.Hour)
	}
	s.attachmentObjectCursor = next
	if next == "" && s.sharedHub != nil {
		s.sweepingWebhookPayloads = !s.sweepingWebhookPayloads
	}
}

func (s *Service) sweepAttachmentMetadata(ctx context.Context, now time.Time) {
	if s.attachmentAfter == "" && now.Before(s.attachmentSweepAt) {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	rows, err := s.registry.listOrganizations(ctx, organizationSelect+" WHERE id>$1 ORDER BY id LIMIT $2", s.attachmentAfter, hubmaintenance.TenantLimit)
	if err != nil {
		s.config.Logger.Warn("attachment retention registry unavailable", "error", err)
		return
	}
	if s.attachmentAfter == "" {
		s.attachmentSweepAt = now.Add(time.Hour)
	}
	remaining := hubmaintenance.ItemLimit
	for i, org := range rows {
		if ctx.Err() != nil {
			return
		}
		s.attachmentAfter = org.ID
		if org.State == "ready" {
			limit := remaining / (len(rows) - i)
			used, err := s.attachmentHandlers().SweepExpired(ctx, org, limit)
			remaining -= used
			if err != nil {
				s.config.Logger.Warn("attachment retention failed", "organization", org.ID, "error", err)
			}
		}
	}
	if len(rows) < hubmaintenance.TenantLimit {
		s.attachmentAfter = ""
	}
}

var attachmentObjectScopes = [...]struct{ prefix, endpoint string }{
	{"attachments/", "/internal/v1/attachments/exists"},
	{"attempt-diffs/", "/internal/v1/diff-bodies/exists"},
	{"blobs/sha256/", "/internal/v1/attachments/exists"},
}
