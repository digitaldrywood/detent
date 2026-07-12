package kanban

import (
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/telemetry"
)

const (
	overlayContradictionLimit = 2
	removalPendingTTL         = 5 * time.Minute
)

type MutationTracker struct {
	mu          sync.Mutex
	locks       map[string]*sync.Mutex
	states      map[string]pendingState
	synthesized map[string]synthesizedCard
	removed     map[string]pendingRemoval
	reverted    map[string]RevertNotice
}

type pendingState struct {
	snapshot             string
	current              string
	project              string
	issue                telemetry.Issue
	dataSeqAtWrite       uint64
	contradictions       int
	lastContradictionSeq uint64
}

type pendingRemoval struct {
	snapshot       string
	removedAt      time.Time
	dataSeqAtWrite uint64
}

type synthesizedCard struct {
	project string
	issue   telemetry.Issue
}

type RevertNotice struct {
	Identifier string
	From       string
	To         string
	At         time.Time
}

func NewMutationTracker() *MutationTracker {
	return &MutationTracker{
		locks:       map[string]*sync.Mutex{},
		states:      map[string]pendingState{},
		synthesized: map[string]synthesizedCard{},
		removed:     map[string]pendingRemoval{},
		reverted:    map[string]RevertNotice{},
	}
}

func (t *MutationTracker) WithLock(key string, fn func() error) error {
	lock := t.lockFor(key)
	lock.Lock()
	defer lock.Unlock()
	return fn()
}

