package pgtransport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type Pools struct{ reader, writer *pgxpool.Pool }

func Configure(config *pgxpool.Config) {
	config.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeExec
	config.ConnConfig.StatementCacheCapacity = 0
	config.ConnConfig.DescriptionCacheCapacity = 0
	previous := config.AfterConnect
	config.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		if previous != nil {
			if err := previous(ctx, conn); err != nil {
				return err
			}
		}
		timestamp := &pgtype.Type{Name: "timestamptz", OID: pgtype.TimestamptzOID, Codec: &pgtype.TimestamptzCodec{ScanLocation: time.UTC}}
		conn.TypeMap().RegisterType(timestamp)
		conn.TypeMap().RegisterDefaultPgType(json.RawMessage{}, "jsonb")
		registerText[tracker.OrganizationID](conn.TypeMap())
		registerText[tracker.ProjectID](conn.TypeMap())
		registerText[tracker.NativeWorkItemID](conn.TypeMap())
		conn.TypeMap().RegisterType(&pgtype.Type{Name: "_timestamptz", OID: pgtype.TimestamptzArrayOID, Codec: &pgtype.ArrayCodec{ElementType: timestamp}})
		return nil
	}
}

func Open(ctx context.Context, readerConfig, writerConfig *pgxpool.Config) (*Pools, error) {
	if readerConfig == nil {
		return nil, errors.New("PostgreSQL reader configuration is required")
	}
	config := readerConfig.Copy()
	Configure(config)
	reader, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("open PostgreSQL reader: %w", err)
	}
	pools := &Pools{reader: reader, writer: reader}
	if writerConfig != nil {
		config = writerConfig.Copy()
		Configure(config)
		pools.writer, err = pgxpool.NewWithConfig(ctx, config)
		if err != nil {
			reader.Close()
			return nil, fmt.Errorf("open PostgreSQL writer: %w", err)
		}
	}
	for _, pool := range []*pgxpool.Pool{pools.reader, pools.writer} {
		if err := pool.Ping(ctx); err != nil {
			pools.Close()
			return nil, fmt.Errorf("connect PostgreSQL transport: %w", err)
		}
	}
	return pools, nil
}

func (p *Pools) Close() {
	p.reader.Close()
	if p.writer != p.reader {
		p.writer.Close()
	}
}

func Adopt(reader, writer *pgxpool.Pool) (*Pools, error) {
	if reader == nil || writer == nil {
		return nil, errors.New("PostgreSQL reader and writer are required")
	}
	for _, pool := range []*pgxpool.Pool{reader, writer} {
		if err := ValidatePool(pool); err != nil {
			return nil, err
		}
	}
	return &Pools{reader: reader, writer: writer}, nil
}

func registerText[T ~string](mapping *pgtype.Map) {
	var value T
	mapping.RegisterDefaultPgType(value, "text")
	mapping.RegisterDefaultPgType(new(T), "text")
	mapping.RegisterDefaultPgType([]T{}, "_text")
	mapping.RegisterDefaultPgType([]*T{}, "_text")
}

func ValidatePool(pool *pgxpool.Pool) error {
	if pool == nil {
		return errors.New("PostgreSQL pool is required")
	}
	config := pool.Config().ConnConfig
	if config.DefaultQueryExecMode != pgx.QueryExecModeExec || config.StatementCacheCapacity != 0 || config.DescriptionCacheCapacity != 0 {
		return errors.New("PostgreSQL pools require configured native execution")
	}
	return nil
}
