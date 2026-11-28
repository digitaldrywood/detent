package entryauthority

import (
	"bufio"
	"context"
	"encoding/gob"
	"errors"
	"io"
	"net/rpc"
	"sync"
	"time"
)

type ReadControl struct {
	ReadID uint64
	start  func() (context.Context, context.CancelFunc)
}

func (r *ReadControl) readControl() *ReadControl { return r }

type CancelReadRequest struct{ ReadID uint64 }

type authorityCodec struct {
	method string
	conn   io.ReadWriteCloser
	reader *gob.Decoder
	writer *gob.Encoder
	buffer *bufio.Writer
	begin  func(time.Time) (context.Context, context.CancelFunc)
	stop   context.CancelFunc
	mu     sync.Mutex
	active map[uint64]context.CancelFunc
}

func serveAuthority(ctx context.Context, server *rpc.Server, conn io.ReadWriteCloser, requestBudget time.Duration) {
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	if requestBudget <= 0 {
		requestBudget = DefaultTenantRequestBudget
	}
	buffer := bufio.NewWriter(conn)
	server.ServeCodec(&authorityCodec{
		conn: conn, reader: gob.NewDecoder(conn), writer: gob.NewEncoder(buffer), buffer: buffer,
		stop: stop, active: make(map[uint64]context.CancelFunc),
		begin: func(deadline time.Time) (context.Context, context.CancelFunc) {
			return context.WithDeadline(ctx, earlierDeadline(deadline, time.Now().Add(requestBudget)))
		},
	})
}

func earlierDeadline(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func (c *authorityCodec) ReadRequestHeader(request *rpc.Request) error {
	err := c.reader.Decode(request)
	c.method = request.ServiceMethod
	if err != nil {
		c.stop()
	}
	return err
}

func (c *authorityCodec) ReadRequestBody(body any) error {
	if err := c.reader.Decode(body); err != nil {
		return err
	}
	switch c.method {
	case "Authority.PublishContent", "Authority.StoreDetail":
		return nil
	}
	switch request := body.(type) {
	case *CancelReadRequest:
		c.mu.Lock()
		cancel := c.active[request.ReadID]
		c.mu.Unlock()
		if cancel != nil {
			cancel()
		}
	case interface {
		readControl() *ReadControl
		readDeadline() time.Time
	}:
		control := request.readControl()
		id := control.ReadID
		if content, ok := body.(*ContentRequest); ok && content.Write {
			id = 0
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		if id != 0 && c.active[id] != nil {
			return errors.New("duplicate authority read ID")
		}
		ctx, cancel := c.begin(request.readDeadline())
		if id != 0 {
			c.active[id] = cancel
		}
		control.start = func() (context.Context, context.CancelFunc) {
			return ctx, func() {
				cancel()
				if id != 0 {
					c.mu.Lock()
					delete(c.active, id)
					c.mu.Unlock()
				}
			}
		}
	}
	return nil
}

func (c *authorityCodec) WriteResponse(response *rpc.Response, body any) error {
	if err := c.writer.Encode(response); err != nil {
		return errors.Join(err, c.Close())
	}
	if err := c.writer.Encode(body); err != nil {
		return errors.Join(err, c.Close())
	}
	return c.buffer.Flush()
}

func (c *authorityCodec) Close() error {
	c.stop()
	return c.conn.Close()
}

func (*authorityServer) CancelRead(_ CancelReadRequest, _ *bool) error { return nil }
