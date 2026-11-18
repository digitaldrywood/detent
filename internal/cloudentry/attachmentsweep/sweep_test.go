package attachmentsweep

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
)

type bucket struct {
	attachment.Storage
	objects  map[string][]attachment.Object
	removed  string
	listings int
}

func (b *bucket) ObjectPage(_ context.Context, org, namespace, after string) ([]attachment.Object, string, error) {
	b.listings++
	objects := b.objects[org]
	start := sort.Search(len(objects), func(i int) bool { return objects[i].Key > after })
	objects = objects[start:]
	if len(objects) <= attachment.ReferenceBatchSize {
		return objects, "", nil
	}
	page := objects[:attachment.ReferenceBatchSize]
	return page, page[len(page)-1].Key, nil
}

func (b *bucket) Stat(_ context.Context, key string) (attachment.Object, bool, error) {
	for _, objects := range b.objects {
		for _, obj := range objects {
			if obj.Key == key {
				return obj, true, nil
			}
		}
	}
	return attachment.Object{}, false, nil
}

func (b *bucket) Delete(_ context.Context, key string) error { b.removed = key; return nil }

func TestArchiveSweepBatchesTenantRequests(t *testing.T) {
	t.Parallel()
	const count = 513
	now := time.Now()
	b := &bucket{objects: make(map[string][]attachment.Object)}
	for _, org := range []string{"org_alpha", "org_beta"} {
		for i := range count {
			key, err := attachment.ArchiveKey(org, fmt.Sprintf("%064x", i))
			if err != nil {
				t.Fatal(err)
			}
			b.objects[org] = append(b.objects[org], attachment.Object{Key: key, CreatedAt: now.Add(-2 * attachment.OrphanTTL)})
		}
	}
	for _, org := range []string{"org_alpha", "org_beta"} {
		batches, examined, final := 0, 0, 0
		last := b.objects[org][count-1].Key
		page := attachment.ReclaimPage{Changed: []attachment.ReclaimCandidate{{Key: last, Revision: 1}}}
		locked := false
		s := Sweep{Now: func() time.Time { return now }, Storage: b, Lock: func(string) (func(), func() error, error) {
			locked = true
			return func() { locked = false }, func() error { return nil }, nil
		}, Lookup: func(input attachment.ObjectLookup) (int, []byte, error) {
			if !input.Valid(org) {
				t.Fatalf("invalid or foreign lookup in %s", org)
			}
			var result any
			switch {
			case input.Page:
				result = page
			case input.Receipt != nil:
				if input.Receipt.After != page.After || len(input.Receipt.Checked) != len(page.Changed) {
					t.Fatal("acknowledged the wrong page")
				}
				page.After = input.Receipt.Next
				page.Changed = []attachment.ReclaimCandidate{}
				result = true
			case input.Key != "":
				final++
				if !locked || input.Key != last {
					t.Fatal("incorrect final reference check")
				}
				result = map[string]bool{"exists": true}
			default:
				batches++
				live := make([]bool, len(input.Keys))
				for i, key := range input.Keys {
					live[i] = key != last || final > 0
				}
				examined += len(live)
				result = live
			}
			data, err := json.Marshal(result)
			return http.StatusOK, data, err
		}}
		for pass := range 3 {
			if _, err := s.Archives(t.Context(), org, true); err != nil {
				t.Fatal(err)
			}
			if pass == 0 && (page.After == "" || final != 1) {
				t.Fatal("changed key behind the cursor did not get its final check on the first page")
			}
		}
		if batches != 4 || examined != count+1 || page.After != "" || final != 1 || b.removed != "" {
			t.Errorf("%s: batches=%d examined=%d after=%q final=%d removed=%q", org, batches, examined, page.After, final, b.removed)
		}
		for i, object := range b.objects[org][:attachment.ReferenceBatchSize] {
			page.Changed = append(page.Changed, attachment.ReclaimCandidate{Key: object.Key, Revision: int64(i + 1)})
		}
		listings := b.listings
		more, err := s.Archives(t.Context(), org, false)
		if err != nil || !more || batches != 5 {
			t.Fatalf("full change page: more=%v batches=%d err=%v", more, batches, err)
		}
		more, err = s.Archives(t.Context(), org, false)
		if err != nil || more || batches != 5 || b.listings != listings {
			t.Fatalf("drained page: more=%v batches=%d new listings=%d err=%v", more, batches, b.listings-listings, err)
		}

	}
}
