package entryauthority

import (
	"context"
	"errors"
	"net/rpc"
	"os"
	"time"

	"github.com/digitaldrywood/detent/internal/cloudassert"
)

const BootstrapEnvironment = "DETENT_INTERNAL_ENTRY_AUTHORITY"
const BootstrapVersion = "v1:3:4"

type Binding struct {
	Issuer       string
	Organization string
	Generation   int64
}

type Origin struct {
	KeyID             string `json:"key_id"`
	CredentialBinding string `json:"credential_binding"`
	Subject           string `json:"subject"`
	Issuer            string `json:"issuer"`
	Generation        int64  `json:"generation"`
}

type Resolver func(context.Context, Origin) (cloudassert.Claims, error)

type Request struct {
	Origin   Origin
	Deadline time.Time
}

type Handshake struct {
	Binding Binding
	Budget  time.Duration
}

type channel struct {
	reader *os.File
	writer *os.File
	budget time.Duration
}

func (c channel) Read(p []byte) (int, error) { return c.reader.Read(p) }
func (c channel) Write(p []byte) (int, error) {
	if err := c.writer.SetWriteDeadline(time.Now().Add(c.budget)); err != nil {
		return 0, errors.Join(err, c.Close())
	}
	n, err := c.writer.Write(p)
	if err != nil {
		err = errors.Join(err, c.Close())
	}
	return n, err
}
func (c channel) Close() error { return closeFiles(c.reader, c.writer) }

func closeFiles(files ...*os.File) error {
	var result error
	for _, file := range files {
		if err := file.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			result = errors.Join(result, err)
		}
	}
	return result
}

type authorityServer struct {
	binding Binding
	budget  time.Duration
	resolve func(Request) (cloudassert.Claims, error)
	archive func(ArchiveRequest) (ArchiveResponse, error)
}

func (s *authorityServer) Handshake(binding Binding, result *Handshake) error {
	if binding != s.binding {
		return cloudassert.ErrInvalid
	}
	*result = Handshake{Binding: s.binding, Budget: s.budget}
	return nil
}

func (s *authorityServer) Resolve(request Request, result *cloudassert.Claims) error {
	if request.Origin.Issuer != s.binding.Issuer || request.Origin.Generation != s.binding.Generation || request.Deadline.IsZero() {
		return cloudassert.ErrInvalid
	}
	claims, err := s.resolve(request)
	if err != nil {
		return cloudassert.ErrInvalid
	}
	*result = claims
	return nil
}

type Parent struct {
	Files  []*os.File
	stream channel
	cancel context.CancelFunc
	done   chan struct{}
}

func Start(ctx context.Context, binding Binding, budget time.Duration, resolve Resolver, archives ...ArchiveReader) (*Parent, error) {
	parentRead, childWrite, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	childRead, parentWrite, err := os.Pipe()
	if err != nil {
		return nil, errors.Join(err, closeFiles(parentRead, childWrite))
	}
	ctx, cancel := context.WithCancel(ctx)
	p := &Parent{Files: []*os.File{childRead, childWrite}, stream: channel{reader: parentRead, writer: parentWrite, budget: budget}, cancel: cancel, done: make(chan struct{})}
	server := rpc.NewServer()
	current := func(request Request) (cloudassert.Claims, error) {
		lookup, stop := context.WithTimeout(ctx, budget)
		defer stop()
		lookup, end := context.WithDeadline(lookup, request.Deadline)
		defer end()
		return resolve(lookup, request.Origin)
	}
	service := &authorityServer{binding: binding, budget: budget, resolve: current}
	if len(archives) == 1 && archives[0] != nil {
		service.archive = func(request ArchiveRequest) (ArchiveResponse, error) {
			lookup, stop := context.WithTimeout(ctx, budget)
			defer stop()
			lookup, end := context.WithDeadline(lookup, request.Deadline)
			defer end()
			return archives[0](lookup, request.Reference, request.Presign)
		}
	}
	if err := server.RegisterName("Authority", service); err != nil {
		close(p.done)
		return nil, errors.Join(err, p.Close())
	}
	go func() {
		defer close(p.done)
		server.ServeConn(p.stream)
	}()
	return p, nil
}

func (p *Parent) Close() error {
	p.cancel()
	err := errors.Join(closeFiles(p.Files...), p.stream.Close())
	<-p.done
	return err
}

type Client struct {
	client *rpc.Client
	budget time.Duration
}

func Open(ctx context.Context, marker string, binding Binding, budget time.Duration) (*Client, error) {
	if marker != BootstrapVersion {
		return nil, cloudassert.ErrInvalid
	}
	stream, err := inheritedChannel(budget)
	if err != nil {
		return nil, err
	}

	client := &Client{client: rpc.NewClient(stream), budget: budget}
	handshake, err := callAuthority[Handshake](ctx, client, "Handshake", binding)
	if err != nil || handshake.Binding != binding || handshake.Budget <= 0 || handshake.Budget > budget {
		return nil, errors.Join(cloudassert.ErrInvalid, client.Close())
	}
	client.budget = handshake.Budget
	return client, nil
}

func callAuthority[T any](ctx context.Context, c *Client, method string, args any) (T, error) {
	ctx, cancel := context.WithTimeout(ctx, c.budget)
	defer cancel()
	var zero T
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	reply := new(T)
	done := make(chan *rpc.Call, 1)
	go c.client.Go("Authority."+method, args, reply, done)
	select {
	case <-ctx.Done():
		return zero, ctx.Err()
	case result := <-done:
		if result.Error != nil {
			return zero, result.Error
		}
		return *reply, nil
	}
}

func (c *Client) Resolve(ctx context.Context, origin Origin) (cloudassert.Claims, error) {
	ctx, cancel := context.WithTimeout(ctx, c.budget)
	defer cancel()
	deadline, _ := ctx.Deadline()
	return callAuthority[cloudassert.Claims](ctx, c, "Resolve", Request{Origin: origin, Deadline: deadline})
}

func (c *Client) Close() error {
	if err := c.client.Close(); err != nil && !errors.Is(err, rpc.ErrShutdown) {
		return err
	}
	return nil
}
