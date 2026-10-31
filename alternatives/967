package entryauthority

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/rpc"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
)

type contentObjectStore struct {
	attachment.ArchiveStorage
	open func(context.Context) (io.ReadCloser, error)
}

func (s contentObjectStore) Open(ctx context.Context, _ string) (io.ReadCloser, error) {
	return s.open(ctx)
}

type contentReadFailure struct{ cause error }

func (r contentReadFailure) Read([]byte) (int, error) { return 0, r.cause }

func TestContentFailureReasonAndLog(t *testing.T) {
	const secret = "private content bytes must never appear"
	binding := Binding{Issuer: "https://entry.example.test", Organization: "org_example", Generation: 1}
	ref, compressed, err := attachment.PrepareArchive(binding.Organization, 1, strings.NewReader(secret))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		cause  error
		want   error
		reason string
		bytes  int
	}{
		{"partial cancellation", context.Canceled, context.Canceled, "canceled", 5},
		{"partial deadline", context.DeadlineExceeded, context.DeadlineExceeded, "canceled", 5},
		{"wrong digest", nil, artifact.ErrIntegrity, "checksum_mismatch", len(compressed)},
		{"unreachable", io.ErrClosedPipe, artifact.ErrStorage, "storage_unreachable", 0},
		{"canceled integrity wrapper", errors.Join(artifact.ErrIntegrity, context.Canceled), context.Canceled, "canceled", 5},
	} {
		for _, method := range []string{"Content", "ReadContent"} {
			t.Run(test.name+"/"+method, func(t *testing.T) {
				var logs bytes.Buffer
				previous := slog.Default()
				slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
				t.Cleanup(func() { slog.SetDefault(previous) })
				store := contentObjectStore{open: func(context.Context) (io.ReadCloser, error) {
					if test.name == "unreachable" {
						return nil, fmt.Errorf("open object: %w", test.cause)
					}
					data := bytes.Clone(compressed[:test.bytes])
					if test.cause == nil {
						data[0] ^= 1
						return io.NopCloser(bytes.NewReader(data)), nil
					}
					return io.NopCloser(io.MultiReader(bytes.NewReader(data), contentReadFailure{fmt.Errorf("read object: %w", test.cause)})), nil
				}}
				parent, err := Start(t.Context(), binding, time.Second, nil, Archives{
					Contents: attachment.ArchiveContents{Storage: store},
					Content: func(ctx context.Context, request ContentRequest) ([]byte, error) {
						return attachment.ReadArchiveObject(ctx, store, request.Organization, request.Reference)
					},
				})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = parent.Close() })
				client := &Client{client: rpc.NewClient(channel{reader: parent.Files[0], writer: parent.Files[1], budget: time.Second}), budget: time.Second, organization: binding.Organization}
				t.Cleanup(func() { _ = client.Close() })
				var data []byte
				if method == "Content" {
					data, err = client.Content(t.Context(), ContentRequest{Organization: binding.Organization, Reference: ref})
				} else {
					data, err = client.ReadContent(t.Context(), binding.Organization, ref)
				}
				if len(data) != 0 || !errors.Is(err, test.want) {
					t.Errorf("client returned %d bytes, %v; want %v", len(data), err, test.want)
				}
				var record map[string]any
				decoder := json.NewDecoder(&logs)
				if err := decoder.Decode(&record); err != nil {
					t.Fatalf("decode log: %v", err)
				}
				for key, want := range map[string]any{
					"organization": binding.Organization, "reference_id": ref.Key, "reason": test.reason,
					"bytes_read": float64(test.bytes), "expected_size": float64(ref.CompressedSize),
				} {
					if record[key] != want {
						t.Errorf("log %s = %v, want %v", key, record[key], want)
					}
				}
				if elapsed, ok := record["elapsed"].(float64); !ok || elapsed < 0 {
					t.Error("missing elapsed time")
				}
				cause, ok := record["error"].(string)
				if !ok || cause == "" || (test.cause != nil && !strings.Contains(cause, test.cause.Error())) {
					t.Errorf("missing cause: %v", record["error"])
				}
				if test.cause != nil && !strings.Contains(cause, "object:") {
					t.Errorf("lost wrapped operation: %v", cause)
				}
				if strings.Contains(cause, secret) || strings.Contains(fmt.Sprint(record), secret) || (err != nil && strings.Contains(err.Error(), secret)) {
					t.Error("content leaked through diagnostics")
				}
				if err := decoder.Decode(&record); !errors.Is(err, io.EOF) {
					t.Errorf("expected one error log, got %v", err)
				}
			})
		}
	}
}
