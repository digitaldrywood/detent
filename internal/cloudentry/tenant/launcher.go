package tenant

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/entryauthority"
)

type TenantSpec struct {
	ImportedConfiguration []byte
	Organization          Organization
	Directory             string
	Socket                string
	PublicURL             string
	Issuer                string
	PublicKey             string
	Check                 func(context.Context) error
	ResolveKey            entryauthority.Resolver
	ArchiveContents       attachment.ArchiveContentStore
	Content               entryauthority.ContentHandler
	RunDetails            attachment.DetailStore
	Icons                 attachment.IconStore
	ReadArchive           entryauthority.ArchiveReader
}

func (s TenantSpec) ConfigurationSource() ([]byte, error) {
	if len(s.ImportedConfiguration) != 0 {
		return s.ImportedConfiguration, nil
	}
	required := s.Organization.HostedConfigurationKey == nil && s.Organization.State == "ready"
	if s.Directory != "" {
		file, err := os.Open(filepath.Join(s.Directory, "tenant.yaml"))
		if err == nil {
			data, readErr := io.ReadAll(io.LimitReader(file, 128*1024+1))
			if closeErr := file.Close(); readErr != nil || closeErr != nil || len(data) == 0 || len(data) > 128*1024 {
				return nil, errors.New("original hosted configuration could not be read")
			}
			return data, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("original hosted configuration could not be opened")
		}
	}
	if required {
		return nil, errors.New("restore tenant.yaml in the organization directory under allocation.tenant_root or state_directory/tenants to import the registered configuration")
	}
	return nil, nil
}

type Launcher interface {
	Start(context.Context, TenantSpec) error
	WaitReady(context.Context, string) error
	Stop(string) error
	Close() error
	Failure(string) error
}
