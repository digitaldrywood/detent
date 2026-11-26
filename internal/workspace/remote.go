package workspace

import (
	"cmp"
	"context"

	"github.com/digitaldrywood/detent/internal/workspace/gitexec"
)

// RepositoryURL is the https form of the checkout's configured origin URL,
// the reference a published Change Request version names as its repository.
// It reads the configured value rather than the effective one, so a
// url.<base>.insteadOf rewrite that sends transport elsewhere does not turn
// the repository's name into a local path. It is empty when the remote is
// not one an https URL can name.
func RepositoryURL(ctx context.Context, workspacePath string) string {
	output, err := runGitAt(ctx, workspacePath, "config", "--get", "remote."+defaultGitRemote+".url")
	if err != nil {
		return ""
	}
	return HTTPSRemoteURL(output)
}

func HTTPSRemoteURL(remote string) string {
	return gitexec.HTTPSRemoteURL(remote)
}

func verifyPublicationRemote(ctx context.Context, path, remote, repository string) error {
	if err := gitexec.VerifyPublicationRemote(ctx, path, remote, repository); err != nil {
		return refuse(LandRefusalProtected, err.Error())
	}
	return nil
}

func (opts LandOptions) publicationRepository() string {
	return gitexec.PublicationRepository("", cmp.Or(opts.PublicationRepository, opts.Repository))
}

func landingPublicationRepository(ctx context.Context, path, remote string, opts LandOptions) (string, error) {
	repository := opts.publicationRepository()
	return repository, verifyPublicationRemote(ctx, path, remote, repository)
}
