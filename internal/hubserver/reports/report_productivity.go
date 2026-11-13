package reports

import (
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

type productivityMetrics struct {
	Filed            int      `json:"filed"`
	Completed        int      `json:"completed"`
	Cancelled        int      `json:"cancelled"`
	NetBacklog       int      `json:"net_backlog"`
	ChangesLanded    int      `json:"changes_landed"`
	Worked           *int     `json:"worked"`
	Reworked         *int     `json:"reworked"`
	CompletionCost   float64  `json:"completion_cost_usd"`
	CostPerCompleted *float64 `json:"cost_per_completed_usd"`
	CycleP50         *float64 `json:"cycle_p50_seconds"`
	ReworkRate       *float64 `json:"rework_percent"`
	cycles           []float64
}

type productivityProject struct {
	ProjectID string              `json:"project_id"`
	Current   productivityMetrics `json:"current"`
	Previous  productivityMetrics `json:"previous"`
}

type ProductivityCount struct {
	Filed     int `json:"filed"`
	Completed int `json:"completed"`
}

type ProductivityBucket struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
	ProductivityCount
	Projects map[string]ProductivityCount `json:"projects"`
}

type ProductivityActor struct {
	Kind     string `json:"kind"`
	ID       string `json:"id,omitempty"`
	Name     string `json:"name,omitempty"`
	Current  int    `json:"current"`
	Previous int    `json:"previous"`
}

type OrganizationProductivity struct {
	OrganizationID string                       `json:"organization_id"`
	Window         operatortool.AnalyticsWindow `json:"window"`
	PreviousWindow operatortool.AnalyticsWindow `json:"previous_window"`
	PriorRecorded  bool                         `json:"prior_recorded"`
	Current        productivityMetrics          `json:"current"`
	Previous       productivityMetrics          `json:"previous"`
	Projects       []productivityProject        `json:"projects"`
	Series         []ProductivityBucket         `json:"series"`
	PreviousSeries []ProductivityBucket         `json:"previous_series"`
	FiledBy        []ProductivityActor          `json:"filed_by"`
	CompletedBy    []ProductivityActor          `json:"completed_by"`
}

func ProductivityBuckets(w operatortool.AnalyticsWindow, projects []string) []ProductivityBucket {
	out := []ProductivityBucket{}
	for from := w.From; from.Before(w.To); from = from.Add(w.Bucket) {
		b := ProductivityBucket{From: from, To: hubapi.MinTime(from.Add(w.Bucket), w.To), Projects: map[string]ProductivityCount{}}
		for _, project := range projects {
			b.Projects[project] = ProductivityCount{}
		}
		out = append(out, b)
	}
	return out
}

func ProductivityBucketCount(series []ProductivityBucket, w operatortool.AnalyticsWindow, at time.Time, project string, completed bool) {
	i := int(at.Sub(w.From) / w.Bucket)
	counts := series[i].Projects[project]
	if completed {
		series[i].Completed++
		counts.Completed++
	} else {
		series[i].Filed++
		counts.Filed++
	}
	series[i].Projects[project] = counts
}

func ProductivityActorCount(actors map[string]*ProductivityActor, kind, id, name string, previous bool) {
	key := kind + "/" + id
	if actors[key] == nil {
		actors[key] = &ProductivityActor{Kind: kind, ID: id, Name: name}
	}
	if previous {
		actors[key].Previous++
	} else {
		actors[key].Current++
	}
}

func ProductivityActors(actors map[string]*ProductivityActor) []ProductivityActor {
	out := make([]ProductivityActor, 0, len(actors))
	for _, actor := range actors {
		out = append(out, *actor)
	}
	slices.SortFunc(out, func(a, b ProductivityActor) int { return strings.Compare(a.Kind+"/"+a.ID, b.Kind+"/"+b.ID) })
	return out
}

func FilterProductivityProjects(readable, selected []string) []string {
	if len(selected) == 0 {
		return readable
	}
	return slices.DeleteFunc(slices.Clone(readable), func(id string) bool { return !slices.Contains(selected, id) })
}
