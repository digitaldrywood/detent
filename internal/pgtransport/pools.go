package pgtransport

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Pools struct{ reader, writer *pgxpool.Pool }

func Configure(config *pgxpool.Config) {
	config.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeExec
	config.ConnConfig.StatementCacheCapacity = 0
	config.ConnConfig.DescriptionCacheCapacity = 0
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
