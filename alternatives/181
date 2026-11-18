package artifact

import (
	"context"
	"errors"
	"time"
)

type blobWrite struct{ kind, state, id, version string }

func (w blobWrite) command(c CatalogCommand, operation string) CatalogCommand {
	c.Operation = operation + "_" + w.kind + "_write"
	if w.kind == "object" {
		c.Object.WriteID = w.id
		c.Object.StorageVersion = w.version
	} else {
		c.Intent.WriteID = w.id
		c.Intent.StorageVersion = w.version
	}
	return c
}
func (w blobWrite) result(r CatalogResult) blobWrite {
	if w.kind == "object" {
		w.state, w.id, w.version = r.Object.WriteState, r.Object.WriteID, r.Object.StorageVersion
	} else {
		w.state, w.id, w.version = r.Intent.WriteState, r.Intent.WriteID, r.Intent.StorageVersion
	}
	return w
}
func (s *Service) finishBlobWrite(ctx context.Context, c CatalogCommand, w blobWrite, outcome string) (CatalogResult, error) {
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	c = w.command(c, "finish")
	c.Outcome = outcome
	return s.command(finishCtx, c)
}
func (s *Service) readBlob(ctx context.Context, key, version string, limit int64) ([]byte, string, error) {
	if storage, ok := s.storage.(VersionedStorage); ok {
		return storage.GetVersion(ctx, key, version, limit)
	}
	data, err := s.storage.Get(ctx, key, version, limit)
	if err != nil {
		return nil, "", err
	}
	if version == "" {
		return nil, "", ErrUnsupported
	}
	return data, version, nil
}
func (s *Service) putBlobOnce(ctx context.Context, c CatalogCommand, key string, data []byte, w blobWrite) (CatalogResult, error) {
	var result CatalogResult
	if w.state == "ready" {
		w.id = NewID("write")
		var err error
		result, err = s.command(ctx, w.command(c, "begin"))
		if err != nil {
			_, abortErr := s.finishBlobWrite(ctx, c, w, "rejected")
			return result, errors.Join(err, abortErr)
		}
		w = w.result(result)
		if err := ctx.Err(); err != nil {
			_, abortErr := s.finishBlobWrite(ctx, c, w, "rejected")
			return result, errors.Join(err, abortErr)
		}
		version, putErr := s.storage.Put(ctx, key, data)
		outcome := "written"
		if putErr != nil && !errors.Is(putErr, ErrConflict) {
			outcome = "uncertain"
			if errors.Is(putErr, ErrWriteRejected) {
				outcome = "rejected"
			}
		}
		w.version = version
		if outcome == "written" && w.version == "" {
			w.version = "null"
		}
		settled, settleErr := s.finishBlobWrite(ctx, c, w, outcome)
		if putErr != nil && !errors.Is(putErr, ErrConflict) {
			return settled, errors.Join(putErr, settleErr)
		}
		if settleErr != nil {
			return settled, settleErr
		}
		result = settled
		w = w.result(result)
	}
	if w.state != "written" && w.state != "writing" && w.state != "uncertain" {
		return result, ErrConflict
	}
	read, version, err := s.readBlob(ctx, key, w.version, int64(len(data)))
	if err != nil {
		if errors.Is(err, ErrMissing) {
			return result, ErrConflict
		}
		return result, err
	}
	if s.config.Storage.RequireVersioning && version == "null" {
		return result, ErrUnsupported
	}
	if len(read) != len(data) || Digest(read) != Digest(data) {
		return result, ErrIntegrity
	}
	if w.state != "written" {
		w.version = version
		result, err = s.finishBlobWrite(ctx, c, w, "written")
		if err != nil {
			return result, err
		}
	} else if w.kind == "object" {
		result.Object = c.Object
		result.Object.WriteState = w.state
		result.Object.WriteID = w.id
		result.Object.StorageVersion = w.version
	} else {
		result.Intent = c.Intent
		result.Intent.WriteState = w.state
		result.Intent.WriteID = w.id
		result.Intent.StorageVersion = w.version
	}
	return result, nil
}

func (s *Service) settleObjectForCleanup(ctx context.Context, u Upload, o CatalogObject) (CatalogObject, error) {
	c := CatalogCommand{UploadID: u.ArtifactID, Object: o}
	w := blobWrite{kind: "object", state: o.WriteState, id: o.WriteID, version: o.StorageVersion}
	if w.state == "ready" {
		w.id = NewID("write")
		r, err := s.finishBlobWrite(ctx, c, w, "rejected")
		return r.Object, err
	}
	if w.state == "written" || w.state == "rejected" {
		return o, nil
	}
	data, version, err := s.readBlob(ctx, s.scopedObjectKey(u.ProjectID, u.ArtifactID, o.ID), o.StorageVersion, o.Size)
	if err != nil {
		if errors.Is(err, ErrMissing) {
			return o, ErrConflict
		}
		return o, err
	}
	if int64(len(data)) != o.Size || Digest(data) != o.SHA256 {
		return o, ErrIntegrity
	}
	w.version = version
	r, err := s.finishBlobWrite(ctx, c, w, "written")
	return r.Object, err
}
func (s *Service) settleIntentForCleanup(ctx context.Context, u Upload, i CatalogIntent) (CatalogIntent, error) {
	c := CatalogCommand{UploadID: u.ArtifactID, Intent: i}
	w := blobWrite{kind: "manifest", state: i.WriteState, id: i.WriteID, version: i.StorageVersion}
	if w.state == "ready" {
		w.id = NewID("write")
		r, err := s.finishBlobWrite(ctx, c, w, "rejected")
		return r.Intent, err
	}
	if w.state == "written" || w.state == "rejected" {
		return i, nil
	}
	data, version, err := s.readBlob(ctx, i.BodyKey, i.StorageVersion, i.Bytes)
	if err != nil {
		if errors.Is(err, ErrMissing) {
			return i, ErrConflict
		}
		return i, err
	}
	if int64(len(data)) != i.Bytes || Digest(data) != i.SHA256 {
		return i, ErrIntegrity
	}
	w.version = version
	r, err := s.finishBlobWrite(ctx, c, w, "written")
	return r.Intent, err
}
