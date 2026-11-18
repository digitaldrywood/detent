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
	intents   map[string]map[string]CatalogIntent
	settings  CatalogTraffic
}

func newMemoryCatalog(cfg Config) *memoryCatalog {
	return &memoryCatalog{intents: map[string]map[string]CatalogIntent{}, config: cfg, limits: func() Limits { return cfg.Policy.Limits }, uploads: map[string]Upload{}, objects: map[string][]CatalogObject{}, manifests: map[string][]CatalogManifest{}, outcomes: map[string]string{}, traffic: map[int64]CatalogTraffic{}, settings: CatalogTraffic{WindowSeconds: 3600, RetentionSeconds: 86400}}
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
	case "manifest_intents":
		ids := []string{}
		for id, v := range m.intents[c.UploadID] {
			if id > c.After && (c.Outcome != "pending" || v.State == "pending" || v.State == "discarding") && (c.Outcome != "retired" || v.State == "committed" || v.State == "discarded") {
				ids = append(ids, id)
			}
		}
		sort.Strings(ids)
		for _, id := range ids {
			result.Intents = append(result.Intents, m.intents[c.UploadID][id])
			if len(result.Intents) == c.Limit {
				result.Next = id
				break
			}
		}
	case "prepare_manifest":
		if !exists {
			return result, ErrMissing
		}
		if m.intents[c.UploadID] == nil {
			m.intents[c.UploadID] = map[string]CatalogIntent{}
		}
		pending := int64(0)
		for _, v := range m.intents[c.UploadID] {
			if v.State == "pending" || v.State == "discarding" {
				if v.Revision == c.Intent.Revision && v.SHA256 == c.Intent.SHA256 {
					result.Intent = v
					return result, nil
				}
				pending += v.Bytes
			}
		}
		if c.Intent.Bytes > u.ReservedBytes-pending {
			return result, ErrQuota
		}
		v := c.Intent
		v.State = "pending"
		if len(m.intents[c.UploadID]) >= MaxIntentHistory {
			return result, ErrQuota
		}
		v.ID = NewID("intent")
		v.WriteState = "ready"
		v.BodyKey = c.OrganizationID + "/" + c.ProjectID + "/" + c.ServiceID + "/" + c.UploadID + "/manifests/" + fmt.Sprint(v.Revision) + "/" + v.SHA256 + "/" + v.ID
		m.intents[c.UploadID][v.ID] = v
		result.Intent = v
	case "prune_intent":
		v, ok := m.intents[c.UploadID][c.Intent.ID]
		if ok && v.State != "committed" && v.State != "discarded" {
			return result, ErrConflict
		}
		delete(m.intents[c.UploadID], c.Intent.ID)
	case "verify_manifest", "begin_manifest_discard", "finish_manifest_discard":
		v, ok := m.intents[c.UploadID][c.Intent.ID]
		if !ok {
			return result, ErrMissing
		}
		switch c.Operation {
		case "verify_manifest":
			if v.State != "pending" || v.WriteState != "written" {
				return result, ErrConflict
			}
			v.StorageVersion = c.Intent.StorageVersion
		case "begin_manifest_discard":
			if v.State == "committed" {
				return result, ErrConflict
			}
			v.State = "discarding"
			if v.WriteState == "ready" {
				v.WriteState = "rejected"
			}
		case "finish_manifest_discard":
			if v.WriteState != "written" && v.WriteState != "rejected" {
				return result, ErrConflict
			}
			v.State = "discarded"
			v.Receipt = c.Intent.Receipt
		}
		m.intents[c.UploadID][v.ID] = v
		result.Intent = v

	case "begin_manifest_write", "finish_manifest_write":
		v, ok := m.intents[c.UploadID][c.Intent.ID]
		if !ok {
			return result, ErrMissing
		}
		if c.Operation == "begin_manifest_write" {
			if v.WriteState != "ready" || v.State != "pending" || u.State != "uploading" {
				return result, ErrConflict
			}
			v.WriteState, v.WriteID = "writing", c.Intent.WriteID
		} else {
			if c.Outcome == "rejected" {
				if v.WriteState != "ready" && (v.WriteState != "writing" || v.WriteID != c.Intent.WriteID) {
					return result, ErrConflict
				}
			} else if v.WriteID != c.Intent.WriteID || v.WriteState != "writing" && v.WriteState != "uncertain" && v.WriteState != "written" {
				return result, ErrConflict
			}
			v.WriteState, v.WriteID, v.StorageVersion = c.Outcome, c.Intent.WriteID, c.Intent.StorageVersion
		}
		m.intents[c.UploadID][v.ID] = v
		result.Intent = v
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
		result.Complete = true
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
			for i, o := range m.objects[c.UploadID] {
				if o.Sequence == c.Object.Sequence && o.WriteState == "rejected" && !o.Verified && u.State == "uploading" && o.ID != c.Object.ID {
					c.Object.WriteState = "ready"
					c.Object.UploadID = c.UploadID
					m.objects[c.UploadID][i] = c.Object
					result.Object = c.Object
					return result, nil
				}
			}
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
			c.Object.WriteState = "ready"
			c.Object.UploadID = c.UploadID
			m.objects[c.UploadID] = append(m.objects[c.UploadID], c.Object)
			result.Object = c.Object
		case "begin_object_write", "finish_object_write":
			for i, o := range m.objects[c.UploadID] {
				if o.ID != c.Object.ID || o.Sequence != c.Object.Sequence {
					continue
				}
				if c.Operation == "begin_object_write" {
					if o.WriteState != "ready" || u.State != "uploading" {
						return result, ErrConflict
					}
					o.WriteState, o.WriteID = "writing", c.Object.WriteID
				} else {
					if c.Outcome == "rejected" {
						if o.WriteState != "ready" && (o.WriteState != "writing" || o.WriteID != c.Object.WriteID) {
							return result, ErrConflict
						}
					} else if o.WriteID != c.Object.WriteID || o.WriteState != "writing" && o.WriteState != "uncertain" && o.WriteState != "written" {
						return result, ErrConflict
					}
					o.WriteState, o.WriteID, o.StorageVersion = c.Outcome, c.Object.WriteID, c.Object.StorageVersion
				}
				m.objects[c.UploadID][i] = o
				result.Object = o
				return result, nil
			}
			return result, ErrMissing
		case "verify_object", "discard_object":
			for i, o := range m.objects[c.UploadID] {
				if o.Sequence != c.Object.Sequence {
					continue
				}
				if c.Operation == "discard_object" {
					if o.WriteState == "ready" {
						o.WriteState = "rejected"
					}
					if o.WriteState != "written" && o.WriteState != "rejected" {
						return result, ErrConflict
					}
					o.Discarded = true
				} else if !o.Verified {
					if o.WriteState != "written" || u.State != "uploading" && u.State != "deletion_pending" {
						return result, ErrConflict
					}
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
			found := false
			for id, v := range m.intents[c.UploadID] {
				if v.BodyKey == c.Manifest.BodyKey && v.State == "pending" && v.StorageVersion != "" {
					v.State = "committed"
					m.intents[c.UploadID][id] = v
					found = true
				}
			}
			if !found {
				return result, ErrConflict
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
			for _, o := range m.objects[c.UploadID] {
				if o.WriteState != "written" && o.WriteState != "rejected" || !o.Verified && !o.Discarded {
					return result, ErrConflict
				}
			}
			for _, v := range m.intents[c.UploadID] {
				if v.WriteState != "written" && v.WriteState != "rejected" || v.State == "pending" || v.State == "discarding" {
					return result, ErrConflict
				}
			}
			u.State = "deleted"
			u.ReservedBytes = 0
			u.RetainedBytes = 0
			u.DeletedAt = new(c.At)
			m.uploads[c.UploadID] = u
			result.Upload = u
		case "prune":
			if len(m.intents[c.UploadID]) != 0 {
				return result, ErrConflict
			}
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
