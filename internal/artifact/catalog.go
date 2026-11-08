package artifact

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"
)

type CatalogScope struct {
	OrganizationID string `json:"organization_id"`
	ProjectID      string `json:"project_id"`
	ServiceID      string `json:"service_id"`
}

type CatalogObject struct {
	Object
	StorageVersion string `json:"storage_version"`
	Verified       bool   `json:"verified"`
	Discarded      bool   `json:"discarded"`
}

type CatalogManifest struct {
	Reference
	BodyKey           string    `json:"body_key"`
	BodyBytes         int64     `json:"body_bytes"`
	CreatedAt         time.Time `json:"created_at"`
	RetentionPolicyID string    `json:"retention_policy_id"`
	Capture           *Capture  `json:"capture,omitempty"`
}

type CatalogCommand struct {
	CatalogScope
	Operation string          `json:"operation"`
	UploadID  string          `json:"upload_id,omitempty"`
	Key       string          `json:"key,omitempty"`
	Upload    Upload          `json:"upload,omitzero"`
	Object    CatalogObject   `json:"object,omitzero"`
	Manifest  CatalogManifest `json:"manifest,omitzero"`
	Revision  int64           `json:"revision,omitempty"`
	At        time.Time       `json:"at,omitzero"`
	Before    time.Time       `json:"before,omitzero"`
	After     string          `json:"after,omitempty"`
	Limit     int             `json:"limit,omitempty"`
	Outcome   string          `json:"outcome,omitempty"`
	Receipt   string          `json:"receipt,omitempty"`
	Traffic   CatalogTraffic  `json:"traffic,omitzero"`
}

type CatalogTraffic struct {
	ID               string `json:"id,omitempty"`
	UploadBytes      int64  `json:"upload_bytes,omitempty"`
	DownloadBytes    int64  `json:"download_bytes,omitempty"`
	Requests         int64  `json:"requests,omitempty"`
	WindowSeconds    int64  `json:"window_seconds,omitempty"`
	RetentionSeconds int64  `json:"retention_seconds,omitempty"`
}

type CatalogResult struct {
	Upload    Upload            `json:"upload,omitzero"`
	Object    CatalogObject     `json:"object,omitzero"`
	Objects   []CatalogObject   `json:"objects,omitempty"`
	Manifest  CatalogManifest   `json:"manifest,omitzero"`
	Manifests []CatalogManifest `json:"manifests,omitempty"`
	Uploads   []Upload          `json:"uploads,omitempty"`
	Usage     Usage             `json:"usage,omitzero"`
	Traffic   CatalogTraffic    `json:"traffic,omitzero"`
	Next      string            `json:"next,omitempty"`
}

type Catalog interface {
	Execute(context.Context, CatalogCommand) (CatalogResult, error)
}

type RemoteCatalog struct {
	Hub *RemoteHub
}

func (c *RemoteCatalog) Execute(ctx context.Context, command CatalogCommand) (CatalogResult, error) {
	var result CatalogResult
	if c == nil || c.Hub == nil || c.Hub.PublisherToken == nil || command.OrganizationID != c.Hub.OrganizationID || command.ServiceID != c.Hub.ServiceID || !ValidID(command.ProjectID, "prj") || c.Hub.ProjectID != "" && command.ProjectID != c.Hub.ProjectID {
		return result, ErrDenied
	}
	err := c.Hub.requestCatalog(ctx, command, &result)
	return result, err
}

func (h *RemoteHub) requestCatalog(ctx context.Context, command CatalogCommand, result *CatalogResult) error {
	return h.requestResultLimit(ctx, h.PublisherToken(), h.base(command.ProjectID)+"/artifact-services/"+h.ServiceID+"/catalog", command, result, 8<<20)
}

type CatalogAuthorization func(context.Context, string, CatalogCommand) error

func NewCatalogHandler(owner Catalog, authorize CatalogAuthorization) (http.Handler, error) {
	if owner == nil || authorize == nil {
		return nil, ErrInvalid
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		fail := func(err error) {
			status := http.StatusServiceUnavailable
			for _, entry := range []struct {
				err    error
				status int
			}{{ErrDenied, 403}, {ErrAuthorization, 401}, {ErrMissing, 404}, {ErrInvalid, 422}, {ErrIntegrity, 422}, {ErrConflict, 409}, {ErrQuota, 429}, {ErrExpired, 410}} {
				if errors.Is(err, entry.err) {
					status = entry.status
					err = entry.err
					break
				}
			}
			w.WriteHeader(status)
			code := "catalog_unavailable"
			if status != http.StatusServiceUnavailable {
				code = err.Error()
			}
			if err := json.NewEncoder(w).Encode(struct {
				Code string `json:"code"`
			}{code}); err != nil {
				return
			}
		}
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var command CatalogCommand
		if err := Decode(r.Body, &command, 2<<20); err != nil {
			fail(err)
			return
		}
		header := r.Header.Values("Authorization")
		if len(header) != 1 || len(header[0]) < 8 || header[0][:7] != "Bearer " {
			fail(ErrDenied)
			return
		}
		if err := authorize(r.Context(), header[0][7:], command); err != nil {
			fail(err)
			return
		}
		result, err := owner.Execute(r.Context(), command)
		if err != nil {
			fail(err)
			return
		}
		if err := json.NewEncoder(w).Encode(result); err != nil {
			return
		}
	}), nil
}

func (s *Service) command(ctx context.Context, command CatalogCommand) (CatalogResult, error) {
	command.CatalogScope = CatalogScope{OrganizationID: s.config.OrganizationID, ProjectID: s.config.ProjectID, ServiceID: s.config.ServiceID}
	return s.catalog.Execute(ctx, command)
}

func (s *Service) Backup(ctx context.Context, path string) (resultErr error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create artifact export: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	encoder := json.NewEncoder(file)
	after := ""
	for {
		result, err := s.command(ctx, CatalogCommand{Operation: "export", After: after, Limit: 32})
		if err != nil {
			return fmt.Errorf("export Hub catalog: %w", err)
		}
		if err := encoder.Encode(result); err != nil {
			return fmt.Errorf("write artifact export: %w", err)
		}
		if result.Next == "" {
			return nil
		}
		if result.Next <= after {
			return ErrIntegrity
		}
		after = result.Next
	}
}
