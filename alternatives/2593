package releaseaudit

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
)

func (e *execution) seedPrior(ctx context.Context) error {
	units := map[string]map[string]json.RawMessage{}
	for _, previous := range e.prior {
		if previous.Identity.Repository != e.request.Identity.Repository {
			continue
		}
		var ledger []map[string]json.RawMessage
		if err := json.Unmarshal(previous.Ledger, &ledger); err != nil {
			return err
		}
		for _, unit := range ledger {
			unit = maps.Clone(unit)
			var id string
			if json.Unmarshal(unit["coverage_id"], &id) != nil || id == "" {
				return errors.New("prior release audit ledger is invalid")
			}
			priorStatus := `"none"`
			switch string(unit["status"]) {
			case `"covered"`:
				priorStatus = `"prior_covered_changed_source"`
				if previous.Identity.Commit == e.request.Identity.Commit {
					priorStatus = `"prior_covered_same_source"`
				}
			case `"blocked"`:
				priorStatus = `"prior_blocked"`
			case `"deferred"`:
				priorStatus = `"prior_deferred"`
			case `"out_of_scope"`:
				priorStatus = `"prior_out_of_scope"`
			}
			unit["prior_status"] = json.RawMessage(priorStatus)
			unit["status"] = json.RawMessage(`"planned"`)
			unit["agent_id"] = json.RawMessage(`null`)
			for _, key := range []string{"attempts", "reviewed_paths", "local_checks", "result_fingerprints", "unresolved"} {
				unit[key] = json.RawMessage(`[]`)
			}
			units[id] = unit
		}
		for _, candidate := range previous.Candidates {
			e.carryCandidate(candidate)
		}
		for _, record := range previous.Findings {
			var finding struct {
				Fingerprint string `json:"fingerprint"`
			}
			if json.Unmarshal(record, &finding) != nil || finding.Fingerprint == "" {
				return errors.New("prior release audit finding is invalid")
			}
			coverage := ""
			for _, unit := range ledger {
				var fingerprints []string
				if err := json.Unmarshal(unit["result_fingerprints"], &fingerprints); err != nil {
					return err
				}
				if slices.Contains(fingerprints, finding.Fingerprint) {
					if err := json.Unmarshal(unit["coverage_id"], &coverage); err != nil {
						return err
					}
					break
				}
			}
			if coverage == "" {
				return errors.New("prior release audit finding is not linked to coverage")
			}
			e.carryCandidate(Candidate{Fingerprint: finding.Fingerprint, CoverageID: coverage, Record: record})
		}
	}
	ids := make([]string, 0, len(units))
	for id := range units {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	ledger := make([]map[string]json.RawMessage, 0, len(ids))
	for _, id := range ids {
		ledger = append(ledger, units[id])
	}
	if len(ledger) == 0 {
		return nil
	}
	raw, err := json.Marshal(ledger)
	if err != nil {
		return err
	}
	if err := validateDocument(ctx, e.directory, "ledger", raw); err != nil {
		return err
	}
	e.state.Ledger = raw
	return nil
}

func (e *execution) carryCandidate(candidate Candidate) {
	if !slices.ContainsFunc(e.state.Candidates, func(existing Candidate) bool { return existing.Fingerprint == candidate.Fingerprint }) {
		e.state.Candidates = append(e.state.Candidates, candidate)
	}
}
