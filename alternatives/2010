package hubserver

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"sync/atomic"
	"time"

	"github.com/labstack/echo/v4"
	"modernc.org/sqlite"
)

// writerTiming accumulates how long one request held the single writer
// connection, so slow requests can be attributed to their own writes or to
// waiting behind someone else's.
type writerTiming struct {
	hold atomic.Int64
	txs  atomic.Int64
}

type writerTimingKey struct{}

func withWriterTiming(ctx context.Context) (context.Context, *writerTiming) {
	timing := &writerTiming{}
	return context.WithValue(ctx, writerTimingKey{}, timing), timing
}

func writerTimingFrom(ctx context.Context) *writerTiming {
	if timing, ok := ctx.Value(writerTimingKey{}).(*writerTiming); ok {
		return timing
	}
	return nil
}

func (t *writerTiming) add(start time.Time) {
	if t == nil {
		return
	}
	t.hold.Add(int64(time.Since(start)))
	t.txs.Add(1)
}

// openTimedWriter opens the writer pool through a connector that records each
// transaction's and auto-commit statement's hold time against the request
// context that started it.
func openTimedWriter(dsn string) (*sql.DB, error) {
	probe, err := sql.Open("sqlite", "")
	if err != nil {
		return nil, err
	}
	base := probe.Driver()
	if err := probe.Close(); err != nil {
		return nil, err
	}
	return sql.OpenDB(timedConnector{dsn: dsn, driver: base}), nil
}

type timedConnector struct {
	driver driver.Driver
	dsn    string
}

func (c timedConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := c.driver.Open(c.dsn)
	if err != nil {
		return nil, err
	}
	return &timedConn{Conn: conn}, nil
}

func (c timedConnector) Driver() driver.Driver { return c.driver }

type timedConn struct {
	driver.Conn
	inTx bool
}

type timedTx struct {
	driver.Tx
	conn   *timedConn
	timing *writerTiming
	start  time.Time
}

func (t *timedTx) Commit() error {
	defer t.end()
	return t.Tx.Commit()
}

func (t *timedTx) Rollback() error {
	defer t.end()
	return t.Tx.Rollback()
}

func (t *timedTx) end() {
	t.conn.inTx = false
	t.timing.add(t.start)
}

func (c *timedConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	start := time.Now()
	beginner, ok := c.Conn.(driver.ConnBeginTx)
	if !ok {
		return nil, errors.New("sqlite driver does not support contextual transactions")
	}
	tx, err := beginner.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	c.inTx = true
	return &timedTx{Tx: tx, conn: c, timing: writerTimingFrom(ctx), start: start}, nil
}

func (c *timedConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	if preparer, ok := c.Conn.(driver.ConnPrepareContext); ok {
		return preparer.PrepareContext(ctx, query)
	}
	return c.Prepare(query)
}

func (c *timedConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	execer, ok := c.Conn.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	if c.inTx {
		return execer.ExecContext(ctx, query, args)
	}
	start := time.Now()
	result, err := execer.ExecContext(ctx, query, args)
	writerTimingFrom(ctx).add(start)
	return result, err
}

func (c *timedConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if queryer, ok := c.Conn.(driver.QueryerContext); ok {
		return queryer.QueryContext(ctx, query, args)
	}
	return nil, driver.ErrSkip
}

func (c *timedConn) Ping(ctx context.Context) error {
	if pinger, ok := c.Conn.(driver.Pinger); ok {
		return pinger.Ping(ctx)
	}
	return nil
}

func (c *timedConn) ResetSession(ctx context.Context) error {
	if resetter, ok := c.Conn.(driver.SessionResetter); ok {
		return resetter.ResetSession(ctx)
	}
	return nil
}

func (c *timedConn) IsValid() bool {
	if validator, ok := c.Conn.(driver.Validator); ok {
		return validator.IsValid()
	}
	return true
}

func (c *timedConn) CheckNamedValue(value *driver.NamedValue) error {
	if checker, ok := c.Conn.(driver.NamedValueChecker); ok {
		return checker.CheckNamedValue(value)
	}
	return driver.ErrSkip
}

func (c *timedConn) NewBackup(destination string) (*sqlite.Backup, error) {
	backuper, ok := c.Conn.(onlineBackuper)
	if !ok {
		return nil, errors.New("sqlite driver does not support online backup")
	}
	return backuper.NewBackup(destination)
}

// slowRequest is the duration or writer hold above which a request is logged
// with its writer attribution.
const slowRequest = 250 * time.Millisecond

// timeWriterUse attributes writer hold time to each request, logs slow
// requests with it, and once a minute logs how long requests queued for the
// writer, so production latency can be split into waiting and holding.
func (s *Service) timeWriterUse(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		start := time.Now()
		ctx, timing := withWriterTiming(c.Request().Context())
		c.SetRequest(c.Request().WithContext(ctx))
		err := next(c)
		elapsed := time.Since(start)
		hold := time.Duration(timing.hold.Load())
		if elapsed >= slowRequest || hold >= slowRequest/5 {
			s.config.Logger.Info("hub request timing", "method", c.Request().Method, "route", c.Path(), "status", c.Response().Status,
				"duration_ms", elapsed.Milliseconds(), "writer_hold_ms", hold.Milliseconds(), "writer_txs", timing.txs.Load())
		}
		s.logWriterWaits()
		return err
	}
}

func (s *Service) logWriterWaits() {
	now := time.Now().UnixNano()
	last := s.writerStatsAt.Load()
	if now-last < int64(time.Minute) || !s.writerStatsAt.CompareAndSwap(last, now) {
		return
	}
	stats := s.database.db.Stats()
	waits, waited := stats.WaitCount-s.writerWaits.Swap(stats.WaitCount), stats.WaitDuration-time.Duration(s.writerWaited.Swap(int64(stats.WaitDuration)))
	if last == 0 {
		return
	}
	s.config.Logger.Info("hub writer waits", "waits", waits, "wait_ms", waited.Milliseconds(), "in_use", stats.InUse, "interval_s", (now-last)/int64(time.Second))
}
