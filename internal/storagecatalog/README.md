# Raw source mapping catalog

`tools/storageinventory/mappings/v1.json` is the versioned column contract for
Hub, request-metrics, entry-registry and entry-auth source snapshots. It contains
all 305 tables and 2,671 columns in the P01 inventory. Each table keeps P01's
family, disposition and atomicity contract. The atomic groups in
`tools/storageinventory/routing.json` remain authoritative; a mapping never
permits splitting their transaction boundaries.

Run `go run ./tools/storageinventory` to check the routing and column contracts.
The catalog must match every source column's full snapshot definition, including
type, nullability, default, key position and ordinal. Unknown, missing, changed
and stale columns or tables fail closed. A different historical schema requires
its own reviewed version and exact definitions; current defaults are never
silently applied to legacy rows. Tests exercise explicitly different synthetic
legacy and current catalogs. Version 1 describes only the checked-in source
snapshots, not arbitrary historical databases or live staging schemas.

Target-only PostgreSQL objects have a separate typed contract in
`internal/storagecatalog/targets/v1.json`. It records family, tenant ownership
and every target column without inventing a SQLite source row. The Hub target
classification and storage routing inventory must cover the same target-only
tables. Existing PostgreSQL migrations are pinned by digest as a legacy boundary;
new migrations are enumerated and require explicit target contract coverage,
including schema-qualified table names. This scoped contract is not a complete
inventory of the pre-existing PostgreSQL schema, which remains a cutover prerequisite.

Every column declares its codec, SQL NULL policy, empty-value policy and one of
these dispositions:

| Disposition | Contract |
| --- | --- |
| `column` | An explicit existing PostgreSQL table, column and type. Preserve the source scalar exactly, within its declared bounds and enum domain. |
| `object` | Exact inline UTF-8 bytes to prepare and verify through the owning object store before publishing the listed metadata fields. JSON validation happens in Go without unmarshalling numbers into floating point. |
| `transform` | A named table-level transformation into the listed typed targets. The owning importer validates and transforms the source value; scalar decoding cannot perform this operation. |
| `pending` | Preserve the source and do not project this column. It remains pending until the owning family's target schema or relational extraction is mapped. Never invent a target, coerce to opaque SQL data or report this column as imported. |
| `rebuild` | Retain the table's stated source contract; reconstruct the named projection or percentile from retained inputs. This is not permission to discard durable history or rollups. |
| `retired` | Source engine/catalog bookkeeping only, according to P01. Retain snapshot provenance and recover identity allocation from imported keys. |

The catalog currently has 502 scalar mappings, eight inline payload mappings,
five named transforms, 1,964 pending columns, 101 rebuild columns and 91 retired columns. Pending
includes fields missing from otherwise landed families; family membership does
not prove complete target coverage. Nested identity/provenance, organization
selections, existing archive references, compressed payloads and other relational
extractions stay pending until their complete field and object contracts are
explicit. Nothing in this package declares migration or tenant-cutover readiness.

`Read` validates the catalog. `ValidateInventory` checks it against P01 and the
four checked-in schema snapshots. `Bind` additionally requires an exact catalog
version, the caller-supplied source snapshot schema definition and explicit source authority:

- Hub and metrics sources bind one named staging schema to one exact organization
  ID. Legacy tables without an organization column inherit only this binding.
  A row carrying a different organization fails.
- Registry/auth sources require an explicit set of organization IDs. Organization
  rows and mapped organization references must belong to that set. Global
  credential rows keep platform authority and global uniqueness; binding never
  rewrites, normalizes or generates an ID.

`ValidateRow` checks all columns of one row, organization authority and decodable
values. A pending or transformed column rejects this complete-row path.
`ValidateMappedRow` validates explicitly mapped scalar/object values and
organization authority; it skips pending and transformed columns so an owning
importer can retain source-only fields and execute its named transforms. That
importer must keep pending fields unprojected and report their status separately.
Retired/rebuild values have no ordinary scalar decoder. `Decode` returns a typed
value or unchanged payload bytes for one scalar/object column; it grants no write
authority and creates no object reference. A validated row still needs the
owning importer's target constraints, foreign-key checks, object verification,
uniqueness/reconciliation and atomic transaction.

Non-NULL cells name their encoding explicitly. Text uses `utf8`; binary data
uses `binary`, `hex`, canonical padded `base64`, or SQLite `X'hex'` with
`sqlite-hex`. Encoding is never guessed. Ciphertext is copied without decryption
or re-encryption. Empty binary data remains distinct from SQL NULL. Text rejects
invalid UTF-8 and NUL. Integers parse directly to signed 64-bit values without a
floating-point intermediate; booleans accept only `0` and `1`. Real values must
be finite decimal numbers. Timestamps require RFC3339 with an offset, normalize
to UTC and reject nonzero precision finer than PostgreSQL microseconds, including
digits beyond Go's nanosecond parser. Integer epoch seconds use a separate codec;
integer nanosecond fields stay integers. Histograms require exactly 64 nonnegative
integer bins with no NULL entries or sum overflow. Empty timestamps become NULL
only where explicitly cataloged. Errors expose neither values nor tenant IDs.

PostgreSQL tests compare every declared target to the migrated schema and use
isolated synthetic text/bytea sources. No production system or credential is
used (INV-22). There are no database schema changes or SQL JSON operations
(INV-20). No tracker knowledge is copied into this catalog (INV-16). It is a
normative source contract, not an evidence log. No request/background writes,
transactions or shared locks are added (INV-18). Binding work is bounded by its
explicit schema and organization manifest; row validation is linear in that
row's column/byte count, with constant-time organization membership checks,
independent of the 10,000-tenant/runner fleet (INV-21).
