package labels

import (
	"context"
	"hash/fnv"
	"slices"
	"sort"
	"strings"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Deps struct {
	Repository  tenantstore.Repository
	RecordError func(error) error
}

type Service struct{ deps Deps }

func New(deps Deps) *Service {
	if deps.RecordError == nil {
		deps.RecordError = hubauth.RecordError
	}
	return &Service{deps: deps}
}

var managedLabelPrefixes = []string{"priority:", "effort:", "detent:"}

var labelPalette = []string{
	"#6e79d6",
	"#3f9e6f",
	"#c2803a",
	"#c05b6b",
	"#4d94bb",
	"#8a6bbf",
	"#3f9a94",
	"#a8763f",
	"#5f8f45",
	"#b5607f",
}

type Label = tracker.NativeLabel

type List struct {
	Items []Label `json:"items"`
}

func IsManaged(label string) bool {
	folded := strings.ToLower(strings.TrimSpace(label))
	if folded == "" {
		return true
	}
	return slices.ContainsFunc(managedLabelPrefixes, func(prefix string) bool {
		return strings.HasPrefix(folded, prefix)
	})
}

func Color(name string) string {
	digest := fnv.New32a()
	_, _ = digest.Write([]byte(strings.ToLower(strings.TrimSpace(name))))
	return labelPalette[int(digest.Sum32()%uint32(len(labelPalette)))]
}

func (s *Service) Read(ctx context.Context, scope hubapi.Scope) ([]Label, error) {
	if s.deps.Repository == nil {
		return nil, hubapi.WorkItemStorageUnavailable()
	}
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	var counts []corestore.LabelCount
	err := s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		project, err := r.Project(ctx, scope.Project)
		if err != nil {
			return err
		}
		if project.DeletedAt != nil {
			return corestore.ErrNotFound
		}
		counts, err = r.LabelCatalog(ctx, scope.Project, scope.HostedRole == "reporter", scope.Actor.PrincipalID)
		return err
	})
	if err != nil {
		return nil, s.deps.RecordError(err)
	}
	labels := make([]Label, 0, len(counts))
	for _, label := range counts {
		if !IsManaged(label.Name) {
			labels = append(labels, Label{Name: label.Name, Count: int(label.Count), Color: Color(label.Name)})
		}
	}
	sort.Slice(labels, func(a, b int) bool {
		if labels[a].Count != labels[b].Count {
			return labels[a].Count > labels[b].Count
		}
		return labels[a].Name < labels[b].Name
	})
	return labels, nil
}
