package cli

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/digitaldrywood/detent/internal/localboard"
)

type localBoardImporter interface {
	ImportLocalBoard(context.Context, localboard.Request) (localboard.Result, error)
}

func importLocalBoardRecords(ctx context.Context, client localBoardImporter, records []localboard.Record, dryRun bool) ([]localboard.Result, error) {
	plans := make([]localboard.Result, 0, len(records))
	for _, record := range records {
		if err := localboard.Validate(record); err != nil {
			return nil, err
		}
		request := localboard.Request{Record: record, DryRun: true}
		raw, err := json.Marshal(request)
		if err != nil {
			return nil, errors.New("local board import request encoding failed; issue content omitted")
		}
		if len(raw) > 1<<20 {
			return nil, errors.New("local board item exceeds 1 MiB request bound")
		}
		plan, err := client.ImportLocalBoard(ctx, request)
		if err != nil {
			return nil, errors.New("local board import preflight failed; issue content omitted")
		}
		plans = append(plans, plan)
	}
	if dryRun {
		return plans, nil
	}
	results := make([]localboard.Result, 0, len(records))
	for _, record := range records {
		result, err := client.ImportLocalBoard(ctx, localboard.Request{Record: record})
		if err != nil {
			return nil, errors.New("local board item publication failed; completed items can be replayed safely; issue content omitted")
		}
		results = append(results, result)
	}
	return results, nil
}
