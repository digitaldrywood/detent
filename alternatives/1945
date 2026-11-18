package githubbinding

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/githubcontract"
	"github.com/digitaldrywood/detent/internal/hubserver/githubwebhook"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Request struct {
	tracker.Mutation
	ExpectedRevision tracker.Revision `json:"expected_revision,string"`
	Repository       string           `json:"repository"`
	Source           string           `json:"source,omitempty"`
}

type Deps struct {
	Repository     tenantstore.Repository
	Now            func() time.Time
	Backend        githubcontract.ReconcileBackend
	Recheck        func(context.Context, hubapi.Scope) error
	Receipt        func(context.Context, hubapi.Scope, string, string, string) (json.RawMessage, bool, error)
	ReadProject    func(context.Context, hubapi.Scope) (projectstore.Record, error)
	Mutate         func(context.Context, hubapi.Scope, string, tracker.Mutation, any, any, func(tenantstore.Writer) error) (json.RawMessage, error)
	RequireFeature func(context.Context, tenantstore.Reader, string, time.Time) error
}

type Service struct{ deps Deps }

func New(deps Deps) *Service { return &Service{deps: deps} }

func Refusal(code, message string) error {
	return &hubapi.Error{Code: code, Message: message, Status: http.StatusUnprocessableEntity, Public: true}
}

func (s *Service) Bind(ctx context.Context, scope hubapi.Scope, operation string, request Request) (json.RawMessage, error) {

	if request.Source == "runner_checkout" {
		return s.bindCheckout(ctx, scope, operation, request)
	}
	if _, _, valid := githubwebhook.SplitRepositoryFullName(request.Repository); !valid {
		return nil, hubapi.PublicInvalid("Repository must be owner/name")
	}
	if request.Source != "" {
		return nil, hubapi.PublicInvalid("Repository source is invalid")
	}
	return s.bindGitHub(ctx, scope, operation, request)
}
