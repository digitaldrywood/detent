package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var smokeSkipped = strings.Fields(`
 billing_usage.hosted_billing_checkout billing_usage.hosted_credit_checkout billing_usage.hosted_credit_auto_fund billing_usage.hosted_billing_portal
 billing_usage.api_budget_override_set billing_usage.api_budget_override_clear
 stop_run delete_comment remove_item acknowledge_parks dispose_security_finding post_operator_chat
 upload_attachment delete_attachment reference_attachment delete_conversation_attachment
 create_native_project create_hosted_project save_onboarding bind_native_repository
 start_git_hub_import advance_git_hub_import command_git_hub_batch cutover_project
 approve_project_policy revoke_project_policy project_native_summary remove_project_secret import_sprite_usage
 update_organization_model_selection update_project_model_selection
 apply_local_project_policy drain_local_project detach_local_project
 session_logout organization_switch organization_create organization_delete resume_provisioning
 organization_project_rank_update invitation_accept invitation_send invitation_edit invitation_resend invitation_revoke
 member_remove member_role member_grant credential_create credential_rotate credential_revoke credential_grant support_start
 refresh capacity_clear tracker_availability_clear forge_availability_clear failure_breaker_canary update_apply
 mark_urgent_runner_update issue_progress_credit acknowledge_staleness_warnings recover_work_attempt
 create_runner_enrollment revoke_runner_enrollment revoke_runner_identity
 update_runner_capacity update_runner_routing update_runner_host update_fleet_runner update_fleet_host
 create_workspace delete_workspace create_conversation post_conversation_command link_conversation patch_conversation
 upload_conversation_attachment create_project_action patch_project_action delete_project_action create_project_action_run
 artifact_access create_change publish_change_version discuss_change review_change view_change_file
 approve_change_review_policy bind_artifact_service
`)

var smokeWrites = strings.Fields(`file_issue edit_item add_comment edit_comment move_item set_dependency order_item set_queue_priority archive_item restore_item set_priority update_project_integration`)
var errSmokeFixture = errors.New("smoke resource fixture unavailable")

var smokeKnownGaps = map[string]string{
	operatortool.OrderItem: "#724", operatortool.SetQueuePriority: "#724", "board_session_history": "#724", "get_cutover_receipt": "#724", "get_change_review_policy": "#724",
	"change_viewed_files": "#725", "get_artifact_reference": "#725", "get_attempt_diff": "#725", "get_change": "#725", "get_change_version": "#725",
	"get_conversation": "#725", "get_conversation_attachment": "#725", "get_git_hub_import": "#725", "get_native_run": "#725", "get_runner_capacity": "#725",
	"get_runner_update": "#725", "github_scope_timings": "#725", "list_conversation_messages": "#725", "list_git_hub_import_records": "#725", "read_attachment": "#725",
	"read_attachment_metadata": "#725", "stream_conversation_events": "#725", "work_attempt_receipt": "#725", "get_project_policy": "#725",
}

var smokeHostedGaps = []string{operatortool.ArchiveItem, operatortool.OrderItem, operatortool.SetQueuePriority}

func smokePolicy(definitions []operatortool.Definition) error {
	catalog := map[string]operatortool.Definition{}
	for _, definition := range definitions {
		catalog[definition.Name] = definition
	}
	var failures []error
	for _, name := range smokeSkipped {
		if _, found := catalog[name]; !found {
			failures = append(failures, fmt.Errorf("staging smoke skip %s absent from catalog", name))
		}
	}
	for _, definition := range definitions {
		if !definition.Annotations.ReadOnly && !slices.Contains(smokeSkipped, definition.Name) && !slices.Contains(smokeWrites, definition.Name) {
			failures = append(failures, fmt.Errorf("staging smoke %s: write tool requires an explicit exercise or skip decision", definition.Name))
		}
	}
	return errors.Join(failures...)
}

var smokeHosts = map[string]string{"staging": "staging.cloud.detent.build", "production": "cloud.detent.build"}

