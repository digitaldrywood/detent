---
identity:
  name: non-code-artifact-demo

tracker:
  kind: local_sqlite
  local_sqlite:
    path: .detent/non-code-artifact-demo.db
    project_id: content-production-demo
  active_states: []
  observed_states:
    - Intake
    - Research
    - Draft
    - Review
    - Rework
    - Package
    - Blocked
  terminal_states:
    - Publish
    - Cancelled
  issues:
    - id: content-demo-001
      identifier: content-demo-001
      number: 1
      title: Research launch proof points
      description: Gather source facts for the release article and record source paths in the artifact manifest.
      state: Research
      assigned_to_worker: false
      labels:
        - content-demo
        - research
      priority: 1
      fields:
        artifact_type: research_brief
        audience: evaluator
        channel: launch-blog
        render_status: queued
        owner: research
      metadata:
        workflow_step: Research
        source_assets: inputs/customer-interviews.md; inputs/product-notes.md
        expected_manifest: output/research-launch-proof-points/manifest.json
      deliverable:
        kind: artifact
        path: output/research-launch-proof-points/manifest.json
        review_url: http://127.0.0.1:4101/review/content-demo-001
        external_id: content-demo-001
        metadata:
          artifact_gate: render_status
          expected_format: markdown
          output_directory: output/research-launch-proof-points
    - id: content-demo-002
      identifier: content-demo-002
      number: 2
      title: Draft article narrative
      description: Turn approved research into a publication draft with section-level source notes.
      state: Draft
      assigned_to_worker: false
      labels:
        - content-demo
        - draft
      priority: 2
      fields:
        artifact_type: markdown_draft
        audience: evaluator
        channel: launch-blog
        render_status: pending_review
        owner: editorial
      metadata:
        workflow_step: Draft
        source_assets: output/research-launch-proof-points/manifest.json
        expected_manifest: output/draft-article-narrative/manifest.json
      deliverable:
        kind: artifact
        path: output/draft-article-narrative/manifest.json
        review_url: http://127.0.0.1:4101/review/content-demo-002
        external_id: content-demo-002
        metadata:
          artifact_gate: render_status
          expected_format: markdown
          output_directory: output/draft-article-narrative
    - id: content-demo-003
      identifier: content-demo-003
      number: 3
      title: Review image captions
      description: Review social-card captions and mark the render_status field when approved or returned for rework.
      state: Review
      assigned_to_worker: false
      labels:
        - content-demo
        - review
      priority: 2
      fields:
        artifact_type: caption_pack
        audience: social
        channel: linkedin
        render_status: pending_review
        owner: reviewer
      metadata:
        workflow_step: Review
        source_assets: output/draft-article-narrative/manifest.json
        expected_manifest: output/review-image-captions/manifest.json
      deliverable:
        kind: artifact
        path: output/review-image-captions/manifest.json
        review_url: http://127.0.0.1:4101/review/content-demo-003
        external_id: content-demo-003
        metadata:
          artifact_gate: render_status
          expected_format: json
          output_directory: output/review-image-captions
    - id: content-demo-004
      identifier: content-demo-004
      number: 4
      title: Package approved newsletter
      description: This Review card starts with render_status approved so the artifact gate can promote it to Package on refresh.
      state: Review
      assigned_to_worker: false
      labels:
        - content-demo
        - approved
      priority: 1
      fields:
        artifact_type: newsletter_package
        audience: customers
        channel: email
        render_status: approved
        owner: reviewer
      metadata:
        workflow_step: Review
        source_assets: output/draft-article-narrative/manifest.json
        expected_manifest: output/package-approved-newsletter/manifest.json
      deliverable:
        kind: artifact
        path: output/package-approved-newsletter/manifest.json
        review_url: http://127.0.0.1:4101/review/content-demo-004
        external_id: content-demo-004
        metadata:
          artifact_gate: render_status
          expected_format: zip-manifest
          output_directory: output/package-approved-newsletter
    - id: content-demo-005
      identifier: content-demo-005
      number: 5
      title: Rework short-form teaser
      description: This Review card starts with render_status recut so the artifact gate can route it to Rework on refresh.
      state: Review
      assigned_to_worker: false
      labels:
        - content-demo
        - needs-rework
      priority: 3
      fields:
        artifact_type: short_form_teaser
        audience: social
        channel: video
        render_status: recut
        owner: reviewer
      metadata:
        workflow_step: Review
        source_assets: output/draft-article-narrative/manifest.json
        expected_manifest: output/rework-short-form-teaser/manifest.json
      deliverable:
        kind: artifact
        path: output/rework-short-form-teaser/manifest.json
        review_url: http://127.0.0.1:4101/review/content-demo-005
        external_id: content-demo-005
        metadata:
          artifact_gate: render_status
          expected_format: video-manifest
          output_directory: output/rework-short-form-teaser
    - id: content-demo-006
      identifier: content-demo-006
      number: 6
      title: Publish package handoff
      description: Package the reviewed article, captions, and newsletter assets for publication.
      state: Package
      assigned_to_worker: false
      labels:
        - content-demo
        - package
      priority: 2
      fields:
        artifact_type: publication_bundle
        audience: publishing
        channel: multi-channel
        render_status: approved
        owner: production
      metadata:
        workflow_step: Package
        source_assets: output/package-approved-newsletter/manifest.json
        expected_manifest: output/publish-package-handoff/manifest.json
      deliverable:
        kind: artifact
        path: output/publish-package-handoff/manifest.json
        review_url: http://127.0.0.1:4101/review/content-demo-006
        external_id: content-demo-006
        metadata:
          artifact_gate: render_status
          expected_format: directory-manifest
          output_directory: output/publish-package-handoff

workspace:
  kind: filesystem
  root: .detent/workspaces
  source_root: .
  output_root: output

deliverable:
  kind: artifact
  output_root: output
  review_url: http://127.0.0.1:4101/review

agent:
  auto_promote:
    enabled: true
    quiet_seconds: 0
    optout_label: requires-human-review
    source_state: Review
    pass_state: Package
    rework_state: Rework
    rework_limit: 3
    no_progress_limit: 0

gate:
  kind: artifact
  ci_failure_action: skip
  validator:
    enabled: false
  artifact:
    status_field: render_status
    pass_statuses:
      - approved
      - valid
    wait_statuses:
      - queued
      - rendering
      - pending_review
    rework_statuses:
      - recut
      - invalid
      - missing_assets

server:
  kanban:
    mode: integration
    allowed_transitions:
      Intake:
        - Research
        - Blocked
      Research:
        - Draft
        - Blocked
      Draft:
        - Review
        - Blocked
      Review:
        - Package
        - Rework
        - Blocked
      Rework:
        - Draft
        - Blocked
      Package:
        - Publish
        - Rework
        - Blocked
      Blocked:
        - Intake
        - Research
        - Draft
      Publish: []
      Cancelled: []
---
# Non-Code Artifact Demo

You are operating a local content-production workflow. The tracker is local
SQLite, the workspace is the filesystem, and the deliverable is an artifact
manifest under the configured output directory.

Do not require GitHub issues, pull requests, branches, CI, or a merge train.
Use each work item's title, description, fields, metadata, and deliverable
metadata as the production brief. Write or update the artifact manifest at the
work item deliverable path.

When an artifact is ready for review, set the configured `render_status` field
to `pending_review`. A human or external renderer can set `render_status` to
`approved` or `valid` to let Detent promote the card from Review to Package, or
set it to `recut`, `invalid`, or `missing_assets` to send it to Rework.
