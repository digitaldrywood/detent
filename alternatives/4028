package storageconversion

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"
)

type FamilyCursor struct {
	Family     string
	Turn       int64
	Positioned bool
	Keys       [4]string
	Sequence   int64
}

type Usage struct {
	Rows      int
	Bytes     int64
	Oversized int
}

type CursorUpdate int

const (
	Reserve CursorUpdate = iota
	Restore
	Record
)

type Turn struct {
	Visit   func(Candidate) bool
	Charge  func(int) error
	Expand  func(int) error
	Advance func(func(context.Context) error)
}

type Storage interface {
	Start(context.Context) ([]string, error)
	Next(context.Context, []string) (FamilyCursor, error)
	Update(context.Context, FamilyCursor, CursorUpdate, Usage) error
	Convert(context.Context, FamilyCursor, Turn) error
}

const BackgroundBudget = 2 * time.Second
const storageConversionRows = MaxRows
const storageConversionBytes = 1 << 20

type storageBatchKey struct{}

type storageRowPosition struct {
	candidate Candidate
}

type storageBatchBudget struct {
	bytes     int64
	rows      int
	oversized int
	turn      int64
	reserve   func() error
	advance   func(context.Context) error
	itemBytes int
	visited   map[storageRowPosition]bool
}

var ErrBatchFull = errors.New("storage conversion batch is full")

func ConvertStorageBatch(ctx context.Context, storage Storage) error {
	return convertStorageBatchLimit(ctx, storage, storageConversionRows)
}

func convertStorageBatchLimit(ctx context.Context, storage Storage, limit int) error {
	return convertStorageBatchBudget(ctx, storage, limit, &storageBatchBudget{})
}

func convertStorageBatchBudget(ctx context.Context, storage Storage, limit int, budget *storageBatchBudget) error {
	ctx = context.WithValue(ctx, storageBatchKey{}, budget)
	names, err := storage.Start(ctx)
	if err != nil {
		return err
	}
	familyCount := len(names)
	var failures error
	for turns := 0; turns < limit*familyCount && budget.rows < limit && len(names) > 0; turns++ {
		cursor, err := storage.Next(ctx, names)
		if err != nil {
			return errors.Join(failures, err)
		}
		budget.reserve = func() error { return storage.Update(ctx, cursor, Reserve, Usage{}) }

		budget.advance = nil
		budget.itemBytes = 0
		budget.turn = cursor.Turn
		beforeRows, beforeBytes, beforeOversized := budget.rows, budget.bytes, budget.oversized
		err = storage.Convert(ctx, cursor, Turn{
			Visit: func(candidate Candidate) bool {
				if budget.visited == nil {
					budget.visited = make(map[storageRowPosition]bool)
				}
				key := storageRowPosition{candidate: candidate}
				visited := budget.visited[key]
				budget.visited[key] = true
				return visited
			},
			Charge:  func(size int) error { return Charge(ctx, size) },
			Expand:  func(size int) error { return Expand(ctx, size) },
			Advance: func(advance func(context.Context) error) { budget.advance = advance },
		})
		if errors.Is(err, ErrBatchFull) {
			if budget.reserve != nil {
				return failures
			}
			restoreErr := storage.Update(ctx, cursor, Restore, Usage{})
			return errors.Join(failures, restoreErr)
		}
		if budget.rows > beforeRows && budget.advance != nil {
			advanceCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), BackgroundBudget)
			advanceErr := budget.advance(advanceCtx)
			cancel()
			if advanceErr != nil {
				return errors.Join(failures, err, advanceErr)
			}
		}
		if budget.rows > beforeRows {
			if recordErr := storage.Update(ctx, cursor, Record, Usage{Rows: budget.rows - beforeRows, Bytes: budget.bytes - beforeBytes, Oversized: budget.oversized - beforeOversized}); recordErr != nil {
				return errors.Join(failures, err, recordErr)
			}
		} else {
			names = slices.DeleteFunc(names, func(name string) bool { return name == cursor.Family })
		}

		if err != nil {
			failures = errors.Join(failures, fmt.Errorf("convert legacy %s: %w", cursor.Family, err))
		}
		if ctx.Err() != nil {
			return errors.Join(failures, ctx.Err())
		}
		if budget.bytes >= storageConversionBytes {
			break
		}
	}
	return failures
}

func Active(ctx context.Context) bool {
	return ctx.Value(storageBatchKey{}) != nil
}

func Charge(ctx context.Context, size int) error {
	budget, ok := ctx.Value(storageBatchKey{}).(*storageBatchBudget)
	if !ok {
		return nil
	}
	size = max(1, size)
	if budget.bytes > 0 && budget.bytes+int64(size) > storageConversionBytes {
		return ErrBatchFull
	}
	if budget.reserve != nil {
		if err := budget.reserve(); err != nil {
			return err
		}
		budget.reserve = nil
	}
	budget.itemBytes = size
	budget.rows++
	budget.bytes += int64(size)
	if size > storageConversionBytes {
		budget.oversized++
	}
	return nil
}

func Expand(ctx context.Context, size int) error {
	budget, ok := ctx.Value(storageBatchKey{}).(*storageBatchBudget)
	if !ok || size <= budget.itemBytes {
		return nil
	}
	expanded := budget.bytes + int64(size-budget.itemBytes)
	if budget.rows > 1 && expanded > storageConversionBytes {
		return ErrBatchFull
	}
	if budget.bytes <= storageConversionBytes && expanded > storageConversionBytes {
		budget.oversized++
	}
	budget.bytes = expanded
	budget.itemBytes = size
	return nil
}

func ConvertBatch(ctx context.Context, storage Storage, limit int) (Usage, error) {
	budget := &storageBatchBudget{}
	err := convertStorageBatchBudget(ctx, storage, limit, budget)
	return Usage{Rows: budget.rows, Bytes: budget.bytes, Oversized: budget.oversized}, err
}
