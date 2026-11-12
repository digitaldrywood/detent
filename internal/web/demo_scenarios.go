package web

const (
	DemoScenarioHeader = "X-Detent-Demo-Scenario"
	DemoClockFrozen    = "frozen"
	DemoClockPlay      = "play"
)

type DemoScenarioManifest struct {
	ID             string            `json:"id"`
	Route          string            `json:"route"`
	Method         string            `json:"method"`
	Headers        map[string]string `json:"headers"`
	Viewport       DemoViewport      `json:"viewport"`
	ScreenshotName string            `json:"screenshot_name"`
	WaitSelector   string            `json:"wait_selector"`
	KeySelectors   []string          `json:"key_selectors,omitempty"`
}

type DemoViewport struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}
