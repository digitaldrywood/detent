package entryauthority

import (
	"context"
	"errors"
	"net/rpc"
	"os"
	"sync/atomic"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/cloudassert"
)

const BootstrapEnvironment = "DETENT_INTERNAL_ENTRY_AUTHORITY"
const RequestBudgetEnvironment = "DETENT_INTERNAL_ENTRY_REQUEST_BUDGET"
const BootstrapVersion = "v1:3:6"
const DefaultTenantRequestBudget = 30 * time.Second

var ErrUnavailable = errors.New("entry authority unavailable")

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
	Icons    attachment.IconStore
}

type authorityServer struct {
	icons               attachment.IconStore
	binding             Binding
	budget              time.Duration
	resolve             func(Request) (cloudassert.Claims, error)
	contents            attachment.ArchiveContentStore
	storeDetail         func(DetailRequest) (attachment.ArchiveReference, error)
	readCanonicalDetail func(context.Context, DetailRequest) ([]byte, error)
	readDetail          func(context.Context, DetailRequest) ([]byte, error)
	readDetails         func(context.Context, DetailBatchRequest) ([]attachment.DetailReadResult, error)
	contentContext      func(time.Time) (context.Context, context.CancelFunc)
	archive             func(context.Context, ArchiveRequest) (ArchiveResponse, error)
	content             func(context.Context, ContentRequest) ([]byte, error)
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
		for _, kind := range []error{cloudassert.ErrInvalid, context.DeadlineExceeded, context.Canceled} {
			if errors.Is(err, kind) {
				return kind
			}
		}
		return ErrUnavailable
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

func Start(ctx context.Context, binding Binding, budget, requestBudget time.Duration, resolve Resolver, archives ...Archives) (*Parent, error) {
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
	authority := newAuthorityServer(ctx, binding, budget, resolve, archives)
	if err := server.RegisterName("Authority", authority); err != nil {
		close(p.done)
		return nil, errors.Join(err, p.Close())
	}
	go func() {
		defer close(p.done)
		serveAuthority(ctx, server, p.stream, requestBudget)
	}()
	return p, nil
}

func newAuthorityServer(ctx context.Context, binding Binding, budget time.Duration, resolve Resolver, archives []Archives) *authorityServer {
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
			authority.content = func(lookup context.Context, request ContentRequest) ([]byte, error) {
				if err := lookup.Err(); err != nil {
					return nil, err
				}
				return archives[0].Content(lookup, request)
			}
		}
		if archives[0].Reader != nil {
			authority.archive = func(lookup context.Context, request ArchiveRequest) (ArchiveResponse, error) {
				if err := lookup.Err(); err != nil {
					return ArchiveResponse{}, err
				}
				return archives[0].Reader(lookup, request.Reference, request.Presign)
			}
		}
	}
	if len(archives) == 1 {
		authority.icons = archives[0].Icons
	}
	if len(archives) == 1 && archives[0].Details != nil {
		details := archives[0].Details
		if recovery, ok := details.(attachment.CanonicalDetailStore); ok {
			authority.readCanonicalDetail = func(lookup context.Context, request DetailRequest) ([]byte, error) {
				return recovery.ReadCanonicalDetail(lookup, request.Organization, request.Project, request.Key)
			}
		}
		authority.readDetails = func(lookup context.Context, request DetailBatchRequest) ([]attachment.DetailReadResult, error) {
			if batch, ok := details.(attachment.DetailBatchStore); ok {
				return batch.ReadDetails(lookup, request.Organization, request.References)
			}
			return attachment.ReadDetailBatch(lookup, request.Organization, request.References, details.ReadDetail)
		}

		authority.storeDetail = func(request DetailRequest) (attachment.ArchiveReference, error) {
			lookup, stop := context.WithTimeout(ctx, budget)
			defer stop()
			lookup, end := context.WithDeadline(lookup, request.Deadline)
			defer end()
			return details.StoreDetail(lookup, request.Organization, request.Data)
		}
		authority.readDetail = func(lookup context.Context, request DetailRequest) ([]byte, error) {
			return details.ReadDetail(lookup, request.Organization, request.Reference)
		}
	}
	return authority
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
	readID       atomic.Uint64
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
	return callAuthorityContext[T](ctx, c.client, method, args)
}

func callAuthorityContext[T any](ctx context.Context, client *rpc.Client, method string, args any) (T, error) {
	return callAuthorityRPC[T](ctx, client, method, args, 0)
}

func callAuthorityRPC[T any](ctx context.Context, client *rpc.Client, method string, args any, readID uint64) (T, error) {
	var zero T
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	reply := new(T)
	done := make(chan *rpc.Call, 1)
	go func() {
		if readID == 0 {
			client.Go("Authority."+method, args, reply, done)
			return
		}
		response := make(chan *rpc.Call, 1)
		client.Go("Authority."+method, args, reply, response)
		select {
		case result := <-response:
			done <- result
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.Canceled) {
				client.Go("Authority.CancelRead", CancelReadRequest{ReadID: readID}, new(bool), make(chan *rpc.Call, 1))
			}
		}
	}()
	select {
	case <-ctx.Done():
		return zero, ctx.Err()
	case result := <-done:
		if result.Error != nil {
			var remote rpc.ServerError
			if errors.As(result.Error, &remote) {
				for _, kind := range []error{cloudassert.ErrInvalid, context.DeadlineExceeded, context.Canceled, ErrUnavailable} {
					if string(remote) == kind.Error() {
						return zero, kind
					}
				}
			}
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

func (c *Client) readContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, c.budget)
}

