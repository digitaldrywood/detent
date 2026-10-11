package apikey

import (
	"slices"
	"strings"
)

type ProjectContext struct {
	Access   string   `json:"project_access"`
	Projects []string `json:"project_ids"`
}

func (p ProjectContext) Valid() bool {
	if len(p.Projects) > 200 || len(NormalizeProjectIDs(p.Projects)) != len(p.Projects) {
		return false
	}
	for _, id := range p.Projects {
		if strings.TrimSpace(id) != id || len(id) > 256 {
			return false
		}
	}
	switch p.Access {
	case "all":
		return len(p.Projects) == 0
	case "selected":
		return len(p.Projects) > 0
	case "project":
		return len(p.Projects) == 1
	default:
		return false
	}
}

func (p ProjectContext) Allows(project string) bool {
	return p.Valid() && (p.Access == "all" || slices.Contains(p.Projects, project))
}

type KeyAuthority struct {
	ProjectContext
	ID              string   `json:"id"`
	Kind            string   `json:"kind"`
	Permission      Scope    `json:"permission"`
	BlockedProjects []string `json:"blocked_project_ids,omitempty"`
}

func (k KeyAuthority) Valid() bool {
	return k.ID != "" && len(k.ID) <= 256 && (k.Kind == "personal" || k.Kind == "service") && ValidScope(k.Permission) && k.ProjectContext.Valid() && len(k.BlockedProjects) <= 200
}

func (k KeyAuthority) Allows(project string) bool {
	return k.Valid() && k.ProjectContext.Allows(project) && !slices.Contains(k.BlockedProjects, project)
}

type Refusal struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (r *Refusal) Error() string { return r.Code + ": " + r.Message }
