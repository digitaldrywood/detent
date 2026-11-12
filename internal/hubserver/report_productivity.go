package hubserver

import (
	"slices"
	"strings"
	"time"

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

type productivityCount struct {
	Filed     int `json:"filed"`
	Completed int `json:"completed"`
}

type productivityBucket struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
	productivityCount
	Projects map[string]productivityCount `json:"projects"`
}

type productivityActor struct {
	Kind     string `json:"kind"`
	ID       string `json:"id,omitempty"`
	Name     string `json:"name,omitempty"`
	Current  int    `json:"current"`
	Previous int    `json:"previous"`
}

type organizationProductivity struct {
	OrganizationID string                       `json:"organization_id"`
	Window         operatortool.AnalyticsWindow `json:"window"`
	PreviousWindow operatortool.AnalyticsWindow `json:"previous_window"`
	PriorRecorded  bool                         `json:"prior_recorded"`
	Current        productivityMetrics          `json:"current"`
	Previous       productivityMetrics          `json:"previous"`
	Projects       []productivityProject        `json:"projects"`
	Series         []productivityBucket         `json:"series"`
	PreviousSeries []productivityBucket         `json:"previous_series"`
	FiledBy        []productivityActor          `json:"filed_by"`
	CompletedBy    []productivityActor          `json:"completed_by"`
}

func productivityBuckets(w operatortool.AnalyticsWindow, projects []string) []productivityBucket {
	out := []productivityBucket{}
	for from := w.From; from.Before(w.To); from = from.Add(w.Bucket) {
		b := productivityBucket{From: from, To: minTime(from.Add(w.Bucket), w.To), Projects: map[string]productivityCount{}}
		for _, project := range projects {
			b.Projects[project] = productivityCount{}
		}
		out = append(out, b)
	}
	return out
}

func productivityBucketCount(series []productivityBucket, w operatortool.AnalyticsWindow, at time.Time, project string, completed bool) {
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

func productivityActorCount(actors map[string]*productivityActor, kind, id, name string, previous bool) {
	key := kind + "/" + id
	if actors[key] == nil {
		actors[key] = &productivityActor{Kind: kind, ID: id, Name: name}
	}
	if previous {
		actors[key].Previous++
	} else {
		actors[key].Current++
	}
}

func productivityActors(actors map[string]*productivityActor) []productivityActor {
	out := make([]productivityActor, 0, len(actors))
	for _, actor := range actors {
		out = append(out, *actor)
	}
	slices.SortFunc(out, func(a, b productivityActor) int { return strings.Compare(a.Kind+"/"+a.ID, b.Kind+"/"+b.ID) })
	return out
}

func filterProductivityProjects(readable, selected []string) []string {
	if len(selected) == 0 {
		return readable
	}
	return slices.DeleteFunc(slices.Clone(readable), func(id string) bool { return !slices.Contains(selected, id) })
}
