package cataloghub

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type PolicyResolver func(context.Context, postgres.ArtifactCatalogReader, artifact.CatalogScope) (artifact.Config, error)
type TransactionAuthorization func(context.Context, postgres.ArtifactCatalogReader, artifact.CatalogCommand, bool) error

type Options struct {
	Authorize TransactionAuthorization
	Now       func() time.Time
}

type Catalog struct {
	repository *postgres.Repository
	policy     PolicyResolver
	authorize  TransactionAuthorization
	now        func() time.Time
}

func New(repository *postgres.Repository, policy PolicyResolver, options ...Options) (*Catalog, error) {
	if repository == nil || policy == nil || len(options) > 1 {
		return nil, artifact.ErrInvalid
	}
	catalog := &Catalog{repository: repository, policy: policy, now: time.Now}
	if len(options) == 1 {
		catalog.authorize = options[0].Authorize
		if options[0].Now != nil {
			catalog.now = options[0].Now
		}
	}
	return catalog, nil
}

func (c *Catalog) Execute(ctx context.Context, command artifact.CatalogCommand) (artifact.CatalogResult, error) {
	var result artifact.CatalogResult
	if command.UploadID != "" && !artifact.ValidID(command.UploadID, "artifact") {
		return result, artifact.ErrInvalid
	}
	if command.ProjectID == "" && command.UploadID != "" {
		var rows []postgres.CatalogUpload
		err := c.read(ctx, command, func(r postgres.ArtifactCatalogReader) error {
			var err error
			rows, err = r.CatalogServiceUploads(ctx, postgres.CatalogServiceUploadsInput{OrganizationID: tracker.OrganizationID(command.OrganizationID), ServiceID: command.ServiceID, ID: command.UploadID})
			return err
		})
		if err != nil {
			return result, err
		}
		if len(rows) == 0 {
			return result, artifact.ErrMissing
		}
		if len(rows) != 1 {
			return result, artifact.ErrConflict
		}
		command.ProjectID = string(rows[0].ProjectID)
	}
	var cfg artifact.Config
	err := c.read(ctx, command, func(r postgres.ArtifactCatalogReader) error {
		var err error
		cfg, err = c.policy(ctx, r, command.CatalogScope)
		return err
	})
	if err != nil {
		return result, err
	}
	if cfg.Validate() != nil || cfg.OrganizationID != command.OrganizationID || cfg.ProjectID != command.ProjectID || cfg.ServiceID != command.ServiceID {
		return result, artifact.ErrDenied
	}
	if command.ProjectID == "" && command.Operation == "traffic" {
		var binding postgres.CatalogBinding
		err := c.read(ctx, command, func(r postgres.ArtifactCatalogReader) error {
			var err error
			binding, err = r.CatalogServiceBinding(ctx, postgres.CatalogServiceBindingInput{OrganizationID: tracker.OrganizationID(command.OrganizationID), ID: command.ServiceID, PublisherTokenID: cfg.PublisherID})
			return err
		})
		if err != nil {
			return result, err
		}
		command.ProjectID = string(binding.ProjectID)
		return c.Execute(ctx, command)
	}
	now := c.now().UTC()
	org, project := tracker.OrganizationID(command.OrganizationID), tracker.ProjectID(command.ProjectID)
	scope := postgres.CatalogScope{OrganizationID: org, ProjectID: project, ServiceID: command.ServiceID, UploadID: command.UploadID}
	if command.ProjectID == "" && (command.Operation == "traffic_settings" || command.Operation == "set_traffic_settings") {
		window, retention := cfg.Policy.Limits.WindowSeconds, cfg.Policy.Limits.TelemetrySeconds
		if window == 0 {
			window = 3600
		}
		if retention == 0 {
			retention = 86400
		}
		result.Traffic = artifact.CatalogTraffic{WindowSeconds: window, RetentionSeconds: retention}
		err := c.read(ctx, command, func(postgres.ArtifactCatalogReader) error { return nil })
		return result, err
	}
	switch command.Operation {
	case "pending", "retention", "export", "prune_traffic":
		if command.Limit < 1 || command.Limit > 32 {
			return result, artifact.ErrInvalid
		}
		command.At = now
		command.Before = now
		if command.Operation == "prune_traffic" {
			retention := cfg.Policy.Limits.TelemetrySeconds
			if retention == 0 {
				retention = 86400
			}
			command.Before = now.Add(-time.Duration(retention) * time.Second)
		}
		return c.page(ctx, command, cfg)
	case "upload", "upload_by_key", "objects", "manifest", "usage", "traffic_settings", "manifest_intents":
		err = c.read(ctx, command, func(r postgres.ArtifactCatalogReader) error {
			switch command.Operation {
			case "manifest_intents":
				if command.Limit < 1 || command.Limit > 32 {
					return artifact.ErrInvalid
				}
				rows, err := r.CatalogManifestIntents(ctx, postgres.CatalogManifestIntentsInput{OrganizationID: org, ProjectID: project, ServiceID: scope.ServiceID, UploadID: scope.UploadID, ID: command.After, Limit: int32(command.Limit), PendingOnly: command.Outcome == "pending"})
				for _, row := range rows {
					result.Intents = append(result.Intents, intent(row))
				}
				if len(rows) == command.Limit {
					result.Next = rows[len(rows)-1].ID
				}
				return err

			case "upload":
				row, err := r.CatalogUpload(ctx, postgres.CatalogUploadInput{OrganizationID: org, ProjectID: project, ServiceID: command.ServiceID, ID: command.UploadID})
				result.Upload = upload(row, cfg)
				if err == nil && reservationHash(result.Upload.Reservation) != row.RequestHash {
					return artifact.ErrIntegrity
				}
				return err
			case "upload_by_key":
				row, err := r.CatalogUploadByKey(ctx, postgres.CatalogUploadByKeyInput{OrganizationID: org, ProjectID: project, ServiceID: command.ServiceID, IdempotencyKey: command.Key})
				result.Upload = upload(row, cfg)
				if err == nil && reservationHash(result.Upload.Reservation) != row.RequestHash {
					return artifact.ErrIntegrity
				}
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
				row, err := r.CatalogServiceUsage(ctx, postgres.CatalogServiceUsageInput{OrganizationID: org, ServiceID: scope.ServiceID})
				if err != nil {
					return err
				}
				result.Usage = artifact.Usage{RetainedBytes: row.RetainedBytes, ReservedBytes: row.ReservedBytes}
				if cfg.Mode != "hosted" {
					return nil
				}
				window := cfg.Policy.Limits.WindowSeconds
				if window == 0 {
					window = 3600
				}
				start := time.Unix(now.Unix()/window*window, 0).UTC()
				result.Usage.WindowStart = start
				result.Usage.ObservedAt = now
				traffic, err := r.CatalogServiceTraffic(ctx, postgres.CatalogServiceTrafficInput{OrganizationID: org, ServiceID: scope.ServiceID, Minute: start, Minute_2: now.Add(time.Minute)})
				result.Usage.RelayBytes = traffic.RelayBytes
				result.Usage.StorageRequests = traffic.Requests
				return err
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
		err = c.write(ctx, command, func(w postgres.ArtifactCatalogWriter) error {
			current, err := c.policy(ctx, w, command.CatalogScope)
			if err != nil {
				return err
			}
			if current.Validate() != nil || current.OrganizationID != command.OrganizationID || current.ProjectID != command.ProjectID || current.ServiceID != command.ServiceID {
				return artifact.ErrDenied
			}
			cfg = current
			storagePrefix := cfg.OrganizationID + "/" + cfg.ProjectID + "/" + cfg.ServiceID + "/" + scope.UploadID + "/"

			switch command.Operation {
			case "begin_object_write", "finish_object_write":
				v := command.Object
				if !artifact.ValidID(v.ID, "object") || !artifact.ValidID(v.WriteID, "write") {
					return artifact.ErrInvalid
				}
				arg := postgres.CatalogWrite{CatalogScope: scope, ID: v.ID, Sequence: int32(v.Sequence), WriteID: v.WriteID, Outcome: command.Outcome, StorageVersion: v.StorageVersion, At: now}
				var row postgres.CatalogObject
				var err error
				if command.Operation == "begin_object_write" {
					row, err = w.BeginCatalogObjectWrite(ctx, arg)
				} else {
					row, err = w.FinishCatalogObjectWrite(ctx, arg)
				}
				result.Object = object(row)
				return err
			case "begin_manifest_write", "finish_manifest_write":
				v := command.Intent
				if !artifact.ValidID(v.ID, "intent") || !artifact.ValidID(v.WriteID, "write") {
					return artifact.ErrInvalid
				}
				arg := postgres.CatalogWrite{CatalogScope: scope, ID: v.ID, WriteID: v.WriteID, Outcome: command.Outcome, StorageVersion: v.StorageVersion, At: now}
				var row postgres.CatalogManifestIntent
				var err error
				if command.Operation == "begin_manifest_write" {
					row, err = w.BeginCatalogManifestWrite(ctx, arg)
				} else {
					row, err = w.FinishCatalogManifestWrite(ctx, arg)
				}
				result.Intent = intent(row)
				return err
			case "prepare_manifest":
				v := command.Intent
				if !artifact.ValidID(v.ID, "intent") {
					return artifact.ErrInvalid
				}
				key := storagePrefix + "manifests/" + strconv.FormatInt(v.Revision, 10) + "/" + v.SHA256 + "/" + v.ID
				row, err := w.PrepareCatalogManifestIntent(ctx, postgres.CatalogManifestIntent{OrganizationID: org, ProjectID: project, ServiceID: scope.ServiceID, UploadID: scope.UploadID, ID: v.ID, Revision: v.Revision, BodyObjectKey: key, Sha256: v.SHA256, BodyBytes: v.Bytes, ManifestState: v.ManifestState}, now)
				result.Intent = intent(row)
				return err
			case "verify_manifest", "begin_manifest_discard", "finish_manifest_discard":
				if !artifact.ValidID(command.Intent.ID, "intent") || command.Operation == "finish_manifest_discard" && command.Intent.Receipt != storagePrefix+"discarded/"+command.Intent.ID {
					return artifact.ErrInvalid
				}
				state := "verified"
				if command.Operation == "begin_manifest_discard" {
					state = "discarding"
				}
				if command.Operation == "finish_manifest_discard" {
					state = "discarded"
				}
				row, err := w.TransitionCatalogManifestIntent(ctx, postgres.CatalogIntentTransition{CatalogScope: scope, ID: command.Intent.ID, State: state, StorageVersion: command.Intent.StorageVersion, ReceiptObjectKey: command.Intent.Receipt})
				result.Intent = intent(row)
				return err

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
				deadline := now.Add(time.Duration(cfg.Policy.AbandonedUploadSeconds) * time.Second)
				expires := now.Add(time.Duration(cfg.Policy.Limits.RetentionSeconds) * time.Second)
				if !command.Upload.Deadline.IsZero() && command.Upload.Deadline.Before(deadline) {
					deadline = command.Upload.Deadline
				}
				if !command.Upload.ExpiresAt.IsZero() && command.Upload.ExpiresAt.Before(expires) {
					expires = command.Upload.ExpiresAt
				}
				if !deadline.After(now) || !expires.After(now) {
					return artifact.ErrExpired
				}
				partLimit := cfg.Policy.Limits.UploadBytes
				if command.Upload.PartLimit > 0 {
					partLimit = min(partLimit, command.Upload.PartLimit)
				}
				window := cfg.Policy.Limits.WindowSeconds
				if window == 0 {
					window = 3600
				}

				orgRetained, orgReserved := cfg.OrganizationRetainedLimit, cfg.OrganizationReservedLimit
				if orgRetained == 0 {
					orgRetained = cfg.Policy.Limits.RetainedBytes
				}
				if orgReserved == 0 {
					orgReserved = cfg.Policy.Limits.ReservedBytes
				}
				row, err := w.ReserveCatalogUpload(ctx, postgres.CatalogReservation{Upload: postgres.CatalogUpload{BindingMode: cfg.Mode, HostedOptIn: cfg.HostedOptIn, OrganizationID: org, ProjectID: project, ServiceID: cfg.ServiceID, ID: command.Upload.ArtifactID, IdempotencyKey: requested.Key, RequestHash: rawHash, WorkItemID: requested.WorkItemID, RunID: requested.RunID, AttemptID: requested.AttemptID, VersionID: requested.VersionID, Kind: requested.Kind, LeaseID: requested.LeaseID, FencingToken: requested.FencingToken, RequestedBytes: requested.Bytes, PartLimit: partLimit, CreatedAt: now, UploadDeadline: deadline, ExpiresAt: expires}, RetainedLimit: orgRetained, ReservedLimit: orgReserved, ServiceRetainedLimit: cfg.Policy.Limits.RetainedBytes, ServiceReservedLimit: cfg.Policy.Limits.ReservedBytes, EnforceRelay: cfg.Mode == "hosted", RelayLimit: cfg.Policy.Limits.RelayBytes, RelayWindowStart: time.Unix(now.Unix()/window*window, 0).UTC(), At: now})
				result.Upload = upload(row, cfg)
				return err
			case "put_object", "verify_object", "discard_object":
				o := command.Object
				if !artifact.ValidID(o.ID, "object") || o.Sequence < 0 || o.Sequence >= artifact.MaxObjects || command.Operation == "discard_object" && command.Receipt != storagePrefix+"discarded/"+o.ID {
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
				if !artifact.ValidID(m.ManifestID, "manifest") || m.Objects < 0 || m.Objects > artifact.MaxObjects || !strings.HasPrefix(m.BodyKey, storagePrefix+"manifests/"+strconv.FormatInt(m.Revision, 10)+"/"+m.SHA256+"/") {
					return artifact.ErrInvalid
				}
				if m.OrganizationID != cfg.OrganizationID || m.ProjectID != cfg.ProjectID || m.ServiceID != cfg.ServiceID || m.ArtifactID != command.UploadID {
					return artifact.ErrDenied
				}
				row := postgres.CatalogManifest{OrganizationID: org, ProjectID: project, ServiceID: cfg.ServiceID, UploadID: scope.UploadID, Revision: m.Revision, ID: m.ManifestID, State: m.State, Sha256: m.SHA256, BodyObjectKey: m.BodyKey, BodyBytes: m.BodyBytes, TotalBytes: m.Bytes, Objects: int32(m.Objects), RetentionPolicyID: m.RetentionPolicyID, CreatedAt: m.CreatedAt, ExpiresAt: m.ExpiresAt, ObservedAt: now}
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
				if command.Receipt != storagePrefix+"deleted" {
					return artifact.ErrInvalid
				}
				row, err := w.FinishCatalogDeletion(ctx, postgres.CatalogDeletion{CatalogScope: scope, At: now, ReceiptObjectKey: command.Receipt})
				result.Upload = upload(row, cfg)
				return err
			case "prune":
				return w.PruneCatalogUpload(ctx, scope, now.Add(-time.Duration(cfg.Policy.DeletionRecordSeconds)*time.Second))
			case "traffic":
				v := command.Traffic
				return w.RecordCatalogTrafficNow(ctx, postgres.CatalogTrafficReport{OrganizationID: org, ProjectID: project, ServiceID: scope.ServiceID, ID: v.ID, Minute: now.Truncate(time.Minute), UploadBytes: v.UploadBytes, DownloadBytes: v.DownloadBytes, Requests: v.Requests}, now)
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
	return artifact.Upload{Reservation: artifact.Reservation{Scope: artifact.Scope{OrganizationID: string(row.OrganizationID), ProjectID: string(row.ProjectID), WorkItemID: row.WorkItemID, RunID: row.RunID, AttemptID: row.AttemptID, VersionID: row.VersionID}, ServiceID: row.ServiceID, Mode: row.BindingMode, HostedOptIn: row.HostedOptIn, Key: row.IdempotencyKey, Kind: row.Kind, Bytes: row.RequestedBytes, LeaseID: row.LeaseID, FencingToken: row.FencingToken}, ArtifactID: row.ID, PartLimit: row.PartLimit, State: row.State, ReservedBytes: row.ReservedBytes, RetainedBytes: row.RetainedBytes, ManifestRevision: row.ManifestRevision, CreatedAt: row.CreatedAt, Deadline: row.UploadDeadline, ExpiresAt: row.ExpiresAt, DeletedAt: row.DeletedAt}
}

func object(row postgres.CatalogObject) artifact.CatalogObject {
	return artifact.CatalogObject{WriteState: row.WriteState, WriteID: row.WriteID, ProjectID: string(row.ProjectID), UploadID: row.UploadID, Object: artifact.Object{ID: row.ID, MediaType: row.MediaType, Size: row.Size, SHA256: row.Sha256, Path: row.Path, Side: row.Side, Sequence: int(row.Sequence), Offset: row.ByteOffset}, StorageVersion: row.StorageVersion, Verified: row.VerifiedAt != nil, Discarded: row.DiscardedAt != nil}
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

func intent(row postgres.CatalogManifestIntent) artifact.CatalogIntent {
	return artifact.CatalogIntent{WriteState: row.WriteState, WriteID: row.WriteID, ManifestState: row.ManifestState, UploadID: row.UploadID, ProjectID: string(row.ProjectID), ID: row.ID, Revision: row.Revision, BodyKey: row.BodyObjectKey, SHA256: row.Sha256, Bytes: row.BodyBytes, State: row.State, StorageVersion: row.StorageVersion, Receipt: row.ReceiptObjectKey}
}

func (c *Catalog) read(ctx context.Context, command artifact.CatalogCommand, run func(postgres.ArtifactCatalogReader) error) error {
	return c.repository.ReadArtifactCatalog(ctx, tracker.OrganizationID(command.OrganizationID), corestore.Serving, func(r postgres.ArtifactCatalogReader) error {
		if c.authorize != nil {
			if err := c.authorize(ctx, r, command, false); err != nil {
				return err
			}
		}
		return run(r)
	})
}

func (c *Catalog) write(ctx context.Context, command artifact.CatalogCommand, run func(postgres.ArtifactCatalogWriter) error) error {
	return c.repository.WriteArtifactCatalog(ctx, tracker.OrganizationID(command.OrganizationID), corestore.Serving, func(w postgres.ArtifactCatalogWriter) error {
		if c.authorize != nil {
			if err := c.authorize(ctx, w, command, true); err != nil {
				return err
			}
		}
		return run(w)
	})
}
