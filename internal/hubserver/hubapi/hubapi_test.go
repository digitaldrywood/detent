package hubapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestErrorConstructors(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		err      error
		code     string
		message  string
		status   int
		public   bool
		revision tracker.Revision
		text     string
		encoded  string
	}{
		{"invalid", hubapi.Invalid("title is required"), "invalid_request", "title is required", http.StatusUnprocessableEntity, false, 0, "invalid_request", `{"code":"invalid_request","message":"title is required"}`},
		{"public invalid", hubapi.PublicInvalid("title is required"), "invalid_request", "title is required", http.StatusUnprocessableEntity, true, 0, "title is required", `{"code":"invalid_request","message":"title is required"}`},
		{"not found", hubapi.NotFound(), "not_found", "Resource was not found", http.StatusNotFound, true, 0, "Resource was not found", `{"code":"not_found","message":"Resource was not found"}`},
		{"conflict", hubapi.Conflict(7), "revision_conflict", "Resource has changed", http.StatusConflict, true, 7, "Resource has changed", `{"code":"revision_conflict","message":"Resource has changed","current_revision":"7"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var failure *hubapi.Error
			if !errors.As(fmt.Errorf("step: %w", test.err), &failure) {
				t.Fatalf("error %T is not a hubapi.Error", test.err)
			}
			if failure.Code != test.code || failure.Message != test.message || failure.Status != test.status || failure.Public != test.public || failure.CurrentRevision != test.revision {
				t.Fatalf("error = %+v", failure)
			}
			if failure.Error() != test.text {
				t.Fatalf("Error() = %q, want %q", failure.Error(), test.text)
			}
			encoded, err := json.Marshal(failure)
			if err != nil {
				t.Fatal(err)
			}
			if string(encoded) != test.encoded {
				t.Fatalf("JSON = %s, want %s", encoded, test.encoded)
			}
		})
	}
}

func TestNotFoundRecordsCallSite(t *testing.T) {
	t.Parallel()
	var failure *hubapi.Error
	if !errors.As(hubapi.NotFound(), &failure) {
		t.Fatal("NotFound is not a hubapi.Error")
	}
	attributes := failure.LogAttributes()
	if len(attributes) != 2 || attributes[0] != "not_found_source" || !strings.HasPrefix(fmt.Sprint(attributes[1]), "hubapi_test.go:") {
		t.Fatalf("log attributes = %v", attributes)
	}
	attributes[0] = "changed"
	if failure.LogAttributes()[0] != "not_found_source" {
		t.Fatal("LogAttributes exposed its backing array")
	}
}

type authorizeReader struct{ accessstore.Reader }

func TestScopeAuthorize(t *testing.T) {
	t.Parallel()
	denied := errors.New("denied")
	facts := hubapi.Facts{Organization: "org_example", Project: "prj_example", CredentialID: "credential_example", Admin: true, Actor: tracker.Actor{Kind: "human", PrincipalID: "user_example"}}
	for _, test := range []struct {
		name      string
		scope     func(*[]bool) hubapi.Scope
		reader    accessstore.Reader
		write     bool
		want      error
		wantCalls []bool
	}{
		{"read allowed", func(calls *[]bool) hubapi.Scope {
			return hubapi.NewScope(facts, func(_ context.Context, _ accessstore.Reader, write bool) error {
				*calls = append(*calls, write)
				return nil
			})
		}, authorizeReader{}, false, nil, []bool{false}},
		{"write denied", func(calls *[]bool) hubapi.Scope {
			return hubapi.NewScope(facts, func(_ context.Context, _ accessstore.Reader, write bool) error {
				*calls = append(*calls, write)
				if write {
					return denied
				}
				return nil
			})
		}, authorizeReader{}, true, denied, []bool{true}},
		{"zero scope", func(*[]bool) hubapi.Scope { return hubapi.Scope{} }, authorizeReader{}, false, accessstore.ErrDenied, nil},
		{"nil authorizer", func(*[]bool) hubapi.Scope { return hubapi.NewScope(facts, nil) }, authorizeReader{}, true, accessstore.ErrDenied, nil},
		{"nil reader", func(calls *[]bool) hubapi.Scope {
			return hubapi.NewScope(facts, func(_ context.Context, _ accessstore.Reader, write bool) error {
				*calls = append(*calls, write)
				return nil
			})
		}, nil, false, accessstore.ErrDenied, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var calls []bool
			err := test.scope(&calls).Authorize(t.Context(), test.reader, test.write)
			if !errors.Is(err, test.want) || (test.want == nil) != (err == nil) {
				t.Fatalf("Authorize = %v, want %v", err, test.want)
			}
			if fmt.Sprint(calls) != fmt.Sprint(test.wantCalls) {
				t.Fatalf("authorizer calls = %v, want %v", calls, test.wantCalls)
			}
		})
	}
	scope := hubapi.NewScope(facts, nil)
	if scope.Organization != facts.Organization || scope.Project != facts.Project || scope.CredentialID != facts.CredentialID || !scope.Admin || scope.Actor != facts.Actor {
		t.Fatalf("scope facts = %+v", scope.Facts)
	}
}

func TestNewID(t *testing.T) {
	t.Parallel()
	pattern := regexp.MustCompile(`^prj_[0-9a-f]{32}$`)
	seen := map[string]bool{}
	for range 64 {
		id := hubapi.NewID("prj")
		if !pattern.MatchString(id) {
			t.Fatalf("NewID = %q", id)
		}
		if seen[id] {
			t.Fatalf("NewID repeated %q", id)
		}
		seen[id] = true
	}
}

func TestCommandHash(t *testing.T) {
	t.Parallel()
	type command struct {
		Title string `json:"title"`
		Count int    `json:"count"`
	}
	for _, test := range []struct {
		name    string
		input   any
		same    any
		differs any
		want    string
	}{
		{"struct", command{"Example", 1}, command{"Example", 1}, command{"Example", 2}, ""},
		{"map key order", map[string]int{"a": 1, "b": 2}, map[string]int{"b": 2, "a": 1}, map[string]int{"a": 2, "b": 1}, "43258cff783fe7036d8a43033f830adfc60ec037382473548ac742b888292777"},
		{"null", nil, nil, "", "74234e98afe7498fb5daf1f36ac2d78acc339464f950703b8c019892f982b90b"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			first, err := hubapi.CommandHash(test.input)
			if err != nil {
				t.Fatal(err)
			}
			second, err := hubapi.CommandHash(test.same)
			if err != nil {
				t.Fatal(err)
			}
			other, err := hubapi.CommandHash(test.differs)
			if err != nil {
				t.Fatal(err)
			}
			if first != second || first == other || len(first) != 64 {
				t.Fatalf("hashes first=%q second=%q other=%q", first, second, other)
			}
			if test.want != "" && first != test.want {
				t.Fatalf("hash = %q, want %q", first, test.want)
			}
		})
	}
	if _, err := hubapi.CommandHash(make(chan int)); err == nil {
		t.Fatal("CommandHash accepted a value JSON cannot encode")
	}
}

func TestDecodeJSON(t *testing.T) {
	t.Parallel()
	type payload struct {
		Title string `json:"title"`
	}
	for _, test := range []struct {
		name    string
		body    string
		limit   int64
		want    string
		wantErr string
	}{
		{"valid", `{"title":"Example"}`, 0, "Example", ""},
		{"unknown field", `{"title":"Example","other":1}`, 0, "", "unknown field"},
		{"second value", `{"title":"Example"} {"title":"Other"}`, 0, "", "one JSON value"},
		{"trailing garbage", `{"title":"Example"} x`, 0, "", "invalid character"},
		{"empty", ``, 0, "", "EOF"},
		{"over limit", `{"title":"` + strings.Repeat("a", 64) + `"}`, 16, "", "request body too large"},
		{"default limit", `{"title":"` + strings.Repeat("a", int(hubapi.MaxRequestBodyBytes)) + `"}`, 0, "", "request body too large"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(test.body))
			c := echo.New().NewContext(request, httptest.NewRecorder())
			var decoded payload
			var err error
			if test.limit == 0 {
				err = hubapi.DecodeJSON(c, &decoded)
			} else {
				err = hubapi.DecodeJSONLimit(c, &decoded, test.limit)
			}
			if test.wantErr == "" {
				if err != nil || decoded.Title != test.want {
					t.Fatalf("decoded %+v, err %v", decoded, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("err = %v, want %q", err, test.wantErr)
			}
		})
	}
	if err := hubapi.DecodeJSON(nil, &payload{}); err == nil {
		t.Fatal("DecodeJSON accepted a nil context")
	}
}
