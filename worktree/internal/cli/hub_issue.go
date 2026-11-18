package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/spf13/cobra"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func newHubIssueCommand(lookupEnv func(string) string) *cobra.Command {
	var configPath, projectID, hubURL, organization, identityFile, tokenEnv, cursor string
	root := &cobra.Command{Use: "issue", Short: "Read and change native Detent work items", Example: "detent hub issue get wi_example --project prj_example", Args: NoArgs}
	root.PersistentFlags().StringVar(&configPath, "config", "", "Customer global configuration path")
	root.PersistentFlags().StringVar(&projectID, "project", "", "Configured project name or native project ID")
	root.PersistentFlags().StringVar(&hubURL, "hub-url", "", "Explicit Hub URL; otherwise use customer configuration")
	root.PersistentFlags().StringVar(&organization, "organization", "", "Native organization ID")
	root.PersistentFlags().StringVar(&identityFile, "identity-file", "", "Enrolled runner identity file")
	root.PersistentFlags().StringVar(&tokenEnv, "token-env", "DETENT_HUB_TOKEN", "Environment variable containing a scoped Hub credential")
	root.PersistentFlags().StringVar(&cursor, "cursor", "", "Continuation cursor for comments or history")
	for _, action := range []string{"get", "create", "edit", "comment", "edit-comment", "transition", "dependency", "comments", "history", "changes", "create-change", "change", "publish-change", "review-change", "check-change", "discuss-change", "approve-review-policy"} {
		args, usage := 1, action+" <work-item>"
		if action == "create" || action == "approve-review-policy" {
			args, usage = 0, action
		}
		if action == "edit-comment" {
			args, usage = 2, action+" <work-item> <comment>"
		}
		switch action {
		case "change", "publish-change", "discuss-change":
			args, usage = 2, action+" <work-item> <change>"
		case "review-change", "check-change":
			args, usage = 3, action+" <work-item> <change> <version>"
		}
		command := &cobra.Command{Use: usage, Short: "Native issue " + action + "; mutations read a versioned JSON request from stdin", Example: "detent hub issue " + action + " --project prj_example", Args: cobra.ExactArgs(args), RunE: func(cmd *cobra.Command, args []string) error {
			cfg := hubclient.Config{URL: hubURL, IdentityFile: identityFile, TokenSource: func() string { return lookupEnv(tokenEnv) }}
			org, project := organization, projectID
			if cfg.URL == "" {
				resolution, err := globalconfig.ResolvePath(configPath)
				if err != nil {
					return err
				}
				settings, err := globalconfig.Read(resolution.Path)
				if err != nil {
					return err
				}
				cfg.URL, cfg.IdentityFile = settings.Client.URL, settings.Client.IdentityFile
				org = settings.Client.OrganizationID
				if id := settings.Client.NativeProjects[projectID]; id != "" {
					project = id
				}
			}
			client, err := hubclient.New(cfg)
			if err != nil {
				return err
			}
			native, err := client.Native(tracker.OrganizationID(org), tracker.ProjectID(project))
			if err != nil {
				return err
			}
			value, err := executeNativeIssueCommand(cmd.Context(), native, action, args, cursor, cmd.InOrStdin())
			if err != nil {
				return err
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(value)
		}}
		root.AddCommand(command)
	}
	return root
}

func executeNativeIssueCommand(ctx context.Context, client *hubclient.NativeClient, action string, args []string, cursor string, input io.Reader) (any, error) {
	var id tracker.NativeWorkItemID
	if len(args) > 0 {
		id = tracker.NativeWorkItemID(args[0])
	}
	switch action {
	case "get":
		return client.Issue(ctx, id)
	case "comments":
		return client.Comments(ctx, id, cursor)
	case "history":
		return client.History(ctx, id, cursor)
	case "create":
		return nativeIssueInput(input, func(request tracker.CreateIssue) (any, error) { return client.CreateIssue(ctx, request) })
	case "edit":
		return nativeIssueInput(input, func(request tracker.UpdateIssue) (any, error) { return client.UpdateIssue(ctx, id, request) })
	case "comment":
		return nativeIssueInput(input, func(request tracker.CreateComment) (any, error) { return client.CreateComment(ctx, id, request) })
	case "edit-comment":
		return nativeIssueInput(input, func(request tracker.UpdateComment) (any, error) {
			return client.UpdateComment(ctx, id, args[1], request)
		})
	case "transition":
		return nativeIssueInput(input, func(request tracker.Transition) (any, error) { return client.Transition(ctx, id, request) })
	case "dependency":
		return nativeIssueInput(input, func(request tracker.DependencyMutation) (any, error) { return client.Dependency(ctx, id, request) })
	default:
		return executeNativeChangeCommand(ctx, client, action, args, input)
	}
}

func nativeIssueInput[T any](input io.Reader, execute func(T) (any, error)) (any, error) {
	raw, err := io.ReadAll(io.LimitReader(input, (1<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > 1<<20 {
		return nil, errors.New("native issue request exceeds 1 MiB")
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	var request T
	if err := decoder.Decode(&request); err != nil {
		return nil, err
	}
	if decoder.Decode(new(json.RawMessage)) != io.EOF {
		return nil, errors.New("native issue request must contain exactly one JSON object")
	}
	return execute(request)
}
