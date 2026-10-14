package workflowmetrics

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"time"
)

func PublicActivityProfile(p ActivityProfile) ActivityProfile {
	detailFrom := p.StartedAt
	if p.Summary != nil {
		summary := *p.Summary
		detailFrom = summary.DetailFrom
		summary.Breakdown.ByKind = publicActivityKinds(summary.Breakdown.ByKind)
		summary.Hourly = slices.Clone(summary.Hourly)
		if len(summary.Hourly) > ActivityHourLimit {
			summary.Hourly = summary.Hourly[len(summary.Hourly)-ActivityHourLimit:]
		}
		for i := range summary.Hourly {
			summary.Hourly[i].Breakdown.ByKind = publicActivityKinds(summary.Hourly[i].Breakdown.ByKind)
		}
		p.Summary = &summary
	}
	unfinishedSummary := slices.Contains(p.CoverageNotes, "summarized_intervals_exclude_unfinished_spans")
	p.Instance = ""
	p.AttemptRef = publicActivityHash(p.AttemptRef)
	p.ProviderThreadRef = publicActivityHash(p.ProviderThreadRef)
	p.ProviderSessionRef = publicActivityHash(p.ProviderSessionRef)
	p.Stage = publicActivityLabel(p.Stage, "implementation", "rework", "planning", "merging", "validation")
	p.Status = publicActivityLabel(p.Status, "running", "completed", "failed", "cancelled", "ended_without_terminal_event")
	p.Coverage = "partial"
	p.CoverageNotes = []string{"native_actions_have_no_individual_timing_or_outcomes", "provider_instruction_origin_unavailable", "read_requests_are_not_causal_proof", "instruction_versions_are_recorder_snapshots_not_provider_read_bytes", "instruction_snapshots_limited_to_workspace_regular_files_64_reads_64_sources_256KiB_each", "native_actions_limited_to_32_and_8KiB_per_event_4_inferred_candidates_per_action", "native_projection_bounded_to_128KiB"}
	p.CoverageNotes = append(p.CoverageNotes, "repeat_counts_limited_to_bounded_fingerprints", "historical_gap_intervals_not_retained_in_timing_summary")
	if unfinishedSummary {
		p.CoverageNotes = append(p.CoverageNotes, "summarized_intervals_exclude_unfinished_spans")
	}
	p.Sources = publicInstructionRefs(p.Sources)
	p.Spans = slices.Clone(p.Spans)
	for i := range p.Spans {
		s := &p.Spans[i]
		s.ID, s.ParentID = publicActivityHash(s.ID), publicActivityHash(s.ParentID)
		s.Fingerprint = publicActivityDigest(s.Fingerprint)
		s.Head = publicActivityDigest(s.Head)
		s.HeadAttribution = publicActivityLabel(s.HeadAttribution, "last_observed_workspace_snapshot")
		s.Kind = publicActivityLabel(s.Kind, "context_read", "implementation", "waiting", "local_validation", "rebase", "merge", "review", "tool_execution", "unclassified")
		s.Evidence = publicActivityLabel(s.Evidence, "read_tool", "edit_tool", "wait_tool", "opaque_tool_execution", "read_or_search", "go_test", "go_vet", "go_build", "make_validation", "make_check", "make_check_fast", "make_check_invariants", "make_test", "make_lint", "make_build", "lint_command", "git_rebase_or_conflict", "git_merge", "git_review", "gh_pr_merge", "gh_pr_review", "ci_check_observation", "ci_check_watch", "forge_merge_endpoint", "sleep_command", "tool_lifecycle", "compound_command", "validation_lock_marker", "native_read", "native_search", "opaque_native_action", "mixed_native_actions", "partial_native_actions")
		s.Outcome = publicActivityLabel(s.Outcome, "running", "completed", "failed", "cancelled", "unknown", "unobserved")
		s.Attribution = publicActivityLabel(s.Attribution, "observed", "unattributed", "observed_read_request", "inferred_text_match", "mixed_evidence")
		s.CausalAttribution = "unknown_provider_origin"
		s.WaitReason = publicActivityLabel(s.WaitReason, "validation_lock")
		s.Sources = publicInstructionRefs(s.Sources)
		s.Actions = slices.Clone(s.Actions)
		if len(s.Actions) > 32 {
			s.ActionsDropped += len(s.Actions) - 32
			s.Actions = s.Actions[:32]
		}
		for j := range s.Actions {
			a := &s.Actions[j]
			a.Type = publicActivityLabel(a.Type, "read", "search", "listFiles", "unknown")
			a.TypeRef, a.NameRef, a.PathRef, a.Fingerprint = publicActivityDigest(a.TypeRef), publicActivityDigest(a.NameRef), publicActivityDigest(a.PathRef), publicActivityDigest(a.Fingerprint)
			a.Evidence = publicActivityLabel(a.Evidence, s.Evidence, "native_read", "native_search", "opaque_native_action")
			a.Kind = publicActivityLabel(a.Kind, "context_read", "implementation", "waiting", "local_validation", "rebase", "merge", "review", "tool_execution", "unclassified")
			a.Attribution = publicActivityLabel(a.Attribution, "unattributed", "observed_read_request", "inferred_text_match")
			a.CausalAttribution = "unknown_provider_origin"
			a.SourceCoverage = publicActivityLabel(a.SourceCoverage, "instruction_path_unavailable", "not_instruction_file", "outside_workspace", "snapshot_limit", "snapshot_unavailable", "recorder_snapshot", "inferred_candidates_limited")
			a.Sources = publicInstructionRefs(a.Sources)
		}
	}
	end := p.AsOf
	if !p.FinishedAt.IsZero() {
		end = p.FinishedAt
	}
	p.SummarizeThrough(end)
	p.CoverageNotes = append(p.CoverageNotes, "hourly_timing_limited_to_latest_168_buckets_and_projection_size")
	if p.Summary != nil {
		p.Summary.DetailFrom = detailFrom
	}
	spans, omitted := p.Spans, p.ProjectionOmitted
	p.Spans = nil
	p.ProjectionOmitted = omitted + uint64(len(spans))
	publicActivityDetailFrom(&p, detailFrom)
	for p.Summary != nil && len(p.Summary.Hourly) > 0 {
		raw, err := json.Marshal(p)
		if err == nil && len(raw) <= 128*1024 {
			break
		}
		p.Summary.Hourly = p.Summary.Hourly[1:]
	}
	low, high := 0, len(spans)+1
	for low+1 < high {
		keep := low + (high-low)/2
		p.Spans, p.ProjectionOmitted = spans[len(spans)-keep:], omitted+uint64(len(spans)-keep)
		publicActivityDetailFrom(&p, detailFrom)
		raw, err := json.Marshal(p)
		if err != nil || len(raw) > 128*1024 {
			high = keep
		} else {
			low = keep
		}
	}
	p.Spans, p.ProjectionOmitted = spans[len(spans)-low:], omitted+uint64(len(spans)-low)
	publicActivityDetailFrom(&p, detailFrom)
	return p
}

