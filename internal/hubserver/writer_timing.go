package hubserver

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/labstack/echo/v4"
	"modernc.org/sqlite"
)

// writerTiming accumulates how long one request held the single writer
// connection, so slow requests can be attributed to their own writes or to
// waiting behind someone else's.
type writerTiming struct {
	route string
	hold  atomic.Int64
	txs   atomic.Int64
	mu    sync.Mutex
	spans []timingSpan
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

func heartbeatWriterSteps(ctx context.Context) func(string) {
	timing := writerTimingFrom(ctx)
	if timing == nil {
		return func(string) {}
	}
	var name string
	var start time.Time
	return func(next string) {
		now := time.Now()
		if name != "" {
			timing.mu.Lock()
			timing.spans = append(timing.spans, timingSpan{name: name, duration: now.Sub(start)})
			timing.mu.Unlock()
		}
		name, start = next, now
	}
}

func (t *writerTiming) logAttrs() []any {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.spans) == 0 {
		return nil
	}
	spans := make([]any, 0, len(t.spans))
	for _, span := range t.spans {
		spans = append(spans, slog.Float64(span.name, milliseconds(span.duration)))
	}
	return []any{slog.Group("heartbeat_writer_ms", spans...)}
}

// openTimedWriter opens the writer pool through a connector that records each
// transaction's and auto-commit statement's hold time against the request
// context that started it.
func openTimedWriter(dsn string, logger *slog.Logger, committed ...chan struct{}) (*sql.DB, error) {
	return openTimedDB(dsn, true, logger, committed...)
}

// openTimedReader opens a reader pool whose statements count toward the
// request's db span but never toward writer hold time.
func openTimedReader(dsn string) (*sql.DB, error) {
	return openTimedDB(dsn, false, nil)
}

func openTimedDB(dsn string, writer bool, logger *slog.Logger, committed ...chan struct{}) (*sql.DB, error) {
	if err := registerNativeSearchText(); err != nil {
		return nil, err
	}
	if err := registerStorageConversionDigest(); err != nil {
		return nil, err
	}
	probe, err := sql.Open("sqlite", "")
	if err != nil {
		return nil, err
	}
	base := probe.Driver()
	if err := probe.Close(); err != nil {
		return nil, err
	}
	var wake chan struct{}
	if len(committed) > 0 {
		wake = committed[0]
	}
	return sql.OpenDB(timedConnector{dsn: dsn, driver: base, writer: writer, logger: logger, committed: wake}), nil
}

type timedConnector struct {
	committed chan struct{}
	driver    driver.Driver
	dsn       string
	writer    bool
	logger    *slog.Logger
	now       func() time.Time
}

func (c timedConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := c.driver.Open(c.dsn)
	if err != nil {
		return nil, err
	}
	now := c.now
	if now == nil {
		now = time.Now
	}
	logger := c.logger
	if logger == nil {
		logger = slog.Default()
	}
	return &timedConn{Conn: conn, writer: c.writer, committed: c.committed, logger: logger, now: now}, nil
}

func (c timedConnector) Driver() driver.Driver { return c.driver }

type timedConn struct {
	committed chan struct{}
	driver.Conn
	inTx   bool
	writer bool
	logger *slog.Logger
	now    func() time.Time
}

type timedTx struct {
	driver.Tx
	conn    *timedConn
	timing  *writerTiming
	request *requestTiming
	start   time.Time
	holder  writerHold
}

type writerHold struct {
	start time.Time
	pcs   [32]uintptr
	n     int
	route string
}

type writerHoldOwner string

func (o writerHoldOwner) LogValue() slog.Value {
	return slog.StringValue(string(o))
}

func (c *timedConn) startWriterHold(ctx context.Context) writerHold {
	if !c.writer || c.inTx {
		return writerHold{}
	}
	hold := writerHold{start: c.now()}
	hold.n = runtime.Callers(2, hold.pcs[:])
	if timing := writerTimingFrom(ctx); timing != nil {
		hold.route = timing.route
	}
	return hold
}

func (c *timedConn) endWriterHold(hold *writerHold) {
	n := hold.n
	hold.n = 0
	if n == 0 {
		return
	}
	elapsed := c.now().Sub(hold.start)
	if elapsed < time.Second {
		return
	}
	owner := "unknown"
	frames := runtime.CallersFrames(hold.pcs[:n])
	for {
		frame, more := frames.Next()
		if writerHoldOwnerFrame(frame) {
			owner = frame.Function
			break
		}
		if !more {
			break
		}
	}
	attrs := []any{"owner", writerHoldOwner(owner), "hold_ms", elapsed.Milliseconds(), "txs", 1}
	if hold.route != "" {
		attrs = append(attrs, "route", hold.route)
	}
	c.logger.Warn("hub writer held", attrs...)
}