func deploySmoke(ctx context.Context, environment string, getenv func(string) string, output io.Writer) error {
	token := strings.TrimSpace(getenv("DETENT_SMOKE_API_KEY"))
	if token == "" {
		return fmt.Errorf("MCP %s smoke: DETENT_SMOKE_API_KEY is absent; provision the dedicated smoke project key as the %s environment secret", environment, environment)
	}
	endpoint, err := smokeEndpoint(getenv("DETENT_MCP_URL"), environment)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	m := &cloudMCP{endpoint: endpoint, token: token, client: &http.Client{Timeout: 30 * time.Second}, diagnostic: true}
	if err := m.initialize(ctx); err != nil {
		return err
	}
	return (&mcpSmoke{mcp: m, output: output, fixtures: map[string]any{}, called: map[string]bool{}, prefix: environment + "-smoke-" + rand.Text(), knownGaps: smokeKnownGaps}).run(ctx)
}

type mcpSmoke struct {
	knownGaps map[string]string
	mcp       *cloudMCP
	output    io.Writer
	fixtures  map[string]any
	called    map[string]bool
	prefix    string
	sequence  int
	project   string
	item      string
	revision  int64
	states    []tracker.NativeState
}

func (s *mcpSmoke) call(ctx context.Context, name string, args map[string]any) (map[string]any, error) {
	definition, found := s.mcp.tools[name]
	if !found {
		return nil, fmt.Errorf("%s: absent from live tools/list", name)
	}
	s.sequence++
	var schema smokeSchema
	if err := json.Unmarshal(definition.InputSchema, &schema); err != nil {
		return nil, fmt.Errorf("%s schema: %w", name, err)
	}
	if _, accepts := schema.Properties["request_id"]; accepts {
		args["request_id"] = fmt.Sprintf("%s-%d", s.prefix, s.sequence)
	}
	var payload any
	err := s.mcp.call(ctx, name, args, &payload)
	result, objectOK := payload.(map[string]any)
	if !objectOK {
		result = map[string]any{"data": payload}
	}
	if err == nil {
		result, err = s.settle(ctx, result)
	}
	s.called[name] = true
	if err != nil {
		fragments := map[string]smokeSchema{}
		for key := range args {
			if fragment, ok := schema.Properties[key]; ok {
				fragments[key] = fragment
			}
		}
		raw, marshalErr := json.Marshal(fragments)
		if marshalErr != nil {
			return nil, fmt.Errorf("%s: schema fragments: %w", name, marshalErr)
		}
		failure := fmt.Errorf("%s: advertised arguments %s; %w", name, raw, err)
		fmt.Fprintln(s.output, failure)
		return nil, failure
	}
	fmt.Fprintf(s.output, "%s ok\n", name)
	s.remember(result)
	return result, nil
}

func (s *mcpSmoke) settle(ctx context.Context, result map[string]any) (map[string]any, error) {
	for {
		if id, ok := result["id"].(string); ok && result["status"] != nil {
			s.fixtures["action_id"] = id
		}
		status, statusOK := result["status"].(string)
		if result["status"] != nil && !statusOK {
			return nil, errors.New("action status could not be decoded")
		}
		switch status {
		case "failed", "rejected", "cancelled", "pending_approval":
			return nil, fmt.Errorf("action %s: %v", status, result["result"])
		case "pending", "executing", "queued":
			id, idOK := result["id"].(string)
			if !idOK || id == "" {
				return nil, errors.New("action result has no id")
			}
			timer := time.NewTimer(250 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
			}
			if err := s.mcp.call(ctx, operatortool.ActionResult, map[string]any{"action_id": id}, &result); err != nil {
				return nil, err
			}
			s.called[operatortool.ActionResult] = true
		default:
			return result, nil
		}
	}
}

