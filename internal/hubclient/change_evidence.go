package hubclient

import (
	"context"
	"encoding/json"
	"fmt"
	"unicode/utf8"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (e *nativeExecution) retainChangeValidation(ctx context.Context, input *tracker.ChangeVersionInput) error {
	if input.Validation == nil {
		return nil
	}
	bounded := tracker.PublicGateResult(*input.Validation)
	raw, err := json.Marshal(bounded)
	if err != nil {
		return err
	}
	if len(raw) <= 64<<10 {
		return nil
	}
	a := &e.artifacts
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.client == nil {
		stored := *input
		stored.Validation = &bounded
		full, err := json.Marshal(stored)
		if err != nil {
			return err
		}
		if len(full) > operatortool.MaxResultBytes {
			return fmt.Errorf("retain Change validation: %w", artifact.ErrMissing)
		}
		input.Validation = &bounded
		return nil
	}
	raw, err = json.Marshal(input.Validation)
	if err != nil {
		return err
	}
	reservation := a.reservation
	reservation.Kind = "log"
	reservation.Key = e.data.AttemptID + ":validation:" + artifact.Digest(raw)
	reference, err := uploadChangeValidation(ctx, a.client, reservation, raw)
	if err != nil {
		return err
	}
	bounded.Pipeline = nil
	bounded.EvidenceArtifact = &reference
	input.Validation = &bounded
	return nil
}

// validationPartBytes is the largest part a validation receipt upload sends;
// the reservation budgets partial manifests for parts of this size.
const validationPartBytes = 64 << 10

type validationUploader interface {
	Reserve(context.Context, artifact.Reservation) (artifact.Upload, error)
	Append(context.Context, string, artifact.Part) (artifact.Object, error)
	Finalize(context.Context, string, string, *artifact.Capture) (artifact.Reference, error)
}

func uploadChangeValidation(ctx context.Context, uploader validationUploader, reservation artifact.Reservation, raw []byte) (artifact.Reference, error) {
	reservation.Bytes = int64(len(raw)) + artifact.MaxManifestBytes + validationManifestBudget(len(raw), validationPartBytes)
	upload, err := uploader.Reserve(ctx, reservation)
	if err != nil {
		return artifact.Reference{}, fmt.Errorf("reserve Change validation: %w", err)
	}
	if upload.State != "complete" {
		partBytes := validationPartBytes
		if upload.PartLimit > 0 && upload.PartLimit < int64(partBytes) {
			partBytes = int(upload.PartLimit)
			if partBytes <= utf8.UTFMax || int64(len(raw))+validationManifestBudget(len(raw), partBytes) > reservation.Bytes {
				return artifact.Reference{}, fmt.Errorf("store Change validation: part limit %d cannot carry a %d byte receipt within its reservation: %w", upload.PartLimit, len(raw), artifact.ErrQuota)
			}
		}
		for sequence, offset := 0, 0; offset < len(raw); sequence++ {
			end := min(offset+partBytes, len(raw))
			for end < len(raw) && end > offset && !utf8.RuneStart(raw[end]) {
				end--
			}
			if end == offset {
				return artifact.Reference{}, fmt.Errorf("store Change validation: %w", artifact.ErrQuota)
			}
			part := artifact.Part{Sequence: sequence, MediaType: "text/plain; charset=utf-8", Data: raw[offset:end], SHA256: artifact.Digest(raw[offset:end])}
			if _, err := uploader.Append(ctx, upload.ArtifactID, part); err != nil {
				return artifact.Reference{}, fmt.Errorf("store Change validation: %w", err)
			}
			offset = end
		}
	}
	reference, err := uploader.Finalize(ctx, upload.ArtifactID, "complete", nil)
	if err != nil {
		return artifact.Reference{}, fmt.Errorf("finalize Change validation: %w", err)
	}
	if reference.ServiceID != reservation.ServiceID || reference.Scope != reservation.Scope || reference.Kind != "log" || reference.State != "complete" || reference.Availability != "available" || reference.ArtifactID != upload.ArtifactID || reference.Bytes != int64(len(raw)) || !artifact.ValidHash(reference.SHA256, 64) {
		return artifact.Reference{}, artifact.ErrIntegrity
	}
	return reference, nil
}

// validationDescriptorBytes bounds one serialized artifact object descriptor.
const validationDescriptorBytes = 256

// validationManifestBudget bounds the partial manifests a log upload retains:
// every appended part seals a manifest that lists all parts so far.
func validationManifestBudget(payload, partBytes int) int64 {
	parts := payload/(partBytes-utf8.UTFMax) + 2
	var budget int64
	for count := 1; count <= parts; count++ {
		budget += 2 * (2048 + int64(count*validationDescriptorBytes))
	}
	return budget
}
