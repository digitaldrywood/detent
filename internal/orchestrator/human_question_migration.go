package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/dependencyline"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
)

func MigrateHumanQuestion(ctx context.Context, tracker connector.Connector, attempts store.WorkAttemptStore, request connector.HumanQuestionMigration) error {
	source, err := dependencyline.CanonicalReference(request.Source, "")
	if err != nil {
		return err
	}
	dependent, err := dependencyline.CanonicalReference(request.Dependent, "")
	if err != nil {
		return err
	}
	request.Source, request.Dependent = source, dependent
	migrator, ok := tracker.(connector.HumanQuestionMigrator)
	if !ok {
		return errors.New("tracker does not support human question migration")
	}
	questions, ok := attempts.(store.HumanQuestionStore)
	if !ok {
		return errors.New("durable human question storage unavailable")
	}
	issue, err := migrator.PrepareHumanQuestionMigration(ctx, request)
	if err != nil {
		return err
	}
	key := "migration:" + strings.ToLower(request.Source)
	body := strings.TrimSpace(request.Question) + "\n\nContext and audit: " + request.Source + ". This migration does not approve the unresolved decision or any external action. Please reply here in ordinary language.\n\n<!-- detent-question-migration:" + strings.ToLower(request.Source) + " -->"
	o := &Orchestrator{connector: tracker, workAttempts: attempts}
	o.cfg.Project.ID = request.ProjectID
	run := RunRequest{Issue: issue}
	o.attachHumanQuestionTool(&run)
	if run.AgentToolHandler == nil {
		return errors.New("authorized question tooling unavailable")
	}
	arguments, err := json.Marshal(map[string]string{"key": key, "question": body})
	if err != nil {
		return err
	}
	result, err := run.AgentToolHandler(ctx, runner.AgentToolCall{Name: "ask_human_question", Arguments: arguments})
	if err != nil {
		return err
	}
	if !result.Success {
		return errors.New(result.Content)
	}
	records, err := questions.HumanQuestions(ctx, request.ProjectID, issue.ID)
	if err != nil {
		return err
	}
	for _, q := range records {
		if q.Key == key {
			return migrator.RetireHumanQuestion(ctx, request, q.QuestionCommentID)
		}
	}
	return errors.New("persisted migration question not found")
}

func (o *Orchestrator) withoutMigratedHumanBlockers(ctx context.Context, issue connector.Issue, blockers []implementDependencyBlocker) []implementDependencyBlocker {
	questions, ok := o.workAttempts.(store.HumanQuestionStore)
	if !ok {
		return blockers
	}
	records, err := questions.HumanQuestions(ctx, o.cfg.Project.ID, issue.ID)
	if err != nil {
		return blockers
	}
	migrated := map[string]bool{}
	for _, q := range records {
		if source, ok := strings.CutPrefix(q.Key, "migration:"); ok && q.QuestionCommentID != "" {
			migrated[strings.ToLower(source)] = true
		}
	}
	var remaining []implementDependencyBlocker
	for _, blocker := range blockers {
		if !migrated[strings.ToLower(blocker.Identifier)] {
			remaining = append(remaining, blocker)
		}
	}
	return remaining
}
