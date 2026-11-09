package hubserver

import (
	"errors"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/localboard"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) importLocalBoard(c echo.Context) error {
	var request localboard.Request
	if err := decodeAPIJSON(c, &request); err != nil {
		var oversized *http.MaxBytesError
		if !errors.As(err, &oversized) {
			err = errors.New("invalid local board request")
		}
		return s.invalidAPIRequest(c, err)
	}
	scope := nativeRequestScope(c)
	if err := localboard.Validate(request.Record); err != nil {
		return s.nativeAPIError(c, nativeInvalid(err.Error()))
	}
	if err := validateNativeContent(request.Record.Title, request.Record.Body, nil, nil, request.Record.Priority); err != nil {
		return s.nativeAPIError(c, err)
	}
	result, err := s.importLocalBoardRecord(c.Request().Context(), scope, request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, result)
}

func localBoardState(states []tracker.NativeState, source string) (string, bool, error) {
	for _, state := range states {
		if strings.EqualFold(strings.TrimSpace(source), state.Name) {
			return state.Name, false, nil
		}
	}
	for _, name := range []string{"Backlog", "Triage", ""} {
		for _, state := range states {
			if !state.Dispatchable && !state.Terminal && (name == "" || state.Name == name) {
				return state.Name, true, nil
			}
		}
	}
	return "", false, nativePublicInvalid("Local board import requires a nondispatchable nonterminal fallback state")
}
