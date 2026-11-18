package hubmaintenance

import "errors"

func RefreshThenReplay[E any](items int, refresh func() (bool, error), load func(limit int) ([]E, error), replay func(E) (bool, error)) (Outcome, error) {
	if items < 1 {
		return Outcome{}, errors.New("maintenance replay requires an item")
	}
	used := 1
	ready, err := refresh()
	if err != nil || !ready {
		return Outcome{Progress: err == nil, Items: used}, err
	}
	allowance := items - used
	if allowance == 0 {
		return Outcome{Progress: true, Items: used}, nil
	}
	events, err := load(allowance)
	if err != nil {
		return Outcome{Progress: true, Items: used}, err
	}
	if len(events) > allowance {
		return Outcome{Progress: true, Items: used}, errors.New("maintenance replay loaded more events than its allowance")
	}
	replayed := 0
	for _, event := range events {
		complete, err := replay(event)
		if err != nil {
			return Outcome{Progress: true, Items: used + replayed + 1}, err
		}
		if !complete {
			return Outcome{Progress: true, Items: used + replayed + 1}, nil
		}
		replayed++
	}
	return Outcome{Progress: true, Items: used + replayed}, nil
}
