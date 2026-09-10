---
name: eager-file-identity-snapshots
description: Capture stable file identity when testing replacement across POSIX and Windows.
when_to_use: Use when a Go replacement test compares os.FileInfo values across a rename and passes on POSIX but reports the same file on Windows.
---

- Inspect the active Go toolchain's Windows `os` implementation before treating
  `os.FileInfo` from a pathname as a complete identity snapshot. Path-based
  `os.Stat` can defer loading the volume and file index until `os.SameFile`.
  Comparing after replacement can therefore resolve both snapshots to the new
  destination, even though replacement succeeded.
- Open the original file, call `file.Stat()`, and close the handle before the
  mutation. Handle-based stat captures the identity eagerly; closing it avoids
  imposing Windows open-handle replacement restrictions on the writer.
- After replacement, stat the destination and compare identities. Also verify
  the published content; identity alone does not establish content correctness.
- Use this deterministic snapshot ordering instead of timestamp sleeps or
  scheduler-sensitive reader/writer loops. Confirm the fixture rejects an
  in-place rewrite, and run it on Windows as well as a POSIX platform.
