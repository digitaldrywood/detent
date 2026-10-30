package attachment

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/artifact"
)

type archiveReadStorage struct {
	ArchiveStorage
	open func(context.Context) (io.ReadCloser, error)
}

func (s archiveReadStorage) Open(ctx context.Context, _ string) (io.ReadCloser, error) {
	return s.open(ctx)
}

type interruptedArchiveBody struct {
	data   []byte
	cause  error
	cancel context.CancelFunc
	closed bool
}

func (b *interruptedArchiveBody) Read(p []byte) (int, error) {
	n := copy(p, b.data)
	b.data = b.data[n:]
	if b.cancel != nil {
		b.cancel()
	}
	return n, b.cause
}

func (b *interruptedArchiveBody) Close() error {
	b.closed = true
	return nil
}

func TestArchiveReadFailures(t *testing.T) {
	t.Parallel()
	ref, compressed, err := PrepareArchive("org_example", 1, strings.NewReader("private archive content"))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		cause  error
		want   error
		cancel bool
	}{
		{"partial cancellation", context.Canceled, context.Canceled, true},
		{"partial deadline", context.DeadlineExceeded, context.DeadlineExceeded, false},
		{"canceled EOF", io.EOF, context.Canceled, true},
		{"unreachable", io.ErrClosedPipe, artifact.ErrStorage, false},
		{"wrong digest", nil, artifact.ErrIntegrity, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, reader := range []struct {
				name string
				read func(context.Context, Storage, string, ArchiveReference) ([]byte, error)
			}{
				{"compressed", ReadArchiveObject},
				{"detail", ReadArchive},
				{"content", func(ctx context.Context, storage Storage, org string, ref ArchiveReference) ([]byte, error) {
					return (ArchiveContents{Storage: storage.(ArchiveStorage)}).ReadContent(ctx, org, ref)
				}},
			} {
				t.Run(reader.name, func(t *testing.T) {
					ctx, cancel := context.WithCancel(t.Context())
					defer cancel()
					body := &interruptedArchiveBody{data: bytes.Clone(compressed), cause: test.cause}
					if test.cancel {
						body.cancel = cancel
					}
					wantBytes := int64(5)
					if test.cause == nil {
						body.data[0] ^= 1
						body.cause = io.EOF
						wantBytes = int64(len(compressed))
					} else {
						body.data = body.data[:5]
					}
					store := archiveReadStorage{open: func(context.Context) (io.ReadCloser, error) {
						if test.name == "unreachable" {
							return nil, fmt.Errorf("open object: %w", test.cause)
						}
						return body, nil
					}}
					data, err := reader.read(ctx, store, "org_example", ref)
					if len(data) != 0 || !errors.Is(err, test.want) {
						t.Fatalf("read returned %d bytes, %v; want %v", len(data), err, test.want)
					}
					if test.cause != nil && !errors.Is(test.cause, io.EOF) && !errors.Is(err, test.cause) {
						t.Fatalf("lost read cause: %v", err)
					}
					if test.name == "unreachable" {
						wantBytes = 0
					}
					var readErr *ArchiveReadError
					if !errors.As(err, &readErr) || readErr.BytesRead != wantBytes || readErr.ExpectedSize != ref.CompressedSize {
						t.Fatalf("missing read diagnostics: %+v", readErr)
					}
					if !errors.Is(test.want, artifact.ErrIntegrity) && errors.Is(err, artifact.ErrIntegrity) {
						t.Fatal("incomplete read classified as integrity failure")
					}
					if test.name != "unreachable" && !body.closed {
						t.Fatal("read body was not closed")
					}
				})
			}
		})
	}
}