func (s *mcpSmoke) remember(value any) {
	switch node := value.(type) {
	case map[string]any:
		for key, child := range node {
			if text, ok := child.(string); ok && text != "" {
				if strings.HasSuffix(key, "_id") || key == "sha256" || key == "path" {
					s.fixtures[key] = text
				}
				if key == "id" || key == "attempt_id" {
					for _, kind := range []string{"runner", "machine", "workspace", "conversation", "attachment", "change", "version", "artifact", "action", "run", "recording"} {
						if strings.HasPrefix(text, kind+"_") {
							s.fixtures[kind+"_id"] = text
						}
					}
					if strings.HasPrefix(text, "attempt_") {
						s.fixtures["native_attempt_id"] = text
					}
				}
			}
			s.remember(child)
		}
	case []any:
		for _, child := range node {
			s.remember(child)
		}
	}
}

func smokeRevision(value map[string]any) (int64, error) {
	switch revision := value["revision"].(type) {
	case string:
		return strconv.ParseInt(revision, 10, 64)
	case float64:
		if revision == float64(int64(revision)) {
			return int64(revision), nil
		}
	}
	if raw, ok := value["result"].(string); ok {
		var receipt map[string]any
		if json.Unmarshal([]byte(raw), &receipt) == nil {
			return smokeRevision(receipt)
		}
	}
	return 0, errors.New("receipt lacks a decodable revision")
}

func smokeRevisionArgument(schema smokeSchema, revision int64) (any, error) {
	switch schema.Type {
	case "string":
		return strconv.FormatInt(revision, 10), nil
	case "integer":
		return revision, nil
	default:
		return nil, fmt.Errorf("expected_revision advertises unsupported type %v", schema.Type)
	}
}

func (s *mcpSmoke) mutate(ctx context.Context, name string, args map[string]any, previous int64) (map[string]any, int64, error) {
	definition, found := s.mcp.tools[name]
	if !found && slices.Contains(smokeHostedGaps, name) {
		fmt.Fprintf(s.output, "%s expected hosted gap (#615): absent from tools/list\n", name)
		s.called[name] = true
		return map[string]any{"expected_gap": true}, previous, nil
	}
	var schema smokeSchema
	if err := json.Unmarshal(definition.InputSchema, &schema); err != nil {
		return nil, previous, fmt.Errorf("%s: schema: %w", name, err)
	}
	args["project_id"] = s.project
	if s.item != "" {
		args["identifier"] = s.item
	}
	if fragment, found := schema.Properties["expected_revision"]; found {
		value, err := smokeRevisionArgument(fragment, previous)
		if err != nil {
			return nil, previous, fmt.Errorf("%s: %w", name, err)
		}
		args["expected_revision"] = value
	}
	result, err := s.call(ctx, name, args)
	if err != nil && slices.Contains(smokeHostedGaps, name) && strings.Contains(err.Error(), "application service is unavailable") {
		fmt.Fprintf(s.output, "%s expected hosted gap (#615)\n", name)
		return map[string]any{"expected_gap": true}, previous, nil
	}
	if err != nil {
		return nil, previous, err
	}
	revision, err := smokeRevision(result)
	if err != nil || revision <= previous {
		return nil, previous, errors.Join(fmt.Errorf("%s: revision did not advance from %d: result=%v", name, previous, result), err)
	}
	return result, revision, nil
}

