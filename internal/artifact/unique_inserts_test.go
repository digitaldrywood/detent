package artifact

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"sync"
)

type memoryCatalog struct {
	mu        sync.Mutex
	config    Config
	limits    func() Limits
	uploads   map[string]Upload
	objects   map[string][]CatalogObject
	manifests map[string][]CatalogManifest
	outcomes  map[string]string
	traffic   map[int64]CatalogTraffic
	settings  CatalogTraffic
}

func newMemoryCatalog(cfg Config) *memoryCatalog {
	return &memoryCatalog{config: cfg, limits: func() Limits { return cfg.Policy.Limits }, uploads: map[string]Upload{}, objects: map[string][]CatalogObject{}, manifests: map[string][]CatalogManifest{}, outcomes: map[string]string{}, traffic: map[int64]CatalogTraffic{}, settings: CatalogTraffic{WindowSeconds: 3600, RetentionSeconds: 86400}}
}

func (m *memoryCatalog) Execute(_ context.Context, c CatalogCommand) (CatalogResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var result CatalogResult
	if c.OrganizationID != m.config.OrganizationID || c.ProjectID != m.config.ProjectID || c.ServiceID != m.config.ServiceID {
		return result, ErrDenied
	}
	u, exists := m.uploads[c.UploadID]
	usage := Usage{}
	for _, u := range m.uploads {
		usage.ReservedBytes += u.ReservedBytes
		usage.RetainedBytes += u.RetainedBytes
	}
	switch c.Operation {
	case "traffic_settings":
		result.Traffic = m.settings
	case "set_traffic_settings":
		m.settings = c.Traffic
		result.Traffic = m.settings
	case "traffic":
		minute := c.At.Unix() / 60 * 60
		v := m.traffic[minute]
		v.UploadBytes += c.Traffic.UploadBytes
		v.DownloadBytes += c.Traffic.DownloadBytes
		v.Requests += c.Traffic.Requests
		m.traffic[minute] = v
	case "prune_traffic":
		for minute := range m.traffic {
			if minute < c.Before.Unix() {
				delete(m.traffic, minute)
			}
		}
	case "usage":
		result.Usage = usage
		{
			result.Usage.WindowStart = c.Before
			result.Usage.ObservedAt = c.At
			for minute, v := range m.traffic {
				if minute >= c.Before.Unix() {
					result.Usage.RelayBytes += v.UploadBytes + v.DownloadBytes
					result.Usage.StorageRequests += v.Requests
				}
			}
		}
	case "upload_by_key":
		for _, u := range m.uploads {
			if u.Key == c.Key {
				result.Upload = u
				return result, nil
			}
		}
		return result, ErrMissing
	case "reserve":
		for _, u := range m.uploads {
			if u.Key == c.Upload.Key {
				if u.Reservation != c.Upload.Reservation {
					return result, ErrConflict
				}
				result.Upload = u
				return result, nil
			}
		}
		limits := m.limits()
		if c.Upload.Bytes > limits.ReservedBytes-usage.ReservedBytes || c.Upload.Bytes > limits.RetainedBytes-usage.ReservedBytes-usage.RetainedBytes {
			return result, ErrQuota
		}
		m.uploads[c.Upload.ArtifactID] = c.Upload
		result.Upload = c.Upload
	case "pending":
		manifests := []CatalogManifest{}
		for id, rows := range m.manifests {
			upload := m.uploads[id]
			if upload.State == "deleted" || upload.State == "deletion_pending" || !c.At.Before(upload.ExpiresAt) {
				continue
			}
			for _, row := range rows {
				if m.outcomes[row.ManifestID] != "committed" {
					manifests = append(manifests, row)
				}
			}
		}
		sort.Slice(manifests, func(i, j int) bool { return catalogCursor(manifests[i]) < catalogCursor(manifests[j]) })
		for _, row := range manifests {
			if catalogCursor(row) > c.After {
				result.Manifests = append(result.Manifests, row)
				if len(result.Manifests) == c.Limit {
					result.Next = catalogCursor(row)
					break
				}
			}
		}
	case "publication":
		m.outcomes[c.Manifest.ManifestID] = c.Outcome
	case "retention":
		for _, upload := range m.uploads {
			if upload.State == "deleted" && upload.DeletedAt != nil && upload.DeletedAt.Before(c.Before) || upload.State == "deletion_pending" || upload.State != "deleted" && (!c.At.Before(upload.ExpiresAt) || upload.State == "uploading" && !c.At.Before(upload.Deadline)) {
				result.Uploads = append(result.Uploads, upload)
			}
		}
		sort.Slice(result.Uploads, func(i, j int) bool { return result.Uploads[i].ArtifactID < result.Uploads[j].ArtifactID })
		if len(result.Uploads) > c.Limit {
			result.Uploads = result.Uploads[:c.Limit]
		}
	case "export":
		for _, upload := range m.uploads {
			result.Uploads = append(result.Uploads, upload)
			result.Objects = append(result.Objects, m.objects[upload.ArtifactID]...)
			result.Manifests = append(result.Manifests, m.manifests[upload.ArtifactID]...)
		}
	default:
		if !exists {
			return result, ErrMissing
		}
		switch c.Operation {
		case "upload":
			result.Upload = u
		case "objects":
			result.Objects = slices.Clone(m.objects[c.UploadID])
		case "put_object":
			for _, o := range m.objects[c.UploadID] {
				if o.Sequence == c.Object.Sequence {
					if o.Object != c.Object.Object {
						return result, ErrConflict
					}
					result.Object = o
					return result, nil
				}
			}
			pending := int64(0)
			for _, o := range m.objects[c.UploadID] {
				if !o.Verified && !o.Discarded {
					pending += o.Size
				}
			}
			if c.Object.Size > u.PartLimit || c.Object.Size > u.ReservedBytes-pending {
				return result, ErrQuota
			}
			m.objects[c.UploadID] = append(m.objects[c.UploadID], c.Object)
			result.Object = c.Object
		case "verify_object", "discard_object":
			for i, o := range m.objects[c.UploadID] {
				if o.Sequence != c.Object.Sequence {
					continue
				}
				if c.Operation == "discard_object" {
					o.Discarded = true
				} else if !o.Verified {
					o.Verified = true
					o.StorageVersion = c.Object.StorageVersion
					u.RetainedBytes += o.Size
					u.ReservedBytes -= o.Size
					m.uploads[c.UploadID] = u
				}
				m.objects[c.UploadID][i] = o
				result.Object = o
				return result, nil
			}
			return result, ErrMissing
		case "manifest":
			for _, v := range m.manifests[c.UploadID] {
				if v.Revision == c.Revision {
					result.Manifest = v
					return result, nil
				}
			}
			return result, ErrMissing
		case "commit":
			for _, v := range m.manifests[c.UploadID] {
				if v.Revision == c.Manifest.Revision {
					a, _ := json.Marshal(v)
					b, _ := json.Marshal(c.Manifest)
					if string(a) != string(b) {
						return result, ErrConflict
					}
					result.Manifest = v
					return result, nil
				}
			}
			if u.State != "uploading" || c.Manifest.Revision != u.ManifestRevision+1 {
				return result, ErrConflict
			}
			if c.Manifest.BodyBytes > u.ReservedBytes {
				return result, ErrQuota
			}
			for _, o := range m.objects[c.UploadID] {
				if !o.Verified && !o.Discarded && c.Manifest.State != "partial" {
					return result, ErrConflict
				}
			}
			u.ManifestRevision = c.Manifest.Revision
			u.RetainedBytes += c.Manifest.BodyBytes
			u.ReservedBytes -= c.Manifest.BodyBytes
			if c.Manifest.State != "partial" {
				u.State = c.Manifest.State
				u.ReservedBytes = 0
			}
			m.uploads[c.UploadID] = u
			m.manifests[c.UploadID] = append(m.manifests[c.UploadID], c.Manifest)
			result.Manifest = c.Manifest
		case "begin_delete":
			u.State = "deletion_pending"
			m.uploads[c.UploadID] = u
			result.Upload = u
		case "finish_delete":
			u.State = "deleted"
			u.ReservedBytes = 0
			u.RetainedBytes = 0
			u.DeletedAt = new(c.At)
			m.uploads[c.UploadID] = u
			result.Upload = u
		case "prune":
			delete(m.uploads, c.UploadID)
			delete(m.objects, c.UploadID)
			delete(m.manifests, c.UploadID)
		default:
			return result, ErrInvalid
		}
	}
	return result, nil
}

func catalogCursor(m CatalogManifest) string {
	return fmt.Sprintf("%020d/%s/%020d", m.CreatedAt.Unix(), m.ArtifactID, m.Revision)
}