func publicActivityDetailFrom(p *ActivityProfile, from time.Time) {
	if p.Summary == nil {
		return
	}
	if p.ProjectionOmitted > 0 && len(p.Spans) > 0 && p.Spans[0].StartedAt.After(from) {
		from = p.Spans[0].StartedAt
	}
	if len(p.Spans) == 0 {
		from = p.Summary.Through
	}
	p.Summary.DetailFrom = from
}

func publicActivityKinds(kinds map[string]float64) map[string]float64 {
	result := make(map[string]float64)
	for _, kind := range []string{"context_read", "implementation", "waiting", "local_validation", "rebase", "merge", "review", "tool_execution", "unclassified", "unknown", "concurrent", "unobserved"} {
		if seconds, ok := kinds[kind]; ok {
			result[kind] = seconds
		}
	}
	return result
}

func publicInstructionRefs(refs []InstructionRef) []InstructionRef {
	refs = slices.Clone(refs)
	if len(refs) > 64 {
		refs = refs[:64]
	}
	for i := range refs {
		r := &refs[i]
		r.Name = publicActivityLabel(r.Name, "AGENTS.md", "WORKFLOW.md", "CLAUDE.md", "WORKFLOW.md (effective)", "AGENTS.md (effective)")
		r.Hash, r.PathRef, r.Version = publicActivityDigest(r.Hash), publicActivityDigest(r.PathRef), publicActivityDigest(r.Version)
		r.Evidence = publicActivityLabel(r.Evidence, "startup_snapshot", "recorder_snapshot_after_native_read")
	}
	return refs
}

func publicActivityLabel(s string, allowed ...string) string {
	if s == "" || slices.Contains(allowed, s) {
		return s
	}
	return "unknown"
}

func publicActivityHash(s string) string {
	if s == "" {
		return ""
	}
	if len(s) == 64 && publicActivityDigest(s) != "" {
		return s
	}
	digest := sha256.Sum256([]byte(s))
	return hex.EncodeToString(digest[:])
}

func publicActivityDigest(s string) string {
	if len(s) != 40 && len(s) != 64 {
		return ""
	}
	if _, err := hex.DecodeString(s); err != nil {
		return ""
	}
	return s
}