func (s *mcpSmoke) run(ctx context.Context) error {
	if err := smokePolicy(operatortool.Registry()); err != nil {
		return err
	}
	projects, err := s.call(ctx, "list_projects", map[string]any{})
	if err != nil {
		return err
	}
	raw, marshalErr := json.Marshal(projects["data"])
	if marshalErr != nil {
		return fmt.Errorf("list_projects: %w", marshalErr)
	}
	var page struct {
		Projects   []tracker.NativeProject `json:"projects"`
		NextCursor string                  `json:"next_cursor"`
	}
	if err := json.Unmarshal(raw, &page); err != nil || len(page.Projects) != 1 || page.NextCursor != "" {
		return errors.New("staging smoke key must authorize exactly one dedicated native smoke project")
	}
	project := page.Projects[0]
	if project.Profile != "native" {
		return errors.New("staging smoke requires a native project")
	}
	s.project = string(project.ID)
	s.states = project.States
	s.fixtures["project_id"] = s.project
	s.fixtures["organization_id"] = string(project.OrganizationID)
	first, second := "", ""
	for _, from := range s.states {
		if from.Dispatchable || from.Terminal || from.OperatorOnly {
			continue
		}
		for _, to := range s.states {
			if to.Name != from.Name && !to.Dispatchable && !to.Terminal && !to.OperatorOnly && slices.Contains(from.Transitions, to.Name) {
				first, second = from.Name, to.Name
				break
			}
		}
		if first != "" {
			break
		}
	}
	if first == "" {
		return errors.New("smoke project needs a transition between two nondispatchable nonterminal states")
	}
	dependency, err := s.call(ctx, operatortool.WorkList, map[string]any{"project_id": s.project, "query": "MCP smoke dependency", "limit": 1})
	if err != nil {
		return err
	}
	related := smokeFindItem(dependency)
	if related == "" {
		result, _, createErr := s.mutate(ctx, operatortool.FileIssue, map[string]any{"title": "MCP smoke dependency", "state": first}, 0)
		if createErr != nil {
			return createErr
		}
		related = smokeFindItem(result)
	}
	created, revision, err := s.mutate(ctx, operatortool.FileIssue, map[string]any{"title": "MCP staging smoke " + s.prefix, "state": first}, 0)
	if err != nil {
		return err
	}
	s.item = smokeFindItem(created)
	s.revision = revision
	if s.item == "" {
		return errors.New("file_issue: receipt lacks native work item id")
	}
	s.fixtures["reference"] = related
	s.fixtures["work_item_id"] = related
	s.fixtures["identifier"] = s.item
	var failures []error
	for _, step := range []struct {
		name string
		args map[string]any
	}{
		{operatortool.EditItem, map[string]any{"title": "MCP staging smoke edited " + s.prefix}},
		{operatortool.SetDependency, map[string]any{"related": related, "operation": "add"}},
		{operatortool.SetDependency, map[string]any{"related": related, "operation": "remove"}},
		{operatortool.MoveItem, map[string]any{"target_state": second}},
		{operatortool.SetPriority, map[string]any{"priority": "High"}},
		{operatortool.OrderItem, map[string]any{"queue_scope": "project", "state": second, "rank": "1"}},
		{operatortool.SetQueuePriority, map[string]any{"queue_scope": "project", "state": second, "queue_priority": "high"}},
	} {
		if step.name == operatortool.SetPriority {
			if _, found := s.mcp.tools[step.name]; !found {
				continue
			}
		}
		_, next, stepErr := s.mutate(ctx, step.name, step.args, s.revision)
		if stepErr != nil {
			failures = append(failures, stepErr)
			break
		}
		s.revision = next
	}
	comment, commentRevision, commentErr := s.mutate(ctx, operatortool.AddComment, map[string]any{"body": "MCP staging smoke comment"}, 0)
	if commentErr != nil {
		failures = append(failures, commentErr)
	} else {
		commentID, commentOK := comment["comment_id"].(string)
		if !commentOK || commentID == "" {
			failures = append(failures, errors.New("add_comment: receipt lacks comment_id"))
		}
		s.fixtures["comment_id"] = commentID
		_, _, editErr := s.mutate(ctx, operatortool.EditComment, map[string]any{"comment_id": commentID, "body": "MCP staging smoke edited comment"}, commentRevision)
		if editErr != nil {
			failures = append(failures, editErr)
		}
	}
	if err := s.integration(ctx); err != nil {
		failures = append(failures, err)
	}
	names := make([]string, 0, len(s.mcp.tools))
	for name := range s.mcp.tools {
		names = append(names, name)
	}
	slices.SortFunc(names, func(a, b string) int {
		al, bl := strings.HasPrefix(a, "list_"), strings.HasPrefix(b, "list_")
		if al != bl {
			if al {
				return -1
			}
			return 1
		}
		return strings.Compare(a, b)
	})
	pending := []string{}
	for _, name := range names {
		tool := s.mcp.tools[name]
		if slices.Contains(smokeSkipped, name) {
			fmt.Fprintf(s.output, "%s skipped: external or operator mutation\n", name)
			continue
		}
		if tool.Annotations.ReadOnly && !s.called[name] {
			pending = append(pending, name)
		}
	}
	for len(pending) > 0 {
		deferred := []string{}
		madeCall := false
		for _, name := range pending {
			args, argErr := s.arguments(s.mcp.tools[name])
			if errors.Is(argErr, errSmokeFixture) {
				deferred = append(deferred, name)
				continue
			}
			if argErr != nil {
				failures = append(failures, argErr)
				fmt.Fprintln(s.output, argErr)
				continue
			}
			madeCall = true
			if _, callErr := s.call(ctx, name, args); callErr != nil {
				failures = append(failures, callErr)
			}
		}
		if !madeCall {
			for _, name := range deferred {
				_, argErr := s.arguments(s.mcp.tools[name])
				failures = append(failures, argErr)
				fmt.Fprintln(s.output, argErr)
			}
			break
		}
		pending = deferred
	}

	archived, next, archiveErr := s.mutate(ctx, operatortool.ArchiveItem, map[string]any{}, s.revision)
	if archiveErr != nil {
		failures = append(failures, archiveErr)
	} else if archived["expected_gap"] != true {
		s.revision = next
		_, next, restoreErr := s.mutate(ctx, operatortool.RestoreItem, map[string]any{}, s.revision)
		if restoreErr != nil {
			failures = append(failures, restoreErr)
		} else {
			s.revision = next
			_, _, cleanupErr := s.mutate(ctx, operatortool.ArchiveItem, map[string]any{}, s.revision)
			if cleanupErr != nil {
				failures = append(failures, cleanupErr)
			}
		}
	} else {
		fmt.Fprintln(s.output, "restore_item expected hosted gap (#615): archive cycle unavailable")
		for _, from := range s.states {
			if from.Name != second {
				continue
			}
			for _, to := range s.states {
				if !to.Terminal || !slices.Contains(from.Transitions, to.Name) {
					continue
				}
				_, _, cleanupErr := s.mutate(ctx, operatortool.MoveItem, map[string]any{"target_state": to.Name}, s.revision)
				if cleanupErr != nil {
					failures = append(failures, cleanupErr)
				}
				break
			}
		}
	}
	for _, name := range names {
		if !s.called[name] && !slices.Contains(smokeSkipped, name) && name != operatortool.RestoreItem {
			failures = append(failures, fmt.Errorf("%s: non-skipped tool was not exercised", name))
		}
	}
	return smokeVerdict(s.output, failures, s.called, s.knownGaps)
}

