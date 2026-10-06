---
name: split-issue
aliases:
  - decompose-issue
  - break-down-issue
description: "Break one large Detent issue into small issues that each land on their own, linked by native dependencies so workers can run the independent ones in parallel."
when_to_use: "Use when an issue spans several packages or surfaces, would produce a PR too large to review in one pass, or bundles independent pieces of work that could run at the same time."
---

# Split a large issue into dependent issues

Decide whether to split at all:

- Leave the issue whole if it fits in one focused PR that touches one package or surface. Splitting adds dispatch, review and merge overhead to every piece.
- Split when the work crosses packages, layers (store, API, MCP, UI) or products (Hub, runner, local board), or when parts of it can be built without waiting on each other.

Read before you cut:

- Read the issue body, comments and the latest Workpad, plus the code it touches. Name the files and packages each piece will change.
- Check the tracker for open issues that already cover a piece. Comment on the match instead of filing a duplicate.

Shape each child issue:

- Each child lands on `develop` by itself with green checks and leaves the product working. Never file a child that only makes sense once a sibling merges unless the sibling is its declared blocker.
- Keep each child to one reviewable PR, ideally one package or one surface.
- Give each child a conventional-commit title, a short problem statement, the files it is expected to touch, and concrete acceptance criteria. New or changed Go behavior needs focused table-driven standard-library tests. A UI child also needs new Playwright coverage with unchanged desktop baselines.
- Carry over the parent's priority. Copy this exact `detent-agent` block into each child; never add a `model`:

  ```detent-agent
  schema: 1
  effort: high
  ```

- Put all shared context in the child bodies or tracker comments. Never pass knowledge through repository files (INV-16).

Draw the dependency graph:

- Aim for a wide, shallow graph. Every edge serializes work, so add one only when the child truly cannot be built or merged before its blocker lands, for example a schema before the code that reads it.
- Siblings that run in parallel must not edit the same files. Shared files cause merge conflicts and send cards to Rework. If two pieces must touch one file, chain them or merge them into one child.
- Timestamp migrations from parallel children do not collide; regenerate output from combined source inputs and publish generated files through the child's completion contract.
- Link only issues. A PR cannot gate dispatch; an issue blocked on an open PR waits on nothing.
- No epics. A tracking epic is never dispatched, so the parent must not become one.

Respect the scope rules:

- A child that adds or expands a mechanism (brake, breaker, lease, park, recovery path, revocation, reason code, reconciliation loop) goes to Backlog for human approval (INV-11). Everything else can go to Todo.
- Under INV-15, a child may add visible UI only when the human-authored parent names that UI change. Quote the parent's words in the child. If the parent does not name it, describe the idea in a comment and do not file a UI child.
- Do not widen the parent's scope while splitting. Unrelated defects you find become their own issues.

Propose in Luna:

- Load this skill with `load_split_issue_skill`, then read the parent with `explain_issue`. Luna cannot read source files or execute commands; make missing code context explicit in the proposal.
- Use `propose_issue_split` once for the whole split. Supply `parent_work_item_id`, every child's title, description, priority (0 urgent through 3 low, omitted when unset), target `state`, and all dependency `edges`. Children use one-based positions; 0 is the parent. Each edge has a `dependent` and a `blocker`.
- Include edges blocking the parent on the children for its remaining end-to-end acceptance. Show the entire split for one browser confirmation. Nothing is filed on proposal or cancellation; confirmation creates all children, links and the parent comment atomically. Never use individual filing tools or treat chat text as approval.

File and link outside Luna through an authorized tracker owner:

- Native projects: file each child with the project's `file_issue`, then link it with `set_dependency` (`identifier` is the dependent, `related` is the blocker, and the call needs the dependent's current revision). Dispatch skips any issue with an unfinished blocker while `require_dependencies` is on.
- GitHub-tracked projects: use native GitHub blocked-by links; a `Depends on: #N` body line is only a fallback.
- Leave the parent as the last node: link it as blocked by every child, and post a comment on it listing the children and the graph. When the children land, the parent's remaining work is the end-to-end acceptance check.
- Read the graph back and confirm every child shows the blockers you intended and that the first wave has none.
