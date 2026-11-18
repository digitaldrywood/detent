package threads

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/threadstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const unreadPageSize = 50
const unreadJobsPerTick = 4

type Deps struct {
	Repository   tenantstore.Repository
	Details      func() attachment.DetailStore
	Now          func() time.Time
	Organization func() tracker.OrganizationID
	RecordError  func(error) error
	IsNotFound   func(error) bool
}

type Service struct{ deps Deps }

func New(deps Deps) *Service { return &Service{deps: deps} }

type Caller struct {
	Organization   tracker.OrganizationID
	Project        tracker.ProjectID
	Item           string
	Principal      string
	ActorPrincipal string
	CredentialID   string
	Name           string
	Agent          string
	HostedRole     string
	HostedName     string
	HostedSubject  string
	Hosted         bool
	EntryKey       bool
	Personal       bool
	AllProjects    bool
	ProjectFilter  accessstore.ProjectAccessFilter
	Authority      func(project tracker.ProjectID, item string) accessstore.Authority
	Authorize      func(ctx context.Context, reader accessstore.Reader, project tracker.ProjectID, item string, write bool) error
	RequireItem    func(ctx context.Context, reader tenantstore.Reader, project tracker.ProjectID, item string, id string) error
	ReadAttachment func(ctx context.Context, reader tenantstore.Reader, project tracker.ProjectID, item string, id string) error
}

type UnreadSummary struct {
	Count   *int64 `json:"count"`
	Pending bool   `json:"pending"`
}

type access struct {
	caller  Caller
	project tracker.ProjectID
	item    string
}

func newAccess(caller Caller) access {
	return access{caller: caller, project: caller.Project, item: caller.Item}
}

func (a access) authorize(ctx context.Context, reader accessstore.Reader, write bool) error {
	if a.caller.Authorize == nil || reader == nil {
		return accessstore.ErrDenied
	}
	return a.caller.Authorize(ctx, reader, a.project, a.item, write)
}

func (a access) requireItem(ctx context.Context, reader tenantstore.Reader, id string) error {
	if a.caller.RequireItem == nil {
		return accessstore.ErrDenied
	}
	return a.caller.RequireItem(ctx, reader, a.project, a.item, id)
}

func (a access) readAttachment(ctx context.Context, reader tenantstore.Reader, id string) error {
	if a.caller.ReadAttachment == nil {
		return accessstore.ErrDenied
	}
	return a.caller.ReadAttachment(ctx, reader, a.project, a.item, id)
}

func Invalid() error {
	return hubapi.PublicInvalid("Thread request is invalid")
}

func Limit(n int) int {
	if n == 0 {
		return 25
	}
	return n
}

func CheckArguments(in operatortool.ThreadArguments) error {
	if len(in.ThreadID) > 256 || len(in.ProjectID) > 256 || len(in.IssueID) > 256 || in.Limit < 0 || in.Limit > 50 || len(in.Search) > 200 || len(in.Key) > 200 || len(in.References) > 20 {
		return Invalid()
	}
	return nil
}

func RecordError(err error, fallback func(error) error) error {
	if errors.Is(err, threadstore.ErrIdempotencyConflict) {
		return &hubapi.Error{Code: "idempotency_conflict", Message: "Thread request key was already used", Status: http.StatusConflict, Public: true}
	}
	if errors.Is(err, corestore.ErrInvalidPage) {
		return Invalid()
	}
	if fallback == nil {
		return err
	}
	return fallback(err)
}

func (s *Service) recordError(err error) error {
	return RecordError(err, s.deps.RecordError)
}

func (s *Service) notFound(err error) bool {
	var failure *hubapi.Error
	if errors.As(err, &failure) && failure != nil && failure.Code == "not_found" {
		return true
	}
	return s.deps.IsNotFound != nil && s.deps.IsNotFound(err)
}

func (s *Service) Operation(ctx context.Context, caller Caller, name string, in operatortool.ThreadArguments) (any, error) {
	return s.operation(ctx, newAccess(caller), name, in)
}

func (s *Service) List(ctx context.Context, caller Caller, in operatortool.ThreadArguments) (threadstore.Page, error) {
	return s.listThreadRecords(ctx, newAccess(caller), in)
}

func (s *Service) RequestUnread(ctx context.Context, caller Caller, projects []string, refresh bool) (UnreadSummary, error) {
	if s.deps.Repository == nil {
		return UnreadSummary{}, operatortool.ErrServiceUnavailable
	}
	return s.requestUnread(ctx, newAccess(caller), projects, refresh)
}

func (s *Service) MaintainUnread(ctx context.Context) error {
	for range unreadJobsPerTick {
		more, _, err := s.AdvanceUnread(ctx)
		if err != nil {
			return err
		}
		if !more {
			return nil
		}
	}
	return nil
}

func (s *Service) AdvanceUnread(ctx context.Context) (bool, int, error) {
	if s.deps.Repository == nil {
		return false, 0, operatortool.ErrServiceUnavailable
	}
	return s.AdvanceUnreadRecord(ctx)
}
