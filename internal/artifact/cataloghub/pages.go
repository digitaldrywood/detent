package cataloghub

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strconv"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type pagePosition struct {
	Project string    `json:"project,omitempty"`
	ID      string    `json:"id,omitempty"`
	Value   string    `json:"value,omitempty"`
	At      time.Time `json:"at,omitzero"`
}

type pageCursor struct {
	Phase     int             `json:"phase"`
	Position  pagePosition    `json:"position,omitzero"`
	Retention [5]pagePosition `json:"retention"`
	Traffic   [2]time.Time    `json:"traffic,omitzero"`
}

func readCursor(value string) (pageCursor, error) {
	var cursor pageCursor
	if value == "" {
		return cursor, nil
	}
	if len(value) > 4096 {
		return cursor, artifact.ErrInvalid
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || json.Unmarshal(raw, &cursor) != nil || cursor.Phase < 0 || cursor.Phase > 8 {
		return pageCursor{}, artifact.ErrInvalid
	}
	return cursor, nil
}

func nextCursor(cursor pageCursor) (string, error) {
	raw, err := json.Marshal(cursor)
	if err != nil {
		return "", artifact.ErrInvalid
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func (c *Catalog) page(ctx context.Context, command artifact.CatalogCommand, cfg artifact.Config) (artifact.CatalogResult, error) {
	result := artifact.CatalogResult{CatalogScope: command.CatalogScope, ObservedAt: command.At}
	cursor, err := readCursor(command.After)
	if err != nil {
		return result, err
	}
	org := tracker.OrganizationID(command.OrganizationID)
	if command.Operation == "prune_traffic" {
		err := c.write(ctx, command, func(w postgres.ArtifactCatalogWriter) error {
			progress, err := w.PruneScopedCatalogTraffic(ctx, postgres.ScopedCatalogTrafficPrune{OrganizationID: org, PublisherID: cfg.PublisherID, ServiceID: command.ServiceID, ProjectID: command.ProjectID, Before: command.Before, PageLimit: int32(command.Limit), AfterProject: cursor.Position.Project, AfterServiceMinute: cursor.Traffic[0], AfterOrganizationMinute: cursor.Traffic[1]})
			if err != nil {
				return err
			}
			cursor.Position.Project = progress.ProjectID
			cursor.Traffic = [2]time.Time{progress.ServiceMinute, progress.OrganizationMinute}
			if progress.ProjectID != "" || !progress.ServiceMinute.IsZero() || !progress.OrganizationMinute.IsZero() {
				result.Next, err = nextCursor(cursor)
			}
			return err
		})
		return result, err
	}
	err = c.read(ctx, command, func(r postgres.ArtifactCatalogReader) error {
		switch command.Operation {
		case "pending":
			rows, err := r.ScopedPendingCatalogManifests(ctx, postgres.ScopedPendingCatalogManifestsInput{OrganizationID: org, ServiceID: command.ServiceID, ProjectID: command.ProjectID, PublisherID: cfg.PublisherID, Now: command.At, AfterProject: cursor.Position.Project, AfterID: cursor.Position.ID, PageLimit: int32(command.Limit)})
			if err != nil {
				return err
			}
			for _, row := range rows {
				u, err := r.CatalogUpload(ctx, postgres.CatalogUploadInput{OrganizationID: org, ProjectID: row.ProjectID, ServiceID: row.ServiceID, ID: row.UploadID})
				if err != nil {
					return err
				}
				result.Manifests = append(result.Manifests, manifest(row, u))
			}
			if len(rows) == command.Limit {
				last := rows[len(rows)-1]
				cursor.Position = pagePosition{Project: string(last.ProjectID), ID: last.ID}
				result.Next, err = nextCursor(cursor)
			}
			return err
		case "retention":
			for category := range 5 {
				pos := cursor.Retention[category]
				budget := max(1, command.Limit/5)
				rows, err := r.ScopedCatalogRetention(ctx, postgres.ScopedCatalogRetentionInput{OrganizationID: org, ServiceID: command.ServiceID, ProjectID: command.ProjectID, PublisherID: cfg.PublisherID, Category: int32(category), Now: command.At, Before: command.Before, AfterTime: pos.At, AfterProject: pos.Project, AfterID: pos.ID, PageLimit: int32(budget)})
				if err != nil {
					return err
				}
				for _, row := range rows {
					u := upload(row, cfg)
					u.IntentCleanup = category == 4
					result.Uploads = append(result.Uploads, u)
				}
				cursor.Retention[category] = pagePosition{}
				if len(rows) == budget {
					last := rows[len(rows)-1]
					at := last.ExpiresAt
					if category == 4 {
						at = last.CreatedAt
					}
					if category == 1 {
						at = last.UploadDeadline
					}
					if category == 3 && last.DeletedAt != nil {
						at = *last.DeletedAt
					}
					cursor.Retention[category] = pagePosition{At: at, Project: string(last.ProjectID), ID: last.ID}
				}
			}
			cursor.Phase = 0
			result.Next, err = nextCursor(cursor)
			return err
		case "export":
			return c.exportPage(ctx, r, command, cfg, cursor, &result)
		}
		return artifact.ErrInvalid
	})
	return result, err
}

func (c *Catalog) exportPage(ctx context.Context, r postgres.ArtifactCatalogReader, command artifact.CatalogCommand, cfg artifact.Config, cursor pageCursor, result *artifact.CatalogResult) error {
	org := tracker.OrganizationID(command.OrganizationID)
	pos := cursor.Position
	count := 0
	var next pagePosition
	switch cursor.Phase {
	case 0:
		result.ExportKind = "uploads"
		rows, err := r.ScopedCatalogUploads(ctx, postgres.ScopedCatalogUploadsInput{OrganizationID: org, ServiceID: command.ServiceID, ProjectID: command.ProjectID, PublisherID: cfg.PublisherID, AfterProject: pos.Project, AfterID: pos.ID, PageLimit: int32(command.Limit)})
		if err != nil {
			return err
		}
		for _, row := range rows {
			result.Uploads = append(result.Uploads, upload(row, cfg))
			next = pagePosition{Project: string(row.ProjectID), ID: row.ID}
		}
		count = len(rows)
	case 1:
		result.ExportKind = "objects"
		value := int64(-1)
		if pos.Value != "" {
			var err error
			value, err = strconv.ParseInt(pos.Value, 10, 32)
			if err != nil {
				return artifact.ErrInvalid
			}
		}
		rows, err := r.ScopedCatalogObjects(ctx, postgres.ScopedCatalogObjectsInput{OrganizationID: org, ServiceID: command.ServiceID, ProjectID: command.ProjectID, PublisherID: cfg.PublisherID, AfterProject: pos.Project, AfterUpload: pos.ID, AfterValue: int32(value), PageLimit: int32(command.Limit)})
		if err != nil {
			return err
		}
		for _, row := range rows {
			result.Objects = append(result.Objects, object(row))
			next = pagePosition{Project: string(row.ProjectID), ID: row.UploadID, Value: strconv.FormatInt(int64(row.Sequence), 10)}
		}
		count = len(rows)
	case 2:
		result.ExportKind = "manifests"
		value := int64(0)
		if pos.Value != "" {
			var err error
			value, err = strconv.ParseInt(pos.Value, 10, 64)
			if err != nil {
				return artifact.ErrInvalid
			}
		}
		rows, err := r.ScopedCatalogManifests(ctx, postgres.ScopedCatalogManifestsInput{OrganizationID: org, ServiceID: command.ServiceID, ProjectID: command.ProjectID, PublisherID: cfg.PublisherID, AfterProject: pos.Project, AfterUpload: pos.ID, AfterValue: value, PageLimit: int32(command.Limit)})
		if err != nil {
			return err
		}
		for _, row := range rows {
			u, err := r.CatalogUpload(ctx, postgres.CatalogUploadInput{OrganizationID: org, ProjectID: row.ProjectID, ServiceID: row.ServiceID, ID: row.UploadID})
			if err != nil {
				return err
			}
			result.Manifests = append(result.Manifests, manifest(row, u))
			next = pagePosition{Project: string(row.ProjectID), ID: row.UploadID, Value: strconv.FormatInt(row.Revision, 10)}
		}
		count = len(rows)
	case 3:
		result.ExportKind = "manifest_intents"
		rows, err := r.ScopedCatalogIntents(ctx, postgres.ScopedCatalogIntentsInput{OrganizationID: org, ServiceID: command.ServiceID, ProjectID: command.ProjectID, PublisherID: cfg.PublisherID, AfterProject: pos.Project, AfterUpload: pos.ID, AfterValue: pos.Value, PageLimit: int32(command.Limit)})
		if err != nil {
			return err
		}
		for _, row := range rows {
			result.Intents = append(result.Intents, intent(row))
			next = pagePosition{Project: string(row.ProjectID), ID: row.UploadID, Value: row.ID}
		}
		count = len(rows)
	case 4:
		result.ExportKind = "publications"
		rows, err := r.ScopedCatalogPublications(ctx, postgres.ScopedCatalogPublicationsInput{OrganizationID: org, ServiceID: command.ServiceID, ProjectID: command.ProjectID, PublisherID: cfg.PublisherID, After0: pos.Project, After1: pos.ID, PageLimit: int32(command.Limit)})
		if err != nil {
			return err
		}
		for _, row := range rows {
			result.Publications = append(result.Publications, artifact.CatalogPublication{ProjectID: string(row.ProjectID), ManifestID: row.ManifestID, Outcome: row.Outcome, Receipt: row.ExternalReceipt, UpdatedAt: row.UpdatedAt})
			next = pagePosition{Project: string(row.ProjectID), ID: row.ManifestID}
		}
		count = len(rows)
	case 5:
		result.ExportKind = "traffic_buckets"
		rows, err := r.ScopedCatalogTrafficBuckets(ctx, postgres.ScopedCatalogTrafficBucketsInput{OrganizationID: org, ServiceID: command.ServiceID, ProjectID: command.ProjectID, PublisherID: cfg.PublisherID, After0: pos.Project, After1: pos.At, PageLimit: int32(command.Limit)})
		if err != nil {
			return err
		}
		for _, row := range rows {
			result.TrafficRecords = append(result.TrafficRecords, artifact.CatalogTrafficRecord{ProjectID: string(row.ProjectID), Minute: row.Minute, CatalogTraffic: artifact.CatalogTraffic{UploadBytes: row.UploadBytes, DownloadBytes: row.DownloadBytes, Requests: row.Requests}})
			next = pagePosition{Project: string(row.ProjectID), At: row.Minute}
		}
		count = len(rows)
	case 6:
		result.ExportKind = "traffic_receipts"
		rows, err := r.ScopedCatalogTrafficReceipts(ctx, postgres.ScopedCatalogTrafficReceiptsInput{OrganizationID: org, ServiceID: command.ServiceID, ProjectID: command.ProjectID, PublisherID: cfg.PublisherID, After0: pos.Project, After1: pos.ID, PageLimit: int32(command.Limit)})
		if err != nil {
			return err
		}
		for _, row := range rows {
			result.TrafficRecords = append(result.TrafficRecords, artifact.CatalogTrafficRecord{ProjectID: string(row.ProjectID), Minute: row.Minute, CatalogTraffic: artifact.CatalogTraffic{ID: row.ID, UploadBytes: row.UploadBytes, DownloadBytes: row.DownloadBytes, Requests: row.Requests}})
			next = pagePosition{Project: string(row.ProjectID), ID: row.ID}
		}
		count = len(rows)
	case 7:
		result.ExportKind = "settings"
		rows, err := r.ScopedCatalogSettings(ctx, postgres.ScopedCatalogSettingsInput{OrganizationID: org, ServiceID: command.ServiceID, ProjectID: command.ProjectID, PublisherID: cfg.PublisherID, After0: pos.Project, PageLimit: int32(command.Limit)})
		if err != nil {
			return err
		}
		for _, row := range rows {
			result.Settings = append(result.Settings, artifact.CatalogSettings{ProjectID: string(row.ProjectID), WindowSeconds: row.WindowSeconds, RetentionSeconds: row.RetentionSeconds})
			next = pagePosition{Project: string(row.ProjectID)}
		}
		count = len(rows)

	case 8:
		result.ExportKind = "policies"
		rows, err := r.ScopedCatalogPolicies(ctx, postgres.ScopedCatalogPoliciesInput{OrganizationID: org, ServiceID: command.ServiceID, ProjectID: command.ProjectID, PublisherID: cfg.PublisherID, After0: pos.Project, PageLimit: int32(command.Limit)})
		if err != nil {
			return err
		}
		for _, row := range rows {
			policy := artifact.Policy{ID: row.CatalogPolicyID, Limits: artifact.Limits{RetainedBytes: row.CatalogRetainedLimit, ReservedBytes: row.CatalogReservedLimit, ArtifactBytes: row.CatalogArtifactLimit, UploadBytes: row.CatalogUploadLimit, RetentionSeconds: row.CatalogRetentionSeconds}, AbandonedUploadSeconds: row.CatalogAbandonedSeconds, DeletionRecordSeconds: row.CatalogDeletionSeconds, BackupSeconds: row.CatalogBackupSeconds}
			binding := artifact.Binding{ServiceID: row.ID, Origin: row.Origin, Mode: row.BindingMode, HostedOptIn: row.HostedOptIn, PublisherTokenID: row.PublisherTokenID}
			result.Settings = append(result.Settings, artifact.CatalogSettings{ProjectID: string(row.ProjectID), Policy: &policy, Binding: &binding})
			next = pagePosition{Project: string(row.ProjectID)}
		}
		count = len(rows)
	}
	if count == command.Limit {
		cursor.Position = next
	} else {
		cursor.Phase++
		cursor.Position = pagePosition{}
	}
	if cursor.Phase > 8 {
		result.Complete = true
		return nil
	}
	var err error
	result.Next, err = nextCursor(cursor)
	return err
}
