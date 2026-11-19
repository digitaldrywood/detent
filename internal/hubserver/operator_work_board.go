package hubserver

import (
	"context"
	"encoding/json"
	"net/url"

	"github.com/digitaldrywood/detent/internal/hubserver/boardview"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/operatorcommands"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

func (s *Service) executeWorkBoard(ctx context.Context, call operatortool.Call) (operatortool.Result, error) {
	deps := operatorcommands.WorkBoardDeps{
		EncodeState: func(archived bool, window string) ([]byte, error) {
			state := defaultWorkBoardState()
			state.Archived = archived
			if window != "" {
				state.CompletedWindow = window
			}
			return json.Marshal(state)
		},
		ReadBoard: func(ctx context.Context, credential hubapi.APICredential, organization string, params url.Values) (*boardview.CompactBoard, error) {
			request, err := parseWorkBoardRequest(params)
			if err != nil {
				return nil, err
			}
			view, err := s.readWorkBoardFor(ctx, credential, organization, request)
			return view.compact, err
		},
	}
	return deps.Read(ctx, call)
}
