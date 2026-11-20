package runobserve

type AgentTokenUsage struct {
	InputTokens           int64
	CachedInputTokens     int64
	OutputTokens          int64
	ReasoningOutputTokens int64
	TotalTokens           int64
	ThreadTotal           *AgentTokenCounts
	Last                  *AgentTokenCounts
	ModelContextWindow    *int64
}

type AgentTokenCounts struct {
	InputTokens           int64
	CachedInputTokens     int64
	OutputTokens          int64
	ReasoningOutputTokens int64
	TotalTokens           int64
}

type sessionTokenUsage struct {
	resumed  bool
	baseline *AgentTokenCounts
}

func NewSessionTokenUsage(resumed bool) *sessionTokenUsage {
	return &sessionTokenUsage{resumed: resumed}
}

func (s *sessionTokenUsage) Normalize(tokens AgentTokenUsage) AgentTokenUsage {
	if tokens.ThreadTotal == nil {
		return tokens
	}
	threadTotal := *tokens.ThreadTotal
	if !s.resumed {
		return agentTokenUsageWithCounts(tokens, threadTotal)
	}
	if s.baseline == nil {
		baseline := AgentTokenCounts{}
		if tokens.Last != nil {
			baseline = subtractAgentTokenCounts(threadTotal, *tokens.Last)
		}
		s.baseline = &baseline
	}
	return agentTokenUsageWithCounts(tokens, subtractAgentTokenCounts(threadTotal, *s.baseline))
}

func agentTokenUsageWithCounts(tokens AgentTokenUsage, counts AgentTokenCounts) AgentTokenUsage {
	tokens.InputTokens = counts.InputTokens
	tokens.CachedInputTokens = counts.CachedInputTokens
	tokens.OutputTokens = counts.OutputTokens
	tokens.ReasoningOutputTokens = counts.ReasoningOutputTokens
	tokens.TotalTokens = counts.TotalTokens
	return tokens
}

func subtractAgentTokenCounts(total AgentTokenCounts, baseline AgentTokenCounts) AgentTokenCounts {
	return AgentTokenCounts{
		InputTokens:           max(0, total.InputTokens-baseline.InputTokens),
		CachedInputTokens:     max(0, total.CachedInputTokens-baseline.CachedInputTokens),
		OutputTokens:          max(0, total.OutputTokens-baseline.OutputTokens),
		ReasoningOutputTokens: max(0, total.ReasoningOutputTokens-baseline.ReasoningOutputTokens),
		TotalTokens:           max(0, total.TotalTokens-baseline.TotalTokens),
	}
}
