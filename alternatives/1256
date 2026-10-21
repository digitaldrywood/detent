package entryauthority

import (
	"context"
	"errors"
	"net/rpc"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/cloudassert"
)

func TestCurrentAuthorityChannel(t *testing.T) {
	t.Parallel()
	binding := Binding{Issuer: "entry", Organization: "org_example", Generation: 2}
	for _, scenario := range []string{"concurrent lookups", "wrong allocation", "expired deadline", "shutdown pending lookup", "stalled reader"} {
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
			})
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
			client := &Client{client: rpc.NewClient(channel{reader: parent.Files[0], writer: parent.Files[1], budget: time.Second}), budget: time.Second}
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
			origin := Origin{Issuer: binding.Issuer, Generation: binding.Generation, Subject: "original"}
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
