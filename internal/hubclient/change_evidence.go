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
	reservation.Bytes = int64(len(raw)) + artifact.MaxManifestBytes
	upload, err := a.client.Reserve(ctx, reservation)
	if err != nil {
		return fmt.Errorf("reserve Change validation: %w", err)
	}
	if upload.State != "complete" {
		partBytes := 64 << 10
		if upload.PartLimit > 0 {
			partBytes = int(min(int64(partBytes), upload.PartLimit))
		}
		for sequence, offset := 0, 0; offset < len(raw); sequence++ {
			end := min(offset+partBytes, len(raw))
			for end < len(raw) && end > offset && !utf8.RuneStart(raw[end]) {
				end--
			}
			if end == offset {
				return fmt.Errorf("store Change validation: %w", artifact.ErrQuota)
			}
			part := artifact.Part{Sequence: sequence, MediaType: "text/plain; charset=utf-8", Data: raw[offset:end], SHA256: artifact.Digest(raw[offset:end])}
			if _, err := a.client.Append(ctx, upload.ArtifactID, part); err != nil {
				return fmt.Errorf("store Change validation: %w", err)
			}
			offset = end
		}
	}
	reference, err := a.client.Finalize(ctx, upload.ArtifactID, "complete", nil)
	if err != nil {
		return fmt.Errorf("finalize Change validation: %w", err)
	}
	if reference.ServiceID != reservation.ServiceID || reference.Scope != reservation.Scope || reference.Kind != "log" || reference.State != "complete" || reference.Availability != "available" || reference.ArtifactID != upload.ArtifactID || reference.Bytes != int64(len(raw)) || !artifact.ValidHash(reference.SHA256, 64) {
		return artifact.ErrIntegrity
	}
	bounded.Pipeline = nil
	bounded.EvidenceArtifact = &reference
	input.Validation = &bounded
	return nil
}
