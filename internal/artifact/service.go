package artifact

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

type Limits struct {
	RelayBytes       int64 `json:"relay_bytes,omitempty"`
	WindowSeconds    int64 `json:"window_seconds,omitempty"`
	TelemetrySeconds int64 `json:"telemetry_seconds,omitempty"`
	RetainedBytes    int64 `json:"retained_bytes"`
	ReservedBytes    int64 `json:"reserved_bytes"`
	ArtifactBytes    int64 `json:"artifact_bytes"`
	UploadBytes      int64 `json:"upload_bytes"`
	RetentionSeconds int64 `json:"retention_seconds"`
}

type Allowances interface {
	Limits(context.Context, string) (Limits, error)
}

type Policy struct {
	ID                     string `json:"id"`
	Limits                 Limits `json:"limits"`
	AbandonedUploadSeconds int64  `json:"abandoned_upload_seconds"`
	DeletionRecordSeconds  int64  `json:"deletion_record_seconds"`
	BackupSeconds          int64  `json:"backup_seconds"`
}

type Config struct {
	ServiceID                 string        `json:"service_id"`
	Mode                      string        `json:"mode"`
	HostedOptIn               bool          `json:"hosted_opt_in"`
	OrganizationID            string        `json:"organization_id"`
	ProjectID                 string        `json:"project_id,omitempty"`
	PublisherID               string        `json:"-"`
	OrganizationRetainedLimit int64         `json:"-"`
	OrganizationReservedLimit int64         `json:"-"`
	Catalog                   Catalog       `json:"-"`
	DatabasePath              string        `json:"database_path"`
	Policy                    Policy        `json:"policy"`
	Storage                   StorageConfig `json:"storage"`
	HubOrigin                 string        `json:"hub_origin"`
	PublishTokenEnv           string        `json:"publish_token_env"`
	AllowedOrigins            []string      `json:"allowed_origins"`
}

type Reservation struct {
	Scope
	ServiceID    string `json:"service_id"`
	Mode         string `json:"mode"`
	HostedOptIn  bool   `json:"hosted_opt_in"`
	Key          string `json:"idempotency_key"`
	Kind         string `json:"kind"`
	Bytes        int64  `json:"bytes"`
	LeaseID      string `json:"lease_id"`
	FencingToken int64  `json:"fencing_token"`
}

type Upload struct {
	PartLimit  int64  `json:"part_limit"`
	ArtifactID string `json:"artifact_id"`
	Reservation
	State            string     `json:"state"`
	CreatedAt        time.Time  `json:"created_at"`
	Deadline         time.Time  `json:"deadline"`
	ExpiresAt        time.Time  `json:"expires_at"`
	RetainedBytes    int64      `json:"retained_bytes"`
	ReservedBytes    int64      `json:"reserved_bytes"`
	ManifestRevision int64      `json:"manifest_revision"`
	DeletedAt        *time.Time `json:"deleted_at,omitempty"`
}

type Part struct {
	Sequence  int    `json:"sequence"`
	MediaType string `json:"media_type"`
	Path      string `json:"path,omitempty"`
	Side      string `json:"side,omitempty"`
	SHA256    string `json:"sha256"`
	Data      []byte `json:"data"`
}

type Usage struct {
	WindowStart     time.Time `json:"window_start,omitzero"`
	ObservedAt      time.Time `json:"observed_at,omitzero"`
	RelayBytes      int64     `json:"relay_bytes,omitempty"`
	StorageRequests int64     `json:"storage_requests,omitempty"`
	RetainedBytes   int64     `json:"retained_bytes"`
	ReservedBytes   int64     `json:"reserved_bytes"`
}

type Service struct {
	trafficWindow    atomic.Int64
	trafficRetention atomic.Int64
	config           Config
	storage          Storage
	catalog          Catalog
	allowances       Allowances
	now              func() time.Time
	maintainAfter    string
	publishAfter     publishCursor
	publishMu        sync.Mutex
}

