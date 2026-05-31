package connector

type Backend string

const (
	BackendMemory Backend = "memory"
	BackendLinear Backend = "linear"
	BackendGitHub Backend = "github"
	BackendGitLab Backend = "gitlab"
	BackendJira   Backend = "jira"
)

func (b Backend) String() string {
	return string(b)
}
