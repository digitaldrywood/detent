package selector

import (
	"errors"
	"fmt"
	"slices"

	"github.com/digitaldrywood/detent/internal/connector"
)

func NativeFilterHint(auth Selector, ctx Context) (connector.IssueFilterHint, error) {
	if problems := auth.Validate("authorization"); len(problems) != 0 {
		return connector.IssueFilterHint{}, fmt.Errorf("native authorization: %s", problems[0])
	}
	var result connector.IssueFilterHint
	var visit func(Selector) error
	visit = func(s Selector) error {
		if len(s.Or) > 1 || len(s.Fields) > 0 || len(s.PriorityIn) > 0 {
			return errors.New("native authorization cannot represent alternatives, fields or priority constraints")
		}
		for _, identity := range []struct {
			values   []string
			target   *[]string
			multiple bool
		}{{s.AuthorIn, &result.Authors, false}, {s.AssigneeIn, &result.Assignees, true}} {
			if len(identity.values) == 0 {
				continue
			}
			var resolved []string
			for _, value := range identity.values {
				for _, candidate := range resolveValue(value, ctx) {
					candidate = normalizeIdentity(candidate)
					if candidate != "" && !slices.Contains(resolved, candidate) {
						resolved = append(resolved, candidate)
					}
				}
			}
			if len(resolved) == 0 {
				return errors.New("native authorization identity has no resolved values")
			}
			previous := *identity.target
			if len(previous) == 0 {
				*identity.target = resolved
				continue
			}
			var intersection []string
			for _, value := range previous {
				if slices.Contains(resolved, value) {
					intersection = append(intersection, value)
				}
			}
			if len(intersection) == 0 || identity.multiple && len(intersection) != len(previous) && len(intersection) != len(resolved) {
				return errors.New("native authorization cannot represent composed identity constraints")
			}
			*identity.target = intersection
		}
		for _, labels := range []struct {
			values []string
			target *[]string
		}{{s.Labels.Include, &result.LabelInclude}, {s.Labels.Exclude, &result.LabelExclude}} {
			for _, value := range labels.values {
				value = normalizeLabel(value)
				if !slices.Contains(*labels.target, value) {
					*labels.target = append(*labels.target, value)
				}
			}
		}
		for _, child := range s.And {
			if err := visit(child); err != nil {
				return err
			}
		}
		for _, child := range s.Or {
			if err := visit(child); err != nil {
				return err
			}
		}
		return nil
	}
	if err := visit(auth); err != nil {
		return connector.IssueFilterHint{}, err
	}
	return result, nil
}