func (cfg Config) Validate() error {
	if cfg.ProjectID != "" && !ValidID(cfg.ProjectID, "prj") || !ValidID(cfg.ServiceID, "service") || !ValidID(cfg.OrganizationID, "org") || !slices.Contains([]string{"customer", "hosted"}, cfg.Mode) || cfg.Mode == "hosted" && !cfg.HostedOptIn {
		return ErrInvalid
	}
	return cfg.Policy.Validate()
}

func (p Policy) Validate() error {
	if p.ID == "" || len(p.ID) > 128 || !validLimits(p.Limits) || p.AbandonedUploadSeconds <= 0 || p.AbandonedUploadSeconds > 365*86400 || p.BackupSeconds <= 0 || p.BackupSeconds > 365*86400 || p.DeletionRecordSeconds <= p.BackupSeconds+p.AbandonedUploadSeconds || p.DeletionRecordSeconds > 10*365*86400 {
		return ErrInvalid
	}
	return nil
}

func validLimits(l Limits) bool {
	return l.RetainedBytes > 0 && l.RetainedBytes <= 1<<60 && l.ReservedBytes > 0 && l.ReservedBytes <= 1<<60 && l.ArtifactBytes > 0 && l.ArtifactBytes <= MaxArtifactBytes && l.UploadBytes > 0 && l.UploadBytes <= MaxVideoBytes && l.RetentionSeconds > 0 && l.RetentionSeconds <= 10*365*86400
}

func NewService(ctx context.Context, cfg Config, storage Storage, allowances Allowances) (*Service, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if storage == nil {
		return nil, ErrUnsupported
	}
	if cfg.Mode == "hosted" && allowances == nil {
		return nil, ErrQuota
	}
	catalog := cfg.Catalog
	if catalog == nil {
		return nil, ErrAuthorization
	}
	service := &Service{config: cfg, storage: storage, catalog: catalog, allowances: allowances, now: time.Now}
	settings, err := service.command(ctx, CatalogCommand{Operation: "traffic_settings"})
	if err != nil {
		return nil, err
	}
	service.trafficWindow.Store(settings.Traffic.WindowSeconds)
	service.trafficRetention.Store(settings.Traffic.RetentionSeconds)
	if service.trafficWindow.Load() < 60 || service.trafficRetention.Load() < service.trafficWindow.Load() {
		return nil, ErrIntegrity
	}
	if cfg.Mode == "hosted" {
		service.storage = hostedStorage{service: service, storage: storage}
	}
	return service, nil
}

func (s *Service) Close() error { return nil }

func (s *Service) limits(ctx context.Context) (Limits, error) {
	l := s.config.Policy.Limits
	if s.config.Mode != "hosted" {
		return l, nil
	}
	current, err := s.allowances.Limits(ctx, s.config.OrganizationID)
	if err != nil || !validLimits(current) {
		return Limits{}, ErrQuota
	}
	if err := s.configureTraffic(ctx, current); err != nil {
		return Limits{}, err
	}
	l.RelayBytes = current.RelayBytes
	if s.config.Policy.Limits.RelayBytes > 0 {
		l.RelayBytes = min(l.RelayBytes, s.config.Policy.Limits.RelayBytes)
	}
	l.RetainedBytes = min(l.RetainedBytes, current.RetainedBytes)
	l.ReservedBytes = min(l.ReservedBytes, current.ReservedBytes)
	l.ArtifactBytes = min(l.ArtifactBytes, current.ArtifactBytes)
	l.UploadBytes = min(l.UploadBytes, current.UploadBytes)
	l.RetentionSeconds = min(l.RetentionSeconds, current.RetentionSeconds)
	return l, nil
}

func (s *Service) Usage(ctx context.Context) (Usage, error) {
	now := s.now().UTC()
	window := s.trafficWindow.Load()
	result, err := s.command(ctx, CatalogCommand{Operation: "usage", At: now, Before: time.Unix(now.Unix()/window*window, 0).UTC()})
	if s.config.Mode != "hosted" {
		result.Usage.WindowStart = time.Time{}
		result.Usage.ObservedAt = time.Time{}
		result.Usage.RelayBytes = 0
		result.Usage.StorageRequests = 0
	}
	return result.Usage, err
}

