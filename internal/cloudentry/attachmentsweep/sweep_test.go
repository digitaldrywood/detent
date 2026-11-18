package attachmentsweep

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
)

type bucket struct {
	attachment.Storage
	objects []attachment.Object
}

func (b bucket) Walk(_ context.Context, prefix string, visit func(attachment.Object) error) error {
	for _, object := range b.objects {
		if strings.HasPrefix(object.Key, prefix) {
			if err := visit(object); err != nil {
				return err
			}
		}
	}
	return nil
}

func TestArchiveSweepBatchesTenantRequests(t *testing.T) {
	t.Parallel()
	const count = 513
	now := time.Now()
	b := bucket{}
	for _, org := range []string{"org_alpha", "org_beta"} {
		for i := range count {
			key, err := attachment.ArchiveKey(org, fmt.Sprintf("%064x", i))
			if err != nil {
				t.Fatal(err)
			}
			b.objects = append(b.objects, attachment.Object{Key: key, CreatedAt: now.Add(-2 * attachment.OrphanTTL)})
		}
	}
	for _, org := range []string{"org_alpha", "org_beta"} {
		calls, examined := 0, 0
		s := Sweep{Now: func() time.Time { return now }, Storage: b, Lookup: func(input attachment.ObjectLookup) (int, []byte, error) {
			calls++
			if len(input.Keys) == 0 || len(input.Keys) > attachment.ReferenceBatchSize || !input.Valid(org) {
				t.Fatalf("invalid or foreign lookup in %s", org)
			}
			live := make([]bool, len(input.Keys))
			for i := range live {
				live[i] = true
			}
			examined += len(live)
			data, err := json.Marshal(live)
			return http.StatusOK, data, err
		}}
		if err := s.Run(t.Context(), org, "blobs/sha256/"); err != nil {
			t.Fatal(err)
		}
		if calls != 3 || examined != count {
			t.Errorf("%s: %d requests for %d objects, want three requests for %d", org, calls, examined, count)
		}
	}
}
