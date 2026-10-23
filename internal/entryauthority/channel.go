package entryauthority

import (
	"context"
	"errors"
	"log/slog"
	"net/rpc"
	"os"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/cloudassert"
)

const BootstrapEnvironment = "DETENT_INTERNAL_ENTRY_AUTHORITY"
const BootstrapVersion = "v1:3:4"
const contentTimeout = 15 * time.Second

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

type Archives struct {
	Reader   ArchiveReader
	Contents attachment.ArchiveContentStore
	Content  ContentHandler
	Details  attachment.DetailStore
}

type authorityServer struct {
	binding        Binding
	budget         time.Duration
	resolve        func(Request) (cloudassert.Claims, error)
	contents       attachment.ArchiveContentStore
	storeDetail    func(DetailRequest) (attachment.ArchiveReference, error)
	readDetail     func(DetailRequest) ([]byte, error)
	contentContext func(time.Time) (context.Context, context.CancelFunc)
	archive        func(ArchiveRequest) (ArchiveResponse, error)
	content        func(ContentRequest) ([]byte, error)
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

func Start(ctx context.Context, binding Binding, budget time.Duration, resolve Resolver, archives ...Archives) (*Parent, error) {
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
	authority := &authorityServer{binding: binding, budget: budget, resolve: current, contentContext: func(deadline time.Time) (context.Context, context.CancelFunc) {
		lookup, cancel := context.WithTimeout(ctx, budget)
		lookup, end := context.WithDeadline(lookup, deadline)
		return lookup, func() { end(); cancel() }
	}}
	if len(archives) == 1 {
		authority.contents = archives[0].Contents
		if archives[0].Content != nil {
			authority.content = func(request ContentRequest) ([]byte, error) {
				lookup, cancel := context.WithDeadline(ctx, request.Deadline)
				defer cancel()
				lookup, stop := context.WithTimeout(lookup, contentTimeout)
				defer stop()
				if err := lookup.Err(); err != nil {
					return nil, err
				}
				return archives[0].Content(lookup, request)
			}
		}
		if archives[0].Reader != nil {
			authority.archive = func(request ArchiveRequest) (ArchiveResponse, error) {
				lookup, stop := context.WithTimeout(ctx, budget)
				defer stop()
				lookup, end := context.WithDeadline(lookup, request.Deadline)
				defer end()
				return archives[0].Reader(lookup, request.Reference, request.Presign)
			}
		}
	}
	if len(archives) == 1 && archives[0].Details != nil {
		details := archives[0].Details
		authority.storeDetail = func(request DetailRequest) (attachment.ArchiveReference, error) {
			lookup, stop := context.WithTimeout(ctx, budget)
			defer stop()
			lookup, end := context.WithDeadline(lookup, request.Deadline)
			defer end()
			return details.StoreDetail(lookup, request.Organization, request.Data)
		}
		authority.readDetail = func(request DetailRequest) ([]byte, error) {
			lookup, stop := context.WithTimeout(ctx, budget)
			defer stop()
			lookup, end := context.WithDeadline(lookup, request.Deadline)
			defer end()
			return details.ReadDetail(lookup, request.Organization, request.Reference)
		}
	}
	if err := server.RegisterName("Authority", authority); err != nil {
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
	organization string
	client       *rpc.Client
	budget       time.Duration
}

func Open(ctx context.Context, marker string, binding Binding, budget time.Duration) (*Client, error) {
	if marker != BootstrapVersion {
		return nil, cloudassert.ErrInvalid
	}
	stream, err := inheritedChannel(budget)
	if err != nil {
		return nil, err
	}

	client := &Client{client: rpc.NewClient(stream), budget: budget, organization: binding.Organization}
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

type ContentRequest struct {
	Organization string
	Content      []byte
	Data         []byte
	Write        bool
	Reference    attachment.ArchiveReference
	Deadline     time.Time
}

func (s *authorityServer) PublishContent(request ContentRequest, ref *attachment.ArchiveReference) error {
	if s.contents == nil || request.Organization != s.binding.Organization || request.Deadline.IsZero() || len(request.Content) > attachment.MaxArchiveBytes {
		return cloudassert.ErrInvalid
	}
	ctx, cancel := s.contentContext(request.Deadline)
	defer cancel()
	if ctx.Err() != nil {
		return cloudassert.ErrInvalid
	}
	result, err := s.contents.PublishContent(ctx, s.binding.Organization, request.Content)
	if err != nil {
		return errors.New("archive publication unavailable")
	}
	*ref = result
	return nil
}
func (s *authorityServer) ReadContent(request ContentRequest, content *[]byte) error {
	if s.contents == nil || request.Organization != s.binding.Organization || request.Deadline.IsZero() || request.Reference.Validate(s.binding.Organization) != nil {
		return cloudassert.ErrInvalid
	}
	ctx, cancel := s.contentContext(request.Deadline)
	defer cancel()
	if ctx.Err() != nil {
		return cloudassert.ErrInvalid
	}
	result, err := s.contents.ReadContent(ctx, s.binding.Organization, request.Reference)
	if err != nil {
		slog.Error("archive content unavailable", "error", err)
		return errors.New("archive content unavailable")
	}
	*content = result
	return nil
}
func (c *Client) PublishContent(ctx context.Context, organization string, content []byte) (attachment.ArchiveReference, error) {
	ctx, cancel := context.WithTimeout(ctx, c.budget)
	defer cancel()
	deadline, _ := ctx.Deadline()
	return callAuthority[attachment.ArchiveReference](ctx, c, "PublishContent", ContentRequest{Organization: organization, Content: content, Deadline: deadline})
}
func (c *Client) ReadContent(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, c.budget)
	defer cancel()
	deadline, _ := ctx.Deadline()
	return callAuthority[[]byte](ctx, c, "ReadContent", ContentRequest{Organization: organization, Reference: ref, Deadline: deadline})
}

type ContentHandler func(context.Context, ContentRequest) ([]byte, error)

func (s *authorityServer) Content(request ContentRequest, result *[]byte) error {
	if s.content == nil || request.Organization != s.binding.Organization || request.Deadline.IsZero() || len(request.Data) > attachment.MaxArchiveBytes || request.Reference.Validate(s.binding.Organization) != nil {
		return cloudassert.ErrInvalid
	}
	data, err := s.content(request)
	if err != nil {
		slog.Error("immutable content unavailable", "error", err)
		return errors.New("immutable content unavailable")
	}
	*result = data
	return nil
}

func (c *Client) Content(ctx context.Context, request ContentRequest) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, contentTimeout)
	defer cancel()
	request.Deadline, _ = ctx.Deadline()
	client := &Client{client: c.client, budget: contentTimeout}
	return callAuthority[[]byte](ctx, client, "Content", request)
}

