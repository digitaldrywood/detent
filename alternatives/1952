package githubcontract

import "time"

type GitHubAppInstallation struct {
	Slug       string
	InstallURL string
	Installed  bool
}

type GitHubRequestCount struct {
	Profile       string    `json:"profile"`
	Operation     string    `json:"operation"`
	Requests      int64     `json:"requests"`
	Errors        int64     `json:"errors"`
	LastRequestAt time.Time `json:"last_request_at"`
}
