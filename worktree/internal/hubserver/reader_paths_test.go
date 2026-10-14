package hubserver

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
)

type writerReadGuardKey struct{}

type writerReadGuard struct {
	mu         sync.Mutex
	violations []string
	queries    atomic.Int64
}

var writerReadAllowlist = map[string]string{
	"recordHostedRequest": "Hosted GET request accounting updates consumption in its own write transaction.",
}

func (g *writerReadGuard) check(ctx context.Context, operation string) error {
	if marked, _ := ctx.Value(writerReadGuardKey{}).(bool); !marked {
		return nil
	}
	pcs := make([]uintptr, 40)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(2, pcs)])
	var stack []string
	for {
		frame, more := frames.Next()
		if strings.Contains(frame.Function, "/hubserver.") {
			name := frame.Function[strings.LastIndex(frame.Function, ".")+1:]
			if reason := writerReadAllowlist[name]; reason != "" {
				return nil
			}
			stack = append(stack, name)
		}
		if !more {
			break
		}
	}
	message := operation + ": " + strings.Join(stack, " -> ")
	g.mu.Lock()
	g.violations = append(g.violations, message)
	g.mu.Unlock()
	return errors.New("read-only GET used the writer: " + message)
}

type guardedWriterConnector struct {
	timedConnector
	guard *writerReadGuard
}

func (c guardedWriterConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.timedConnector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &guardedWriterConn{timedConn: conn.(*timedConn), guard: c.guard}, nil
}

type guardedWriterConn struct {
	*timedConn
	guard *writerReadGuard
}

func (c *guardedWriterConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.guard.queries.Add(1)
	if err := c.guard.check(ctx, "query"); err != nil {
		return nil, err
	}
	return c.timedConn.QueryContext(ctx, query, args)
}

func (c *guardedWriterConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if err := c.guard.check(ctx, "transaction"); err != nil {
		return nil, err
	}
	return c.timedConn.BeginTx(ctx, opts)
}

func installWriterReadGuard(t *testing.T, s *Service) *writerReadGuard {
	t.Helper()
	guard := &writerReadGuard{}
	original := s.database.db
	pool := sql.OpenDB(guardedWriterConnector{timedConnector: timedConnector{dsn: sqliteWriterDSN(s.database.path, s.config.BusyTimeout, s.config.Hosted != nil), driver: original.Driver()}, guard: guard})
	pool.SetMaxOpenConns(1)
	s.database.db = pool
	if s.conversations != nil {
		s.conversations.store.db = pool
	}
	s.echo.Pre(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if c.Request().Method == http.MethodGet {
				c.SetRequest(c.Request().WithContext(context.WithValue(c.Request().Context(), writerReadGuardKey{}, true)))
			}
			return next(c)
		}
	})
	t.Cleanup(func() {
		s.tokenUseWork.Wait()
		s.database.db = original
		if s.conversations != nil {
			s.conversations.store.db = original
		}
		if err := pool.Close(); err != nil {
			t.Error(err)
		}
	})
	return guard
}

func TestReadOnlyGETsNeverAcquireWriter(t *testing.T) {
	f := newNativeFixture(t, nil, "", "reader-guard")
	issue := f.create(t, "readable issue")
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	guard := installWriterReadGuard(t, f.service)
	for _, suffix := range []string{"", "/onboarding", "/policy", "/labels", "/integration", "/actions", "/work-items/" + string(issue.WorkItemID), "/work-items/" + string(issue.WorkItemID) + "/comments", "/work-items/" + string(issue.WorkItemID) + "/runtime", "/work-items/" + string(issue.WorkItemID) + "/history", "/work-items/" + string(issue.WorkItemID) + "/attempts", "/work-items/" + string(issue.WorkItemID) + "/changes"} {
		t.Run(suffix, func(t *testing.T) {
			r := performHubAPIRequest(t, f.service, http.MethodGet, f.base+suffix, f.token, nil)
			if r.Code != http.StatusOK {
				t.Errorf("GET %s returned %d: %s", suffix, r.Code, r.Body.String())
			}
		})
	}
	f.service.tokenUseWork.Wait()
	guard.mu.Lock()
	defer guard.mu.Unlock()
	if len(guard.violations) > 0 {
		t.Fatalf("writer reads: %s", strings.Join(guard.violations, "\n"))
	}
}

func TestWriterReadGuardDetectsQueriesAndTransactions(t *testing.T) {
	f := newNativeFixture(t, nil, "", "guard-negative")
	guard := installWriterReadGuard(t, f.service)
	ctx := context.WithValue(t.Context(), writerReadGuardKey{}, true)
	for _, test := range []struct {
		name string
		run  func() error
	}{
		{"query", func() error {
			var count int
			return f.service.database.db.QueryRowContext(ctx, "SELECT count(*) FROM projects").Scan(&count)
		}},
		{"transaction", func() error {
			tx, err := f.service.database.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
			if err == nil {
				err = tx.Rollback()
			}
			return err
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.run(); err == nil || !strings.Contains(err.Error(), "read-only GET used the writer") {
				t.Fatalf("guard error=%v", err)
			}
		})
	}
	if len(guard.violations) != 2 {
		t.Fatalf("guard recorded %d violations", len(guard.violations))
	}
}

