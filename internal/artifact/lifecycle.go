package artifact

import (
	"context"
	"errors"
	"strconv"
	"time"
)

type Publisher interface {
	Publish(context.Context, Reference) error
}

type publishCursor struct{ after string }

const publishPage = 32

func (s *Service) discardPending(ctx context.Context, upload Upload) error {
	id := upload.ArtifactID
	result, err := s.command(ctx, CatalogCommand{Operation: "objects", UploadID: id, Limit: MaxObjects})
	if err != nil {
		return err
	}
	for _, obj := range result.Objects {
		if obj.Verified || obj.Discarded {
			continue
		}
		key := s.scopedObjectKey(upload.ProjectID, id, obj.ID)
		if err := s.storage.Delete(ctx, key, obj.StorageVersion); err != nil {
			return err
		}
		if _, err := s.storage.Get(ctx, key, obj.StorageVersion, 0); !errors.Is(err, ErrMissing) {
			return errors.Join(ErrStorage, err)
		}
		receipt := s.scopedObjectKey(upload.ProjectID, id, "discarded/"+obj.ID)
		version, err := s.storage.Put(ctx, receipt, []byte(obj.SHA256))
		if err != nil && !errors.Is(err, ErrConflict) {
			return err
		}
		if _, err := s.storage.Get(ctx, receipt, version, 128); err != nil {
			return err
		}
		if _, err := s.command(ctx, CatalogCommand{Operation: "discard_object", UploadID: id, Object: obj, At: s.now().UTC(), Receipt: receipt}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) PublishPending(ctx context.Context, publisher Publisher) error {
	if publisher == nil {
		return ErrInvalid
	}
	s.publishMu.Lock()
	after := s.publishAfter.after
	s.publishMu.Unlock()
	result, err := s.command(ctx, CatalogCommand{Operation: "pending", At: s.now().UTC(), After: after, Limit: publishPage})
	if err != nil {
		return err
	}
	s.publishMu.Lock()
	s.publishAfter.after = result.Next
	s.publishMu.Unlock()
	var failures []error
	for _, manifest := range result.Manifests {
		err := publisher.Publish(ctx, manifest.Reference)
		outcome, receipt := "committed", manifest.ManifestID
		if err != nil {
			outcome, receipt = "uncertain", ""
			for _, rejected := range []error{ErrDenied, ErrInvalid, ErrIntegrity, ErrMissing, ErrConflict, ErrQuota, ErrExpired} {
				if errors.Is(err, rejected) {
					outcome = "pending"
					break
				}
			}
			failures = append(failures, err)
		}
		recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		_, recordErr := s.command(recordCtx, CatalogCommand{Operation: "publication", UploadID: manifest.ArtifactID, Manifest: manifest, Outcome: outcome, Receipt: receipt, At: s.now().UTC()})
		cancel()
		if recordErr != nil {
			failures = append(failures, recordErr)
		}
		if ctx.Err() != nil {
			return errors.Join(append(failures, ctx.Err())...)
		}
	}
	return errors.Join(failures...)
}

func (s *Service) Maintain(ctx context.Context) error {
	now := s.now().UTC()
	s.publishMu.Lock()
	after := s.maintainAfter
	s.publishMu.Unlock()
	result, err := s.command(ctx, CatalogCommand{After: after, Operation: "retention", At: now, Before: now.Add(-time.Duration(s.config.Policy.DeletionRecordSeconds) * time.Second), Limit: 32})
	if err != nil {
		return err
	}
	if !result.ObservedAt.IsZero() {
		now = result.ObservedAt
	}
	s.publishMu.Lock()
	s.maintainAfter = result.Next
	s.publishMu.Unlock()
	var failures []error
	for _, upload := range result.Uploads {
		if err := s.maintainUpload(ctx, upload, now); err != nil {
			failures = append(failures, err)
		}
		if ctx.Err() != nil {
			return errors.Join(append(failures, ctx.Err())...)
		}
	}
	if _, err := s.command(ctx, CatalogCommand{Operation: "prune_traffic", Before: now.Add(-time.Duration(s.trafficRetention.Load()) * time.Second), Limit: 32}); err != nil {
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}

func (s *Service) maintainUpload(ctx context.Context, upload Upload, now time.Time) error {
	id := upload.ArtifactID
	if upload.State == "deleted" {
		if err := s.pruneReceipts(ctx, upload); err != nil {
			return err
		}
		_, err := s.command(ctx, CatalogCommand{Operation: "prune", UploadID: id, Before: now.Add(-time.Duration(s.config.Policy.DeletionRecordSeconds) * time.Second)})
		return err
	}
	if upload.State == "uploading" && now.Before(upload.ExpiresAt) && upload.Kind == "log" {
		_, err := s.seal(ctx, id, "interrupted", nil)
		return err
	}
	return s.delete(ctx, id)
}

func (s *Service) pruneReceipts(ctx context.Context, upload Upload) error {
	result, err := s.command(ctx, CatalogCommand{Operation: "objects", UploadID: upload.ArtifactID, Limit: MaxObjects})
	if err != nil {
		return err
	}
	for _, object := range result.Objects {
		if object.Discarded {
			if err := s.deleteReceipt(ctx, s.scopedObjectKey(upload.ProjectID, upload.ArtifactID, "discarded/"+object.ID)); err != nil {
				return err
			}
		}
	}
	after := ""
	for {
		result, err := s.command(ctx, CatalogCommand{Operation: "manifest_intents", UploadID: upload.ArtifactID, After: after, Limit: 32})
		if err != nil {
			return err
		}
		for _, intent := range result.Intents {
			if intent.Receipt != "" {
				if err := s.deleteReceipt(ctx, s.scopedObjectKey(upload.ProjectID, upload.ArtifactID, "discarded/"+intent.ID)); err != nil {
					return err
				}
			}
		}
		if result.Next == "" {
			break
		}
		if result.Next == after {
			return ErrIntegrity
		}
		after = result.Next
	}
	return s.deleteReceipt(ctx, s.scopedObjectKey(upload.ProjectID, upload.ArtifactID, "deleted"))
}

func (s *Service) deleteReceipt(ctx context.Context, key string) error {
	if err := s.storage.Delete(ctx, key, ""); err != nil {
		return err
	}
	if _, err := s.storage.Get(ctx, key, "", 0); !errors.Is(err, ErrMissing) {
		return errors.Join(ErrStorage, err)
	}
	return nil
}

func (s *Service) Delete(ctx context.Context, id string) error { return s.delete(ctx, id) }

func (s *Service) delete(ctx context.Context, id string) error {
	upload, err := s.Upload(ctx, id)
	if err != nil {
		return err
	}
	if upload.State == "deleted" {
		return nil
	}
	if _, err := s.command(ctx, CatalogCommand{Operation: "begin_delete", UploadID: id}); err != nil {
		return err
	}
	receipt := s.scopedObjectKey(upload.ProjectID, id, "deleted")
	version, err := s.storage.Put(ctx, receipt, []byte(strconv.FormatInt(s.now().Unix(), 10)))
	if err != nil && !errors.Is(err, ErrConflict) {
		return err
	}
	if _, err := s.storage.Get(ctx, receipt, version, 128); err != nil {
		return err
	}
	result, err := s.command(ctx, CatalogCommand{Operation: "objects", UploadID: id, Limit: MaxObjects})
	if err != nil {
		return err
	}
	for _, obj := range result.Objects {
		key := s.scopedObjectKey(upload.ProjectID, id, obj.ID)
		if err := s.storage.Delete(ctx, key, obj.StorageVersion); err != nil {
			return err
		}
		if _, err := s.storage.Get(ctx, key, obj.StorageVersion, 0); !errors.Is(err, ErrMissing) {
			return errors.Join(ErrStorage, err)
		}
	}
	for revision := int64(1); revision <= upload.ManifestRevision; revision++ {
		result, err := s.command(ctx, CatalogCommand{Operation: "manifest", UploadID: id, Revision: revision})
		if err != nil {
			return err
		}
		if err := s.storage.Delete(ctx, result.Manifest.BodyKey, ""); err != nil {
			return err
		}
		if _, err := s.storage.Get(ctx, result.Manifest.BodyKey, "", 0); !errors.Is(err, ErrMissing) {
			return errors.Join(ErrStorage, err)
		}
	}
	after := ""
	for {
		candidates, err := s.command(ctx, CatalogCommand{Operation: "manifest_intents", UploadID: id, After: after, Limit: 32})
		if err != nil {
			return err
		}
		for _, intent := range candidates.Intents {
			if err := s.storage.Delete(ctx, intent.BodyKey, intent.StorageVersion); err != nil {
				return err
			}
			if _, err := s.storage.Get(ctx, intent.BodyKey, intent.StorageVersion, 0); !errors.Is(err, ErrMissing) {
				return errors.Join(ErrStorage, err)
			}
		}
		if candidates.Next == "" {
			break
		}
		if candidates.Next == after {
			return ErrIntegrity
		}
		after = candidates.Next
	}
	_, err = s.command(ctx, CatalogCommand{Operation: "finish_delete", UploadID: id, At: s.now().UTC(), Receipt: receipt})
	return err
}

func (s *Service) cleanManifestIntents(ctx context.Context, upload Upload, revision int64, digest string) error {
	id := upload.ArtifactID
	result, err := s.command(ctx, CatalogCommand{Operation: "manifest_intents", UploadID: id, Outcome: "pending", Limit: 32})
	if err != nil {
		return err
	}
	for _, intent := range result.Intents {
		if intent.State == "pending" && intent.Revision == revision && intent.SHA256 == digest {
			continue
		}
		if _, err := s.command(ctx, CatalogCommand{Operation: "begin_manifest_discard", UploadID: id, Intent: intent}); err != nil {
			return err
		}
		if err := s.storage.Delete(ctx, intent.BodyKey, intent.StorageVersion); err != nil {
			return err
		}
		if _, err := s.storage.Get(ctx, intent.BodyKey, intent.StorageVersion, 0); !errors.Is(err, ErrMissing) {
			return errors.Join(ErrStorage, err)
		}
		receipt := s.scopedObjectKey(upload.ProjectID, id, "discarded/"+intent.ID)
		version, err := s.storage.Put(ctx, receipt, []byte(intent.SHA256))
		if err != nil && !errors.Is(err, ErrConflict) {
			return err
		}
		if _, err := s.storage.Get(ctx, receipt, version, 128); err != nil {
			return err
		}
		intent.Receipt = receipt
		if _, err := s.command(ctx, CatalogCommand{Operation: "finish_manifest_discard", UploadID: id, Intent: intent}); err != nil {
			return err
		}
	}
	if result.Next != "" {
		return ErrConflict
	}
	return nil
}