func (s *Service) Reserve(ctx context.Context, r Reservation) (Upload, error) {
	if r.ServiceID != s.config.ServiceID || r.Mode != s.config.Mode || r.HostedOptIn != s.config.HostedOptIn || r.OrganizationID != s.config.OrganizationID || s.config.ProjectID != "" && r.ProjectID != s.config.ProjectID {
		return Upload{}, ErrDenied
	}
	if r.Validate() != nil || r.Key == "" || len(r.Key) > 128 || !slices.Contains([]string{"log", "diff", "screenshot", "video"}, r.Kind) || r.Bytes <= 0 || r.Bytes > MaxArtifactBytes {
		return Upload{}, ErrInvalid
	}
	existing, err := s.command(ctx, CatalogCommand{CatalogScope: CatalogScope{ProjectID: r.ProjectID}, Operation: "upload_by_key", Key: r.Key})
	if err == nil {
		if existing.Upload.Reservation != r {
			return Upload{}, ErrConflict
		}
		return existing.Upload, nil
	}
	if !errors.Is(err, ErrMissing) {
		return Upload{}, err
	}
	limits, err := s.limits(ctx)
	if err != nil {
		return Upload{}, err
	}
	usage, err := s.Usage(ctx)
	if err != nil {
		return Upload{}, err
	}
	if r.Bytes > limits.ArtifactBytes || r.Bytes > limits.ReservedBytes-usage.ReservedBytes || r.Bytes > limits.RetainedBytes-usage.RetainedBytes-usage.ReservedBytes {
		return Upload{}, ErrQuota
	}
	if s.config.Mode == "hosted" && r.Bytes > (limits.RelayBytes-usage.RelayBytes)/2-usage.ReservedBytes {
		return Upload{}, ErrQuota
	}
	now := s.now().UTC().Truncate(time.Second)
	upload := Upload{Reservation: r, ArtifactID: NewID("artifact"), PartLimit: limits.UploadBytes, State: "uploading", ReservedBytes: r.Bytes, CreatedAt: now, Deadline: now.Add(time.Duration(s.config.Policy.AbandonedUploadSeconds) * time.Second), ExpiresAt: now.Add(time.Duration(limits.RetentionSeconds) * time.Second)}
	result, err := s.command(ctx, CatalogCommand{Operation: "reserve", Upload: upload, At: now})
	return result.Upload, err
}

func (s *Service) Upload(ctx context.Context, id string) (Upload, error) {
	result, err := s.command(ctx, CatalogCommand{Operation: "upload", UploadID: id})
	if err != nil {
		return Upload{}, err
	}
	u := result.Upload
	if u.ServiceID != s.config.ServiceID || u.OrganizationID != s.config.OrganizationID || s.config.ProjectID != "" && u.ProjectID != s.config.ProjectID || u.Mode != s.config.Mode || u.HostedOptIn != s.config.HostedOptIn {
		return Upload{}, ErrDenied
	}
	return u, nil
}

