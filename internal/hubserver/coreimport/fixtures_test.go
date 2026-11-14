package coreimport

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"sync"
	"testing"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"

	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/neutralimport/neutralimporttest"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMain(m *testing.M) { hubtest.Main(m) }

var repositoryFS = os.DirFS("../../..")

func newCoreNeutralFixture(t *testing.T, organization tracker.OrganizationID, names ...string) *neutralimporttest.Fixture {
	t.Helper()
	return neutralimporttest.New(t, repositoryFS, organization, names...)
}

func coreImportPostgres(t *testing.T) *postgres.Repository {
	t.Helper()
	repository, _ := coreImportPostgresWithAdmin(t, 3)
	return repository
}

func coreImportPostgresWithAdmin(t *testing.T, maxConnections int, configure ...func(*pgxpool.Config)) (*postgres.Repository, *sql.DB) {
	t.Helper()
	admin := hubtest.Database(t)
	options := []hubtest.Option{hubtest.WithMaxConnections(max(maxConnections, hubtest.DefaultMaxConnections))}
	for _, apply := range configure {
		options = append(options, hubtest.WithPoolConfig(apply))
	}
	return hubtest.RepositoryOn(t, admin, options...), admin
}

type memoryRunDetails struct {
	mu         sync.Mutex
	data       map[string][]byte
	beforeRead func(context.Context) error
}

func (m *memoryRunDetails) StoreDetail(_ context.Context, organization string, data []byte) (attachment.ArchiveReference, error) {
	ref, _, err := attachment.PrepareArchive(organization, 1, bytes.NewReader(data))
	if err != nil {
		return ref, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.data == nil {
		m.data = make(map[string][]byte)
	}
	m.data[ref.Key] = bytes.Clone(data)
	return ref, nil
}

func (m *memoryRunDetails) ReadDetail(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	if m.beforeRead != nil {
		if err := m.beforeRead(ctx); err != nil {
			return nil, err
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if ref.Validate(organization) != nil {
		return nil, attachment.ErrInvalid
	}
	data, ok := m.data[ref.Key]
	if !ok {
		return nil, artifact.ErrMissing
	}
	return bytes.Clone(data), nil
}

func testExecutions(t *testing.T) *Executions {
	t.Helper()
	return NewExecutions(Deps{
		CleanupRequired: func(tracker.NativeRunData) bool { return false },
		DecodeCheckpoint: func(context.Context, leasestore.Attempt, []byte, func(context.Context, *hubrecords.RunDetailReference) ([]byte, error)) (*tracker.NativeCheckpoint, error) {
			t.Error("unexpected checkpoint decode in a fixture without checkpoints")
			return nil, errors.New("unexpected checkpoint decode")
		},
	})
}

func checkpointExecutions(t *testing.T) *Executions {
	t.Helper()
	return NewExecutions(Deps{CleanupRequired: func(tracker.NativeRunData) bool { return false }, DecodeCheckpoint: hubrecords.DecodeAttemptCheckpoint})
}