func smokeVerdict(output io.Writer, failures []error, called map[string]bool, gaps map[string]string) error {
	failed := map[string]bool{}
	var blocking []error
	for _, failure := range failures {
		name, _, _ := strings.Cut(failure.Error(), ":")
		name = strings.TrimSuffix(name, " schema")
		if owner, known := gaps[name]; known {
			failed[name] = true
			fmt.Fprintf(output, "%s known gap (%s): %v\n", name, owner, failure)
			continue
		}
		blocking = append(blocking, failure)
	}
	for _, name := range slices.Sorted(maps.Keys(gaps)) {
		if called[name] && !failed[name] {
			blocking = append(blocking, fmt.Errorf("%s: known gap %s now passes; remove it from smokeKnownGaps", name, gaps[name]))
		}
	}
	return errors.Join(blocking...)
}

func smokeFindItem(value any) string {
	switch node := value.(type) {
	case map[string]any:
		for _, key := range []string{"work_item_id", "resource_id"} {
			if text, ok := node[key].(string); ok && strings.HasPrefix(text, "wi_") {
				return text
			}
		}
		for _, child := range node {
			if id := smokeFindItem(child); id != "" {
				return id
			}
		}
	case []any:
		for _, child := range node {
			if id := smokeFindItem(child); id != "" {
				return id
			}
		}
	}
	return ""
}

