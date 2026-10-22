package entryauthority

import (
	"bytes"
	"context"
	"errors"
	"net/rpc"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/cloudassert"
)

func TestArchiveAuthorityChannel(t *testing.T) {
	for _, scenario := range []string{"read", "signed read", "foreign organization", "expired deadline", "oversized section"} {
		t.Run(scenario, func(t *testing.T) {
			binding := Binding{Issuer: "entry", Organization: "org_example", Generation: 2}
			ref, data, err := attachment.PrepareArchive(binding.Organization, 1, strings.NewReader(`{"schema_version":1}`))
			if err != nil {
				t.Fatal(err)
			}
			var reads atomic.Int32
			parent, err := Start(t.Context(), binding, time.Second, nil, Archives{Contents: &authorityContents{content: map[string][]byte{}}, Reader: func(_ context.Context, got attachment.ArchiveReference, presign bool) (ArchiveResponse, error) {
				reads.Add(1)
				if got != ref {
					t.Error("archive reference changed in transit")
				}
				response := ArchiveResponse{Data: data}
				if presign {
					response.Access = &attachment.ArchiveRead{ArchiveReference: ref, URL: "https://objects.example.test/section", ExpiresAt: time.Now().Add(attachment.ArchiveReadLifetime)}
				}
				return response, nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := parent.Close(); err != nil {
					t.Error(err)
				}
			})
			client := &Client{client: rpc.NewClient(channel{reader: parent.Files[0], writer: parent.Files[1], budget: time.Second}), budget: time.Second, organization: binding.Organization}
			t.Cleanup(func() {
				if err := client.Close(); err != nil {
					t.Error(err)
				}
			})
			request := ref
			ctx := t.Context()
			if scenario == "foreign organization" {
				request, _, err = attachment.PrepareArchive("org_other", 1, strings.NewReader(`{"schema_version":1}`))
				if err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "oversized section" {
				request.UncompressedSize = attachment.MaxSectionBytes + 1
			}
			if scenario == "expired deadline" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer cancel()
			}
			response, err := client.ReadArchive(ctx, request, scenario == "signed read")
			if scenario == "foreign organization" || scenario == "expired deadline" || scenario == "oversized section" {
				if err == nil || reads.Load() != 0 {
					t.Fatalf("rejected read reached storage: reads=%d error=%v", reads.Load(), err)
				}
				return
			}
			if err != nil || string(response.Data) != string(data) || reads.Load() != 1 || (response.Access != nil) != (scenario == "signed read") {
				t.Fatalf("archive RPC failed: reads=%d error=%v", reads.Load(), err)
			}
			content := []byte("conversation content")
			contentRef, err := client.PublishContent(ctx, binding.Organization, content)
			if err != nil {
				t.Fatal(err)
			}
			read, err := client.ReadContent(ctx, binding.Organization, contentRef)
			if err != nil || !bytes.Equal(read, content) {
				t.Fatalf("combined archive channel lost conversation content: %v", err)
			}
		})
	}
}

func TestCurrentAuthorityChannel(t *testing.T) {
	t.Parallel()
	binding := Binding{Issuer: "entry", Organization: "org_example", Generation: 2}
	for _, scenario := range []string{"detail round trip", "detail owner unavailable", "concurrent lookups", "wrong allocation", "expired deadline", "shutdown pending lookup", "stalled reader", "archive content", "content read and write", "foreign content"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			if scenario == "stalled reader" {
				serverRead, clientWrite, err := os.Pipe()
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = serverRead.Close() })
				clientRead, serverWrite, err := os.Pipe()
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = serverWrite.Close() })
				client := &Client{client: rpc.NewClient(channel{reader: clientRead, writer: clientWrite, budget: 2 * time.Second}), budget: 4 * time.Second}
				t.Cleanup(func() { _ = client.Close() })
				payload := strings.Repeat("x", 1<<20)
				ctx, cancel := context.WithCancel(t.Context())
				t.Cleanup(cancel)
				canceledResult := make(chan error, 1)
				go func() {
					_, err := callAuthority[cloudassert.Claims](ctx, client, "Resolve", payload)
					canceledResult <- err
				}()
				if err := serverRead.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
					t.Fatal(err)
				}
				if _, err := serverRead.Read(make([]byte, 1)); err != nil {
					t.Fatalf("request never entered stalled pipe: %v", err)
				}
				deadlineResult := make(chan error, 1)
				go func() {
					ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
					defer cancel()
					_, err := callAuthority[cloudassert.Claims](ctx, client, "Resolve", payload)
					deadlineResult <- err
				}()
				cancel()
				for _, pending := range []struct {
					result <-chan error
					want   error
				}{
					{canceledResult, context.Canceled},
					{deadlineResult, context.DeadlineExceeded},
				} {
					select {
					case err := <-pending.result:
						if !errors.Is(err, pending.want) {
							t.Fatalf("blocked send returned %v, want %v", err, pending.want)
						}
					case <-time.After(time.Second):
						t.Fatal("blocked send escaped caller deadline")
					}
				}
				_, err = callAuthority[cloudassert.Claims](t.Context(), client, "Resolve", "after write failure")
				if err == nil || errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("write failure did not close channel: %v", err)
				}
				return
			}
			entered := make(chan struct{}, 1)
			details := &channelDetailStore{fail: scenario == "detail owner unavailable"}
			parent, err := Start(t.Context(), binding, time.Second, func(ctx context.Context, origin Origin) (cloudassert.Claims, error) {
				if scenario == "shutdown pending lookup" {
					entered <- struct{}{}
					<-ctx.Done()
					return cloudassert.Claims{}, ctx.Err()
				}
				if err := ctx.Err(); err != nil {
					return cloudassert.Claims{}, err
				}
				return cloudassert.Claims{Subject: origin.Subject, Audience: binding.Organization}, nil
			}, Archives{Details: details, Contents: &authorityContents{content: map[string][]byte{}}, Content: func(ctx context.Context, request ContentRequest) ([]byte, error) {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				if request.Write {
					return request.Data, nil
				}
				return []byte("stored object"), nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			var closeOnce sync.Once
			closeParent := func() {
				closeOnce.Do(func() {
					if err := parent.Close(); err != nil {
						t.Error(err)
					}
				})
			}
			t.Cleanup(closeParent)
			client := &Client{client: rpc.NewClient(channel{reader: parent.Files[0], writer: parent.Files[1], budget: time.Second}), budget: time.Second, organization: binding.Organization}
			t.Cleanup(func() { _ = client.Close() })
			selected := binding
			if scenario == "wrong allocation" {
				selected.Organization = "org_other"
			}
			handshake, err := callAuthority[Handshake](t.Context(), client, "Handshake", selected)
			if scenario == "wrong allocation" {
				if err == nil {
					t.Fatal("foreign allocation handshake accepted")
				}
				return
			}
			if err != nil || handshake.Binding != binding {
				t.Fatalf("handshake: %+v %v", handshake, err)
			}

			if scenario == "archive content" {
				content := []byte(strings.Repeat("immutable conversation detail ", 100))
				ref, err := client.PublishContent(t.Context(), binding.Organization, content)
				if err != nil {
					t.Fatal(err)
				}
				read, err := client.ReadContent(t.Context(), binding.Organization, ref)
				if err != nil || !bytes.Equal(read, content) {
					t.Fatalf("archive round trip: %v", err)
				}
				if _, err := client.PublishContent(t.Context(), "org_other", content); err == nil {
					t.Fatal("foreign upload accepted")
				}
				if _, err := client.ReadContent(t.Context(), "org_other", ref); err == nil {
					t.Fatal("foreign read accepted")
				}
				foreign, _, err := attachment.PrepareArchive("org_other", 1, bytes.NewReader(content))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := client.ReadContent(t.Context(), binding.Organization, foreign); err == nil {
					t.Fatal("foreign reference accepted")
				}
				return
			}
			if scenario == "detail round trip" || scenario == "detail owner unavailable" {
				content := []byte("run checkpoint and validation output")
				ref, err := client.StoreDetail(t.Context(), binding.Organization, content)
				if scenario == "detail owner unavailable" {
					if err == nil || strings.Contains(err.Error(), "fixture-secret") {
						t.Fatalf("unsafe upload failure: %v", err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				read, err := client.ReadDetail(t.Context(), binding.Organization, ref)
				if err != nil || !bytes.Equal(read, content) {
					t.Fatalf("detail round trip: %v", err)
				}
				if _, err := callAuthority[[]byte](t.Context(), client, "ReadDetail", DetailRequest{Organization: "org_foreign", Reference: ref, Deadline: time.Now().Add(time.Second)}); err == nil {
					t.Fatal("parent accepted foreign organization detail")
				}
				if _, err := callAuthority[attachment.ArchiveReference](t.Context(), client, "StoreDetail", DetailRequest{Organization: "org_foreign", Data: content, Deadline: time.Now().Add(time.Second)}); err == nil {
					t.Fatal("parent accepted foreign upload")
				}
				return
			}
			origin := Origin{Issuer: binding.Issuer, Generation: binding.Generation, Subject: "original"}
			if scenario == "content read and write" || scenario == "foreign content" {
				ref, data, err := attachment.PrepareArchive(binding.Organization, 1, strings.NewReader("durable content"))
				if err != nil {
					t.Fatal(err)
				}
				request := ContentRequest{Organization: binding.Organization, Reference: ref, Write: true, Data: data}
				if scenario == "foreign content" {
					request.Organization = "org_other"
				}
				result, err := client.Content(t.Context(), request)
				if scenario == "foreign content" {
					if err == nil {
						t.Fatal("foreign content accepted")
					}
					return
				}
				if err != nil || string(result) != string(data) {
					t.Fatalf("content write: %v", err)
				}
				request.Write = false
				request.Data = nil
				result, err = client.Content(t.Context(), request)
				if err != nil || string(result) != "stored object" {
					t.Fatalf("content read: %v", err)
				}
				return
			}

			if scenario == "expired deadline" {
				claims, err := callAuthority[cloudassert.Claims](t.Context(), client, "Resolve", Request{Origin: origin, Deadline: time.Now().Add(-time.Second)})
				if err == nil || claims.Subject != "" {
					t.Fatalf("expired lookup returned authority: %+v %v", claims, err)
				}
				return
			}
			if scenario == "shutdown pending lookup" {
				result := make(chan error, 1)
				go func() { _, err := client.Resolve(t.Context(), origin); result <- err }()
				select {
				case <-entered:
				case <-time.After(time.Second):
					t.Fatal("lookup never entered current owner")
				}
				closeParent()
				if err := <-result; err == nil {
					t.Fatal("closed child channel returned authority")
				}
				return
			}
			var group sync.WaitGroup
			for _, subject := range []string{"first", "second", "third"} {
				group.Go(func() {
					request := origin
					request.Subject = subject
					claims, err := client.Resolve(t.Context(), request)
					if err != nil || claims.Subject != subject || claims.Audience != binding.Organization {
						t.Errorf("mixed concurrent authority: %+v %v", claims, err)
					}
				})
			}
			group.Wait()
			cancelled, cancel := context.WithCancel(t.Context())
			cancel()
			if _, err := client.Resolve(cancelled, origin); !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled lookup: %v", err)
			}
			if claims, err := client.Resolve(t.Context(), origin); err != nil || claims.Subject != origin.Subject {
				t.Fatalf("one cancellation closed shared channel: %+v %v", claims, err)
			}
		})
	}
}

type authorityContents struct{ content map[string][]byte }

func (s *authorityContents) PublishContent(ctx context.Context, org string, content []byte) (attachment.ArchiveReference, error) {
	if err := ctx.Err(); err != nil {
		return attachment.ArchiveReference{}, err
	}
	ref, _, err := attachment.PrepareArchive(org, 1, bytes.NewReader(content))
	if err != nil {
		return ref, err
	}
	s.content[ref.Key] = bytes.Clone(content)
	return ref, nil
}
func (s *authorityContents) ReadContent(ctx context.Context, _ string, ref attachment.ArchiveReference) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return bytes.Clone(s.content[ref.Key]), nil
}

type channelDetailStore struct {
	data      []byte
	reference attachment.ArchiveReference
	fail      bool
}

func (s *channelDetailStore) StoreDetail(_ context.Context, organization string, data []byte) (attachment.ArchiveReference, error) {
	if s.fail {
		return attachment.ArchiveReference{}, errors.New("fixture-secret upload failure")
	}
	ref, _, err := attachment.PrepareArchive(organization, 1, bytes.NewReader(data))
	if err != nil {
		return ref, err
	}
	s.data, s.reference = bytes.Clone(data), ref
	return ref, nil
}

func (s *channelDetailStore) ReadDetail(_ context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	if ref.Validate(organization) != nil || ref != s.reference {
		return nil, attachment.ErrInvalid
	}
	return bytes.Clone(s.data), nil
}
