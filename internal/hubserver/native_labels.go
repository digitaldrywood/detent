package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"net/http"
	"slices"
	"sort"
	"strings"

	"github.com/labstack/echo/v4"
)

// The project's label catalogue (decisions section 19.1).
//
// A tracker that lets a reader pick a label has to be able to say which
// labels exist. The hub models labels as a JSON array on each work item and
// has no label table, so the catalogue is the union of what the project's
// items actually carry — which is the honest answer, and the same set the
// board's filter menu builds client-side from one loaded page. Serving it
// here means the picker sees every label in the project rather than only the
// ones on the page in front of it.
//
// Two kinds of label never appear:
//
//   - a managed label. `priority:` and `effort:` are fields the hub owns and
//     shows in their own rows, and `detent:` names the reserved run-mode
//     labels `requireUnreservedLabels` already refuses on a write. Offering
//     any of them in a label picker would invite a reader to edit a field
//     through the wrong control.
//   - the empty string, which no write can produce but an imported item can.
//
// A colour comes with each name because Linear's picker draws a dot and the
// hub has no colour to serve: it is derived from the name, so the same label
// is the same colour in every client, on every project, for ever, without a
// column to store it in. A tracker that does supply its own colours can
// answer with them here later; nothing about the shape has to change.

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

type nativeLabel struct {
	Name  string `json:"name"`
	Color string `json:"color"`
	// Count is how many of the project's work items carry the label. The
	// picker orders suggestions by it, so a label one issue used once does
	// not sit above the one the project actually runs on.
	Count int `json:"count"`
}

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
	if err := validateNativeQuery(c.QueryParams()); err != nil {
		return s.nativeAPIError(c, err)
	}
	labels, err := readNativeLabels(c.Request().Context(), s.database.db, nativeRequestScope(c))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, nativeLabelList{Items: labels})
}

func readNativeLabels(ctx context.Context, query nativeQueryer, scope nativeScope) ([]nativeLabel, error) {
	rows, err := query.QueryContext(ctx, `SELECT labels_json FROM issues WHERE organization_id = ? AND project_id = ?`, scope.organization, scope.project)
	if err != nil {
		return nil, fmt.Errorf("read project labels: %w", err)
	}
	defer func() { _ = rows.Close() }()
	counts := map[string]int{}
	for rows.Next() {
		var encoded sql.NullString
		if err := rows.Scan(&encoded); err != nil {
			return nil, fmt.Errorf("scan project labels: %w", err)
		}
		if !encoded.Valid || encoded.String == "" {
			continue
		}
		var names []string
		if err := json.Unmarshal([]byte(encoded.String), &names); err != nil {
			// One unreadable row must not take the catalogue down with it:
			// the endpoint answers with every label it could read.
			continue
		}
		seen := map[string]bool{}
		for _, name := range names {
			trimmed := strings.TrimSpace(name)
			if isManagedLabel(trimmed) || seen[trimmed] {
				continue
			}
			seen[trimmed] = true
			counts[trimmed]++
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read project labels: %w", err)
	}
	labels := make([]nativeLabel, 0, len(counts))
	for name, count := range counts {
		labels = append(labels, nativeLabel{Name: name, Color: labelColor(name), Count: count})
	}
	// Busiest first, then alphabetical, so the order is total and two calls
	// against an unchanged project answer identically.
	sort.Slice(labels, func(a, b int) bool {
		if labels[a].Count != labels[b].Count {
			return labels[a].Count > labels[b].Count
		}
		return labels[a].Name < labels[b].Name
	})
	return labels, nil
}
