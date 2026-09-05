# Local visual review

Detent can attach an immutable visual evidence package to a pull request and
open it from the work item's **Human Review** sheet. Feedback, annotations, and
per-asset recommendations are stored in the runtime SQLite database; media is
stored beside it in `<runtime-db>.visual-review/` unless the server is given an
explicit visual-review media directory. Back up both paths together.

Import a package produced by the `workflow:visual-review` skill through the
running Detent service:

```sh
DETENT_API_TOKEN=... detent visual-review import \
  --project detent \
  --issue ISSUE_NODE_ID \
  --package /absolute/path/to/review-package
```

The API token needs write access to that project. Detent validates the v1
manifest, media signatures, dimensions and hashes, then copies media into an
app-owned immutable directory. Browser requests never provide server file
paths.

A review is writable only when its repository, PR number, and head SHA match
the current Detent snapshot and it is the newest imported capture. Older
captures remain available from the round history but are read-only, and a new
capture starts without inherited per-asset recommendations. Missing or changed
media also makes a capture read-only.

This milestone is intentionally node-local. Reviewer names and recommendations
are unverified assertions: they do not approve GitHub, dispatch rework, satisfy
a Detent merge gate, or merge a PR. Hub synchronization, R2/object storage,
retention automation, rework delivery, and gate enforcement are deferred.
