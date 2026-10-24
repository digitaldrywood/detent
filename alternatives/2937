package securityaudit

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"unicode/utf8"
)

const chunkInstructions = `The chunk field is host-generated coverage metadata for one immutable complete audit. textual_diff contains exactly the indicated byte range, which may begin or end inside a file, hunk, or line. Context repeats original file/hunk headers only for orientation. Review every supplied byte, including fragments and binary notices. Continue the same audit conversation and consider interactions with previously supplied files and hunks. Report findings from this chunk and any cross-file interactions now established. Never resolve findings from an earlier chunk of this same audit; the host retains them independently. previous_audit, if present, is a separate trusted earlier-head audit and retains its normal continuation rules. A model summary is not evidence that any source bytes were reviewed.`

type ChunkIdentity struct {
	SnapshotDigest string `json:"snapshot_digest"`
	DiffDigest     string `json:"diff_digest"`
	Index          int    `json:"index"`
	Count          int    `json:"count"`
	Start          int    `json:"start"`
	End            int    `json:"end"`
	FileStart      int    `json:"file_start"`
	HunkStart      int    `json:"hunk_start"`
	LineStart      int    `json:"line_start"`
	Context        string `json:"context,omitempty"`
}

type ChunkOutput struct {
	Identity ChunkIdentity `json:"identity"`
	Output   string        `json:"output"`
}

type Plan struct {
	snapshot Snapshot
	chunks   []ChunkIdentity
	prompts  []string
}

func BuildPlan(snapshot Snapshot, limit int) (Plan, error) {
	if limit <= 0 {
		limit = DefaultMaxDiffBytes
	}
	promptLimit := min(limit, 512*1024)
	snapshot.FindingFiles = maps.Clone(snapshot.FindingFiles)
	if snapshot.Previous != nil {
		previous := *snapshot.Previous
		previous.Findings = slices.Clone(previous.Findings)
		snapshot.Previous = &previous
	}
	plan := Plan{snapshot: snapshot}
	if snapshot.Chunk != nil || snapshot.DiffTruncated || !utf8.ValidString(snapshot.Diff) || (snapshot.DiffBytes != 0 && snapshot.DiffBytes != len(snapshot.Diff)) {
		return Plan{}, errors.New("security audit requires a complete original UTF-8 snapshot")
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return Plan{}, fmt.Errorf("encode audit source: %w", err)
	}
	identity := ChunkIdentity{SnapshotDigest: OutputDigest(string(raw)), DiffDigest: OutputDigest(snapshot.Diff), Count: 1, End: len(snapshot.Diff), FileStart: -1, HunkStart: -1}
	prompt, err := BuildPrompt(snapshot, limit)
	if err == nil && encodedPromptBytes(prompt) <= promptLimit {
		plan.chunks = []ChunkIdentity{identity}
		plan.prompts = []string{prompt}
		return plan, nil
	}
	var sizeErr *DiffTooLargeError
	if err == nil {
		sizeErr = &DiffTooLargeError{ActualBytes: encodedPromptBytes(prompt), LimitBytes: promptLimit}
		err = sizeErr
	}
	if !errors.As(err, &sizeErr) || len(snapshot.Diff) == 0 {
		return Plan{}, err
	}
	contextBytes := min(4096, promptLimit/16)
	position := 0
	lineStart, fileStart, hunkStart := 0, -1, -1
	fileHeader, hunkHeader := "", ""
	for start := 0; start < len(snapshot.Diff); {
		for position <= start && position < len(snapshot.Diff) {
			end := strings.IndexByte(snapshot.Diff[position:], '\n')
			if end < 0 {
				end = len(snapshot.Diff)
			} else {
				end += position + 1
			}
			line := snapshot.Diff[position:end]
			lineStart = position
			if strings.HasPrefix(line, "diff --git ") {
				fileStart, hunkStart = position, -1
				fileHeader, hunkHeader = boundedUTF8(line, contextBytes), ""
			} else if strings.HasPrefix(line, "@@") {
				hunkStart = position
				hunkHeader = boundedUTF8(line, contextBytes)
			}
			position = end
		}
		chunk := identity
		chunk.Index, chunk.Start = len(plan.chunks), start
		chunk.FileStart, chunk.HunkStart, chunk.LineStart = fileStart, hunkStart, lineStart
		chunk.Context = fileHeader + hunkHeader
		chunk.Count, chunk.End = len(snapshot.Diff), len(snapshot.Diff)
		part := snapshot
		part.Chunk, part.Diff, part.DiffBytes = &chunk, "", 0
		overhead, err := sourceBytes(part)
		if err != nil {
			return Plan{}, err
		}
		end := min(len(snapshot.Diff), start+promptLimit-overhead)
		for {
			if end <= start {
				return Plan{}, sizeErr
			}
			for end > start && end < len(snapshot.Diff) && !utf8.RuneStart(snapshot.Diff[end]) {
				end--
			}
			if end <= start {
				return Plan{}, sizeErr
			}
			if end < len(snapshot.Diff) {
				if newline := strings.LastIndexByte(snapshot.Diff[start:end], '\n'); newline >= 0 {
					end = start + newline + 1
				}
			}
			chunk.End = end
			part.Diff, part.DiffBytes = snapshot.Diff[start:end], end-start
			candidate, buildErr := BuildPrompt(part, limit)
			if buildErr != nil {
				return Plan{}, buildErr
			}
			if encodedPromptBytes(candidate) <= promptLimit {
				break
			}
			end -= min(end-start, max(1, min((end-start)/2, encodedPromptBytes(candidate)-promptLimit)))
		}
		chunk.End = end
		plan.chunks = append(plan.chunks, chunk)
		start = end
	}
	for index := range plan.chunks {
		chunk := &plan.chunks[index]
		chunk.Count = len(plan.chunks)
		part := snapshot
		part.Chunk, part.Diff, part.DiffBytes = chunk, snapshot.Diff[chunk.Start:chunk.End], chunk.End-chunk.Start
		prompt, err := BuildPrompt(part, limit)
		if err != nil {
			return Plan{}, err
		}
		plan.prompts = append(plan.prompts, prompt)
	}
	return plan, nil
}

