package hubserver

import (
	"context"
	"hash/fnv"
	"net/http"
	"slices"
	"sort"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// managedLabelPrefixes are the prefixes the hub owns. Compared lower-case.
var managedLabelPrefixes = []string{"priority:", "effort:", defaultLabelPrefix}

var labelPalette = []string{
	"#6e79d6", // indigo
	"#3f9e6f", // green
	"#c2803a", // amber
	"#c05b6b", // rose
	"#4d94bb", // sky
	"#8a6bbf", // violet
	"#3f9a94", // teal
	"#a8763f", // bronze
	"#5f8f45", // olive
	"#b5607f", // magenta
}

type nativeLabel = tracker.NativeLabel

type nativeLabelList struct {
	Items []nativeLabel `json:"items"`
}

// isManagedLabel reports whether a label names a field the hub owns rather
// than something a reader may attach.
func isManagedLabel(label string) bool {
	folded := strings.ToLower(strings.TrimSpace(label))
	if folded == "" {
		return true
	}
	return slices.ContainsFunc(managedLabelPrefixes, func(prefix string) bool {
		return strings.HasPrefix(folded, prefix)
	})
}

// labelColor derives a stable colour from a label's name. Case-insensitive,
// so `Bug` and `bug` cannot end up two colours in two clients.
func labelColor(name string) string {
	digest := fnv.New32a()
	_, _ = digest.Write([]byte(strings.ToLower(strings.TrimSpace(name))))
	return labelPalette[int(digest.Sum32()%uint32(len(labelPalette)))]
}

// listNativeLabels answers GET {nativeBase}/labels.
func (s *Service) listNativeLabels(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	if err := validateNativeQuery(c.QueryParams()); err != nil {
		return s.nativeAPIError(c, err)
	}
	labels, err := s.readNativeLabels(ctx, nativeRequestScope(c))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, nativeLabelList{Items: labels})
}

func (s *Service) readNativeLabels(ctx context.Context, scope nativeScope) ([]nativeLabel, error) {
	if s.config.TenantRepository == nil {
		return nil, workItemStorageUnavailable()
	}
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	var counts []corestore.LabelCount
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		project, err := r.Project(ctx, scope.project)
		if err != nil {
			return err
		}
		if project.DeletedAt != nil {
			return corestore.ErrNotFound
		}
		counts, err = r.LabelCatalog(ctx, scope.project, scope.credential.HostedRole == "reporter", scope.actor().PrincipalID)
		return err
	})
	if err != nil {
		return nil, projectRecordError(err)
	}
	labels := make([]nativeLabel, 0, len(counts))
	for _, label := range counts {
		if !isManagedLabel(label.Name) {
			labels = append(labels, nativeLabel{Name: label.Name, Count: int(label.Count), Color: labelColor(label.Name)})
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
