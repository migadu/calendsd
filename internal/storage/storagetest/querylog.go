package storagetest

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/stdlib"

	"github.com/migadu/calendsd/internal/pgdial"
)

// QueryLog records the text of every statement run on a database opened by
// NewLoggedDB, so a test can count the queries a code path issues and check
// what they select. Transaction control (BEGIN, COMMIT, ROLLBACK) goes
// through the driver's transaction methods, not a statement, and is not
// recorded.
type QueryLog struct {
	mu         sync.Mutex
	statements []string
}

// Statements returns the recorded statement texts in execution order.
func (l *QueryLog) Statements() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.statements...)
}

// Count returns how many statements were recorded.
func (l *QueryLog) Count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.statements)
}

// Reset discards the recorded statements, typically after seeding.
func (l *QueryLog) Reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.statements = nil
}

func (l *QueryLog) record(query string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.statements = append(l.statements, query)
}

// NewLoggedDB is NewDB plus a second handle on the same database whose
// statements are recorded. Migrations and AdminSeed go through the plain
// handle, so the log starts empty; a test that seeds more through the
// returned handle calls Reset afterwards.
func NewLoggedDB(t testing.TB, name string) (*sql.DB, *QueryLog) {
	t.Helper()
	NewDB(t, name)
	cfg, err := pgdial.Config(DSN(name))
	if err != nil {
		t.Fatal(err)
	}
	log := &QueryLog{}
	db := sql.OpenDB(&loggingConnector{Connector: stdlib.GetConnector(*cfg), log: log})
	t.Cleanup(func() { db.Close() })
	return db, log
}

type loggingConnector struct {
	driver.Connector
	log *QueryLog
}

func (c *loggingConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &loggingConn{Conn: conn.(*stdlib.Conn), log: c.log}, nil
}

// loggingConn forwards everything to the pgx connection (embedding keeps the
// optional interfaces database/sql probes for, NamedValueChecker among them)
// and records the text of each query, exec and prepare.
type loggingConn struct {
	*stdlib.Conn
	log *QueryLog
}

func (c *loggingConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.log.record(query)
	return c.Conn.QueryContext(ctx, query, args)
}

func (c *loggingConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.log.record(query)
	return c.Conn.ExecContext(ctx, query, args)
}

func (c *loggingConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	c.log.record(query)
	return c.Conn.PrepareContext(ctx, query)
}

func (c *loggingConn) Prepare(query string) (driver.Stmt, error) {
	c.log.record(query)
	return c.Conn.Prepare(query)
}
