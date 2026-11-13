package hubapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

const MaxAPIRequestBodyBytes = 1 << 20

const MaxChangeVersionRequestBytes = 28 << 20

// MaxAttemptDiffRequestBytes bounds the one endpoint whose body is legitimately
// larger than every other: a stored attempt diff carries the patches
// themselves, and the attempt diff contract bounds those at tracker.MaxDiffBytes.
// The allowance is that bound at the worst case of JSON string escaping plus
// room for the framing, so a diff the contract accepts is never refused by the
// transport instead. The factor is six because an encoder writes <, >, & and
// every control byte as a six-byte \uXXXX sequence, and a patch of HTML or
// generated code can be dense with them.
const MaxAttemptDiffRequestBytes = 6*tracker.MaxDiffBytes + (1 << 20)

// MaxActionRunReportBytes bounds a runner's action run report, which carries
// the run's whole output up to workspacesession.MaxExecOutputBytes, with the
// same six-fold escaping allowance: coloured build output is dense with ESC.
const MaxActionRunReportBytes = 6*workspacesession.MaxExecOutputBytes + (1 << 20)

func DecodeAPIJSON(c echo.Context, target any) error {
	if c == nil || c.Request() == nil {
		return errors.New("request is required")
	}
	request := c.Request()
	request.Body = http.MaxBytesReader(c.Response(), request.Body, APIRequestBodyLimit(c))
	decoder := json.NewDecoder(request.Body)
	credential, ok := c.Get("hub_api_credential").(APICredential)
	if !ok || credential.Scope != APIScopeWorker || !strings.HasSuffix(c.Path(), "/heartbeat") {
		decoder.DisallowUnknownFields()
	}
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("request body must contain one JSON value")
		}
		return err
	}
	return nil
}

// APIRequestBodyLimit is how many bytes this route's body may carry.
func APIRequestBodyLimit(c echo.Context) int64 {
	if c.Path() == NativeBase+"/conversations/:conversation/actions" {
		return 4 * CoordinatorWorkflowArgumentBytes
	}
	if limit := SharedEntryRequestBodyLimit(c.Request().Method, c.Path()); limit > cloudassert.MaxBodyBytes {
		return limit
	}
	if c.Path() == NativeBase+"/work-items/:item/source-intake" || c.Path() == NativeBase+"/onboarding/issue-intake/result" {
		return 64 << 20
	}
	if c.Path() == NativeBase+"/workspaces/:workspace/worker/action-runs" {
		return MaxActionRunReportBytes
	}
	return MaxAPIRequestBodyBytes
}

func SharedEntryRequestBodyLimit(method, path string) int64 {
	if method != http.MethodPost {
		return cloudassert.MaxBodyBytes
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) > 2 && parts[0] == "organizations" {
		parts = parts[2:]
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return cloudassert.MaxBodyBytes
		}
	}
	if len(parts) < 6 || parts[0] != "api" || parts[1] != "v2" || parts[2] != "organizations" || parts[4] != "projects" {
		return cloudassert.MaxBodyBytes
	}
	if len(parts) == 9 && parts[6] == "work-items" && parts[8] == "events" {
		return 8 * MaxAPIRequestBodyBytes
	}
	if len(parts) == 13 && parts[6] == "work-items" && parts[8] == "changes" && parts[10] == "versions" && parts[12] == "landing" {
		return MaxChangeVersionRequestBytes
	}
	if len(parts) == 11 && parts[6] == "work-items" && parts[8] == "changes" && parts[10] == "versions" {
		return MaxChangeVersionRequestBytes
	}
	if (len(parts) == 9 && parts[8] == "diff" || len(parts) == 10 && parts[8] == "diff" && parts[9] == "check") && parts[6] == "attempts" {
		return MaxAttemptDiffRequestBytes
	}
	return cloudassert.MaxBodyBytes
}

const (
	DefaultAPIPageLimit = 50
	MaxAPIPageLimit     = 200
)

func ParsePageLimit(value string) (int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return DefaultAPIPageLimit, nil
	}
	limit, err := strconv.Atoi(value)
	if err != nil || limit < 1 || limit > MaxAPIPageLimit {
		return 0, errors.New("page limit is invalid")
	}
	return limit, nil
}

const CoordinatorToolArgumentBytes = 16 << 10

const CoordinatorWorkflowArgumentBytes = 6*(128<<10) + CoordinatorToolArgumentBytes
