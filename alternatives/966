package entryauthority

import (
	"context"
	"errors"
	"log/slog"
	"net/rpc"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
)

func contentFailure(request ContentRequest, started time.Time, message string, err error) error {
	reason := artifact.ErrStorage
	code := reason.Error()
	switch {
	case errors.Is(err, context.Canceled):
		reason, code = context.Canceled, "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		reason, code = context.DeadlineExceeded, "canceled"
	case errors.Is(err, artifact.ErrIntegrity):
		reason, code = artifact.ErrIntegrity, artifact.ErrIntegrity.Error()
	}
	bytesRead := int64(-1)
	var readErr *attachment.ArchiveReadError
	if errors.As(err, &readErr) {
		bytesRead = readErr.BytesRead
	}
	slog.Error(message,
		"organization", request.Organization,
		"reference_id", request.Reference.Key,
		"reason", code,
		"bytes_read", bytesRead,
		"expected_size", request.Reference.CompressedSize,
		"elapsed", time.Since(started),
		"error", err)
	return reason
}

func contentClientError(err error) error {
	var remote rpc.ServerError
	if !errors.As(err, &remote) {
		return err
	}
	for _, reason := range []error{context.Canceled, context.DeadlineExceeded, artifact.ErrStorage, artifact.ErrIntegrity} {
		if string(remote) == reason.Error() {
			return reason
		}
	}
	return err
}