func encodedPromptBytes(prompt string) int {
	encoded, err := json.Marshal(prompt)
	if err != nil {
		return math.MaxInt
	}
	return len(encoded)
}

func sourceBytes(snapshot Snapshot) (int, error) {
	size := max(len(snapshot.Diff), snapshot.DiffBytes)
	for path, content := range snapshot.FindingFiles {
		size += len(path) + len(content)
	}
	if snapshot.Previous != nil {
		raw, err := json.Marshal(snapshot.Previous)
		if err != nil {
			return 0, err
		}
		size += len(raw)
	}
	if snapshot.Verification != nil {
		raw, err := json.Marshal(snapshot.Verification)
		if err != nil {
			return 0, err
		}
		size += len(raw)
	}
	if snapshot.Chunk != nil {
		raw, err := json.Marshal(snapshot.Chunk)
		if err != nil {
			return 0, err
		}
		size += len(raw)
	}
	return size, nil
}

func (p Plan) Len() int { return len(p.chunks) }

func (p Plan) Chunk(index int) (ChunkIdentity, string) {
	return p.chunks[index], p.prompts[index]
}

func (p Plan) Aggregate(outputs []ChunkOutput) (Result, string, error) {
	if len(p.chunks) == 0 || len(outputs) != len(p.chunks) {
		return Result{}, "", errors.New("security audit chunk sequence is incomplete")
	}
	result := Result{Verdict: VerdictPass, Summary: fmt.Sprintf("Reviewed %d bounded chunks covering %d source bytes.", len(outputs), len(p.snapshot.Diff)), Findings: []Finding{}}
	findings := make(map[string]Finding)
	prior := make(map[string]bool)
	if p.snapshot.Previous != nil {
		for _, finding := range p.snapshot.Previous.Findings {
			prior[finding.ID] = true
		}
	}
	if p.snapshot.Verification != nil {
		for _, finding := range p.snapshot.Verification.Findings {
			prior[finding.ID] = true
		}
	}
	for index, output := range outputs {
		if output.Identity != p.chunks[index] {
			return Result{}, "", errors.New("security audit chunk identity mismatch")
		}
		chunkResult, err := ParseOutput(output.Output)
		if err == nil {
			err = ValidateSnapshotContinuation(p.snapshot, chunkResult)
		}
		if err != nil {
			return Result{}, "", fmt.Errorf("security audit chunk %d: %w", index, err)
		}
		if len(outputs) == 1 {
			return chunkResult, output.Output, nil
		}
		for _, finding := range chunkResult.Findings {
			if !prior[finding.ID] {
				raw, err := json.Marshal(struct {
					Path string
					Line int
					Body string
				}{finding.Path, finding.Line, finding.Body})
				if err != nil {
					return Result{}, "", err
				}
				finding.ID = "chunk-" + OutputDigest(string(raw))
				for prior[finding.ID] {
					finding.ID = "chunk-" + finding.ID
				}
			}
			existing, ok := findings[finding.ID]
			if ok {
				if existing.Certainty != CertaintyUndetermined {
					if finding.Certainty == CertaintyUndetermined || existing.Status != "resolved" && existing.Status != "rejected" && (finding.Status == "resolved" || finding.Status == "rejected") {
						finding = existing
					}
				}
				if existing.Severity != "" && finding.Severity != "" && existing.Severity < finding.Severity {
					finding.Severity = existing.Severity
				}
			}
			findings[finding.ID] = finding
		}
	}
	ids := make([]string, 0, len(findings))
	for id := range findings {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		finding := findings[id]
		result.Findings = append(result.Findings, finding)
		if finding.Status != "resolved" && finding.Status != "rejected" {
			if finding.Certainty == CertaintyUndetermined {
				if result.Verdict != VerdictFail {
					result.Verdict = VerdictUndetermined
				}
			} else {
				result.Verdict = VerdictFail
			}
		}
	}
	raw, err := json.Marshal(struct {
		Chunks []ChunkOutput `json:"chunks"`
		Result Result        `json:"result"`
	}{outputs, result})
	if err != nil {
		return Result{}, "", err
	}
	return result, string(raw), nil
}
