---
name: unprivileged-windows-profile-links
description: Share files and directories in an isolated Windows profile without requiring symbolic-link privileges.
when_to_use: A profile or workspace mirroring change works in elevated CI but fails for ordinary Windows accounts when os.Symlink requires Developer Mode or elevation.
---

- Keep symbolic links on platforms that support them without extra privileges. On Windows, use hard links for same-volume files and directory junctions for directories when shared identity is required.
- Prefer native junction creation to introducing shell startup into a previously filesystem-only initialization path. Encode the mount-point reparse buffer with byte offsets and UTF-16 lengths, and use FSCTL_SET_REPARSE_POINT on a directory opened with OPEN_REPARSE_POINT and BACKUP_SEMANTICS.
- Close the native handle on every path and remove only the newly created empty junction directory if initialization fails.
- Hard links preserve file identity but do not follow atomic replacement of the source pathname. Refresh stale links during profile preparation and test that replacement explicitly.
- Test shared identity with os.SameFile rather than requiring ModeSymlink or Readlink. Include repeated preparation, file replacement, directory access, and paths containing spaces and non-ASCII characters. Run junction tests on Windows; cross-compilation alone does not verify runtime behavior.
- Filter reserved filenames using case-insensitive comparisons when exclusion must also hold on default macOS and Windows filesystems. Test the actual profile directory contents, including migration of older links.
