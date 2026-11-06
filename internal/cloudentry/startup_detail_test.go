package cloudentry

import (
	"bytes"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
)

func TestTenantDetailStoreWritesBeforeTenantHTTPStarts(t *testing.T) {
	store := newSpacesFixture(t, false)
	var tenantCalls atomic.Int64
	f := newEntryFixture(t, entryPostgresConfig(t), func(config *Config) {
		attachments := store.config()
		config.Attachments = &attachments
		config.attachmentTransport = store.transport
		config.transport = func(Organization) (http.RoundTripper, error) {
			return runtimeTransport(func(request *http.Request) (*http.Response, error) {
				if request.URL.Path == "/internal/v1/attachments/exists" {
					tenantCalls.Add(1)
				}
				return nil, errors.New("tenant HTTP listener has not started")
			}), nil
		}
	})
	raw := []byte(`{"action":"create_plan","reason":"startup seed"}`)
	for _, organization := range []string{"org_alpha", "org_beta"} {
		allocation, err := f.service.readyOrganization(t.Context(), organization)
		if err != nil {
			t.Fatal(err)
		}
		owner := tenantDetailStore{service: f.service, allocation: allocation}
		first, err := owner.StoreDetail(t.Context(), organization, raw)
		if err != nil {
			t.Fatalf("store startup audit: %v", err)
		}
		aged := time.Now().Add(-2 * attachment.OrphanTTL)
		store.mu.Lock()
		object := store.objects[first.Key]
		object.created = aged
		store.objects[first.Key] = object
		store.mu.Unlock()
		second, err := owner.StoreDetail(t.Context(), organization, raw)
		if err != nil || first != second {
			t.Fatalf("idempotent startup store: %v", err)
		}
		store.mu.Lock()
		refreshed := store.objects[first.Key].created.After(aged)
		store.mu.Unlock()
		if !refreshed {
			t.Fatal("retried uncommitted archive retained its expired orphan age")
		}
		decoded, err := owner.ReadDetail(t.Context(), organization, first)
		if err != nil || !bytes.Equal(decoded, raw) {
			t.Fatalf("verify startup audit: %v", err)
		}
		if _, err := owner.StoreDetail(t.Context(), "org_foreign", raw); !errors.Is(err, attachment.ErrInvalid) {
			t.Fatal("foreign organization accepted")
		}
		decoded, err = owner.ReadCanonicalDetail(t.Context(), organization, "prj_example", first.Key)
		if err != nil || !bytes.Equal(decoded, raw) {
			t.Fatalf("canonical startup audit: %v", err)
		}
		if _, err := owner.ReadCanonicalDetail(t.Context(), "org_foreign", "prj_example", first.Key); !errors.Is(err, attachment.ErrInvalid) {
			t.Fatal("foreign canonical read accepted")
		}
		rows, err := owner.ReadDetails(t.Context(), organization, []attachment.ArchiveReference{first, second})
		if err != nil || len(rows) != 2 {
			t.Fatalf("batch read: %v", err)
		}
		for _, row := range rows {
			data, err := row.Bytes()
			if err != nil || !bytes.Equal(data, raw) {
				t.Fatalf("batch bytes: %v", err)
			}
		}
		if _, err := owner.ReadDetails(t.Context(), "org_foreign", []attachment.ArchiveReference{first}); !errors.Is(err, attachment.ErrInvalid) {
			t.Fatal("foreign batch accepted")
		}
		stale := owner
		stale.allocation.Generation++
		if _, err := stale.ReadCanonicalDetail(t.Context(), organization, "prj_example", first.Key); !errors.Is(err, attachment.ErrInvalid) {
			t.Fatal("stale canonical read accepted")
		}
		if _, err := stale.ReadDetails(t.Context(), organization, []attachment.ArchiveReference{first}); !errors.Is(err, attachment.ErrInvalid) {
			t.Fatal("stale batch accepted")
		}
		if _, err := stale.StoreDetail(t.Context(), organization, raw); !errors.Is(err, attachment.ErrInvalid) {
			t.Fatal("stale allocation accepted")
		}
	}
	if tenantCalls.Load() != 0 {
		t.Fatalf("startup store called tenant HTTP %d times", tenantCalls.Load())
	}
}
