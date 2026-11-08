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

func (s *Service) discardPending(ctx context.Context, id string) error {
	result, err := s.command(ctx, CatalogCommand{Operation: "objects", UploadID: id, Limit: MaxObjects})
	if err != nil {
		return err
	}
	for _, obj := range result.Objects {
		if obj.Verified || obj.Discarded {
			continue
		}
		key := s.objectKey(id, obj.ID)
		if err := s.storage.Delete(ctx, key, obj.StorageVersion); err != nil {
			return err
		}
		if _, err := s.storage.Get(ctx, key, obj.StorageVersion, 0); !errors.Is(err, ErrMissing) {
			return errors.Join(ErrStorage, err)
		}
		receipt := s.objectKey(id, "discarded/"+obj.ID)
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
	result, err := s.command(ctx, CatalogCommand{Operation: "retention", At: now, Before: now.Add(-time.Duration(s.config.Policy.DeletionRecordSeconds) * time.Second), Limit: 32})
	if err != nil {
		return err
	}
	for _, upload := range result.Uploads {
		id := upload.ArtifactID
		if upload.State == "deleted" {
			if _, err := s.command(ctx, CatalogCommand{Operation: "prune", UploadID: id, Before: now.Add(-time.Duration(s.config.Policy.DeletionRecordSeconds) * time.Second)}); err != nil {
				return err
			}
			continue
		}
		if upload.State == "uploading" && now.Before(upload.ExpiresAt) && upload.Kind == "log" {
			if _, err := s.seal(ctx, id, "interrupted", nil); err != nil {
				return err
			}
			continue
		}
		if err := s.delete(ctx, id); err != nil {
			return err
		}
	}
	if _, err := s.command(ctx, CatalogCommand{Operation: "prune_traffic", Before: now.Add(-time.Duration(s.trafficRetention.Load()) * time.Second), Limit: 32}); err != nil {
		return err
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
	receipt := s.objectKey(id, "deleted")
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
		key := s.objectKey(id, obj.ID)
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
	_, err = s.command(ctx, CatalogCommand{Operation: "finish_delete", UploadID: id, At: s.now().UTC(), Receipt: receipt})
	return err
}
