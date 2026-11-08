package cataloghub

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type PolicyResolver func(context.Context, artifact.CatalogScope) (artifact.Config, error)
type PageOwner func(context.Context, artifact.CatalogCommand) (artifact.CatalogResult, error)

type Catalog struct {
	repository *postgres.Repository
	policy     PolicyResolver
	pages      PageOwner
	now        func() time.Time
}

func New(repository *postgres.Repository, policy PolicyResolver, pages PageOwner) (*Catalog, error) {
	if repository == nil || policy == nil || pages == nil {
		return nil, artifact.ErrInvalid
	}
	return &Catalog{repository: repository, policy: policy, pages: pages, now: time.Now}, nil
}

func (c *Catalog) Execute(ctx context.Context, command artifact.CatalogCommand) (artifact.CatalogResult, error) {
	var result artifact.CatalogResult
	if command.UploadID != "" && !artifact.ValidID(command.UploadID, "artifact") {
		return result, artifact.ErrInvalid
	}
	cfg, err := c.policy(ctx, command.CatalogScope)
	if err != nil {
		return result, err
	}
	if cfg.Validate() != nil || cfg.OrganizationID != command.OrganizationID || cfg.ProjectID != command.ProjectID || cfg.ServiceID != command.ServiceID {
		return result, artifact.ErrDenied
	}
	now := c.now().UTC()
	org, project := tracker.OrganizationID(command.OrganizationID), tracker.ProjectID(command.ProjectID)
	scope := postgres.CatalogScope{OrganizationID: org, ProjectID: project, ServiceID: command.ServiceID, UploadID: command.UploadID}
	switch command.Operation {
	case "pending", "retention", "export", "prune_traffic":
		if command.Limit < 1 || command.Limit > 32 {
			return result, artifact.ErrInvalid
		}
		command.At = now
		command.Before = now.Add(-time.Duration(cfg.Policy.DeletionRecordSeconds) * time.Second)
		if command.Operation == "prune_traffic" {
			retention := cfg.Policy.Limits.TelemetrySeconds
			if retention == 0 {
				retention = 86400
			}
			command.Before = now.Add(-time.Duration(retention) * time.Second)
		}
		return c.pages(ctx, command)
	case "upload", "upload_by_key", "objects", "manifest", "usage", "traffic_settings":
		err = c.repository.ReadArtifactCatalog(ctx, org, corestore.Serving, func(r postgres.ArtifactCatalogReader) error {
			switch command.Operation {
			case "upload":
				row, err := r.CatalogUpload(ctx, postgres.CatalogUploadInput{OrganizationID: org, ProjectID: project, ServiceID: command.ServiceID, ID: command.UploadID})
				result.Upload = upload(row, cfg)
				return err
			case "upload_by_key":
				row, err := r.CatalogUploadByKey(ctx, postgres.CatalogUploadByKeyInput{OrganizationID: org, ProjectID: project, ServiceID: command.ServiceID, IdempotencyKey: command.Key})
				result.Upload = upload(row, cfg)
				return err
			case "objects":
				rows, err := r.CatalogObjects(ctx, postgres.CatalogObjectsInput{OrganizationID: org, ProjectID: project, ServiceID: command.ServiceID, UploadID: command.UploadID, Limit: artifact.MaxObjects})
				for _, row := range rows {
					result.Objects = append(result.Objects, object(row))
				}
				return err
			case "manifest":
				row, err := r.CatalogManifest(ctx, postgres.CatalogManifestInput{OrganizationID: org, ProjectID: project, ServiceID: command.ServiceID, UploadID: command.UploadID, Revision: command.Revision})
				if err != nil {
					return err
				}
				u, err := r.CatalogUpload(ctx, postgres.CatalogUploadInput{OrganizationID: org, ProjectID: project, ServiceID: command.ServiceID, ID: command.UploadID})
				result.Manifest = manifest(row, u)
				return err
			case "usage":
				row, err := r.CatalogUsage(ctx)
				if err != nil {
					return err
				}
				result.Usage = artifact.Usage{RetainedBytes: row.RetainedBytes, ReservedBytes: row.ReservedBytes}
				if cfg.Mode != "hosted" {
					return nil
				}
				settings, err := r.CatalogTrafficSettings(ctx, postgres.CatalogTrafficSettingsInput{OrganizationID: org, ProjectID: project, ServiceID: command.ServiceID})
				if err != nil {
					return err
				}
				start := time.Unix(now.Unix()/settings.WindowSeconds*settings.WindowSeconds, 0).UTC()
				result.Usage.WindowStart = start
				result.Usage.ObservedAt = now
				for at := start; at.Before(now); {
					rows, err := r.CatalogTraffic(ctx, postgres.CatalogTrafficInput{OrganizationID: org, ProjectID: project, ServiceID: command.ServiceID, Minute: at, Minute_2: now.Add(time.Minute), Limit: 200})
					if err != nil {
						return err
					}
					for _, row := range rows {
						result.Usage.RelayBytes += row.UploadBytes + row.DownloadBytes
						result.Usage.StorageRequests += row.Requests
					}
					if len(rows) < 200 {
						break
					}
					at = rows[len(rows)-1].Minute.Add(time.Minute)
				}
				return nil
			case "traffic_settings":
				row, err := r.CatalogTrafficSettings(ctx, postgres.CatalogTrafficSettingsInput{OrganizationID: org, ProjectID: project, ServiceID: command.ServiceID})
				result.Traffic = artifact.CatalogTraffic{WindowSeconds: row.WindowSeconds, RetentionSeconds: row.RetentionSeconds}
				return err
			}
			return artifact.ErrInvalid
		})
		if command.Operation == "traffic_settings" && errors.Is(err, corestore.ErrNotFound) {
			command.Operation = "set_traffic_settings"
			return c.Execute(ctx, command)
		}
	default:
		err = c.repository.WriteArtifactCatalog(ctx, org, corestore.Serving, func(w postgres.ArtifactCatalogWriter) error {
			switch command.Operation {
			case "reserve":
				requested := command.Upload.Reservation
				if requested.OrganizationID != cfg.OrganizationID || requested.ProjectID != cfg.ProjectID || requested.ServiceID != cfg.ServiceID || requested.Mode != cfg.Mode || requested.HostedOptIn != cfg.HostedOptIn || requested.Validate() != nil {
					return artifact.ErrDenied
				}
				if !artifact.ValidID(command.Upload.ArtifactID, "artifact") || requested.Key == "" || len(requested.Key) > 128 || !slices.Contains([]string{"log", "diff", "screenshot", "video"}, requested.Kind) {
					return artifact.ErrInvalid
				}
				if requested.Bytes > cfg.Policy.Limits.ArtifactBytes {
					return artifact.ErrQuota
				}
				rawHash := reservationHash(requested)
				row, err := w.ReserveCatalogUpload(ctx, postgres.CatalogReservation{Upload: postgres.CatalogUpload{OrganizationID: org, ProjectID: project, ServiceID: cfg.ServiceID, ID: command.Upload.ArtifactID, IdempotencyKey: requested.Key, RequestHash: rawHash, WorkItemID: requested.WorkItemID, RunID: requested.RunID, AttemptID: requested.AttemptID, VersionID: requested.VersionID, Kind: requested.Kind, LeaseID: requested.LeaseID, FencingToken: requested.FencingToken, RequestedBytes: requested.Bytes, PartLimit: cfg.Policy.Limits.UploadBytes, CreatedAt: now, UploadDeadline: now.Add(time.Duration(cfg.Policy.AbandonedUploadSeconds) * time.Second), ExpiresAt: now.Add(time.Duration(cfg.Policy.Limits.RetentionSeconds) * time.Second)}, RetainedLimit: cfg.Policy.Limits.RetainedBytes, ReservedLimit: cfg.Policy.Limits.ReservedBytes})
				result.Upload = upload(row, cfg)
				return err
			case "put_object", "verify_object", "discard_object":
				o := command.Object
				if o.Sequence < 0 || o.Sequence >= artifact.MaxObjects {
					return artifact.ErrInvalid
				}
				row := postgres.CatalogObject{OrganizationID: org, ProjectID: project, ServiceID: scope.ServiceID, UploadID: scope.UploadID, Sequence: int32(o.Sequence), ID: o.ID, MediaType: o.MediaType, Size: o.Size, Sha256: o.SHA256, Path: o.Path, Side: o.Side, ByteOffset: o.Offset, StorageVersion: o.StorageVersion}
				var err error
				switch command.Operation {
				case "put_object":
					row, err = w.PutCatalogObject(ctx, row, now)
				case "verify_object":
					row, err = w.VerifyCatalogObject(ctx, row, now)
				case "discard_object":
					row, err = w.DiscardCatalogObject(ctx, row, now, command.Receipt)
				}
				result.Object = object(row)
				return err
			case "commit":
				m := command.Manifest
				if m.Objects < 0 || m.Objects > artifact.MaxObjects || m.BodyKey != cfg.OrganizationID+"/"+cfg.ProjectID+"/"+cfg.ServiceID+"/"+command.UploadID+"/manifests/"+strconv.FormatInt(m.Revision, 10) {
					return artifact.ErrInvalid
				}
				if m.OrganizationID != cfg.OrganizationID || m.ProjectID != cfg.ProjectID || m.ServiceID != cfg.ServiceID || m.ArtifactID != command.UploadID {
					return artifact.ErrDenied
				}
				row := postgres.CatalogManifest{OrganizationID: org, ProjectID: project, ServiceID: cfg.ServiceID, UploadID: scope.UploadID, Revision: m.Revision, ID: m.ManifestID, State: m.State, Sha256: m.SHA256, BodyObjectKey: m.BodyKey, BodyBytes: m.BodyBytes, TotalBytes: m.Bytes, Objects: int32(m.Objects), RetentionPolicyID: cfg.Policy.ID, CreatedAt: m.CreatedAt, ExpiresAt: m.ExpiresAt, ObservedAt: now}
				if m.Capture != nil {
					v := m.Capture
					row.CaptureBase = v.Base
					row.CaptureHead = v.Head
					row.CaptureMergeBase = v.MergeBase
					row.CaptureContextLines = int32(v.ContextLines)
					row.CaptureFileContext = v.FileContext
					row.CaptureWorkingTree = v.WorkingTree
				}
				stored, err := w.CommitCatalogManifest(ctx, row)
				if err != nil {
					return err
				}
				u, err := w.CatalogUpload(ctx, postgres.CatalogUploadInput{OrganizationID: org, ProjectID: project, ServiceID: cfg.ServiceID, ID: scope.UploadID})
				result.Manifest = manifest(stored, u)
				return err
			case "publication":
				_, err := w.RecordCatalogPublication(ctx, postgres.CatalogPublicationResult{OrganizationID: org, ProjectID: project, ServiceID: scope.ServiceID, ManifestID: command.Manifest.ManifestID, Outcome: command.Outcome, ExternalReceipt: command.Receipt, UpdatedAt: now})
				return err
			case "begin_delete":
				row, err := w.BeginCatalogDeletion(ctx, scope)
				result.Upload = upload(row, cfg)
				return err
			case "finish_delete":
				row, err := w.FinishCatalogDeletion(ctx, postgres.CatalogDeletion{CatalogScope: scope, At: now, ReceiptObjectKey: command.Receipt})
				result.Upload = upload(row, cfg)
				return err
			case "prune":
				return w.PruneCatalogUpload(ctx, scope, now.Add(-time.Duration(cfg.Policy.DeletionRecordSeconds)*time.Second))
			case "traffic":
				v := command.Traffic
				return w.RecordCatalogTraffic(ctx, postgres.CatalogTrafficReport{OrganizationID: org, ProjectID: project, ServiceID: scope.ServiceID, ID: v.ID, Minute: now.Truncate(time.Minute), UploadBytes: v.UploadBytes, DownloadBytes: v.DownloadBytes, Requests: v.Requests})
			case "set_traffic_settings":
				window, retention := cfg.Policy.Limits.WindowSeconds, cfg.Policy.Limits.TelemetrySeconds
				if window == 0 {
					window = 3600
				}
				if retention == 0 {
					retention = 86400
				}
				row, err := w.PutCatalogTrafficSettings(ctx, postgres.CatalogTrafficPolicy{OrganizationID: org, ProjectID: project, ServiceID: scope.ServiceID, WindowSeconds: window, RetentionSeconds: retention})
				result.Traffic = artifact.CatalogTraffic{WindowSeconds: row.WindowSeconds, RetentionSeconds: row.RetentionSeconds}
				return err
			}
			return artifact.ErrInvalid
		})
	}
	if errors.Is(err, corestore.ErrNotFound) {
		err = artifact.ErrMissing
	}
	if errors.Is(err, corestore.ErrRevisionConflict) {
		err = artifact.ErrConflict
	}
	if errors.Is(err, corestore.ErrTenantMismatch) {
		err = artifact.ErrDenied
	}
	return result, err
}

