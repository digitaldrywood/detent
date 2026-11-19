package coordinator

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/digitaldrywood/detent/internal/runner"

	"github.com/digitaldrywood/detent/internal/hubserver/conversations"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func ActionError(err error) error {
	var request *operatortool.RequestError
	if errors.As(err, &request) {
		return &hubapi.NativeError{Code: cmp.Or(request.Code, "invalid_request"), Message: request.Message, Status: http.StatusUnprocessableEntity, PublicMessage: true}
	}
	var conflict *operatortool.ConflictError
	if errors.As(err, &conflict) {
		message := conflict.Message
		if message == "" && conflict.Code == "revision_conflict" {
			message = fmt.Sprintf("The item changed after this was proposed; its current revision is %d. Ask for a fresh proposal.", conflict.CurrentRevision)
		} else if message == "" {
			message = "The item changed after this was proposed. Ask for a fresh proposal."
		}
		return &hubapi.NativeError{Code: conflict.Code, Message: message, CurrentRevision: tracker.Revision(conflict.CurrentRevision), Status: http.StatusConflict, PublicMessage: true}
	}
	var native *hubapi.NativeError
	if errors.As(hubapi.FromFeatureError(err), &native) {
		return errors.New(native.Message)
	}
	if errors.Is(err, operatortool.ErrAccessDenied) || errors.Is(err, operatortool.ErrInvalidArguments) || errors.Is(err, mutation.ErrConflict) || errors.Is(err, ErrToolArguments) || errors.Is(err, ErrProjectUnreadable) {
		return err
	}
	return errors.New("project change is unavailable")
}

func ToolError(err error) runner.AgentToolResult {
	err = hubapi.FromFeatureError(err)
	message := err.Error()
	var native *hubapi.NativeError
	var request *operatortool.RequestError
	switch {
	case errors.As(err, &native) && native.Code == "invalid_request":
		message = native.Message
	case errors.As(err, &request) && request.Message != "":
		message = request.Message
	}
	encoded, encodeErr := json.Marshal(map[string]string{"error": conversations.BoundRunes(message, ErrorRunes)})
	if encodeErr != nil {
		encoded = []byte(`{"error":"tool failed"}`)
	}
	return runner.AgentToolResult{Content: string(encoded), Success: false}
}
