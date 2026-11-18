package hubmaintenance

import "errors"

type Substep func(limit int) (used int, err error)

type PageSubstep func(limit int) (used, progressed int, idle bool, err error)

func ShareItems(items int, backfill, background Substep, pages PageSubstep) (Outcome, error) {
	slice := min(4, items/2)
	reserve := slice / 2
	run := func(step Substep, limit int) (int, error) {
		if limit < 1 {
			return 0, nil
		}
		used, err := step(limit)
		if used < 0 || used > limit {
			return 0, errors.Join(err, errors.New("maintenance substep used items outside its limit"))
		}
		return used, err
	}
	backfillUsed, backfillErr := run(backfill, reserve)
	backgroundUsed, backgroundErr := run(background, slice-backfillUsed)
	if backfillErr == nil && backfillUsed == reserve {
		var more int
		more, backfillErr = run(backfill, slice-backfillUsed-backgroundUsed)
		backfillUsed += more
	}
	remaining := items - backfillUsed - backgroundUsed
	used, progressed, idle, pageErr := pages(remaining)
	if used < 0 || used > remaining || progressed < 0 || progressed > used {
		return Outcome{}, errors.Join(pageErr, errors.New("maintenance page substep used items outside its limit"))
	}
	remaining -= used
	if idle && pageErr == nil && backfillErr == nil {
		var more int
		more, backfillErr = run(backfill, remaining)
		backfillUsed += more
		remaining -= more
	}
	return Outcome{Progress: backfillUsed+backgroundUsed+progressed > 0, Items: items - remaining}, errors.Join(pageErr, backgroundErr, backfillErr)
}
