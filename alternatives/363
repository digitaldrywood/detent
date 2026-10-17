# Native issue archive

Archive keeps an issue and its comments, attempts, changes, versions, and audit
history. It does not change workflow state or delete data. Closed and completed
issues still consume an issue slot until explicitly archived.

Use **Archive issue** on native issue detail. Finish or stop active work first;
a live lease (including an active attempt) refuses archive. Attempts whose
lease expired or was released are already interrupted by the existing lifecycle
and do not block archive. Use **Archived** on the Work
board or list to find archived issues, then **Restore issue** on detail. Archive
and restore require project write access and an operator principal. Archived
issues remain readable through the normal issue, comment, attempt, change,
history, and version API resources for export.

Hosted plans count `unarchived_issues` for native projects across the organization.
Existing catalogs that omit this allowance default to 200. An explicit allowance
(including zero) wins; new catalog versions can change it. Native creation,
conversation issue promotion, imports, and restore share the existing atomic
hosted entitlement check. A request that increases the count above the allowance
returns HTTP 429 with resource, consumption, and allowance. Retries and import
refreshes that do not allocate an issue do not increase this count. A downgrade
does not remove history or force archives; reads, export, and archive remain
available. Other ordinary hosted allowances still apply to growing operations.
Local and self-hosted instances have no hosted quota.

API actions: `POST /api/v2/organizations/:organization/projects/:project/work-items/:item/archive`
and the corresponding `/restore`, with `idempotency_key` and `expected_revision`.
Lists default to unarchived issues. `?archived=true` lists archived issues and
`?archived=all` includes both; other list filters and pagination apply normally.
An archived unfinished issue remains an unfinished dependency; restore it to
resume execution or change its workflow state explicitly.
