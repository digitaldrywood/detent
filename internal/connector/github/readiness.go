package github

import (
	"context"
)

type ReadinessStatus string

const (
	ReadinessOK   ReadinessStatus = "OK"
	ReadinessWarn ReadinessStatus = "WARN"
	ReadinessFail ReadinessStatus = "FAIL"
)

type ReadinessCheck struct {
	Name   string
	Status ReadinessStatus
	Detail string
	Hint   string
}

type ReadinessConfig struct {
	AuthPath                      string
	LocalStatusMode               bool
	WriteProbeIssue               string
	AllowWriteProbes              bool
	Repositories                  []string
	StatusStates                  []string
	ReadStates                    []string
	RequireProjectRead            bool
	RequireIssueFieldRead         bool
	RequireIssueCommentsRead      bool
	RequireDependencyMetadataRead bool
	RequireIssueChildrenRead      bool
	RequireIssueParentsRead       bool
	RequirePullRequestRead        bool
	RequirePullRequestReviews     bool
	RequirePullRequestChecks      bool
	RequireProjectStatusWrite     bool
	RequireIssueFieldStatusWrite  bool
	RequireLabelStatusWrite       bool
	RequireIssueComments          bool
	RequireAssigneeWrite          bool
	RequireIssueClose             bool
	ProjectFieldWrites            []ReadinessProjectFieldWrite
}

type ReadinessProjectFieldWrite struct {
	Name string
}

func CheckReadiness(context.Context, Config, ReadinessConfig) ([]ReadinessCheck, error) {
	return []ReadinessCheck{{Name: "GitHub tracker", Status: ReadinessFail, Detail: "GitHub trackers are retired; migrate to hub_native."}}, nil
}
