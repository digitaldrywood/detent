package entryauthority

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/rpc"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
)

func TestHostedContentUsesCallerDeadline(t *testing.T) {
	binding := Binding{Issuer: "entry", Organization: "org_example", Generation: 1}
	plain := []byte("archive contents")
	ref, compressed, err := attachment.PrepareArchive(binding.Organization, 1, bytes.NewReader(plain))
	if err != nil {
		t.Fatal(err)
	}
	const budget = 3 * time.Second
	reads := []struct {
		name     string
		duration time.Duration
		want     []byte
		read     func(context.Context, *Client) ([]byte, error)
	}{
		{"Content", 20 * time.Second, compressed, func(ctx context.Context, c *Client) ([]byte, error) {
			return c.Content(ctx, ContentRequest{Organization: binding.Organization, Reference: ref})
		}},
		{"ReadArchive", 4 * time.Second, compressed, func(ctx context.Context, c *Client) ([]byte, error) {
			response, err := c.ReadArchive(ctx, ref, false)
			return response.Data, err
		}},
		{"ReadContent", 4 * time.Second, plain, func(ctx context.Context, c *Client) ([]byte, error) {
			return c.ReadContent(ctx, binding.Organization, ref)
		}},
		{"ReadDetail", 4 * time.Second, plain, func(ctx context.Context, c *Client) ([]byte, error) {
			return c.ReadDetail(ctx, binding.Organization, ref)
		}},
		{"ReadCanonicalDetail", 4 * time.Second, plain, func(ctx context.Context, c *Client) ([]byte, error) {
			return c.ReadCanonicalDetail(ctx, binding.Organization, "prj_example", ref.Key)
		}},
		{"ReadDetails", 4 * time.Second, plain, func(ctx context.Context, c *Client) ([]byte, error) {
			rows, err := c.ReadDetails(ctx, binding.Organization, []attachment.ArchiveReference{ref})
			if err != nil {
				return nil, err
			}
			return rows[0].Bytes()
		}},
	}
	for _, read := range reads {
		for _, tc := range []struct {
			name                     string
			caller, cancelAt, finish time.Duration
			wantErr                  error
		}{
			{name: "long read", caller: 30 * time.Second, finish: read.duration},
			{name: "in-flight cancellation", caller: 30 * time.Second, cancelAt: time.Second, finish: time.Second, wantErr: context.Canceled},
			{name: "short caller deadline", caller: 2 * time.Second, finish: 2 * time.Second, wantErr: context.DeadlineExceeded},
			{name: "negotiated fallback", finish: budget, wantErr: context.DeadlineExceeded},
		} {
			t.Run(read.name+"/"+tc.name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					serverCtx, stopServer := context.WithCancel(t.Context())
					defer stopServer()
					ctx, cancel := context.WithCancel(t.Context())
					defer cancel()
					started := time.Now()
					if tc.caller > 0 {
						var stopDeadline context.CancelFunc
						ctx, stopDeadline = context.WithTimeout(ctx, tc.caller)
						defer stopDeadline()
					}
					received := make(chan time.Time, 1)
					waitForRead := func(ctx context.Context) error {
						deadline, _ := ctx.Deadline()
						received <- deadline
						if tc.cancelAt > 0 {
							timer := time.AfterFunc(tc.cancelAt, cancel)
							defer timer.Stop()
						}
						timer := time.NewTimer(read.duration)
						defer timer.Stop()
						select {
						case <-ctx.Done():
							<-serverCtx.Done()
							return ctx.Err()
						case <-timer.C:
							return nil
						}
					}
					store := deadlineObjectStore{read: func(ctx context.Context) ([]byte, error) {
						return plain, waitForRead(ctx)
					}}
					authority := newAuthorityServer(serverCtx, binding, budget, nil, []Archives{{
						Contents: store, Details: store,
						Content: func(ctx context.Context, _ ContentRequest) ([]byte, error) {
							return compressed, waitForRead(ctx)
						},
						Reader: func(ctx context.Context, _ attachment.ArchiveReference, _ bool) (ArchiveResponse, error) {
							return ArchiveResponse{Data: compressed}, waitForRead(ctx)
						},
					}})
					server := rpc.NewServer()
					if err := server.RegisterName("Authority", authority); err != nil {
						t.Fatal(err)
					}
					serverConn, clientConn := net.Pipe()
					serverDone := make(chan struct{})
					go func() {
						defer close(serverDone)
						server.ServeConn(serverConn)
					}()
					client := &Client{client: rpc.NewClient(clientConn), budget: budget, organization: binding.Organization}
					defer func() {
						stopServer()
						if err := client.Close(); err != nil {
							t.Error(err)
						}
						<-serverDone
					}()
					data, err := read.read(ctx, client)
					if !errors.Is(err, tc.wantErr) || tc.wantErr == nil && !bytes.Equal(data, read.want) {
						t.Errorf("read returned %d bytes and %v, want error %v", len(data), err, tc.wantErr)
					}
					if elapsed := time.Since(started); elapsed != tc.finish {
						t.Errorf("read took %v, want %v", elapsed, tc.finish)
					}
					select {
					case got := <-received:
						want := tc.caller
						if want == 0 {
							want = budget
						}
						if !got.Equal(started.Add(want)) {
							t.Errorf("authority deadline = %v, want %v", got, started.Add(want))
						}
					default:
						t.Error("read did not reach the authority")
					}
				})
			})
		}
	}
}