type ContentRequest struct {
	ReadControl
	Organization string
	Content      []byte
	Data         []byte
	Write        bool
	Reference    attachment.ArchiveReference
	Deadline     time.Time
}

func (r ContentRequest) readDeadline() time.Time { return r.Deadline }

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
	ctx, cancel := request.start()
	defer cancel()
	started := time.Now()
	if s.contents == nil || request.Organization != s.binding.Organization || request.Deadline.IsZero() || request.Reference.Validate(s.binding.Organization) != nil {
		return cloudassert.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return contentFailure(request, started, "archive content unavailable", err)
	}
	result, err := s.contents.ReadContent(ctx, s.binding.Organization, request.Reference)
	if err != nil {
		return contentFailure(request, started, "archive content unavailable", err)
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
	ctx, cancel := c.readContext(ctx)
	defer cancel()
	deadline, _ := ctx.Deadline()
	id := c.readID.Add(1)
	data, err := callAuthorityRPC[[]byte](ctx, c.client, "ReadContent", ContentRequest{ReadControl: ReadControl{ReadID: id}, Organization: organization, Reference: ref, Deadline: deadline}, id)
	return data, contentClientError(err)
}

type ContentHandler func(context.Context, ContentRequest) ([]byte, error)

func (s *authorityServer) Content(request ContentRequest, result *[]byte) error {
	ctx, cancel := request.start()
	defer cancel()
	started := time.Now()
	if s.content == nil || request.Organization != s.binding.Organization || request.Deadline.IsZero() || len(request.Data) > attachment.MaxArchiveBytes || request.Reference.Validate(s.binding.Organization) != nil {
		return cloudassert.ErrInvalid
	}
	data, err := s.content(ctx, request)
	if err != nil {
		return contentFailure(request, started, "immutable content unavailable", err)
	}
	*result = data
	return nil
}

func (c *Client) Content(ctx context.Context, request ContentRequest) ([]byte, error) {
	ctx, cancel := c.readContext(ctx)
	defer cancel()
	request.Deadline, _ = ctx.Deadline()
	request.ReadID = 0
	if !request.Write {
		request.ReadID = c.readID.Add(1)
	}
	data, err := callAuthorityRPC[[]byte](ctx, c.client, "Content", request, request.ReadID)
	return data, contentClientError(err)
}

type DetailRequest struct {
	ReadControl
	Project      string
	Key          string
	Organization string
	Reference    attachment.ArchiveReference
	Data         []byte
	Deadline     time.Time
}

func (r DetailRequest) readDeadline() time.Time { return r.Deadline }

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
	ctx, cancel := request.start()
	defer cancel()
	started := time.Now()
	if s.readDetail == nil || request.Organization != s.binding.Organization || request.Reference.Validate(s.binding.Organization) != nil || request.Deadline.IsZero() {
		return attachment.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	data, err := s.readDetail(ctx, request)
	if err != nil {
		return contentFailure(ContentRequest{Organization: request.Organization, Reference: request.Reference}, started, "run detail unavailable", err)
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
	ctx, cancel := c.readContext(ctx)
	defer cancel()
	deadline, _ := ctx.Deadline()
	id := c.readID.Add(1)
	data, err := callAuthorityRPC[[]byte](ctx, c.client, "ReadDetail", DetailRequest{ReadControl: ReadControl{ReadID: id}, Organization: organization, Reference: ref, Deadline: deadline}, id)
	return data, contentClientError(err)
}

func (s *authorityServer) ReadCanonicalDetail(request DetailRequest, result *[]byte) error {
	ctx, cancel := request.start()
	defer cancel()
	if s.readCanonicalDetail == nil || request.Organization != s.binding.Organization || request.Deadline.IsZero() || attachment.ValidateCanonicalDetailKey(request.Organization, request.Project, request.Key) != nil {
		return attachment.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	data, err := s.readCanonicalDetail(ctx, request)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return artifactDetailError()
	}
	*result = data
	return nil
}

func (c *Client) ReadCanonicalDetail(ctx context.Context, organization, project, key string) ([]byte, error) {
	if organization != c.organization || attachment.ValidateCanonicalDetailKey(organization, project, key) != nil {
		return nil, attachment.ErrInvalid
	}
	ctx, cancel := c.readContext(ctx)
	defer cancel()
	deadline, _ := ctx.Deadline()
	id := c.readID.Add(1)
	data, err := callAuthorityRPC[[]byte](ctx, c.client, "ReadCanonicalDetail", DetailRequest{ReadControl: ReadControl{ReadID: id}, Organization: organization, Project: project, Key: key, Deadline: deadline}, id)
	return data, contentClientError(err)
}