func TestHubWriterQueriesRequireAnOwnedReadReason(t *testing.T) {
	reasons := map[string]string{
		"createHostedProjectJSON":     "Project readback follows creation in this request.",
		"advanceGitHubImport":         "Import readback follows the advance performed by this request.",
		"advanceGitHubBatch":          "The import loop reads back its own preceding batch update.",
		"setSlackIntegration":         "Integration readback follows its transactional update in this request.",
		"openDatabase":                "Startup verifies the writer's schema and hosted binding before serving requests.",
		"configure":                   "Startup verifies settings on the actual writer connection.",
		"enableWAL":                   "Changing journal mode is a database write.",
		"verifyIdentity":              "Startup reads back the application identity it writes on the same connection.",
		"consumeHostedTransaction":    "UPDATE RETURNING atomically consumes a login transaction.",
		"attemptDiffBodyStored":       "The readback verifies the diff-body update just performed by this request.",
		"vacuumAttemptDiffBodies":     "Offline maintenance measures the writer connection before vacuuming.",
		"ensureHostedCustomer":        "The billing-intent readback follows its idempotent writer insertion.",
		"claimHostedOperation":        "Idempotent command readback observes the writer insertion in this request.",
		"tenantRequestMetrics.report": "Request metrics use their independent database, outside the tenant writer pool.",
		"bindHostedDatabase":          "Startup verifies binding through the connection that installs it.",
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, contents, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				name := ""
				writer := false
				switch method := call.Fun.(type) {
				case *ast.SelectorExpr:
					name = method.Sel.Name
					if receiver, ok := method.X.(*ast.SelectorExpr); ok && receiver.Sel.Name == "db" && (name == "QueryContext" || name == "QueryRowContext" || name == "BeginTx") {
						writer = true
					}
				case *ast.Ident:
					name = method.Name
				}
				if strings.HasPrefix(name, "read") || strings.HasPrefix(name, "list") || strings.HasPrefix(name, "load") {
					for _, argument := range call.Args {
						if receiver, ok := argument.(*ast.SelectorExpr); ok && receiver.Sel.Name == "db" {
							writer = true
						}
					}
				}
				if !writer {
					return true
				}
				if name == "BeginTx" {
					readOnly := false
					ast.Inspect(call, func(n ast.Node) bool {
						if field, ok := n.(*ast.KeyValueExpr); ok {
							if key, ok := field.Key.(*ast.Ident); ok && key.Name == "ReadOnly" {
								if value, ok := field.Value.(*ast.Ident); ok && value.Name == "true" {
									readOnly = true
								}
							}
						}
						return true
					})
					if !readOnly {
						return true
					}
				}
				reason := reasons[fn.Name.Name]
				if fn.Recv != nil {
					if pointer, ok := fn.Recv.List[0].Type.(*ast.StarExpr); ok {
						if receiverType, ok := pointer.X.(*ast.Ident); ok {
							if scoped := reasons[receiverType.Name+"."+fn.Name.Name]; scoped != "" {
								reason = scoped
							}
						}
					}
				}
				if reason == "" {
					t.Errorf("%s: %s uses writer %s without a read-your-writes or maintenance reason", path, fn.Name.Name, name)
				}
				return true
			})
		}
	}
}

func TestReadPoolExhaustionReturnsLoggedDeadline(t *testing.T) {
	var logs hostedLogBuffer
	service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	f := newNativeFixture(t, service, "", "exhausted-readers")
	connections := make([]*sql.Conn, 0, readerConnections)
	for range readerConnections {
		conn, err := service.database.reader.Conn(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		connections = append(connections, conn)
	}
	release := func() {
		for _, conn := range connections {
			if err := conn.Close(); err != nil {
				t.Error(err)
			}
		}
		connections = nil
	}
	defer release()
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	request := httptest.NewRequestWithContext(ctx, http.MethodGet, f.base+"/onboarding", nil)
	request.Header.Set("Authorization", "Bearer "+f.token)
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusInternalServerError || !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("pool exhaustion response=%d context=%v", response.Code, ctx.Err())
	}
	if !strings.Contains(logs.String(), "context deadline exceeded") {
		t.Fatalf("pool exhaustion lost its cause: %s", logs.String())
	}
	if service.database.reader.Stats().WaitCount == 0 {
		t.Fatal("request did not wait for the exhausted reader pool")
	}
	release()
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodGet, f.base+"/onboarding", f.token, nil), http.StatusOK)
}

func installReadQueryCounter(t *testing.T, d *database) *writerReadGuard {
	t.Helper()
	counter := &writerReadGuard{}
	original := d.reader
	pool := sql.OpenDB(guardedWriterConnector{timedConnector: timedConnector{dsn: sqliteReaderDSN(d.path, defaultBusyTimeout), driver: original.Driver()}, guard: counter})
	pool.SetMaxOpenConns(readerConnections)
	d.reader = pool
	t.Cleanup(func() {
		d.reader = original
		if err := pool.Close(); err != nil {
			t.Error(err)
		}
	})
	return counter
}