type DetailRequest struct {
	Organization string
	Reference    attachment.ArchiveReference
	Data         []byte
	Deadline     time.Time
}

func (s *authorityServer) StoreDetail(request DetailRequest, result *attachment.ArchiveReference) error {
	if s.storeDetail == nil || request.Organization != s.binding.Organization || len(request.Data) == 0 || len(request.Data) > attachment.MaxArchiveBytes || request.Deadline.IsZero() {
		return attachment.ErrInvalid
	}
	ref, err := s.storeDetail(request)
	if err != nil {
		return artifactDetailError()
	}
	*result = ref
	return nil
}

func (s *authorityServer) ReadDetail(request DetailRequest, result *[]byte) error {
	if s.readDetail == nil || request.Organization != s.binding.Organization || request.Reference.Validate(s.binding.Organization) != nil || request.Deadline.IsZero() {
		return attachment.ErrInvalid
	}
	data, err := s.readDetail(request)
	if err != nil {
		return artifactDetailError()
	}
	*result = data
	return nil
}

func artifactDetailError() error { return errors.New("run detail storage unavailable") }

func (c *Client) StoreDetail(ctx context.Context, organization string, data []byte) (attachment.ArchiveReference, error) {
	if organization != c.organization || len(data) == 0 || len(data) > attachment.MaxArchiveBytes {
		return attachment.ArchiveReference{}, attachment.ErrInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, c.budget)
	defer cancel()
	deadline, _ := ctx.Deadline()
	return callAuthority[attachment.ArchiveReference](ctx, c, "StoreDetail", DetailRequest{Organization: organization, Data: data, Deadline: deadline})
}

func (c *Client) ReadDetail(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	if organization != c.organization || ref.Validate(organization) != nil {
		return nil, attachment.ErrInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, c.budget)
	defer cancel()
	deadline, _ := ctx.Deadline()
	return callAuthority[[]byte](ctx, c, "ReadDetail", DetailRequest{Organization: organization, Reference: ref, Deadline: deadline})
}
