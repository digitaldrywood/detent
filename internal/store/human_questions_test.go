package store

import (
	"path/filepath"
	"testing"
	"time"
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

func TestOpenHumanQuestions(t *testing.T) {
	t.Parallel()
	db := openParkTestStore(t, filepath.Join(t.TempDir(), "questions.db"))
	old := time.Date(2026, 9, 14, 1, 0, 0, 0, time.UTC)
	recent := old.Add(8 * time.Hour)
	for _, tc := range []struct {
		key              string
		at               *time.Time
		posted, answered bool
	}{
		{"recent", &recent, true, false}, {"old", &old, true, false},
		{"answered", &old, true, true}, {"unpublished", &old, false, false},
		{"legacy", nil, true, false},
	} {
		t.Run(tc.key, func(t *testing.T) {
			q := HumanQuestion{ProjectID: tc.key, IssueID: "1", Identifier: "owner/repo#1", Key: tc.key, Body: "Choose?", AskedAt: tc.at}
			if _, err := db.ReserveHumanQuestion(t.Context(), q); err != nil {
				t.Fatal(err)
			}
			if tc.posted {
				q.QuestionCommentID = "123"
				if err := db.RecordHumanQuestionComment(t.Context(), q); err != nil {
					t.Fatal(err)
				}
			}
			if tc.answered {
				q.AnswerCommentID = "124"
				q.AnswerBody = "Yes"
				if err := db.RecordHumanQuestionAnswer(t.Context(), q); err != nil {
					t.Fatal(err)
				}
			}
			if tc.key == "legacy" {
				if _, err := db.db.ExecContext(t.Context(), "UPDATE human_questions SET asked_at = NULL WHERE project_id = 'legacy'"); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
	questions, err := db.OpenHumanQuestions(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(questions) != 3 {
		t.Fatalf("questions = %+v", questions)
	}
	for i, want := range []string{"legacy", "old", "recent"} {
		if questions[i].ProjectID != want {
			t.Fatalf("question %d = %+v", i, questions[i])
		}
	}
	if questions[0].AskedAt != nil || !questions[1].AskedAt.Equal(old) {
		t.Fatalf("ages = %+v", questions)
	}
	// Existing records gain the original comment time on reconciliation.
	q := HumanQuestion{ProjectID: "legacy", IssueID: "1", Key: "legacy", QuestionCommentID: "123", AskedAt: &old}
	if err := db.RecordHumanQuestionComment(t.Context(), q); err != nil {
		t.Fatal(err)
	}
	questions, err = db.OpenHumanQuestions(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if questions[0].AskedAt == nil || !questions[0].AskedAt.Equal(old) {
		t.Fatalf("legacy age = %+v", questions[0])
	}
}
