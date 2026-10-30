package cli

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

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
			session, err := gotestreceipt.Start(ctx, native.GoTestReceipts(), nil)
			if err != nil {
				return err
			}
			command := commandshell.Command(ctx, args[0], "")
			command.Dir = root
			command.Stdin, command.Stdout, command.Stderr = os.Stdin, cmd.OutOrStdout(), cmd.ErrOrStderr()
			command.Env = gotestreceipt.ValidationEnvironment(os.Environ())
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
