package hubgithub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type releasePickTransportFixture struct {
	input        tracker.ReleasePickInput
	alter        string
	pulls        int
	creates      int
	existing     *releasePickPull
	lostResponse bool
	beforeRead   func()
}

func (f *releasePickTransportFixture) REST(_ context.Context, method, path string, request any, out any) error {
	if method != http.MethodGet && method != http.MethodPost {
		return errors.New("unexpected release mutation")
	}
	i := f.input
	repo := strings.TrimPrefix(i.Repository, "https://github.com/")
	root := "/repos/" + repo
	var response any
	pull := releasePickPull{Number: i.PRNumber, State: "open"}
	pull.Head = releasePickRef{Ref: i.HeadRef, SHA: i.HeadSHA}
	pull.Base = releasePickRef{Ref: i.BaseRef, SHA: i.BaseSHA}
	pull.Head.Repo.FullName = repo
	pull.Base.Repo.FullName = repo
	if strings.HasPrefix(path, root+"/pulls?") {
		if f.beforeRead != nil {
			f.beforeRead()
		}
		response = []releasePickPull{}
		if f.existing != nil {
			response = []releasePickPull{*f.existing}
		}
		if f.alter == "ambiguous" && f.existing != nil {
			response = []releasePickPull{*f.existing, *f.existing}
		}
	} else if method == http.MethodPost && path == root+"/pulls" {
		f.creates++
		var payload struct {
			Body                string `json:"body"`
			Head                string `json:"head"`
			Base                string `json:"base"`
			Draft               bool   `json:"draft"`
			MaintainerCanModify bool   `json:"maintainer_can_modify"`
		}
		raw, err := json.Marshal(request)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			return err
		}
		if payload.Head != i.HeadRef || payload.Base != i.BaseRef || payload.Draft || payload.MaintainerCanModify {
			return errors.New("publication widened branch authority")
		}
		pull.Body = payload.Body
		f.existing = &pull
		if f.lostResponse {
			f.lostResponse = false
			return errors.New("lost create response")
		}
		response = pull
	} else {
		switch path {
		case root + "/pulls/7":
			f.pulls++
			switch f.alter {
			case "head":
				pull.Head.SHA = strings.Repeat("0", 40)
			case "base":
				pull.Base.Ref = "develop"
			case "repository":
				pull.Head.Repo.FullName = "owner/foreign"
			case "draft":
				pull.Draft = true
			case "moved during verification":
				if f.pulls == 2 {
					pull.Head.SHA = strings.Repeat("0", 40)
				}
			}
			response = pull
		case root + "/git/ref/heads/pick%2Ffix":
			sha := i.HeadSHA
			if f.alter == "head moved" {
				sha = strings.Repeat("0", 40)
			}
			response = map[string]any{"object": map[string]string{"sha": sha}}
		case root + "/git/ref/heads/release%2Fv1.2":
			sha := i.BaseSHA
			if f.alter == "branch moved" {
				sha = strings.Repeat("0", 40)
			}
			response = map[string]any{"object": map[string]any{"sha": sha}}
		case root + "/compare/" + i.SourceMergeSHA + "...develop":
			status := "ahead"
			if f.alter == "unmerged source" {
				status = "diverged"
			}
			response = map[string]string{"status": status}
		case root + "/compare/" + i.BaseSHA + "...develop":
			sha := i.FrozenSHA
			if f.alter == "frozen identity" {
				sha = strings.Repeat("0", 40)
			}
			response = map[string]any{"merge_base_commit": map[string]string{"sha": sha}}
		case root + "/pulls/7/commits?per_page=2":
			commit := map[string]any{"sha": i.HeadSHA, "parents": []map[string]string{{"sha": i.BaseSHA}}, "commit": map[string]string{"message": "Repair\n\n(cherry picked from commit " + i.SourceMergeSHA + ")"}}
			switch f.alter {
			case "missing trailer":
				commit["commit"] = map[string]string{"message": "Repair"}
			case "wrong trailer":
				commit["commit"] = map[string]string{"message": "(cherry picked from commit " + strings.Repeat("0", 40) + ")"}
			case "extra commit":
				response = []any{commit, commit}
			case "wrong parent":
				commit["parents"] = []map[string]string{{"sha": strings.Repeat("0", 40)}}
			}
			if response == nil {
				response = []any{commit}
			}
		case root + "/commits/" + i.SourceMergeSHA, root + "/commits/" + i.HeadSHA:
			sha := i.SourceMergeSHA
			patch := "@@ -1 +1 @@\n-old\n+new"
			if path == root+"/commits/"+i.HeadSHA {
				sha = i.HeadSHA
				patch = "@@ -2 +2 @@\n-old\n+new"
				if f.alter == "wrong patch" {
					patch = "@@ -2 +2 @@\n-old\n+unrelated"
				}
			}
			message := "Repair\n\n(cherry picked from commit " + i.SourceMergeSHA + ")"
			parent := i.BaseSHA
			if f.alter == "prepared missing trailer" {
				message = "Repair"
			}
			if f.alter == "prepared wrong trailer" {
				message = "Repair\n\n(cherry picked from commit " + strings.Repeat("0", 40) + ")"
			}
			if f.alter == "prepared wrong parent" {
				parent = strings.Repeat("0", 40)
			}
			response = map[string]any{"sha": sha, "parents": []map[string]string{{"sha": parent}}, "commit": map[string]string{"message": message}, "files": []releasePickFile{{Filename: "repair.go", Status: "modified", Patch: patch}}}

		default:
			return errors.New("unexpected release verification path: " + path)
		}
	}
	raw, err := json.Marshal(response)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

func (f *releasePickTransportFixture) RESTPage(ctx context.Context, path string, output any) (string, error) {
	if f.alter == "truncated patch" {
		return "next", nil
	}
	return "", f.REST(ctx, http.MethodGet, strings.TrimSuffix(path, "?per_page=100"), nil, output)
}

func TestVerifyReleasePick(t *testing.T) {
	for _, name := range []string{"valid", "head", "base", "repository", "draft", "branch moved", "unmerged source", "frozen identity", "missing trailer", "wrong trailer", "extra commit", "wrong parent", "wrong patch", "moved during verification", "truncated patch"} {
		t.Run(name, func(t *testing.T) {
			input := tracker.ReleasePickInput{Repository: "https://github.com/example-org/example-project", PRNumber: 7, SourceMergeSHA: strings.Repeat("a", 40), HeadRef: "pick/fix", HeadSHA: strings.Repeat("b", 40), BaseRef: "release/v1.2", BaseSHA: strings.Repeat("c", 40), FrozenSHA: strings.Repeat("d", 40)}
			client := &releasePickTransportFixture{input: input, alter: name}
			err := NewReconciler(client).VerifyReleasePick(t.Context(), input)
			if (err == nil) != (name == "valid") {
				t.Fatalf("verification error=%v", err)
			}
			if err != nil && !errors.Is(err, hubserver.ErrReleasePickMismatch) {
				t.Fatalf("refusal lost provenance classification: %v", err)
			}
			if name == "valid" && client.pulls != 2 {
				t.Fatalf("verified PR observations=%d", client.pulls)
			}
		})
	}
}
