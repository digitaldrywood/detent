package cli

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/digitaldrywood/detent/internal/gotestreceipt"
	"github.com/digitaldrywood/detent/internal/procgroup"
	commandshell "github.com/digitaldrywood/detent/internal/shell"
)

func newValidateCommand(lookup func(string) string, httpClient *http.Client, installWrapper func() (string, error)) *cobra.Command {
	var target nativeProjectFlags
	var dir string
	cmd := &cobra.Command{
		Use:     "validate <command>",
		Short:   "Run a validation command whose go test binaries reuse Hub receipts",
		Example: "  detent validate --project my-project 'make check-release'",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			native, err := target.client(cmd, lookup, httpClient)
			if err != nil {
				return err
			}
			wrapper, err := installWrapper()
			if err != nil {
				return err
			}
			root, err := filepath.Abs(dir)
			if err != nil {
				return fmt.Errorf("resolve validation directory: %w", err)
			}
			ctx := cmd.Context()
			session, err := gotestreceipt.Start(ctx, native.GoTestReceipts(), nil, os.TempDir(), root)
			if err != nil {
				return err
			}
			command := commandshell.Command(ctx, args[0], "")
			command.Dir = root
			command.Stdin, command.Stdout, command.Stderr = os.Stdin, cmd.OutOrStdout(), cmd.ErrOrStderr()
			command.Env = validationEnvironment(os.Environ())
			for name, value := range session.Environment() {
				command.Env = append(command.Env, name+"="+value)
			}
			command.Env = append(command.Env, gotestreceipt.RootEnvironment+"="+root, "GOFLAGS="+gotestreceipt.Goflags(lookup("GOFLAGS"), wrapper))
			procgroup.Configure(ctx, command)
			runErr := command.Run()
			stats, closeErr := session.Close(ctx)
			fmt.Fprintf(cmd.OutOrStdout(), "go test receipts: %d of %d test binaries reused, %d recorded, %d not recordable, %d receipt service failures\n", stats.Hits, stats.Lookups, stats.Records, stats.Refused, stats.Failures)
			if runErr != nil {
				runErr = fmt.Errorf("validation command failed: %w", runErr)
			}
			return errors.Join(runErr, closeErr)
		},
	}
	target.register(cmd)
	cmd.Flags().StringVar(&dir, "dir", ".", "Checkout to validate; the module root for go test receipts")
	return cmd
}

func validationEnvironment(environ []string) []string {
	env := make([]string, 0, len(environ))
	for _, entry := range environ {
		name, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if strings.EqualFold(name, "GOPROXY") && strings.Contains(value, "@") {
			continue
		}
		switch strings.ToUpper(name) {
		case "PATH", "HOME", "USERPROFILE", "HOMEDRIVE", "HOMEPATH",
			"TMPDIR", "TMP", "TEMP", "SYSTEMROOT", "WINDIR", "COMSPEC", "PATHEXT",
			"LANG", "LC_ALL", "LC_CTYPE", "TZ", "TERM", "NO_COLOR", "CI",
			"XDG_CACHE_HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME",
			"GOROOT", "GOPATH", "GOCACHE", "GOMODCACHE", "GOTMPDIR", "GOTOOLCHAIN",
			"GOOS", "GOARCH", "GOAMD64", "GOARM", "GOARM64", "GO386",
			"GOPROXY", "GOSUMDB", "GOPRIVATE", "GONOPROXY", "GONOSUMDB", "GOVCS",
			"GOFLAGS", "GOEXPERIMENT", "GOENV", "GOWORK", "GO111MODULE",
			"GOMAXPROCS", "GOMEMLIMIT", "GOGC", "GODEBUG", "GOTRACEBACK", "GORACE", "GOFIPS140",
			"CGO_ENABLED", "CC", "CXX", "AR", "PKG_CONFIG", "PKG_CONFIG_PATH",
			"CGO_CFLAGS", "CGO_CPPFLAGS", "CGO_CXXFLAGS", "CGO_FFLAGS", "CGO_LDFLAGS",
			"SDKROOT", "DEVELOPER_DIR", "MACOSX_DEPLOYMENT_TARGET",
			"NODE_PATH", "NVM_BIN", "NVM_DIR", "NPM_CONFIG_CACHE", "NPM_CONFIG_PREFIX",
			"PLAYWRIGHT_BROWSERS_PATH", "PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD",
			"GIT_CONFIG_GLOBAL", "GIT_CONFIG_NOSYSTEM", "GIT_CONFIG_SYSTEM", "GIT_TERMINAL_PROMPT",
			"TEST_PROCS":
			env = append(env, entry)
		}
	}
	return env
}