type deadlineObjectStore struct {
	attachment.ArchiveContentStore
	attachment.DetailStore
	read func(context.Context) ([]byte, error)
}

func (s deadlineObjectStore) ReadContent(ctx context.Context, _ string, _ attachment.ArchiveReference) ([]byte, error) {
	return s.read(ctx)
}

func (s deadlineObjectStore) ReadDetail(ctx context.Context, _ string, _ attachment.ArchiveReference) ([]byte, error) {
	return s.read(ctx)
}

func (s deadlineObjectStore) ReadCanonicalDetail(ctx context.Context, _, _, _ string) ([]byte, error) {
	return s.read(ctx)
}

type contentObjectStore struct {
	attachment.ArchiveStorage
	attachment.DetailStore
	open func(context.Context) (io.ReadCloser, error)
}

func (s contentObjectStore) Open(ctx context.Context, _ string) (io.ReadCloser, error) {
	return s.open(ctx)
}

func (s contentObjectStore) ReadDetail(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	return attachment.ReadArchive(ctx, s, organization, ref)
}

type contentReadFailure struct{ cause error }

func (r contentReadFailure) Read([]byte) (int, error) { return 0, r.cause }

type contentStorageTransport func(*http.Request) (*http.Response, error)

func (f contentStorageTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

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
		{"missing S3 object", artifact.ErrMissing, artifact.ErrMissing, "missing", 0},
		{"canceled integrity wrapper", errors.Join(artifact.ErrIntegrity, context.Canceled), context.Canceled, "canceled", 5},
	} {
		for _, method := range []string{"Content", "ReadContent", "ReadDetail"} {
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
				if test.name == "missing S3 object" {
					var mu sync.Mutex
					objects := make(map[string][]byte)
					storage, err := attachment.NewStorage(t.Context(), attachment.Config{
						Endpoint: "https://objects.example.test", Region: "test", Bucket: "private",
						AccessKeyID: "fixture-access", SecretAccessKey: "fixture-secret",
					}, contentStorageTransport(func(request *http.Request) (*http.Response, error) {
						mu.Lock()
						defer mu.Unlock()
						status, body := http.StatusOK, ""
						switch request.Method {
						case http.MethodPut:
							data, err := io.ReadAll(request.Body)
							if err != nil {
								return nil, err
							}
							objects[request.URL.Path] = data
						case http.MethodDelete:
							delete(objects, request.URL.Path)
						case http.MethodGet:
							switch {
							case request.Header.Get("Authorization") == "":
								status = http.StatusForbidden
							default:
								data, ok := objects[request.URL.Path]
								body = string(data)
								if !ok {
									status, body = http.StatusNotFound, `<Error><Code>NoSuchKey</Code></Error>`
								}
							}
						}
						return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
					}))
					if err != nil {
						t.Fatal(err)
					}
					store.ArchiveStorage = storage.(attachment.ArchiveStorage)
					store.open = func(ctx context.Context) (io.ReadCloser, error) {
						return storage.Open(ctx, ref.Key)
					}
				}
				parent, err := Start(t.Context(), binding, time.Second, nil, Archives{
					Details:  store,
					Contents: attachment.ArchiveContents{Storage: store},
					Content: func(ctx context.Context, request ContentRequest) ([]byte, error) {
						if request.Write {
							return nil, attachment.StoreArchive(ctx, store, request.Organization, request.Reference, request.Data)
						}
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
				switch method {
				case "Content":
					data, err = client.Content(t.Context(), ContentRequest{Organization: binding.Organization, Reference: ref})
				case "ReadContent":
					data, err = client.ReadContent(t.Context(), binding.Organization, ref)
				case "ReadDetail":
					data, err = client.ReadDetail(t.Context(), binding.Organization, ref)
				}
				if len(data) != 0 || !errors.Is(err, test.want) || errors.Is(err, artifact.ErrMissing) != errors.Is(test.want, artifact.ErrMissing) {
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
				if test.name == "missing S3 object" {
					if !errors.Is(err, artifact.ErrMissing) {
						t.Fatal("missing RPC classification prevented first upload")
					}
					if _, err := client.Content(t.Context(), ContentRequest{Organization: binding.Organization, Reference: ref, Data: compressed, Write: true}); err != nil {
						t.Fatalf("first upload: %v", err)
					}
					stored, err := client.Content(t.Context(), ContentRequest{Organization: binding.Organization, Reference: ref})
					if err != nil || !bytes.Equal(stored, compressed) {
						t.Fatalf("published compressed content: %v", err)
					}
					for _, read := range []func(context.Context, string, attachment.ArchiveReference) ([]byte, error){client.ReadContent, client.ReadDetail} {
						stored, err := read(t.Context(), binding.Organization, ref)
						if err != nil || string(stored) != secret {
							t.Fatalf("published content: %v", err)
						}
					}
				}
			})
		}
	}
}
