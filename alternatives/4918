package runner

import (
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/runner/prompt"
)

type ValidatorTaskContext = prompt.ValidatorTaskContext

func ValidationContext(issue connector.Issue, policy gate.Config, workflow string) ValidatorTaskContext {
	return prompt.ValidationContext(issue, policy, workflow)
}

func ValidationContextDigest(issue connector.Issue, policy gate.Config, workflow string) string {
	return prompt.ValidationContextDigest(issue, policy, workflow)
}