func upload(row postgres.CatalogUpload, cfg artifact.Config) artifact.Upload {
	return artifact.Upload{Reservation: artifact.Reservation{Scope: artifact.Scope{OrganizationID: string(row.OrganizationID), ProjectID: string(row.ProjectID), WorkItemID: row.WorkItemID, RunID: row.RunID, AttemptID: row.AttemptID, VersionID: row.VersionID}, ServiceID: row.ServiceID, Mode: cfg.Mode, HostedOptIn: cfg.HostedOptIn, Key: row.IdempotencyKey, Kind: row.Kind, Bytes: row.RequestedBytes, LeaseID: row.LeaseID, FencingToken: row.FencingToken}, ArtifactID: row.ID, PartLimit: row.PartLimit, State: row.State, ReservedBytes: row.ReservedBytes, RetainedBytes: row.RetainedBytes, ManifestRevision: row.ManifestRevision, CreatedAt: row.CreatedAt, Deadline: row.UploadDeadline, ExpiresAt: row.ExpiresAt, DeletedAt: row.DeletedAt}
}

func object(row postgres.CatalogObject) artifact.CatalogObject {
	return artifact.CatalogObject{UploadID: row.UploadID, Object: artifact.Object{ID: row.ID, MediaType: row.MediaType, Size: row.Size, SHA256: row.Sha256, Path: row.Path, Side: row.Side, Sequence: int(row.Sequence), Offset: row.ByteOffset}, StorageVersion: row.StorageVersion, Verified: row.VerifiedAt != nil, Discarded: row.DiscardedAt != nil}
}

