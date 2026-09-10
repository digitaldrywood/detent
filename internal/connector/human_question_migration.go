package connector

import "context"

type HumanQuestionMigration struct {
	ProjectID                   string `json:"project_id"`
	Dependent                   string `json:"dependent"`
	Source                      string `json:"source"`
	SourceBodySHA256            string `json:"source_body_sha256"`
	Question                    string `json:"question"`
	PreviouslyGeneratedQuestion bool   `json:"previously_generated_question"`
}

type HumanQuestionMigrator interface {
	PrepareHumanQuestionMigration(context.Context, HumanQuestionMigration) (Issue, error)
	RetireHumanQuestion(context.Context, HumanQuestionMigration, string) error
}