func (s *mcpSmoke) arguments(tool operatortool.Definition) (map[string]any, error) {
	var schema smokeSchema
	if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
		return nil, fmt.Errorf("%s: schema: %w", tool.Name, err)
	}
	for _, alternatives := range []*[]smokeSchema{&schema.AnyOf, &schema.OneOf} {
		for _, candidate := range *alternatives {
			available := true
			for _, key := range candidate.Required {
				if strings.HasSuffix(key, "_id") {
					fixture, found := s.fixtures[key]
					if !found || !smokeFixtureType(schema.Properties[key], fixture) {
						available = false
						break
					}
				}
			}
			if available {
				*alternatives = []smokeSchema{candidate}
				break
			}
		}
	}
	value, err := smokeValue(schema, false)
	if err != nil {
		return nil, fmt.Errorf("%s: schema sample: %w", tool.Name, err)
	}
	args, objectOK := value.(map[string]any)
	if !objectOK {
		return nil, fmt.Errorf("%s: input schema does not describe an object", tool.Name)
	}
	for key := range args {
		if fixture, found := s.fixtures[key]; found && smokeFixtureType(schema.Properties[key], fixture) {
			args[key] = fixture
			continue
		}
		if strings.HasSuffix(key, "_id") || key == "path" || key == "reference" {
			return nil, fmt.Errorf("%s: advertised %s=%s; smoke project lacks a %s fixture: %w", tool.Name, key, tool.InputSchema, key, errSmokeFixture)
		}
	}
	if _, found := schema.Properties["project_id"]; found {
		args["project_id"] = s.project
	}

	if tool.Name == operatortool.ActionResult {
		args["action_id"] = s.fixtures["action_id"]
	}
	return args, nil
}

func (s *mcpSmoke) integration(ctx context.Context) error {
	result, err := s.call(ctx, "get_project_integration", map[string]any{"project_id": s.project})
	if err != nil {
		return err
	}
	data, ok := result["data"].(map[string]any)
	if !ok {
		return errors.New("get_project_integration: missing data")
	}
	revision, err := smokeRevision(data)
	if err != nil {
		return fmt.Errorf("get_project_integration: %w", err)
	}
	var schema smokeSchema
	if err := json.Unmarshal(s.mcp.tools["update_project_integration"].InputSchema, &schema); err != nil {
		return err
	}
	input := map[string]any{}
	for _, key := range []string{"intake", "projection", "repository_enabled"} {
		input[key] = data[key]
	}
	value, err := smokeRevisionArgument(schema.Properties["input"].Properties["expected_revision"], revision)
	if err != nil {
		return err
	}
	input["expected_revision"] = value
	if _, err = s.call(ctx, "update_project_integration", map[string]any{"project_id": s.project, "input": input}); err != nil {
		return err
	}
	after, err := s.call(ctx, "get_project_integration", map[string]any{"project_id": s.project})
	if err != nil {
		return err
	}
	current, ok := after["data"].(map[string]any)
	if !ok {
		return errors.New("get_project_integration: missing updated data")
	}
	next, err := smokeRevision(current)
	if err != nil || next <= revision {
		return errors.Join(fmt.Errorf("update_project_integration: revision did not advance from %d", revision), err)
	}
	for _, key := range []string{"intake", "projection", "repository_enabled"} {
		if current[key] != data[key] {
			return fmt.Errorf("update_project_integration: unchanged %s was modified", key)
		}
	}
	return nil
}

func smokeFixtureType(schema smokeSchema, value any) bool {
	switch schema.Type {
	case "string":
		_, ok := value.(string)
		return ok
	case "integer":
		switch value.(type) {
		case int64, int, float64:
			return true
		}
		return false
	default:
		return true
	}
}

func smokeEndpoint(raw, environment string) (string, error) {
	host, known := smokeHosts[environment]
	endpoint, err := url.Parse(strings.TrimSpace(raw))
	if !known || err != nil || endpoint.Scheme != "https" || endpoint.Host != host || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || !strings.HasSuffix(endpoint.Path, "/mcp") {
		return "", fmt.Errorf("DETENT_MCP_URL must be the copied MCP endpoint on %s for the %s smoke tenant", host, environment)
	}
	return endpoint.String(), nil
}
