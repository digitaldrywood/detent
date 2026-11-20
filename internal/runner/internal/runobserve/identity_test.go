package runobserve

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"

	"github.com/digitaldrywood/detent/internal/store"
)

func TestProviderIdentityRetriesUntilAcknowledged(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		id := store.SessionProviderIdentity{ThreadID: "thread", SessionID: "session"}
		writes := []store.SessionProviderIdentity{}
		failure := errors.New("unavailable")
		failures := make(chan error, 1)
		offer, retry, stop := KeepProviderIdentity(t.Context(), func(_ context.Context, got store.SessionProviderIdentity) error {
			writes = append(writes, got)
			if len(writes) == 1 {
				return failure
			}
			return nil
		}, func(err error) { failures <- err })
		defer stop()
		offer(id)
		synctest.Wait()
		select {
		case err := <-failures:
			if !errors.Is(err, failure) {
				t.Fatal(err)
			}
		default:
			t.Fatal("first write failure was not reported")
		}
		retry()
		synctest.Wait()
		offer(id)
		synctest.Wait()
		if len(writes) != 2 || writes[0] != id || writes[1] != id {
			t.Fatalf("writes = %v", writes)
		}
	})
}