func (s *Service) Append(ctx context.Context, id string, p Part) (Object, error) {
	u, err := s.Upload(ctx, id)
	if err != nil {
		return Object{}, err
	}
	if !s.now().Before(u.ExpiresAt) {
		return Object{}, ErrExpired
	}
	if u.State == "deleted" || u.State == "deletion_pending" {
		return Object{}, ErrMissing
	}
	if p.Sequence < 0 || p.Sequence >= MaxObjects || !validMedia(u.Kind, p.MediaType) || !validPath(p.Path) || int64(len(p.Data)) > min(objectLimit(u.Kind), s.config.Policy.Limits.UploadBytes) || !slices.Contains([]string{"", "base", "head", "diff"}, p.Side) {
		return Object{}, ErrInvalid
	}
	if Digest(p.Data) != p.SHA256 {
		return Object{}, ErrIntegrity
	}
	if (u.Kind == "log" || u.Kind == "diff") && !utf8.Valid(p.Data) {
		return Object{}, ErrInvalid
	}
	if err := validateMedia(p.MediaType, p.Data); err != nil {
		return Object{}, err
	}
	obj := Object{ID: NewID("object"), MediaType: p.MediaType, Size: int64(len(p.Data)), SHA256: p.SHA256, Path: p.Path, Side: p.Side, Sequence: p.Sequence}
	result, err := s.command(ctx, CatalogCommand{Operation: "objects", UploadID: id, Limit: MaxObjects})
	if err != nil {
		return Object{}, err
	}
	var pending *CatalogObject
	var total int64
	count := 0
	for _, stored := range result.Objects {
		if stored.Discarded {
			continue
		}
		total += stored.Size
		if stored.Verified {
			count++
		}
		if stored.Sequence != p.Sequence {
			continue
		}
		obj.ID, obj.Offset = stored.ID, stored.Offset
		if obj != stored.Object {
			return Object{}, ErrConflict
		}
		if stored.Verified {
			if u.Kind == "log" && u.State == "uploading" {
				latest, err := s.command(ctx, CatalogCommand{Operation: "manifest", UploadID: id, Revision: u.ManifestRevision})
				if errors.Is(err, ErrMissing) || err == nil && latest.Manifest.Objects <= p.Sequence {
					if _, err := s.seal(ctx, id, "partial", nil); err != nil {
						return Object{}, err
					}
				} else if err != nil {
					return Object{}, err
				}
			}
			return obj, nil
		}
		copy := stored
		pending = &copy
	}
	if u.State != "uploading" {
		return Object{}, ErrConflict
	}
	if !s.now().Before(u.Deadline) {
		return Object{}, ErrExpired
	}
	if int64(len(p.Data)) > u.PartLimit {
		return Object{}, ErrQuota
	}
	if pending == nil {
		if count != p.Sequence || len(result.Objects) != count {
			return Object{}, ErrConflict
		}
		obj.Offset = total
		objects := make([]Object, 0, len(result.Objects)+1)
		for _, stored := range result.Objects {
			if !stored.Discarded {
				objects = append(objects, stored.Object)
			}
		}
		descriptors, err := json.Marshal(append(objects, obj))
		if err != nil {
			return Object{}, err
		}
		headroom := int64(2048 + len(descriptors))
		if headroom > MaxManifestBytes {
			return Object{}, ErrQuota
		}
		if u.Kind == "log" {
			headroom *= 2
		}
		if obj.Size > u.Bytes-u.RetainedBytes-headroom || obj.Size > u.Bytes-total {
			return Object{}, ErrQuota
		}
		stored, err := s.command(ctx, CatalogCommand{Operation: "put_object", UploadID: id, Object: CatalogObject{Object: obj}, At: s.now().UTC()})
		if err != nil {
			return Object{}, err
		}
		obj = stored.Object.Object
	}
	key := s.scopedObjectKey(u.ProjectID, id, obj.ID)
	version, err := s.storage.Put(ctx, key, p.Data)
	if err != nil && !errors.Is(err, ErrConflict) {
		return Object{}, err
	}
	read, err := s.storage.Get(ctx, key, version, obj.Size)
	if err != nil {
		return Object{}, err
	}
	if int64(len(read)) != obj.Size || Digest(read) != obj.SHA256 {
		return Object{}, ErrIntegrity
	}
	if version == "" {
		version = "null"
	}
	if _, err := s.command(ctx, CatalogCommand{Operation: "verify_object", UploadID: id, Object: CatalogObject{Object: obj, StorageVersion: version}, At: s.now().UTC()}); err != nil {
		return Object{}, err
	}
	if u.Kind == "log" {
		if _, err := s.seal(ctx, id, "partial", nil); err != nil {
			return obj, err
		}
	}
	return obj, nil
}

