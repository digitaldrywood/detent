package github

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

const (
	defaultHTTPClientTimeout    = 30 * time.Second
	defaultHTTPMaxIdleConns     = 100
	defaultHTTPMaxIdleConnsHost = 32
	defaultHTTPIdleConnTimeout  = 90 * time.Second
	defaultHTTPDialTimeout      = 30 * time.Second
	defaultHTTPDialKeepAlive    = 30 * time.Second
)

type HTTPTransportConfig struct {
	MaxIdleConns        int
	MaxIdleConnsPerHost int
	IdleConnTimeout     time.Duration
}

type PooledHTTPClient struct {
	*http.Client
	connections *connectionCounter
}

func NewPooledHTTPClient(cfg HTTPTransportConfig) *PooledHTTPClient {
	cfg = normalizeHTTPTransportConfig(cfg)
	connections := &connectionCounter{}
	transport := newHTTPTransport(cfg, connections)

	return &PooledHTTPClient{
		Client: &http.Client{
			Timeout:   defaultHTTPClientTimeout,
			Transport: transport,
		},
		connections: connections,
	}
}

func (c *PooledHTTPClient) LiveConnections() int {
	if c == nil || c.connections == nil {
		return 0
	}
	return c.connections.live()
}

func normalizeHTTPTransportConfig(cfg HTTPTransportConfig) HTTPTransportConfig {
	if cfg.MaxIdleConns <= 0 {
		cfg.MaxIdleConns = defaultHTTPMaxIdleConns
	}
	if cfg.MaxIdleConnsPerHost <= 0 {
		cfg.MaxIdleConnsPerHost = defaultHTTPMaxIdleConnsHost
	}
	if cfg.IdleConnTimeout <= 0 {
		cfg.IdleConnTimeout = defaultHTTPIdleConnTimeout
	}
	return cfg
}

func newHTTPTransport(cfg HTTPTransportConfig, connections *connectionCounter) *http.Transport {
	transport := defaultHTTPTransport()
	transport.MaxIdleConns = cfg.MaxIdleConns
	transport.MaxIdleConnsPerHost = cfg.MaxIdleConnsPerHost
	transport.IdleConnTimeout = cfg.IdleConnTimeout
	transport.ForceAttemptHTTP2 = true

	dialContext := transport.DialContext
	if dialContext == nil {
		dialer := &net.Dialer{
			Timeout:   defaultHTTPDialTimeout,
			KeepAlive: defaultHTTPDialKeepAlive,
		}
		dialContext = dialer.DialContext
	}

	transport.DialContext = func(ctx context.Context, network string, address string) (net.Conn, error) {
		conn, err := dialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		connections.opened()
		return &countedConn{Conn: conn, connections: connections}, nil
	}

	return transport
}

func defaultHTTPTransport() *http.Transport {
	if transport, ok := http.DefaultTransport.(*http.Transport); ok {
		return transport.Clone()
	}

	dialer := &net.Dialer{
		Timeout:   defaultHTTPDialTimeout,
		KeepAlive: defaultHTTPDialKeepAlive,
	}
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          defaultHTTPMaxIdleConns,
		MaxIdleConnsPerHost:   defaultHTTPMaxIdleConnsHost,
		IdleConnTimeout:       defaultHTTPIdleConnTimeout,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
}

func drainAndClose(body io.ReadCloser) error {
	if body == nil {
		return nil
	}
	_, copyErr := io.Copy(io.Discard, body)
	closeErr := body.Close()
	return errors.Join(copyErr, closeErr)
}

type connectionCounter struct {
	liveConnections int64
}

func (c *connectionCounter) opened() {
	atomic.AddInt64(&c.liveConnections, 1)
}

func (c *connectionCounter) closed() {
	atomic.AddInt64(&c.liveConnections, -1)
}

func (c *connectionCounter) live() int {
	return int(atomic.LoadInt64(&c.liveConnections))
}

type countedConn struct {
	net.Conn
	connections *connectionCounter
	closed      atomic.Bool
}

func (c *countedConn) Close() error {
	err := c.Conn.Close()
	if !c.closed.Swap(true) {
		c.connections.closed()
	}
	return err
}