func manifest(row postgres.CatalogManifest, u postgres.CatalogUpload) artifact.CatalogManifest {
	result := artifact.CatalogManifest{Reference: artifact.Reference{SchemaVersion: 1, Scope: artifact.Scope{OrganizationID: string(row.OrganizationID), ProjectID: string(row.ProjectID), WorkItemID: u.WorkItemID, RunID: u.RunID, AttemptID: u.AttemptID, VersionID: u.VersionID}, ServiceID: row.ServiceID, ArtifactID: row.UploadID, ManifestID: row.ID, Revision: row.Revision, SHA256: row.Sha256, Kind: u.Kind, State: row.State, Availability: "available", Bytes: row.TotalBytes, Objects: int(row.Objects), ExpiresAt: row.ExpiresAt, ObservedAt: row.ObservedAt}, BodyKey: row.BodyObjectKey, BodyBytes: row.BodyBytes, CreatedAt: row.CreatedAt, RetentionPolicyID: row.RetentionPolicyID}
	if row.CaptureBase != "" {
		result.Capture = &artifact.Capture{Base: row.CaptureBase, Head: row.CaptureHead, MergeBase: row.CaptureMergeBase, ContextLines: int(row.CaptureContextLines), FileContext: row.CaptureFileContext, WorkingTree: row.CaptureWorkingTree}
	}
	return result
}

func reservationHash(r artifact.Reservation) string {
	raw, err := json.Marshal(r)
	if err != nil {
		return ""
	}
	return artifact.Digest(raw)
}
