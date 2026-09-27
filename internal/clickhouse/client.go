package clickhouse

import (
	"context"
	"fmt"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// Bounded timeouts. Every public operation derives a context with one of
// these so a hung or unreachable ClickHouse can never block a caller
// indefinitely. They are named constants rather than inline magic numbers.
const (
	connectTimeout = 10 * time.Second
	pingTimeout    = 5 * time.Second
	schemaTimeout  = 30 * time.Second
	insertTimeout  = 15 * time.Second
)

// Client is a connection to the ClickHouse analytical store plus the
// retention configuration used to build the schema.
//
// It is safe for concurrent use: the underlying clickhouse-go connection
// pools connections and manages its own synchronisation.
type Client struct {
	conn          driver.Conn
	retentionDays int
}

// Connect parses dsn, opens a connection pool and verifies the server is
// actually reachable by pinging it. A connection is never returned together
// with an error: if the ping fails (or times out) the pool is closed and the
// error is returned.
//
// dsn is a clickhouse-go DSN, e.g.
//
//	clickhouse://user:pass@localhost:9000/db
//
// retentionDays is the global age cap applied in addition to per-row
// expires_at; <= 0 disables the global TTL, values above maxRetentionDays
// are clamped.
func Connect(ctx context.Context, dsn string, retentionDays int) (*Client, error) {
	if dsn == "" {
		return nil, fmt.Errorf("clickhouse: empty dsn")
	}

	opts, err := clickhouse.ParseDSN(dsn)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: parse dsn: %w", err)
	}

	// Bound the connect handshake itself so a black-holed address fails fast
	// instead of hanging in the TCP dial.
	if opts.DialTimeout == 0 || opts.DialTimeout > connectTimeout {
		opts.DialTimeout = connectTimeout
	}

	conn, err := clickhouse.Open(opts)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: open: %w", err)
	}

	c := &Client{
		conn:          conn,
		retentionDays: clampRetentionDays(retentionDays),
	}

	pingCtx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	if err := c.Ping(pingCtx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("clickhouse: ping %s: %w", opts.Addr, err)
	}

	return c, nil
}

// Ping verifies the server is reachable within pingTimeout.
func (c *Client) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	if err := c.conn.Ping(ctx); err != nil {
		return fmt.Errorf("clickhouse: ping: %w", err)
	}
	return nil
}

// EnsureSchema creates the events table if it does not already exist. It is
// idempotent and safe to call on every start-up.
func (c *Client) EnsureSchema(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, schemaTimeout)
	defer cancel()
	if err := c.conn.Exec(ctx, eventsDDL(c.retentionDays)); err != nil {
		return fmt.Errorf("clickhouse: ensure schema: %w", err)
	}
	return nil
}

// Close releases the connection pool.
func (c *Client) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}
