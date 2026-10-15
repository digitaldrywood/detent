package store

import "testing"

func TestLegacyHumanQuestionReceiptsReadable(t *testing.T) {
	t.Parallel()
	backend := openTestStore(t, t.Context()).(*sqliteStore)
	for _, stmt := range []string{
		`INSERT INTO human_questions(project_id, issue_id, question_key, issue_identifier, body, question_comment_id, answer_comment_id) VALUES ('p', '1', 'decision', 'owner/repo#1', 'Which target?', '123', '')`,
		`INSERT INTO human_questions(project_id, issue_id, question_key, issue_identifier, body, question_comment_id, answer_comment_id) VALUES ('other', '1', 'decision', 'owner/repo#1', 'Other project?', '124', '')`,
	} {
		if _, err := backend.db.ExecContext(t.Context(), stmt); err != nil {
			t.Fatal(err)
		}
	}
	receipts, err := backend.HumanQuestions(t.Context(), "p", "1")
	if err != nil || len(receipts) != 1 || receipts[0].Key != "decision" || receipts[0].Body != "Which target?" {
		t.Fatalf("legacy receipts = %+v, %v", receipts, err)
	}
}
