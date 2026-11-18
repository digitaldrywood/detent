package harness

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/testenv"
)

type pendingArchiveDelete struct {
	attachment.Storage
	key     string
	started chan struct{}
	proceed chan struct{}
}

func (d *pendingArchiveDelete) Delete(ctx context.Context, key string) error {
	if key != d.key {
		return d.Storage.Delete(ctx, key)
	}
	close(d.started)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-d.proceed:
		return d.Storage.Delete(ctx, key)
	}
}

func (objects *Spaces) CheckConcurrentArchivePublication(t *testing.T, db *sql.DB, storage attachment.Storage, org string, owner attachment.DetailStore, sweep func(context.Context, attachment.Storage) error) {
	t.Helper()
	gate := &pendingArchiveDelete{Storage: storage, started: make(chan struct{}), proceed: make(chan struct{})}
	data := []byte("republished detail")
	ref, err := owner.StoreDetail(t.Context(), org, data)
	if err != nil {
		t.Fatal(err)
	}
	gate.key = ref.Key
	objects.AgeArchives(ref)
	ctx, cancel := context.WithCancel(t.Context())
	var workers sync.WaitGroup
	defer func() { cancel(); workers.Wait() }()
	swept := make(chan error, 1)
	workers.Go(func() { swept <- sweep(ctx, gate) })
	select {
	case <-gate.started:
	case err := <-swept:
		t.Fatalf("sweep returned before orphan deletion: %v", err)
	}
	published := make(chan error, 1)
	workers.Go(func() { _, err := owner.StoreDetail(ctx, org, data); published <- err })
	testenv.WaitUntil(t, ctx, func() error {
		select {
		case err := <-published:
			t.Fatalf("second Entry published before deletion finished: %v", err)
		default:
		}
		var waiting bool
		if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE locktype='advisory' AND NOT granted AND database=(SELECT oid FROM pg_database WHERE datname=current_database()))`).Scan(&waiting); err != nil {
			return err
		}
		if !waiting {
			return errors.New("publisher has not reached the object lock")
		}
		return nil
	})
	close(gate.proceed)
	if err := <-swept; err != nil {
		t.Fatal(err)
	}
	if err := <-published; err != nil {
		t.Fatal(err)
	}
	got, err := owner.ReadDetail(ctx, org, ref)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("republished bytes differ: %v", err)
	}
	objects.AgeArchives(ref)
	if err := sweep(ctx, storage); err != nil {
		t.Fatal(err)
	}
}

func (objects *Spaces) AgeArchives(refs ...attachment.ArchiveReference) {
	objects.Mu.Lock()
	defer objects.Mu.Unlock()
	for _, ref := range refs {
		object := objects.Objects[ref.Key]
		object.Created = time.Now().Add(-2 * attachment.OrphanTTL)
		objects.Objects[ref.Key] = object
	}
}

func (objects *Spaces) Contains(key string) bool {
	objects.Mu.Lock()
	defer objects.Mu.Unlock()
	_, ok := objects.Objects[key]
	return ok
}

func (objects *Spaces) CheckSweepAcrossRestarts(t *testing.T, keys []string, visit func()) {
	t.Helper()
	objects.Mu.Lock()
	for _, key := range keys {
		objects.Objects[key] = SpacesObject{Content: []byte("orphan"), Created: time.Now().Add(-2 * attachment.OrphanTTL)}
	}
	objects.Mu.Unlock()
	for i := range keys {
		visit()
		remaining := 0
		for _, key := range keys {
			if objects.Contains(key) {
				remaining++
			}
		}
		if want := len(keys) - i - 1; remaining != want {
			t.Fatalf("after Entry restart %d, %d tenants still have orphans, want %d", i+1, remaining, want)
		}
	}
}
