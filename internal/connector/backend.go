package connector

type Backend string

const (
	BackendMemory      Backend = "memory"
	BackendLinear      Backend = "linear"
	BackendGitHub      Backend = "github"
	BackendGitHubLocal Backend = "github_local"
	BackendLocalSQLite Backend = "local_sqlite"
	BackendGitLab      Backend = "gitlab"
	BackendJira        Backend = "jira"
)

func (b Backend) String() string {
	return string(b)
}