func writerHoldOwnerFrame(frame runtime.Frame) bool {
	if strings.HasSuffix(frame.File, "/writer_timing.go") || strings.HasPrefix(frame.Function, "database/sql.") {
		return false
	}
	name := strings.TrimPrefix(frame.Function, "github.com/digitaldrywood/detent/internal/hubserver.")
	if strings.HasPrefix(name, "prepareContentMutation[") || strings.HasPrefix(name, "(*Service).executeNativeMutation.func") || strings.HasPrefix(name, "(*Service).hubTransact.func") || strings.HasPrefix(name, "(*Service).runnerTransaction.func") {
		return false
	}
	switch name {
	case "(*Service).runnerTransaction", "(*Service).commitRunnerTransaction", "(*Service).runnerAdminTransaction", "(*Service).runnerReadTransaction",
		"(*Service).hubTransact", "(*Service).commitHubTransaction", "(*conversationService).transact", "(*conversationTurnCoordinator).write",
		"(*Service).executeNativeMutation", "(*Service).commitNativeMutation", "(*Service).nativeMutation", "(*Service).nativeMutationStatus",
		"(*Service).commitOutbox", "(*Service).secretMutation":
		return false
	}
	return true
}

func (t *timedTx) Commit() error {
	defer t.end()
	start := time.Now()
	defer t.request.addDB(start)
	defer t.request.span("tx_commit", start)
	err := t.Tx.Commit()
	if err == nil {
		t.conn.notifyCommit()
	}
	return err
}

func (t *timedTx) Rollback() error {
	defer t.end()
	start := time.Now()
	defer t.request.addDB(start)
	return t.Tx.Rollback()
}

func (t *timedTx) end() {
	if t.conn.writer && t.request != nil && t.request.writerTransaction != nil {
		t.request.writerTransaction(false)
	}
	t.conn.inTx = false
	t.timing.add(t.start)
	t.conn.endWriterHold(&t.holder)
}

func (c *timedConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	start := time.Now()
	beginner, ok := c.Conn.(driver.ConnBeginTx)
	if !ok {
		return nil, errors.New("sqlite driver does not support contextual transactions")
	}
	holder := c.startWriterHold(ctx)
	tx, err := beginner.BeginTx(ctx, opts)
	requestTimingFrom(ctx).addDB(start)
	if err != nil {
		return nil, err
	}
	c.inTx = true
	if request := requestTimingFrom(ctx); c.writer && request != nil && request.writerTransaction != nil {
		request.writerTransaction(true)
	}
	var timing *writerTiming
	if c.writer {
		timing = writerTimingFrom(ctx)
	}
	return &timedTx{Tx: tx, conn: c, timing: timing, request: requestTimingFrom(ctx), start: start, holder: holder}, nil
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
	start := time.Now()
	holder := c.startWriterHold(ctx)
	result, err := execer.ExecContext(ctx, query, args)
	timing := requestTimingFrom(ctx)
	timing.statement(start, query, args)
	if c.writer && c.inTx && timing != nil && timing.writerTrace != nil {
		timing.writerTrace(query, args)
	}
	if c.writer && !c.inTx {
		writerTimingFrom(ctx).add(start)
		if err == nil {
			c.notifyCommit()
		}
	}
	c.endWriterHold(&holder)
	return result, err
}

func (c *timedConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	queryer, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	timing := requestTimingFrom(ctx)
	start := time.Now()
	holder := c.startWriterHold(ctx)
	rows, err := queryer.QueryContext(ctx, query, args)
	timing.statement(start, query, args)
	if c.writer && c.inTx && timing != nil && timing.writerTrace != nil {
		timing.writerTrace(query, args)
	}
	if err != nil {
		if holder.n != 0 {
			writerTimingFrom(ctx).add(start)
			c.endWriterHold(&holder)
		}
		return rows, err
	}
	var committed func()
	if c.writer && !c.inTx && c.committed != nil {
		committed = c.notifyCommit
	}
	if timing == nil && holder.n == 0 && committed == nil {
		return rows, nil
	}
	return &timedRows{Rows: rows, timing: timing, committed: committed, writer: c, holder: holder, writerTiming: writerTimingFrom(ctx), writerStart: start}, nil
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
		ctx := c.Request().Context()
		request := requestTimingFrom(ctx)
		if request == nil {
			request = &requestTiming{}
			ctx = withRequestTiming(ctx, request)
		}
		ctx, timing := withWriterTiming(ctx)
		timing.route = c.Path()
		c.SetRequest(c.Request().WithContext(ctx))
		err := next(c)
		elapsed := time.Since(start)
		hold := time.Duration(timing.hold.Load())
		if elapsed >= slowRequest || hold >= slowRequest/5 {
			attrs := []any{"method", c.Request().Method, "route", c.Path(), "status", c.Response().Status,
				"duration_ms", elapsed.Milliseconds(), "writer_hold_ms", hold.Milliseconds(), "writer_txs", timing.txs.Load()}
			attrs = append(attrs, request.logAttrs()...)
			attrs = append(attrs, timing.logAttrs()...)
			s.config.Logger.Info("hub request timing", attrs...)
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
	reader := s.database.reader.Stats()
	readerWaits, readerWaited := reader.WaitCount-s.readerWaits.Swap(reader.WaitCount), reader.WaitDuration-time.Duration(s.readerWaited.Swap(int64(reader.WaitDuration)))
	if last == 0 {
		return
	}
	s.config.Logger.Info("hub writer waits", "waits", waits, "wait_ms", waited.Milliseconds(), "in_use", stats.InUse, "interval_s", (now-last)/int64(time.Second),
		"reader_waits", readerWaits, "reader_wait_ms", readerWaited.Milliseconds(), "reader_in_use", reader.InUse, "reader_max", reader.MaxOpenConnections)
}

func (c *timedConn) notifyCommit() {
	if c.writer && c.committed != nil {
		select {
		case c.committed <- struct{}{}:
		default:
		}
	}
}
