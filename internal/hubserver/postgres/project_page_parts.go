package postgres

import (
	"context"
	"fmt"
	"strings"

	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
)

const projectPartColumns = "kind,name,parent,edge,model,effort,level,duration_ms,tokens,issue_complexity,enabled,preset,normal_model,complex_model,default_level,unavailable,backend_kinds,fallback_order,levels_present,stages_present,rules_present,disabled,roles,efforts,assignees,authors,priorities,include_labels,exclude_labels,field_names,field_values"

func scanProjectPart(row interface{ Scan(...any) error }, body *string) (projectstore.ModelPart, error) {
	var p projectstore.ModelPart
	err := row.Scan(body, &p.Kind, &p.Name, &p.Parent, &p.Edge, &p.Model, &p.Effort, &p.Level, &p.Duration, &p.Tokens, &p.IssueComplexity, &p.Enabled, &p.Preset, &p.NormalModel, &p.ComplexModel, &p.DefaultLevel, &p.Unavailable, &p.BackendKinds, &p.FallbackOrder, &p.LevelsPresent, &p.StagesPresent, &p.RulesPresent, &p.Disabled, &p.Roles, &p.Efforts, &p.Assignees, &p.Authors, &p.Priorities, &p.Include, &p.Exclude, &p.FieldNames, &p.FieldValues)
	return p, err
}

func (s *session) writeProjectParts(ctx context.Context, project string, parts []projectstore.ModelPart) error {
	for start := 0; start < len(parts); start += 1024 {
		end := min(start+1024, len(parts))
		var query strings.Builder
		query.WriteString("INSERT INTO project_model_page_parts(organization_id,project_id,ordinal," + projectPartColumns + ") VALUES ")
		args := make([]any, 0, (end-start)*34)
		for i := start; i < end; i++ {
			if i > start {
				query.WriteByte(',')
			}
			p := parts[i]
			values := []any{s.organization, project, int64(i), p.Kind, p.Name, p.Parent, p.Edge, p.Model, p.Effort, p.Level, p.Duration, p.Tokens, p.IssueComplexity, p.Enabled, p.Preset, p.NormalModel, p.ComplexModel, p.DefaultLevel, p.Unavailable, p.BackendKinds, p.FallbackOrder, p.LevelsPresent, p.StagesPresent, p.RulesPresent, p.Disabled, p.Roles, p.Efforts, p.Assignees, p.Authors, p.Priorities, p.Include, p.Exclude, p.FieldNames, p.FieldValues}
			query.WriteByte('(')
			for j, v := range values {
				if j > 0 {
					query.WriteByte(',')
				}
				args = append(args, v)
				fmt.Fprintf(&query, "$%d", len(args))
			}
			query.WriteByte(')')
		}
		if _, err := s.db.Exec(ctx, query.String(), args...); err != nil {
			return mapError(ctx, err)
		}
	}
	return nil
}
