package store

import (
	"path/filepath"
	"testing"
)

func TestHumanQuestionsPersistence(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "questions.db")
	db := openParkTestStore(t, path)
	q := HumanQuestion{ProjectID: "project", IssueID: "647", Identifier: "owner/repo#647", Key: "sender", Body: "Use reviewed manual delivery?"}
	for _, tt := range []struct {
		name     string
		question HumanQuestion
		created  bool
		invalid  bool
	}{
		{name: "reserve", question: q, created: true},
		{name: "retry", question: q},
		{name: "second question waits", question: HumanQuestion{ProjectID: q.ProjectID, IssueID: q.IssueID, Identifier: q.Identifier, Key: "another", Body: "Another question?"}},
		{name: "invalid", invalid: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			created, err := db.ReserveHumanQuestion(t.Context(), tt.question)
			if (err != nil) != tt.invalid || created != tt.created {
				t.Fatalf("reserve = %v, %v", created, err)
			}
		})
	}
	q.QuestionCommentID = "question"
	if err := db.RecordHumanQuestionComment(t.Context(), q); err != nil {
		t.Fatal(err)
	}
	q.AnswerCommentID, q.AnswerBody = "answer", "Manual delivery only; no live sends authorized."
	if err := db.RecordHumanQuestionAnswer(t.Context(), q); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = openParkTestStore(t, path)
	got, err := db.HumanQuestions(t.Context(), q.ProjectID, q.IssueID)
	if err != nil || len(got) != 1 || got[0] != q {
		t.Fatalf("persisted = %+v, %v", got, err)
	}
	q.Key = "followup"
	if created, err := db.ReserveHumanQuestion(t.Context(), q); err != nil || !created {
		t.Fatalf("followup = %v, %v", created, err)
	}
	for _, project := range []string{"other", ""} {
		got, err := db.HumanQuestions(t.Context(), project, q.IssueID)
		if err != nil || len(got) != 0 {
			t.Fatalf("cross-project questions = %+v, %v", got, err)
		}
	}
}

func TestReleaseHumanQuestionReservation(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, project, body string
		posted, released    bool
	}{
		{name: "unposted", released: true},
		{name: "posted", posted: true},
		{name: "different project", project: "other"},
		{name: "different body", body: "Another question?"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			db := openParkTestStore(t, filepath.Join(t.TempDir(), "questions.db"))
			q := HumanQuestion{ProjectID: "project", IssueID: "647", Identifier: "owner/repo#647", Key: "sender", Body: "Use manual delivery?"}
			if _, err := db.ReserveHumanQuestion(t.Context(), q); err != nil {
				t.Fatal(err)
			}
			if tt.posted {
				q.QuestionCommentID = "comment"
				if err := db.RecordHumanQuestionComment(t.Context(), q); err != nil {
					t.Fatal(err)
				}
			}
			candidate := q
			if tt.project != "" {
				candidate.ProjectID = tt.project
			}
			if tt.body != "" {
				candidate.Body = tt.body
			}
			if err := db.ReleaseHumanQuestionReservation(t.Context(), candidate); err != nil {
				t.Fatal(err)
			}
			records, err := db.HumanQuestions(t.Context(), q.ProjectID, q.IssueID)
			if err != nil || (len(records) == 0) != tt.released {
				t.Fatalf("records = %+v, %v", records, err)
			}
		})
	}
}
