package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/digitaldrywood/detent/internal/issueorigin"
)

func reportReleaseFailure(ctx context.Context, input io.Reader, getenv func(string) string, destination issueDestination) error {
	environment, phase := getenv("DETENT_RELEASE_ENVIRONMENT"), getenv("DETENT_RELEASE_FAILURE_PHASE")
	if getenv("GITHUB_REPOSITORY") != "digitaldrywood/detent" || environment != "staging" && environment != "production" || phase != "deploy" && phase != "smoke" {
		return errors.New("release failure reporting requires the selected Detent environment and phase")
	}
	if _, ok := destination.(*cloudDestination); !ok {
		return errors.New("release failure reporting requires the selected native Cloud owner")
	}
	if err := destination.load(ctx); err != nil {
		return err
	}
	runURL := fmt.Sprintf("%s/%s/actions/runs/%s/attempts/%s", getenv("GITHUB_SERVER_URL"), getenv("GITHUB_REPOSITORY"), getenv("GITHUB_RUN_ID"), getenv("GITHUB_RUN_ATTEMPT"))
	problem := "release:" + getenv("GITHUB_REPOSITORY") + ":" + environment + ":" + phase
	fingerprint := issueorigin.Fingerprint(problem)
	evidence := "Consult the Actions step and retained release artifacts."
	if raw, err := io.ReadAll(io.LimitReader(input, 64*1024)); err == nil {
		evidence += "\n\n" + diagnosticExcerpt(strings.TrimSpace(string(raw)))
	}
	body := fmt.Sprintf("Release **%s** %s %s failed at pinned develop commit %s.\n\nRun: %s\nProblem: `%s`\n\n```text\n%s\n```\n\nThe release workflow stopped; later deployment steps are not authorized by this failed smoke. File or reuse at least High priority through the native reporting owner. Infrastructure and unknown failures remain attributed to the release instance; source repair requires a reproducible source or test diagnostic. Preserve active work, operator holds and terminal history.\n\n```detent-agent\nschema: 1\neffort: high\n```", getenv("DETENT_RELEASE_TAG"), environment, phase, getenv("CI_DEVELOP_SHA"), runURL, problem, evidence)
	body = issueorigin.Stamp(body, issueorigin.Origin{Kind: "doctor", Instance: "github-actions", Source: runURL, Fingerprint: fingerprint})
	key := occurrenceKey(getenv, job{Name: environment + " " + phase}, fingerprint)
	return destination.file(ctx, fingerprint, "release "+environment+" "+phase+" failure", body, key, []string{"release-deployment-failure", "ci-infrastructure-failure"}, false)
}