func (t *MutationTracker) lockFor(key string) *sync.Mutex {
	key = strings.TrimSpace(key)
	if key == "" {
		key = "default"
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	lock, ok := t.locks[key]
	if !ok {
		lock = &sync.Mutex{}
		t.locks[key] = lock
	}
	return lock
}

func (t *MutationTracker) CardState(key string, issueID string, snapshotState string, dataSeq uint64) string {
	stateKey := MutationStateKey(key, issueID)
	if stateKey == "" {
		return snapshotState
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	return t.cardStateLocked(stateKey, snapshotState, dataSeq)
}

func (t *MutationTracker) cardStateLocked(stateKey string, snapshotState string, dataSeq uint64) string {
	pending, ok := t.states[stateKey]
	if !ok {
		return snapshotState
	}
	if dataSeq <= pending.dataSeqAtWrite {
		return pending.current
	}

	switch {
	case NormalizeState(snapshotState) == NormalizeState(pending.current):
		delete(t.states, stateKey)
		return snapshotState
	case NormalizeState(snapshotState) == NormalizeState(pending.snapshot):
		if dataSeq > pending.lastContradictionSeq {
			pending.contradictions++
			pending.lastContradictionSeq = dataSeq
		}
		if pending.contradictions < overlayContradictionLimit {
			t.states[stateKey] = pending
			return pending.current
		}
		delete(t.states, stateKey)
		t.noteRevertLocked(stateKey, pending, snapshotState)
		return snapshotState
	default:
		delete(t.states, stateKey)
		return snapshotState
	}
}

func (t *MutationTracker) NoteCardState(key string, projectID string, issue telemetry.Issue, snapshotState string, currentState string, dataSeqAtWrite uint64) {
	issue.ID = strings.TrimSpace(issue.ID)
	stateKey := MutationStateKey(key, issue.ID)
	if stateKey == "" || strings.TrimSpace(currentState) == "" {
		return
	}
	projectID = strings.TrimSpace(projectID)
	if strings.TrimSpace(issue.ProjectID) == "" {
		issue.ProjectID = projectID
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if pending, ok := t.states[stateKey]; ok && NormalizeState(snapshotState) == NormalizeState(pending.snapshot) {
		snapshotState = pending.snapshot
	}
	delete(t.removed, stateKey)
	delete(t.reverted, stateKey)
	delete(t.synthesized, stateKey)
	t.states[stateKey] = pendingState{
		snapshot:       strings.TrimSpace(snapshotState),
		current:        strings.TrimSpace(currentState),
		project:        projectID,
		issue:          CloneIssue(issue),
		dataSeqAtWrite: dataSeqAtWrite,
	}
}

func (t *MutationTracker) PendingMovedCards(key string, projectID string, snapshot telemetry.Snapshot, terminalStates []string) []telemetry.Issue {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil
	}
	projectID = strings.TrimSpace(projectID)
	dataSeq := SnapshotProjectDataSeq(snapshot, projectID)
	terminal := make(map[string]struct{}, len(terminalStates))
	for _, state := range terminalStates {
		if state = NormalizeState(state); state != "" {
			terminal[state] = struct{}{}
		}
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	present := map[string]struct{}{}
	for _, entry := range visibleSnapshotIssueEntries(snapshot) {
		issueID := strings.TrimSpace(entry.Issue.ID)
		if issueID == "" || !SameProject(entry.Issue, projectID, snapshot.Project.ID) {
			continue
		}
		stateKey := MutationStateKey(key, issueID)
		delete(t.synthesized, stateKey)
		if _, ok := t.states[stateKey]; !ok {
			continue
		}
		present[stateKey] = struct{}{}
	}
	for _, row := range snapshot.Completed {
		issue := row.Issue
		issueID := strings.TrimSpace(issue.ID)
		if issueID == "" || !SameProject(issue, projectID, snapshot.Project.ID) {
			continue
		}
		stateKey := MutationStateKey(key, issueID)
		if _, ok := present[stateKey]; ok {
			continue
		}
		pending, ok := t.states[stateKey]
		if !ok {
			continue
		}
		snapshotState := strings.TrimSpace(issue.State)
		state := t.cardStateLocked(stateKey, snapshotState, dataSeq)
		_, stillPending := t.states[stateKey]
		_, terminalState := terminal[NormalizeState(snapshotState)]
		if !stillPending && !terminalState && NormalizeState(snapshotState) != NormalizeState(pending.current) && NormalizeState(snapshotState) != NormalizeState(pending.snapshot) {
			synthesizedIssue := CloneIssue(pending.issue)
			synthesizedIssue.State = strings.TrimSpace(state)
			t.synthesized[stateKey] = synthesizedCard{
				project: pending.project,
				issue:   synthesizedIssue,
			}
		}
	}

	out := []telemetry.Issue{}
	appendCard := func(stateKey string, project string, issue telemetry.Issue) {
		if _, ok := present[stateKey]; ok {
			return
		}
		issueID := strings.TrimSpace(issue.ID)
		if issueID == "" || MutationStateKey(key, issueID) != stateKey {
			return
		}
		if projectID != "" && project != "" && project != projectID {
			return
		}
		if !SameProject(issue, projectID, snapshot.Project.ID) {
			return
		}
		issue = CloneIssue(issue)
		if strings.TrimSpace(issue.ProjectID) == "" {
			issue.ProjectID = projectID
		}
		out = append(out, issue)
	}
	for stateKey, pending := range t.states {
		issue := CloneIssue(pending.issue)
		issue.State = strings.TrimSpace(pending.current)
		appendCard(stateKey, pending.project, issue)
	}
	for stateKey, synthesized := range t.synthesized {
		appendCard(stateKey, synthesized.project, synthesized.issue)
	}
	sort.SliceStable(out, func(i, j int) bool {
		left := strings.TrimSpace(out[i].Identifier)
		right := strings.TrimSpace(out[j].Identifier)
		if left != right {
			return left < right
		}
		return strings.TrimSpace(out[i].ID) < strings.TrimSpace(out[j].ID)
	})
	return out
}

func (t *MutationTracker) SnapshotCardStates(key string, projectID string, snapshot telemetry.Snapshot) map[string]string {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil
	}
	projectID = strings.TrimSpace(projectID)
	dataSeq := SnapshotProjectDataSeq(snapshot, projectID)

	t.mu.Lock()
	defer t.mu.Unlock()

	out := map[string]string{}
	for _, entry := range visibleSnapshotIssueEntries(snapshot) {
		issueID := strings.TrimSpace(entry.Issue.ID)
		if issueID == "" || !SameProject(entry.Issue, projectID, snapshot.Project.ID) {
			continue
		}
		stateKey := MutationStateKey(key, issueID)
		if _, ok := t.states[stateKey]; !ok {
			continue
		}
		state := t.cardStateLocked(stateKey, entry.State, dataSeq)
		if strings.TrimSpace(state) != "" {
			out[stateKey] = state
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (t *MutationTracker) CardRemoved(key string, issueID string, snapshotState string, dataSeq uint64) bool {
	stateKey := MutationStateKey(key, issueID)
	if stateKey == "" {
		return false
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	removed, ok := t.removed[stateKey]
	if !ok {
		return false
	}
	if time.Since(removed.removedAt) > removalPendingTTL {
		delete(t.removed, stateKey)
		return false
	}
	if dataSeq <= removed.dataSeqAtWrite {
		return true
	}
	if NormalizeState(snapshotState) == NormalizeState(removed.snapshot) {
		return true
	}
	delete(t.removed, stateKey)
	return false
}

func (t *MutationTracker) NoteCardRemoved(key string, issueID string, snapshotState string, dataSeqAtWrite uint64) {
	stateKey := MutationStateKey(key, issueID)
	if stateKey == "" {
		return
	}

	t.mu.Lock()
	delete(t.states, stateKey)
	delete(t.synthesized, stateKey)
	delete(t.reverted, stateKey)
	t.removed[stateKey] = pendingRemoval{
		snapshot:       strings.TrimSpace(snapshotState),
		removedAt:      time.Now(),
		dataSeqAtWrite: dataSeqAtWrite,
	}
	t.mu.Unlock()
}

func (t *MutationTracker) noteRevertLocked(stateKey string, pending pendingState, snapshotState string) {
	identifier := strings.TrimSpace(pending.issue.Identifier)
	if identifier == "" {
		identifier = strings.TrimSpace(pending.issue.ID)
	}
	to := strings.TrimSpace(snapshotState)
	if to == "" {
		to = strings.TrimSpace(pending.snapshot)
	}
	t.reverted[stateKey] = RevertNotice{
		Identifier: identifier,
		From:       strings.TrimSpace(pending.current),
		To:         to,
		At:         time.Now(),
	}
}

func (t *MutationTracker) ConsumeRevertNotices(key string, projectID string) []RevertNotice {
	key = strings.TrimSpace(key)
	projectID = strings.TrimSpace(projectID)
	if key == "" && projectID != "" {
		key = "project:" + projectID
	}
	prefix := ""
	if key != "" {
		prefix = key + "\x00"
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	notices := []RevertNotice{}
	for stateKey, notice := range t.reverted {
		if prefix != "" && !strings.HasPrefix(stateKey, prefix) {
			continue
		}
		notices = append(notices, notice)
		delete(t.reverted, stateKey)
	}
	sort.SliceStable(notices, func(i, j int) bool {
		if !notices[i].At.Equal(notices[j].At) {
			return notices[i].At.Before(notices[j].At)
		}
		return notices[i].Identifier < notices[j].Identifier
	})
	return notices
}

func MutationStateKey(key string, issueID string) string {
	key = strings.TrimSpace(key)
	issueID = strings.TrimSpace(issueID)
	if key == "" || issueID == "" {
		return ""
	}
	return key + "\x00" + issueID
}