func (s *Service) objects(ctx context.Context, id string) ([]Object, error) {
	result, err := s.command(ctx, CatalogCommand{Operation: "objects", UploadID: id, Limit: MaxObjects})
	if err != nil {
		return nil, err
	}
	objects := []Object{}
	for _, stored := range result.Objects {
		if stored.Verified && !stored.Discarded {
			objects = append(objects, stored.Object)
		}
	}
	return objects, nil
}

func (s *Service) Finalize(ctx context.Context, id, state string, capture *Capture) (Reference, error) {
	if state != "complete" && state != "interrupted" {
		return Reference{}, ErrInvalid
	}
	return s.seal(ctx, id, state, capture)
}

func (s *Service) seal(ctx context.Context, id, state string, capture *Capture) (Reference, error) {
	u, err := s.Upload(ctx, id)
	if err != nil {
		return Reference{}, err
	}
	if !s.now().Before(u.ExpiresAt) {
		return Reference{}, ErrExpired
	}
	if u.State != "uploading" {
		if u.State == state {
			body, ref, err := s.Manifest(ctx, id, 0)
			if err != nil {
				return Reference{}, err
			}
			var previous Manifest
			if err := json.Unmarshal(body, &previous); err != nil {
				return Reference{}, ErrIntegrity
			}
			if (capture == nil) != (previous.Capture == nil) || capture != nil && *capture != *previous.Capture {
				return Reference{}, ErrConflict
			}
			return ref, err
		}
		return Reference{}, ErrConflict
	}
	if state == "complete" && !s.now().Before(u.Deadline) {
		return Reference{}, ErrExpired
	}
	objects, err := s.objects(ctx, id)
	if err != nil {
		return Reference{}, err
	}
	result, err := s.command(ctx, CatalogCommand{Operation: "objects", UploadID: id, Limit: MaxObjects})
	if err != nil {
		return Reference{}, err
	}
	for _, stored := range result.Objects {
		if stored.Verified || stored.Discarded {
			continue
		}
		if state == "complete" {
			return Reference{}, ErrConflict
		}
		if state == "interrupted" {
			if err := s.discardPending(ctx, u); err != nil {
				return Reference{}, err
			}
			break
		}
	}
	revision := u.ManifestRevision + 1
	m := Manifest{SchemaVersion: 1, Scope: u.Scope, ArtifactID: id, ManifestID: "", Revision: revision, Kind: u.Kind, State: state, CreatedAt: u.CreatedAt, ExpiresAt: u.ExpiresAt, RetentionPolicyID: s.config.Policy.ID, Objects: objects, Capture: capture}
	for _, obj := range objects {
		m.TotalBytes += obj.Size
	}
	identity, err := json.Marshal(struct {
		ServiceID string
		Manifest
	}{s.config.ServiceID, m})
	if err != nil {
		return Reference{}, err
	}
	m.ManifestID = "manifest_" + Digest(identity)[:32]

	if err := m.Validate(); err != nil {
		return Reference{}, err
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return Reference{}, err
	}
	if int64(len(raw)) > u.Bytes-u.RetainedBytes {
		return Reference{}, ErrQuota
	}
	ref := Reference{SchemaVersion: 1, Scope: m.Scope, ServiceID: s.config.ServiceID, ArtifactID: id, ManifestID: m.ManifestID, Revision: revision, SHA256: Digest(raw), Kind: m.Kind, State: state, Availability: "available", Bytes: m.TotalBytes, Objects: len(objects), ExpiresAt: m.ExpiresAt, ObservedAt: s.now().UTC()}
	if err := s.cleanManifestIntents(ctx, u, revision, ref.SHA256); err != nil {
		return Reference{}, err
	}
	prepared, err := s.command(ctx, CatalogCommand{Operation: "prepare_manifest", UploadID: id, Intent: CatalogIntent{ManifestState: state, ID: NewID("intent"), Revision: revision, SHA256: ref.SHA256, Bytes: int64(len(raw))}})
	if err != nil {
		return Reference{}, err
	}
	intent := prepared.Intent
	if intent.State != "pending" {
		return Reference{}, ErrConflict
	}
	key := intent.BodyKey
	version, err := s.storage.Put(ctx, key, raw)
	if err != nil && !errors.Is(err, ErrConflict) {
		return Reference{}, err
	}
	read, err := s.storage.Get(ctx, key, version, MaxManifestBytes)
	if err != nil {
		return Reference{}, err
	}
	if Digest(read) != ref.SHA256 {
		return Reference{}, ErrConflict
	}
	if version == "" {
		version = "null"
	}
	intent.StorageVersion = version
	if _, err := s.command(ctx, CatalogCommand{Operation: "verify_manifest", UploadID: id, Intent: intent}); err != nil {
		return Reference{}, err
	}
	committed, err := s.command(ctx, CatalogCommand{Operation: "commit", UploadID: id, Manifest: CatalogManifest{Reference: ref, BodyKey: key, BodyBytes: int64(len(raw)), CreatedAt: u.CreatedAt, RetentionPolicyID: m.RetentionPolicyID, Capture: capture}, At: s.now().UTC()})
	return committed.Manifest.Reference, err
}

