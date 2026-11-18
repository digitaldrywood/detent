package projectstore

import (
	"errors"
	"slices"
	"sort"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/selector"
)

func ModelParts(m config.ModelSelection) ([]ModelPart, error) {
	if len(m.Levels)+len(m.Stages)+len(m.Sources) >= PagePartLimit {
		return nil, errors.New("project model exceeds page limit")
	}
	top := ModelPart{Kind: "model", Enabled: m.Enabled, Preset: m.Preset, NormalModel: m.NormalModel, ComplexModel: m.ComplexModel, DefaultLevel: m.DefaultLevel, Unavailable: m.Unavailable, LevelsPresent: m.Levels != nil, StagesPresent: m.Stages != nil, RulesPresent: m.Rules != nil}
	if m.BackendKinds != nil {
		top.BackendKinds = append([]string{}, (*m.BackendKinds)...)
	}
	if m.FallbackOrder != nil {
		top.FallbackOrder = append([]string{}, (*m.FallbackOrder)...)
	}
	parts := []ModelPart{top}
	keys := func(values map[string]string) []string {
		keys := make([]string, 0, len(values))
		for k := range values {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return keys
	}
	for _, name := range keys(m.Sources) {
		parts = append(parts, ModelPart{Kind: "source", Name: name, Model: new(m.Sources[name])})
	}
	names := make([]string, 0, len(m.Levels))
	for name := range m.Levels {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		v := m.Levels[name]
		parts = append(parts, ModelPart{Kind: "level", Name: name, Model: v.Model, Effort: v.Effort, Duration: v.MaxSessionDurationMS, Tokens: v.MaxSessionTokens})
	}
	names = names[:0]
	for name := range m.Stages {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		v := m.Stages[name]
		parts = append(parts, ModelPart{Kind: "stage", Name: name, Model: v.Model, Effort: v.Effort, Level: v.Level, IssueComplexity: v.IssueComplexity})
	}
	var addSelector func(selector.Selector, int64, string) error
	addSelector = func(s selector.Selector, parent int64, edge string) error {
		if len(parts) >= PagePartLimit {
			return errors.New("project model exceeds page limit")
		}
		part := ModelPart{Kind: "selector", Parent: parent, Edge: edge, Assignees: s.AssigneeIn, Authors: s.AuthorIn, Priorities: s.PriorityIn, Include: s.Labels.Include, Exclude: s.Labels.Exclude}
		for _, field := range s.Fields {
			part.FieldNames = append(part.FieldNames, field.Name)
			part.FieldValues = append(part.FieldValues, field.Value)
		}
		index := int64(len(parts))
		parts = append(parts, part)
		for _, child := range s.And {
			if err := addSelector(child, index, "and"); err != nil {
				return err
			}
		}
		for _, child := range s.Or {
			if err := addSelector(child, index, "or"); err != nil {
				return err
			}
		}
		return nil
	}
	if m.Rules != nil {
		for _, r := range *m.Rules {
			index := int64(len(parts))
			parts = append(parts, ModelPart{Kind: "rule", Name: r.Name, Disabled: r.Disabled, Level: new(r.Level), Roles: r.Roles, Efforts: r.Efforts})
			if err := addSelector(r.Selector, index, "root"); err != nil {
				return nil, err
			}
		}
	}
	if len(parts) > PagePartLimit {
		return nil, errors.New("project model exceeds page limit")
	}
	return parts, nil
}

func ModelFromParts(parts []ModelPart) (config.ModelSelection, error) {
	if len(parts) == 0 || len(parts) > PagePartLimit || parts[0].Kind != "model" {
		return config.ModelSelection{}, errors.New("invalid project model page")
	}
	top := parts[0]
	m := config.ModelSelection{Enabled: top.Enabled, Preset: top.Preset, NormalModel: top.NormalModel, ComplexModel: top.ComplexModel, DefaultLevel: top.DefaultLevel, Unavailable: top.Unavailable}
	if top.BackendKinds != nil {
		m.BackendKinds = &top.BackendKinds
	}
	if top.FallbackOrder != nil {
		m.FallbackOrder = &top.FallbackOrder
	}
	if top.LevelsPresent {
		m.Levels = map[string]config.ModelSelectionDefaults{}
	}
	if top.StagesPresent {
		m.Stages = map[string]config.ModelSelectionStage{}
	}
	if top.RulesPresent {
		m.Rules = &[]config.ModelSelectionRule{}
	}
	selectors := make(map[int64]*selector.Selector)
	rules := make(map[int64]int)
	for index, part := range parts[1:] {
		ordinal := int64(index + 1)
		switch part.Kind {
		case "source":
			if part.Model == nil {
				return m, errors.New("invalid project model source")
			}
			if m.Sources == nil {
				m.Sources = map[string]string{}
			}
			m.Sources[part.Name] = *part.Model
		case "level":
			if m.Levels == nil {
				return m, errors.New("invalid project model levels")
			}
			m.Levels[part.Name] = config.ModelSelectionDefaults{Model: part.Model, Effort: part.Effort, MaxSessionDurationMS: part.Duration, MaxSessionTokens: part.Tokens}
		case "stage":
			if m.Stages == nil {
				return m, errors.New("invalid project model stages")
			}
			m.Stages[part.Name] = config.ModelSelectionStage{Model: part.Model, Effort: part.Effort, Level: part.Level, IssueComplexity: part.IssueComplexity}
		case "rule":
			if m.Rules == nil || part.Level == nil {
				return m, errors.New("invalid project model rules")
			}
			rules[ordinal] = len(*m.Rules)
			*m.Rules = append(*m.Rules, config.ModelSelectionRule{Name: part.Name, Disabled: part.Disabled, Level: *part.Level, Roles: part.Roles, Efforts: part.Efforts})
		case "selector":
			if part.Parent >= ordinal || len(part.FieldNames) != len(part.FieldValues) {
				return m, errors.New("invalid project selector parent")
			}
			s := &selector.Selector{AssigneeIn: part.Assignees, AuthorIn: part.Authors, PriorityIn: part.Priorities, Labels: selector.Labels{Include: part.Include, Exclude: part.Exclude}}
			for i, name := range part.FieldNames {
				s.Fields = append(s.Fields, selector.FieldEquals{Name: name, Value: part.FieldValues[i]})
			}
			selectors[ordinal] = s
		default:
			return m, errors.New("invalid project model part")
		}
	}
	for i := len(parts) - 1; i > 0; i-- {
		part := parts[i]
		if part.Kind != "selector" {
			continue
		}
		s := selectors[int64(i)]
		slices.Reverse(s.And)
		slices.Reverse(s.Or)
		switch part.Edge {
		case "root":
			rule, ok := rules[part.Parent]
			if !ok {
				return m, errors.New("invalid project selector rule")
			}
			(*m.Rules)[rule].Selector = *s
		case "and", "or":
			parent, ok := selectors[part.Parent]
			if !ok {
				return m, errors.New("invalid project selector ancestry")
			}
			if part.Edge == "and" {
				parent.And = append(parent.And, *s)
			} else {
				parent.Or = append(parent.Or, *s)
			}
		default:
			return m, errors.New("invalid project selector edge")
		}
	}
	return m, nil
}