func (s *Service) Manifest(ctx context.Context, id string, revision int64) ([]byte, Reference, error) {
	u, err := s.Upload(ctx, id)
	if err != nil {
		return nil, Reference{}, err
	}
	if !s.now().Before(u.ExpiresAt) {
		return nil, Reference{}, ErrExpired
	}
	if u.State == "deleted" || u.State == "deletion_pending" {
		return nil, Reference{}, ErrMissing
	}
	if revision == 0 {
		revision = u.ManifestRevision
	}
	result, err := s.command(ctx, CatalogCommand{Operation: "manifest", UploadID: id, Revision: revision})
	if err != nil {
		return nil, Reference{}, err
	}
	manifest := result.Manifest
	body, err := s.storage.Get(ctx, manifest.BodyKey, "", MaxManifestBytes)
	if err != nil {
		return nil, manifest.Reference, err
	}
	if int64(len(body)) != manifest.BodyBytes || Digest(body) != manifest.SHA256 {
		return nil, manifest.Reference, ErrIntegrity
	}
	return body, manifest.Reference, nil
}

func (s *Service) ReadObject(ctx context.Context, id string, revision int64, objectID string) (Object, []byte, error) {
	body, reference, err := s.Manifest(ctx, id, revision)
	if err != nil {
		return Object{}, nil, err
	}
	var m Manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return Object{}, nil, ErrIntegrity
	}
	for _, obj := range m.Objects {
		if obj.ID != objectID {
			continue
		}
		stored, err := s.command(ctx, CatalogCommand{Operation: "objects", UploadID: id, Limit: MaxObjects})
		if err != nil {
			return obj, nil, err
		}
		version := ""
		for _, entry := range stored.Objects {
			if entry.ID == objectID && entry.Verified {
				version = entry.StorageVersion
				break
			}
		}
		if version == "" {
			return obj, nil, ErrMissing
		}
		data, err := s.storage.Get(ctx, s.scopedObjectKey(reference.ProjectID, id, objectID), version, obj.Size)
		if err != nil {
			return obj, nil, err
		}
		if int64(len(data)) != obj.Size || Digest(data) != obj.SHA256 {
			return obj, nil, ErrIntegrity
		}
		return obj, data, nil
	}
	return Object{}, nil, ErrMissing
}

func (s *Service) objectKey(id, suffix string) string {
	return s.config.OrganizationID + "/" + s.config.ProjectID + "/" + s.config.ServiceID + "/" + id + "/" + suffix
}

func (s *Service) scopedObjectKey(project, id, suffix string) string {
	return s.config.OrganizationID + "/" + project + "/" + s.config.ServiceID + "/" + id + "/" + suffix
}
