# Dashboard capability matrix

Generated from [matrix.json](../internal/operatortool/capability/matrix.json). See [the inventory contract](mcp-capabilities.md) for updates and final parity validation. Pending rows are parity work, not exclusions.

## cloudentry.change_platform_entitlement

Change platform entitlement

- Audience: staff; status: **excluded**; owner: digitaldrywood/detent#3345.
- Decision: Platform/instance staff authority is stricter than organization operator authority. Preserve this restriction; no organization-operator tool grants staff powers.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role shared-entry staff allowlist; entitlement changes additionally require configured entitlement administrator; credential staff session or dedicated private instance-admin credential; not an organization operator credential; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.changePlatformEntitlement; s.config.now, s.csrfValid, s.machineCall, s.registry.recordEntitlementChange, s.tenantEntitlementFailure, s.tenantEntitlements
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / shared entry account/organization service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_dedicated / github,native / shared entry account/organization service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / shared entry account/organization service
- Availability: credential_maintenance / github,native / shared entry account/organization service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: this non-operator source site → not_applicable

Sources: [POST /api/cloud/platform/organizations/:organization/entitlements](../internal/cloudentry/service.go#L218), [web/conversation/src/app/entry/ComplimentaryPlans.tsx:182](../web/conversation/src/app/entry/ComplimentaryPlans.tsx#L182), [web/conversation/src/app/entry/ComplimentaryPlans.tsx:299](../web/conversation/src/app/entry/ComplimentaryPlans.tsx#L299), [web/conversation/src/app/entry/ComplimentaryPlans.tsx:182](../web/conversation/src/app/entry/ComplimentaryPlans.tsx#L182), [web/conversation/src/app/entry/ComplimentaryPlans.tsx:299](../web/conversation/src/app/entry/ComplimentaryPlans.tsx#L299), [web/conversation/src/app/entry/ComplimentaryPlans.tsx:155](../web/conversation/src/app/entry/ComplimentaryPlans.tsx#L155), [web/conversation/src/app/entry/ComplimentaryPlans.tsx:277](../web/conversation/src/app/entry/ComplimentaryPlans.tsx#L277), [web/conversation/src/app/entry/api.ts:259](../web/conversation/src/app/entry/api.ts#L259)
## cloudentry.chooser

Chooser

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3344.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `organization.chooser` — Bounded chooserRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → chooserResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role current account identity and organization membership; credential hosted account session; CSRF for mutation; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.organizationsJSON; s.canCreate, s.organizationChoices, s.pendingOrganizations, s.platformStaff
- Extraction: Extract cloudentry.organizationsJSON application inputs/results and validation from Echo; reuse s.canCreate, s.organizationChoices, s.pendingOrganizations, s.platformStaff. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / shared entry account/organization service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_dedicated / github,native / shared entry account/organization service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / shared entry account/organization service
- Availability: credential_maintenance / github,native / shared entry account/organization service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/cloud/organizations](../internal/cloudentry/service.go#L209), [GET /organizations](../internal/cloudentry/service.go#L201), [web/conversation/src/app/entry/api.ts:244](../web/conversation/src/app/entry/api.ts#L244), [web/conversation/src/app/entry/api.ts:247](../web/conversation/src/app/entry/api.ts#L247), [web/conversation/src/app/account/Organization.tsx:517](../web/conversation/src/app/account/Organization.tsx#L517), [web/conversation/src/app/entry/EntryScreens.tsx:181](../web/conversation/src/app/entry/EntryScreens.tsx#L181)
## cloudentry.complete_login

Complete login

- Audience: authentication; status: **excluded**; owner: digitaldrywood/detent#3336.
- Decision: Identity-provider login exchange belongs to connection setup, not model-controlled tool arguments. Meaningful organization/session commands are separate rows.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role public identity exchange/asset; credential none until connection authentication; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.completeLogin; s.acceptInvitation, s.auth.authorize, s.auth.consumeTransaction, s.completeSupport, s.config.Provider.Exchange, s.config.now, s.landing, s.loginDenied, s.mutationMu.Lock, s.mutationMu.Unlock, s.organizationHome, s.readyOrganization, s.revokeAtTenants, s.staff
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / shared entry account/organization service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_dedicated / github,native / shared entry account/organization service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / shared entry account/organization service
- Availability: credential_maintenance / github,native / shared entry account/organization service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: connection authentication → connection

Sources: [GET /auth/oidc/callback](../internal/cloudentry/service.go#L200)
## cloudentry.create_organization

Create organization

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3344.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `organization.create_organization` — Bounded createOrganizationRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → createOrganizationResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role current account identity and organization membership; credential hosted account session; CSRF for mutation; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.newOrganizationPage; s.clientShell, s.denied, s.platformStaff, s.render
- Extraction: Extract cloudentry.newOrganizationPage application inputs/results and validation from Echo; reuse s.clientShell, s.denied, s.platformStaff, s.render. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=true; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / shared entry account/organization service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_dedicated / github,native / shared entry account/organization service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / shared entry account/organization service
- Availability: credential_maintenance / github,native / shared entry account/organization service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: create an organization with external identity or billing effects → operator

Sources: [GET /organizations/new](../internal/cloudentry/service.go#L202), [POST /organizations](../internal/cloudentry/service.go#L203), [internal/web/templates/hosted.templ:393](../internal/web/templates/hosted.templ#L393), [internal/web/templates/hosted.templ:428](../internal/web/templates/hosted.templ#L428), [internal/web/templates/hosted.templ:415](../internal/web/templates/hosted.templ#L415), [internal/web/templates/hosted.templ:400](../internal/web/templates/hosted.templ#L400), [internal/web/templates/hosted.templ:420](../internal/web/templates/hosted.templ#L420), [internal/web/templates/hosted.templ:447](../internal/web/templates/hosted.templ#L447), [internal/web/templates/hosted.templ:382](../internal/web/templates/hosted.templ#L382), [internal/web/templates/hosted.templ:139](../internal/web/templates/hosted.templ#L139), [internal/web/templates/hosted.templ:362](../internal/web/templates/hosted.templ#L362), [internal/web/templates/hosted.templ:372](../internal/web/templates/hosted.templ#L372), [internal/web/templates/hosted.templ:393](../internal/web/templates/hosted.templ#L393), [internal/web/templates/hosted.templ:428](../internal/web/templates/hosted.templ#L428), [internal/web/templates/hosted.templ:415](../internal/web/templates/hosted.templ#L415), [web/conversation/src/app/entry/api.ts:249](../web/conversation/src/app/entry/api.ts#L249), [web/conversation/src/app/entry/api.ts:251](../web/conversation/src/app/entry/api.ts#L251), [web/conversation/src/app/entry/EntryScreens.tsx:249](../web/conversation/src/app/entry/EntryScreens.tsx#L249), [web/conversation/src/app/entry/EntryScreens.tsx:247](../web/conversation/src/app/entry/EntryScreens.tsx#L247)
## cloudentry.delete_organization

Delete organization

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3344.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `organization.delete_organization` — Bounded deleteOrganizationRequest: organization; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → deleteOrganizationResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role current account identity and organization membership; credential hosted account session; CSRF for mutation; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.deleteOrganizationPage; s.denied, s.ownerOrganization, s.render
- Extraction: Extract cloudentry.deleteOrganizationPage application inputs/results and validation from Echo; reuse s.denied, s.ownerOrganization, s.render. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=false; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / shared entry account/organization service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_dedicated / github,native / shared entry account/organization service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / shared entry account/organization service
- Availability: credential_maintenance / github,native / shared entry account/organization service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /organizations/:organization/delete](../internal/cloudentry/service.go#L207), [POST /organizations/:organization/delete](../internal/cloudentry/service.go#L208)
## cloudentry.health

Health

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3340.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `work_reads.health` — Bounded healthRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → healthResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role current account identity and organization membership; credential hosted account session; CSRF for mutation; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: func(c echo.Context) error { return c.JSON(http.StatusOK, map[string]string{"status": "ok"}) }; handler-owned application validation/read/command
- Extraction: Extract cloudentry.health application inputs/results and validation from Echo; reuse the current handler-owned service logic. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / shared entry account/organization service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_dedicated / github,native / shared entry account/organization service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / shared entry account/organization service
- Availability: credential_maintenance / github,native / shared entry account/organization service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /health](../internal/cloudentry/service.go#L197)
## cloudentry.home

Home

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3340.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `work_reads.home` — Bounded homeRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → homeResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role current account identity and organization membership; credential hosted account session; CSRF for mutation; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.home; s.clientShell, s.landing, s.render
- Extraction: Extract cloudentry.home application inputs/results and validation from Echo; reuse s.clientShell, s.landing, s.render. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / shared entry account/organization service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_dedicated / github,native / shared entry account/organization service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / shared entry account/organization service
- Availability: credential_maintenance / github,native / shared entry account/organization service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /](../internal/cloudentry/service.go#L198)
## cloudentry.join_invitation

Join invitation

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3340.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `work_reads.join_invitation` — Bounded joinInvitationRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → joinInvitationResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role current account identity and organization membership; credential hosted account session; CSRF for mutation; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.joinPage; s.clientShell, s.platformStaff, s.render
- Extraction: Extract cloudentry.joinPage application inputs/results and validation from Echo; reuse s.clientShell, s.platformStaff, s.render. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=false; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / shared entry account/organization service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_dedicated / github,native / shared entry account/organization service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / shared entry account/organization service
- Availability: credential_maintenance / github,native / shared entry account/organization service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /invitations/join](../internal/cloudentry/service.go#L223), [POST /invitations/join](../internal/cloudentry/service.go#L224), [internal/web/templates/hosted.templ:441](../internal/web/templates/hosted.templ#L441), [internal/web/templates/hosted.templ:384](../internal/web/templates/hosted.templ#L384), [internal/web/templates/hosted.templ:441](../internal/web/templates/hosted.templ#L441), [web/conversation/src/app/entry/api.ts:253](../web/conversation/src/app/entry/api.ts#L253), [web/conversation/src/app/entry/EntryScreens.tsx:397](../web/conversation/src/app/entry/EntryScreens.tsx#L397), [web/conversation/src/app/entry/EntryScreens.tsx:395](../web/conversation/src/app/entry/EntryScreens.tsx#L395)
## cloudentry.logout

Logout

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3336.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `connection.logout` — Bounded logoutRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → logoutResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role current account identity and organization membership; credential hosted account session; CSRF for mutation; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.logout; s.csrfValid, s.loginDenied, s.render, s.revokeAtTenants
- Extraction: Extract cloudentry.logout application inputs/results and validation from Echo; reuse s.csrfValid, s.loginDenied, s.render, s.revokeAtTenants. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=false; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / shared entry account/organization service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_dedicated / github,native / shared entry account/organization service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / shared entry account/organization service
- Availability: credential_maintenance / github,native / shared entry account/organization service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [POST /logout](../internal/cloudentry/service.go#L211), [POST /organizations/:organization/logout](../internal/cloudentry/service.go#L212)
## cloudentry.platform_allowlist_json

Platform allowlist j s o n

- Audience: staff; status: **excluded**; owner: digitaldrywood/detent#3344.
- Decision: Platform/instance staff authority is stricter than organization operator authority. Preserve this restriction; no organization-operator tool grants staff powers.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role shared-entry staff allowlist; entitlement changes additionally require configured entitlement administrator; credential staff session or dedicated private instance-admin credential; not an organization operator credential; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.platformAllowlistJSON; handler-owned application validation/read/command
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / shared entry account/organization service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_dedicated / github,native / shared entry account/organization service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / shared entry account/organization service
- Availability: credential_maintenance / github,native / shared entry account/organization service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: this non-operator source site → not_applicable

Sources: [GET /api/cloud/platform/allowlist](../internal/cloudentry/service.go#L215), [web/conversation/src/app/entry/api.ts:255](../web/conversation/src/app/entry/api.ts#L255)
## cloudentry.platform_entitlements_json

Platform entitlements j s o n

- Audience: staff; status: **excluded**; owner: digitaldrywood/detent#3345.
- Decision: Platform/instance staff authority is stricter than organization operator authority. Preserve this restriction; no organization-operator tool grants staff powers.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role shared-entry staff allowlist; entitlement changes additionally require configured entitlement administrator; credential staff session or dedicated private instance-admin credential; not an organization operator credential; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.platformEntitlementsJSON; s.config.now, s.tenantEntitlementFailure, s.tenantEntitlements
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / shared entry account/organization service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_dedicated / github,native / shared entry account/organization service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / shared entry account/organization service
- Availability: credential_maintenance / github,native / shared entry account/organization service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: this non-operator source site → not_applicable

Sources: [GET /api/cloud/platform/organizations/:organization/entitlements](../internal/cloudentry/service.go#L217), [web/conversation/src/app/entry/api.ts:257](../web/conversation/src/app/entry/api.ts#L257)
## cloudentry.platform_health_json

Platform health j s o n

- Audience: staff; status: **excluded**; owner: digitaldrywood/detent#3344.
- Decision: Platform/instance staff authority is stricter than organization operator authority. Preserve this restriction; no organization-operator tool grants staff powers.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role shared-entry staff allowlist; entitlement changes additionally require configured entitlement administrator; credential staff session or dedicated private instance-admin credential; not an organization operator credential; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.platformHealthJSON; s.admissionLoad, s.eachReadyTenant, s.registry.List, s.serviceRequest
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / shared entry account/organization service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_dedicated / github,native / shared entry account/organization service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / shared entry account/organization service
- Availability: credential_maintenance / github,native / shared entry account/organization service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: this non-operator source site → not_applicable

Sources: [GET /api/cloud/platform/health](../internal/cloudentry/service.go#L216), [web/conversation/src/app/entry/api.ts:256](../web/conversation/src/app/entry/api.ts#L256)
## cloudentry.platform_organizations_json

Platform organizations j s o n

- Audience: staff; status: **excluded**; owner: digitaldrywood/detent#3344.
- Decision: Platform/instance staff authority is stricter than organization operator authority. Preserve this restriction; no organization-operator tool grants staff powers.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role shared-entry staff allowlist; entitlement changes additionally require configured entitlement administrator; credential staff session or dedicated private instance-admin credential; not an organization operator credential; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.platformOrganizationsJSON; s.eachReadyTenant, s.entitlementAdministrator, s.platformOrganizations, s.supportActor, s.tenantBillingState
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Current handler authority checks: s.entitlementAdministrator(session),
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / shared entry account/organization service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_dedicated / github,native / shared entry account/organization service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / shared entry account/organization service
- Availability: credential_maintenance / github,native / shared entry account/organization service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: this non-operator source site → not_applicable

Sources: [GET /api/cloud/platform/organizations](../internal/cloudentry/service.go#L214), [web/conversation/src/app/entry/api.ts:254](../web/conversation/src/app/entry/api.ts#L254)
## cloudentry.platform_page

Platform page

- Audience: staff; status: **excluded**; owner: digitaldrywood/detent#3344.
- Decision: Platform/instance staff authority is stricter than organization operator authority. Preserve this restriction; no organization-operator tool grants staff powers.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role shared-entry staff allowlist; entitlement changes additionally require configured entitlement administrator; credential staff session or dedicated private instance-admin credential; not an organization operator credential; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.platformPage; s.clientShell, s.denied, s.platformStaff
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / shared entry account/organization service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_dedicated / github,native / shared entry account/organization service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / shared entry account/organization service
- Availability: credential_maintenance / github,native / shared entry account/organization service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: this non-operator source site → not_applicable

Sources: [GET /platform](../internal/cloudentry/service.go#L213)
## cloudentry.provisioning_page

Provisioning page

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3340.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `work_reads.provisioning_page` — Bounded provisioningPageRequest: organization; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → provisioningPageResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role current account identity and organization membership; credential hosted account session; CSRF for mutation; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.provisioningJSON; s.creatorOrganization, s.organizationHome, s.retryLimit
- Extraction: Extract cloudentry.provisioningJSON application inputs/results and validation from Echo; reuse s.creatorOrganization, s.organizationHome, s.retryLimit. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / shared entry account/organization service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_dedicated / github,native / shared entry account/organization service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / shared entry account/organization service
- Availability: credential_maintenance / github,native / shared entry account/organization service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/cloud/organizations/:organization/provisioning](../internal/cloudentry/service.go#L206), [GET /organizations/:organization/provisioning](../internal/cloudentry/service.go#L204)
## cloudentry.proxy

Proxy

- Audience: transport; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Exact transport/protocol plumbing site. Application payload operations are inventoried separately; do not expose an HTTP or relay proxy tool.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role current account identity and organization membership; credential hosted account session; CSRF for mutation; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.proxy; s.browserClaims, s.browserDenied, s.claims, s.config.transport, s.logProxied, s.readyOrganization, s.registry.Organization
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / shared entry account/organization service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_dedicated / github,native / shared entry account/organization service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / shared entry account/organization service
- Availability: credential_maintenance / github,native / shared entry account/organization service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: this non-operator source site → not_applicable

Sources: [Any /api/v2/organizations/:organization/*](../internal/cloudentry/service.go#L227), [Any /organizations/:organization](../internal/cloudentry/service.go#L225), [Any /organizations/:organization/*](../internal/cloudentry/service.go#L226)
## cloudentry.resume_provisioning

Resume provisioning

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3340.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `work_reads.resume_provisioning` — Bounded resumeProvisioningRequest: organization; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → resumeProvisioningResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role current account identity and organization membership; credential hosted account session; CSRF for mutation; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.resumeProvisioning; s.config.now, s.creatorOrganization, s.csrfValid, s.next, s.refuse, s.registry.store.db.ExecContext, s.wakeAllocator
- Extraction: Extract cloudentry.resumeProvisioning application inputs/results and validation from Echo; reuse s.config.now, s.creatorOrganization, s.csrfValid, s.next, s.refuse, s.registry.store.db.ExecContext, s.wakeAllocator. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=false; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / shared entry account/organization service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_dedicated / github,native / shared entry account/organization service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / shared entry account/organization service
- Availability: credential_maintenance / github,native / shared entry account/organization service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [POST /organizations/:organization/provisioning/resume](../internal/cloudentry/service.go#L205)
## cloudentry.session_json

Session j s o n

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3336.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `connection.session_json` — Bounded sessionJSONRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → sessionJSONResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role current account identity and organization membership; credential hosted account session; CSRF for mutation; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.sessionJSON; s.canCreate, s.platformStaff
- Extraction: Extract cloudentry.sessionJSON application inputs/results and validation from Echo; reuse s.canCreate, s.platformStaff. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / shared entry account/organization service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_dedicated / github,native / shared entry account/organization service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / shared entry account/organization service
- Availability: credential_maintenance / github,native / shared entry account/organization service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/cloud/session](../internal/cloudentry/service.go#L210), [web/conversation/src/app/entry/api.ts:245](../web/conversation/src/app/entry/api.ts#L245)
## cloudentry.start_invitation

Start invitation

- Audience: authentication; status: **excluded**; owner: digitaldrywood/detent#3336.
- Decision: Identity-provider login exchange belongs to connection setup, not model-controlled tool arguments. Meaningful organization/session commands are separate rows.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role hosted owner/admin; owner-only restrictions for owner membership/role changes; invitation acceptance bound to invited identity; credential none until connection authentication; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.startInvitation; s.beginLogin, s.invitationOrganization, s.loginDenied
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / shared entry account/organization service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_dedicated / github,native / shared entry account/organization service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / shared entry account/organization service
- Availability: credential_maintenance / github,native / shared entry account/organization service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: connection authentication → connection

Sources: [GET /invite](../internal/cloudentry/service.go#L222)
## cloudentry.start_login

Start login

- Audience: authentication; status: **excluded**; owner: digitaldrywood/detent#3336.
- Decision: Identity-provider login exchange belongs to connection setup, not model-controlled tool arguments. Meaningful organization/session commands are separate rows.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role public identity exchange/asset; credential none until connection authentication; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.startLogin; s.beginLogin, s.loginDenied, s.readyOrganization
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / shared entry account/organization service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_dedicated / github,native / shared entry account/organization service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / shared entry account/organization service
- Availability: credential_maintenance / github,native / shared entry account/organization service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: connection authentication → connection

Sources: [GET /auth/oidc/start](../internal/cloudentry/service.go#L199)
## cloudentry.start_support

Start support

- Audience: staff; status: **excluded**; owner: digitaldrywood/detent#3344.
- Decision: Platform/instance staff authority is stricter than organization operator authority. Preserve this restriction; no organization-operator tool grants staff powers.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role platform staff/support allowlist or entitlement administrator; credential-maintenance instance admin if applicable; credential staff session or dedicated private instance-admin credential; not an organization operator credential; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.startSupport; s.auth.createTransaction, s.config.generateToken, s.config.now, s.csrfValid, s.loginDenied, s.readyOrganization, s.render, s.supportActor
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / shared entry account/organization service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_dedicated / github,native / shared entry account/organization service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / shared entry account/organization service
- Availability: credential_maintenance / github,native / shared entry account/organization service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: this non-operator source site → not_applicable

Sources: [POST /support/start](../internal/cloudentry/service.go#L220)
## cloudentry.static_assets

Static assets

- Audience: asset; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Static application assets carry no application operation; no MCP asset-serving tool.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role public identity exchange/asset; credential none until connection authentication; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: echo.WrapHandler(http.StripPrefix("/static/", http.FileServerFS(detent.StaticFS()))); handler-owned application validation/read/command
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / shared entry account/organization service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_dedicated / github,native / shared entry account/organization service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / shared entry account/organization service
- Availability: credential_maintenance / github,native / shared entry account/organization service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: this non-operator source site → not_applicable

Sources: [GET /static/*](../internal/cloudentry/service.go#L196)
## cloudentry.stripe_webhook

Stripe webhook

- Audience: transport; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Exact transport/protocol plumbing site. Application payload operations are inventoried separately; do not expose an HTTP or relay proxy tool.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role current account identity and organization membership; credential hosted account session; CSRF for mutation; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.stripeWebhook; s.config.now, s.deliverBillingEvent, s.registry.store.db.ExecContext
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / shared entry account/organization service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_dedicated / github,native / shared entry account/organization service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / shared entry account/organization service
- Availability: credential_maintenance / github,native / shared entry account/organization service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: this non-operator source site → not_applicable

Sources: [POST /webhooks/stripe/:mode](../internal/cloudentry/service.go#L221)
## cloudentry.support_page

Support page

- Audience: staff; status: **excluded**; owner: digitaldrywood/detent#3344.
- Decision: Platform/instance staff authority is stricter than organization operator authority. Preserve this restriction; no organization-operator tool grants staff powers.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role platform staff/support allowlist or entitlement administrator; credential-maintenance instance admin if applicable; credential staff session or dedicated private instance-admin credential; not an organization operator credential; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.supportPage; s.loginDenied, s.registry.List, s.render, s.supportActor
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / shared entry account/organization service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_dedicated / github,native / shared entry account/organization service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / shared entry account/organization service
- Availability: credential_maintenance / github,native / shared entry account/organization service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: this non-operator source site → not_applicable

Sources: [GET /support](../internal/cloudentry/service.go#L219)
## existing.board_state

Existing board_state read tool

- Audience: operator; status: **implemented**; owner: digitaldrywood/detent#3340.
- Decision: Preserve existing name/schema and bounded read behavior. Authority extension is #3336; expanded dashboard coverage stays pending in separate rows.
- Tool: `existing_reads.board_state` — definition(BoardState, "Read live board items, lanes, priorities, blockers, and active run identity. Use this before answering board questions or proposing item actions.", limitedSchema) → board_state existing typed executor result with freshness
- Authority: role authenticated operator; credential current connection authority; project resource project read/write grant where scoped; ownership current organization; resolve identifiers within the authorized project.
- Application: operatortool.Executor.Execute / board_state
- Extraction: None; retain existing definition, argument schema, executor and result schema.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: internal/operatortool/executor_test.go; internal/mcp/server_test.go and http_test.go; current existing read coverage only, not full organization parity
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / telemetry snapshot or explanation service; absent dependency returns existing safe unavailable error
- Availability: hosted_dedicated / github,native / telemetry snapshot or explanation service; absent dependency returns existing safe unavailable error
- Availability: hosted_shared / github,native / telemetry snapshot or explanation service; absent dependency returns existing safe unavailable error
- Confirmation: read or ordinary non-destructive write → none

Sources: [internal/operatortool/catalog.go:30](../internal/operatortool/catalog.go#L30)
## existing.explain_item

Existing explain_item read tool

- Audience: operator; status: **implemented**; owner: digitaldrywood/detent#3340.
- Decision: Preserve existing name/schema and bounded read behavior. Authority extension is #3336; expanded dashboard coverage stays pending in separate rows.
- Tool: `existing_reads.explain_item` — definition(ExplainItem, "Explain an issue's current lane, latest transition reason, eligibility, active or latest attempt, sessions, pull request, required gate, freshness, and evidence from the versioned issue explanation read model.", `{"type":"object","required":["project_id","reference"],"properties":{"project_id":{"type":"string","minLength":1},"reference":{"type":"string","minLength":1}},"additionalProperties":false}`) → explain_item existing typed executor result with freshness
- Authority: role authenticated operator; credential current connection authority; project resource project read/write grant where scoped; ownership current organization; resolve identifiers within the authorized project.
- Application: operatortool.Executor.Execute / explain_item
- Extraction: None; retain existing definition, argument schema, executor and result schema.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: internal/operatortool/executor_test.go; internal/mcp/server_test.go and http_test.go; current existing read coverage only, not full organization parity
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / telemetry snapshot or explanation service; absent dependency returns existing safe unavailable error
- Availability: hosted_dedicated / github,native / telemetry snapshot or explanation service; absent dependency returns existing safe unavailable error
- Availability: hosted_shared / github,native / telemetry snapshot or explanation service; absent dependency returns existing safe unavailable error
- Confirmation: read or ordinary non-destructive write → none

Sources: [internal/operatortool/catalog.go:34](../internal/operatortool/catalog.go#L34)
## existing.fleet_health

Existing fleet_health read tool

- Audience: operator; status: **implemented**; owner: digitaldrywood/detent#3343.
- Decision: Preserve existing name/schema and bounded read behavior. Authority extension is #3336; expanded dashboard coverage stays pending in separate rows.
- Tool: `existing_reads.fleet_health` — definition(FleetHealth, "Read live fleet health, capacity outages, failure breakers, rate limits, refresh state, and running counts.", `{"type":"object","properties":{},"additionalProperties":false}`) → fleet_health existing typed executor result with freshness
- Authority: role authenticated operator; credential current connection authority; project resource project read/write grant where scoped; ownership current organization; resolve identifiers within the authorized project.
- Application: operatortool.Executor.Execute / fleet_health
- Extraction: None; retain existing definition, argument schema, executor and result schema.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: internal/operatortool/executor_test.go; internal/mcp/server_test.go and http_test.go; current existing read coverage only, not full organization parity
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / telemetry snapshot or explanation service; absent dependency returns existing safe unavailable error
- Availability: hosted_dedicated / github,native / telemetry snapshot or explanation service; absent dependency returns existing safe unavailable error
- Availability: hosted_shared / github,native / telemetry snapshot or explanation service; absent dependency returns existing safe unavailable error
- Confirmation: read or ordinary non-destructive write → none

Sources: [internal/operatortool/catalog.go:31](../internal/operatortool/catalog.go#L31)
## existing.recent_activity

Existing recent_activity read tool

- Audience: operator; status: **implemented**; owner: digitaldrywood/detent#3340.
- Decision: Preserve existing name/schema and bounded read behavior. Authority extension is #3336; expanded dashboard coverage stays pending in separate rows.
- Tool: `existing_reads.recent_activity` — definition(RecentActivity, "Read recent events and completed work retained in the current live telemetry snapshot, including merge timestamps. This is live-only activity, not the durable issue activity stream.", activitySchema) → recent_activity existing typed executor result with freshness
- Authority: role authenticated operator; credential current connection authority; project resource project read/write grant where scoped; ownership current organization; resolve identifiers within the authorized project.
- Application: operatortool.Executor.Execute / recent_activity
- Extraction: None; retain existing definition, argument schema, executor and result schema.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: internal/operatortool/executor_test.go; internal/mcp/server_test.go and http_test.go; current existing read coverage only, not full organization parity
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / telemetry snapshot or explanation service; absent dependency returns existing safe unavailable error
- Availability: hosted_dedicated / github,native / telemetry snapshot or explanation service; absent dependency returns existing safe unavailable error
- Availability: hosted_shared / github,native / telemetry snapshot or explanation service; absent dependency returns existing safe unavailable error
- Confirmation: read or ordinary non-destructive write → none

Sources: [internal/operatortool/catalog.go:33](../internal/operatortool/catalog.go#L33)
## existing.telemetry_usage

Existing telemetry_usage read tool

- Audience: operator; status: **implemented**; owner: digitaldrywood/detent#3345.
- Decision: Preserve existing name/schema and bounded read behavior. Authority extension is #3336; expanded dashboard coverage stays pending in separate rows.
- Tool: `existing_reads.telemetry_usage` — definition(TelemetryUsage, "Read live token, spend, throughput, and per-project usage telemetry.", `{"type":"object","properties":{"project_id":{"type":"string"}},"additionalProperties":false}`) → telemetry_usage existing typed executor result with freshness
- Authority: role authenticated operator; credential current connection authority; project resource project read/write grant where scoped; ownership current organization; resolve identifiers within the authorized project.
- Application: operatortool.Executor.Execute / telemetry_usage
- Extraction: None; retain existing definition, argument schema, executor and result schema.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: internal/operatortool/executor_test.go; internal/mcp/server_test.go and http_test.go; current existing read coverage only, not full organization parity
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / telemetry snapshot or explanation service; absent dependency returns existing safe unavailable error
- Availability: hosted_dedicated / github,native / telemetry snapshot or explanation service; absent dependency returns existing safe unavailable error
- Availability: hosted_shared / github,native / telemetry snapshot or explanation service; absent dependency returns existing safe unavailable error
- Confirmation: read or ordinary non-destructive write → none

Sources: [internal/operatortool/catalog.go:32](../internal/operatortool/catalog.go#L32)
## frontend.internal_web_templates_ai_debug_templ.local_ui

ai_debug.templ local ui

- Audience: local_ui; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Exact local ui definitions below are local browser behavior without an application command.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated operator; credential current connection authority; project resource project read/write grant where scoped; ownership current organization; resolve identifiers within the authorized project.
- Application: internal/web/templates/ai_debug.templ; browser dialog/editor/local storage/rendering; no server mutation
- Extraction: None; exact client/protocol sites do not own an operator application command.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Availability: hosted_dedicated / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Availability: hosted_shared / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Confirmation: read or ordinary non-destructive write → none

Sources: [internal/web/templates/ai_debug.templ:35](../internal/web/templates/ai_debug.templ#L35), [internal/web/templates/ai_debug.templ:58](../internal/web/templates/ai_debug.templ#L58)
## frontend.internal_web_templates_api_keys_templ.local_ui

api_keys.templ local ui

- Audience: local_ui; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Exact local ui definitions below are local browser behavior without an application command.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated operator; credential current connection authority; project resource project read/write grant where scoped; ownership current organization; resolve identifiers within the authorized project.
- Application: internal/web/templates/api_keys.templ; browser dialog/editor/local storage/rendering; no server mutation
- Extraction: None; exact client/protocol sites do not own an operator application command.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Availability: hosted_dedicated / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Availability: hosted_shared / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Confirmation: read or ordinary non-destructive write → none

Sources: [internal/web/templates/api_keys.templ:109](../internal/web/templates/api_keys.templ#L109)
## frontend.internal_web_templates_auth_templ.assets

auth.templ assets

- Audience: asset; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Exact assets definitions below are client/transport/identity plumbing; application requests and meaningful results have separate decisions.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated operator; credential current connection authority; project resource project read/write grant where scoped; ownership current organization; resolve identifiers within the authorized project.
- Application: internal/web/templates/auth.templ; serve static asset
- Extraction: None; exact client/protocol sites do not own an operator application command.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Availability: hosted_dedicated / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Availability: hosted_shared / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Confirmation: read or ordinary non-destructive write → none

Sources: [internal/web/templates/auth.templ:13](../internal/web/templates/auth.templ#L13), [internal/web/templates/auth.templ:14](../internal/web/templates/auth.templ#L14), [internal/web/templates/auth.templ:12](../internal/web/templates/auth.templ#L12), [internal/web/templates/auth.templ:15](../internal/web/templates/auth.templ#L15)
## frontend.internal_web_templates_change_files_templ.assets

change_files.templ assets

- Audience: asset; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Exact assets definitions below are client/transport/identity plumbing; application requests and meaningful results have separate decisions.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated operator; credential current connection authority; project resource project read/write grant where scoped; ownership current organization; resolve identifiers within the authorized project.
- Application: internal/web/templates/change_files.templ; serve static asset
- Extraction: None; exact client/protocol sites do not own an operator application command.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Availability: hosted_dedicated / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Availability: hosted_shared / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Confirmation: read or ordinary non-destructive write → none

Sources: [internal/web/templates/change_files.templ:8](../internal/web/templates/change_files.templ#L8)
## frontend.internal_web_templates_change_files_templ.navigation_results

change_files.templ navigation results

- Audience: local_ui; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Exact presentation links below render supplied external/documentation, fragment, or application URLs; they do not read or mutate application state. Meaningful server reads producing resource URLs are inventoried separately.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated operator; credential current connection authority; project resource project read/write grant where scoped; ownership current organization; resolve identifiers within the authorized project.
- Application: internal/web/templates/change_files.templ; return existing navigation targets/URLs from the authorized application read instead of requiring browser scraping
- Extraction: None for link presentation; producing application reads retain typed identifiers/URLs.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / shared application read/command for this frontend source
- Availability: hosted_dedicated / github,native / shared application read/command for this frontend source
- Availability: hosted_shared / github,native / shared application read/command for this frontend source
- Confirmation: read or ordinary non-destructive write → none

Sources: [internal/web/templates/change_files.templ:68](../internal/web/templates/change_files.templ#L68)
## frontend.internal_web_templates_chat_templ.local_ui

chat.templ local ui

- Audience: local_ui; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Exact local ui definitions below are local browser behavior without an application command.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated operator; credential current connection authority; project resource project read/write grant where scoped; ownership current organization; resolve identifiers within the authorized project.
- Application: internal/web/templates/chat.templ; browser dialog/editor/local storage/rendering; no server mutation
- Extraction: None; exact client/protocol sites do not own an operator application command.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Availability: hosted_dedicated / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Availability: hosted_shared / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Confirmation: read or ordinary non-destructive write → none

Sources: [internal/web/templates/chat.templ:80](../internal/web/templates/chat.templ#L80)
## frontend.internal_web_templates_dashboard_templ.assets

dashboard.templ assets

- Audience: asset; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Exact assets definitions below are client/transport/identity plumbing; application requests and meaningful results have separate decisions.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated operator; credential current connection authority; project resource project read/write grant where scoped; ownership current organization; resolve identifiers within the authorized project.
- Application: internal/web/templates/dashboard.templ; serve static asset
- Extraction: None; exact client/protocol sites do not own an operator application command.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Availability: hosted_dedicated / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Availability: hosted_shared / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Confirmation: read or ordinary non-destructive write → none

Sources: [internal/web/templates/dashboard.templ:116](../internal/web/templates/dashboard.templ#L116), [internal/web/templates/dashboard.templ:117](../internal/web/templates/dashboard.templ#L117)
## frontend.internal_web_templates_hosted_templ.assets

hosted.templ assets

- Audience: asset; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Exact assets definitions below are client/transport/identity plumbing; application requests and meaningful results have separate decisions.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated operator; credential current connection authority; project resource project read/write grant where scoped; ownership current organization; resolve identifiers within the authorized project.
- Application: internal/web/templates/hosted.templ; serve static asset
- Extraction: None; exact client/protocol sites do not own an operator application command.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Availability: hosted_dedicated / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Availability: hosted_shared / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Confirmation: read or ordinary non-destructive write → none

Sources: [internal/web/templates/hosted.templ:76](../internal/web/templates/hosted.templ#L76), [internal/web/templates/hosted.templ:77](../internal/web/templates/hosted.templ#L77), [internal/web/templates/hosted.templ:75](../internal/web/templates/hosted.templ#L75), [internal/web/templates/hosted.templ:78](../internal/web/templates/hosted.templ#L78)
## frontend.internal_web_templates_onboarding_templ.assets

onboarding.templ assets

- Audience: asset; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Exact assets definitions below are client/transport/identity plumbing; application requests and meaningful results have separate decisions.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated operator; credential current connection authority; project resource project read/write grant where scoped; ownership current organization; resolve identifiers within the authorized project.
- Application: internal/web/templates/onboarding.templ; serve static asset
- Extraction: None; exact client/protocol sites do not own an operator application command.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Availability: hosted_dedicated / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Availability: hosted_shared / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Confirmation: read or ordinary non-destructive write → none

Sources: [internal/web/templates/onboarding.templ:173](../internal/web/templates/onboarding.templ#L173), [internal/web/templates/onboarding.templ:174](../internal/web/templates/onboarding.templ#L174)
## frontend.internal_web_templates_shell_templ.assets

shell.templ assets

- Audience: asset; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Exact assets definitions below are client/transport/identity plumbing; application requests and meaningful results have separate decisions.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated operator; credential current connection authority; project resource project read/write grant where scoped; ownership current organization; resolve identifiers within the authorized project.
- Application: internal/web/templates/shell.templ; serve static asset
- Extraction: None; exact client/protocol sites do not own an operator application command.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Availability: hosted_dedicated / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Availability: hosted_shared / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Confirmation: read or ordinary non-destructive write → none

Sources: [internal/web/templates/shell.templ:36](../internal/web/templates/shell.templ#L36), [internal/web/templates/shell.templ:37](../internal/web/templates/shell.templ#L37), [internal/web/templates/shell.templ:35](../internal/web/templates/shell.templ#L35), [internal/web/templates/shell.templ:38](../internal/web/templates/shell.templ#L38)
## frontend.internal_web_ui_components_button_button_templ.navigation_results

button.templ navigation results

- Audience: local_ui; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Exact presentation links below render supplied external/documentation, fragment, or application URLs; they do not read or mutate application state. Meaningful server reads producing resource URLs are inventoried separately.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated operator; credential current connection authority; project resource project read/write grant where scoped; ownership current organization; resolve identifiers within the authorized project.
- Application: internal/web/ui/components/button/button.templ; return existing navigation targets/URLs from the authorized application read instead of requiring browser scraping
- Extraction: None for link presentation; producing application reads retain typed identifiers/URLs.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / shared application read/command for this frontend source
- Availability: hosted_dedicated / github,native / shared application read/command for this frontend source
- Availability: hosted_shared / github,native / shared application read/command for this frontend source
- Confirmation: read or ordinary non-destructive write → none

Sources: [internal/web/ui/components/button/button.templ:63](../internal/web/ui/components/button/button.templ#L63)
## frontend.internal_web_ui_components_sidebar_sidebar_templ.navigation_results

sidebar.templ navigation results

- Audience: local_ui; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Exact presentation links below render supplied external/documentation, fragment, or application URLs; they do not read or mutate application state. Meaningful server reads producing resource URLs are inventoried separately.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated operator; credential current connection authority; project resource project read/write grant where scoped; ownership current organization; resolve identifiers within the authorized project.
- Application: internal/web/ui/components/sidebar/sidebar.templ; return existing navigation targets/URLs from the authorized application read instead of requiring browser scraping
- Extraction: None for link presentation; producing application reads retain typed identifiers/URLs.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / shared application read/command for this frontend source
- Availability: hosted_dedicated / github,native / shared application read/command for this frontend source
- Availability: hosted_shared / github,native / shared application read/command for this frontend source
- Confirmation: read or ordinary non-destructive write → none

Sources: [internal/web/ui/components/sidebar/sidebar.templ:582](../internal/web/ui/components/sidebar/sidebar.templ#L582), [internal/web/ui/components/sidebar/sidebar.templ:694](../internal/web/ui/components/sidebar/sidebar.templ#L694)
## frontend.internal_web_ui_primitives_exceptionstrip_templ.navigation_results

exceptionstrip.templ navigation results

- Audience: local_ui; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Exact presentation links below render supplied external/documentation, fragment, or application URLs; they do not read or mutate application state. Meaningful server reads producing resource URLs are inventoried separately.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated operator; credential current connection authority; project resource project read/write grant where scoped; ownership current organization; resolve identifiers within the authorized project.
- Application: internal/web/ui/primitives/exceptionstrip.templ; return existing navigation targets/URLs from the authorized application read instead of requiring browser scraping
- Extraction: None for link presentation; producing application reads retain typed identifiers/URLs.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / shared application read/command for this frontend source
- Availability: hosted_dedicated / github,native / shared application read/command for this frontend source
- Availability: hosted_shared / github,native / shared application read/command for this frontend source
- Confirmation: read or ordinary non-destructive write → none

Sources: [internal/web/ui/primitives/exceptionstrip.templ:59](../internal/web/ui/primitives/exceptionstrip.templ#L59)
## frontend.internal_web_ui_primitives_states_templ.navigation_results

states.templ navigation results

- Audience: local_ui; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Exact presentation links below render supplied external/documentation, fragment, or application URLs; they do not read or mutate application state. Meaningful server reads producing resource URLs are inventoried separately.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated operator; credential current connection authority; project resource project read/write grant where scoped; ownership current organization; resolve identifiers within the authorized project.
- Application: internal/web/ui/primitives/states.templ; return existing navigation targets/URLs from the authorized application read instead of requiring browser scraping
- Extraction: None for link presentation; producing application reads retain typed identifiers/URLs.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / shared application read/command for this frontend source
- Availability: hosted_dedicated / github,native / shared application read/command for this frontend source
- Availability: hosted_shared / github,native / shared application read/command for this frontend source
- Confirmation: read or ordinary non-destructive write → none

Sources: [internal/web/ui/primitives/states.templ:11](../internal/web/ui/primitives/states.templ#L11)
## frontend.internal_web_ui_primitives_trackerlink_templ.navigation_results

trackerlink.templ navigation results

- Audience: local_ui; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Exact presentation links below render supplied external/documentation, fragment, or application URLs; they do not read or mutate application state. Meaningful server reads producing resource URLs are inventoried separately.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated operator; credential current connection authority; project resource project read/write grant where scoped; ownership current organization; resolve identifiers within the authorized project.
- Application: internal/web/ui/primitives/trackerlink.templ; return existing navigation targets/URLs from the authorized application read instead of requiring browser scraping
- Extraction: None for link presentation; producing application reads retain typed identifiers/URLs.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / shared application read/command for this frontend source
- Availability: hosted_dedicated / github,native / shared application read/command for this frontend source
- Availability: hosted_shared / github,native / shared application read/command for this frontend source
- Confirmation: read or ordinary non-destructive write → none

Sources: [internal/web/ui/primitives/trackerlink.templ:17](../internal/web/ui/primitives/trackerlink.templ#L17)
## frontend.static_js_artifacts_js.application

artifacts.js application

- Audience: transport; status: **excluded**; owner: digitaldrywood/detent#3347.
- Decision: Only these exact grant-bound artifact/media fetch and stream-reader sites are client display transport. Typed artifact references/access grants and change-review commands remain in separate pending rows.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated operator; credential current connection authority; project resource project read/write grant where scoped; ownership current organization; resolve identifiers within the authorized project.
- Application: static/js/artifacts.js; extract the application operation behind these exact request/action sites; retain current handler/service validation
- Extraction: None for byte streaming; share the application artifact/change read authorization in #3347.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / shared application read/command for this frontend source
- Availability: hosted_dedicated / github,native / shared application read/command for this frontend source
- Availability: hosted_shared / github,native / shared application read/command for this frontend source
- Confirmation: read or ordinary non-destructive write → none

Sources: [static/js/artifacts.js:35](../static/js/artifacts.js#L35), [static/js/artifacts.js:6](../static/js/artifacts.js#L6), [static/js/artifacts.js:14](../static/js/artifacts.js#L14)
## frontend.static_js_review_js.application

review.js application

- Audience: transport; status: **excluded**; owner: digitaldrywood/detent#3347.
- Decision: Only these exact grant-bound artifact/media fetch and stream-reader sites are client display transport. Typed artifact references/access grants and change-review commands remain in separate pending rows.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated operator; credential current connection authority; project resource project read/write grant where scoped; ownership current organization; resolve identifiers within the authorized project.
- Application: static/js/review.js; extract the application operation behind these exact request/action sites; retain current handler/service validation
- Extraction: None for byte streaming; share the application artifact/change read authorization in #3347.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / shared application read/command for this frontend source
- Availability: hosted_dedicated / github,native / shared application read/command for this frontend source
- Availability: hosted_shared / github,native / shared application read/command for this frontend source
- Confirmation: read or ordinary non-destructive write → none

Sources: [static/js/review.js:128](../static/js/review.js#L128), [static/js/review.js:29](../static/js/review.js#L29), [static/js/review.js:16](../static/js/review.js#L16)
## frontend.web_conversation_src_app_account_Login_tsx.login

Login.tsx login

- Audience: authentication; status: **excluded**; owner: digitaldrywood/detent#3336.
- Decision: Exact login definitions below are client/transport/identity plumbing; application requests and meaningful results have separate decisions.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated operator; credential current connection authority; project resource project read/write grant where scoped; ownership current organization; resolve identifiers within the authorized project.
- Application: web/conversation/src/app/account/Login.tsx; existing provider sign-in connection exchange
- Extraction: None; exact client/protocol sites do not own an operator application command.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Availability: hosted_dedicated / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Availability: hosted_shared / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Confirmation: read or ordinary non-destructive write → none

Sources: [web/conversation/src/app/account/Login.tsx:105](../web/conversation/src/app/account/Login.tsx#L105), [web/conversation/src/app/account/Login.tsx:104](../web/conversation/src/app/account/Login.tsx#L104)
## frontend.web_conversation_src_app_account_Login_tsx.navigation_results

Login.tsx navigation results

- Audience: local_ui; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Exact presentation links below render supplied external/documentation, fragment, or application URLs; they do not read or mutate application state. Meaningful server reads producing resource URLs are inventoried separately.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated operator; credential current connection authority; project resource project read/write grant where scoped; ownership current organization; resolve identifiers within the authorized project.
- Application: web/conversation/src/app/account/Login.tsx; return existing navigation targets/URLs from the authorized application read instead of requiring browser scraping
- Extraction: None for link presentation; producing application reads retain typed identifiers/URLs.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / shared application read/command for this frontend source
- Availability: hosted_dedicated / github,native / shared application read/command for this frontend source
- Availability: hosted_shared / github,native / shared application read/command for this frontend source
- Confirmation: read or ordinary non-destructive write → none

Sources: [web/conversation/src/app/account/Login.tsx:66](../web/conversation/src/app/account/Login.tsx#L66), [web/conversation/src/app/account/Login.tsx:78](../web/conversation/src/app/account/Login.tsx#L78), [web/conversation/src/app/account/Login.tsx:75](../web/conversation/src/app/account/Login.tsx#L75)
## frontend.web_conversation_src_app_account_SpritesCard_tsx.navigation_results

SpritesCard.tsx navigation results

- Audience: local_ui; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Exact presentation links below render supplied external/documentation, fragment, or application URLs; they do not read or mutate application state. Meaningful server reads producing resource URLs are inventoried separately.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated operator; credential current connection authority; project resource project read/write grant where scoped; ownership current organization; resolve identifiers within the authorized project.
- Application: web/conversation/src/app/account/SpritesCard.tsx; return existing navigation targets/URLs from the authorized application read instead of requiring browser scraping
- Extraction: None for link presentation; producing application reads retain typed identifiers/URLs.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / shared application read/command for this frontend source
- Availability: hosted_dedicated / github,native / shared application read/command for this frontend source
- Availability: hosted_shared / github,native / shared application read/command for this frontend source
- Confirmation: read or ordinary non-destructive write → none

Sources: [web/conversation/src/app/account/SpritesCard.tsx:43](../web/conversation/src/app/account/SpritesCard.tsx#L43)
## frontend.web_conversation_src_app_account_api_ts.http_adapter

api.ts http adapter

- Audience: transport; status: **excluded**; owner: digitaldrywood/detent#3339.
- Decision: Exact http adapter definitions below are client/transport/identity plumbing; application requests and meaningful results have separate decisions.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated operator; credential current connection authority; project resource project read/write grant where scoped; ownership current organization; resolve identifiers within the authorized project.
- Application: web/conversation/src/app/account/api.ts; bounded same-origin transport adapter; concrete requests are separately inventoried
- Extraction: None; exact client/protocol sites do not own an operator application command.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Availability: hosted_dedicated / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Availability: hosted_shared / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Confirmation: read or ordinary non-destructive write → none

Sources: [web/conversation/src/app/account/api.ts:115](../web/conversation/src/app/account/api.ts#L115), [web/conversation/src/app/account/api.ts:101](../web/conversation/src/app/account/api.ts#L101)
## frontend.web_conversation_src_app_account_idempotency_ts.local_ui

idempotency.ts local ui

- Audience: local_ui; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Exact local ui definitions below are local browser behavior without an application command.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated operator; credential current connection authority; project resource project read/write grant where scoped; ownership current organization; resolve identifiers within the authorized project.
- Application: web/conversation/src/app/account/idempotency.ts; browser dialog/editor/local storage/rendering; no server mutation
- Extraction: None; exact client/protocol sites do not own an operator application command.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Availability: hosted_dedicated / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Availability: hosted_shared / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Confirmation: read or ordinary non-destructive write → none

Sources: [web/conversation/src/app/account/idempotency.ts:79](../web/conversation/src/app/account/idempotency.ts#L79)
## frontend.web_conversation_src_app_adapters_actionRuns_ts.transport_frames

actionRuns.ts transport frames

- Audience: transport; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Exact transport frames definitions below are client/transport/identity plumbing; application requests and meaningful results have separate decisions.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated operator; credential current connection authority; project resource project read/write grant where scoped; ownership current organization; resolve identifiers within the authorized project.
- Application: web/conversation/src/app/adapters/actionRuns.ts; existing workspace relay or conversation SSE framing; retain runner/terminal grant authority
- Extraction: None; exact client/protocol sites do not own an operator application command.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Availability: hosted_dedicated / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Availability: hosted_shared / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Confirmation: read or ordinary non-destructive write → none

Sources: [web/conversation/src/app/adapters/actionRuns.ts:636](../web/conversation/src/app/adapters/actionRuns.ts#L636), [web/conversation/src/app/adapters/actionRuns.ts:291](../web/conversation/src/app/adapters/actionRuns.ts#L291), [web/conversation/src/app/adapters/actionRuns.ts:189](../web/conversation/src/app/adapters/actionRuns.ts#L189), [web/conversation/src/app/adapters/actionRuns.ts:340](../web/conversation/src/app/adapters/actionRuns.ts#L340)
## frontend.web_conversation_src_app_adapters_terminalStream_ts.transport_frames

terminalStream.ts transport frames

- Audience: transport; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Exact transport frames definitions below are client/transport/identity plumbing; application requests and meaningful results have separate decisions.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated operator; credential current connection authority; project resource project read/write grant where scoped; ownership current organization; resolve identifiers within the authorized project.
- Application: web/conversation/src/app/adapters/terminalStream.ts; existing workspace relay or conversation SSE framing; retain runner/terminal grant authority
- Extraction: None; exact client/protocol sites do not own an operator application command.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Availability: hosted_dedicated / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Availability: hosted_shared / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Confirmation: read or ordinary non-destructive write → none

Sources: [web/conversation/src/app/adapters/terminalStream.ts:408](../web/conversation/src/app/adapters/terminalStream.ts#L408), [web/conversation/src/app/adapters/terminalStream.ts:418](../web/conversation/src/app/adapters/terminalStream.ts#L418), [web/conversation/src/app/adapters/terminalStream.ts:246](../web/conversation/src/app/adapters/terminalStream.ts#L246), [web/conversation/src/app/adapters/terminalStream.ts:202](../web/conversation/src/app/adapters/terminalStream.ts#L202), [web/conversation/src/app/adapters/terminalStream.ts:425](../web/conversation/src/app/adapters/terminalStream.ts#L425), [web/conversation/src/app/adapters/terminalStream.ts:342](../web/conversation/src/app/adapters/terminalStream.ts#L342), [web/conversation/src/app/adapters/terminalStream.ts:357](../web/conversation/src/app/adapters/terminalStream.ts#L357)
## frontend.web_conversation_src_app_adapters_workspaceRelay_ts.transport_frames

workspaceRelay.ts transport frames

- Audience: transport; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Exact transport frames definitions below are client/transport/identity plumbing; application requests and meaningful results have separate decisions.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated operator; credential current connection authority; project resource project read/write grant where scoped; ownership current organization; resolve identifiers within the authorized project.
- Application: web/conversation/src/app/adapters/workspaceRelay.ts; existing workspace relay or conversation SSE framing; retain runner/terminal grant authority
- Extraction: None; exact client/protocol sites do not own an operator application command.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Availability: hosted_dedicated / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Availability: hosted_shared / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Confirmation: read or ordinary non-destructive write → none

Sources: [web/conversation/src/app/adapters/workspaceRelay.ts:344](../web/conversation/src/app/adapters/workspaceRelay.ts#L344), [web/conversation/src/app/adapters/workspaceRelay.ts:744](../web/conversation/src/app/adapters/workspaceRelay.ts#L744), [web/conversation/src/app/adapters/workspaceRelay.ts:723](../web/conversation/src/app/adapters/workspaceRelay.ts#L723), [web/conversation/src/app/adapters/workspaceRelay.ts:486](../web/conversation/src/app/adapters/workspaceRelay.ts#L486), [web/conversation/src/app/adapters/workspaceRelay.ts:786](../web/conversation/src/app/adapters/workspaceRelay.ts#L786), [web/conversation/src/app/adapters/workspaceRelay.ts:642](../web/conversation/src/app/adapters/workspaceRelay.ts#L642), [web/conversation/src/app/adapters/workspaceRelay.ts:361](../web/conversation/src/app/adapters/workspaceRelay.ts#L361), [web/conversation/src/app/adapters/workspaceRelay.ts:329](../web/conversation/src/app/adapters/workspaceRelay.ts#L329)
## frontend.web_conversation_src_app_components_ComposerContextStrip_tsx.navigation_results

ComposerContextStrip.tsx navigation results

- Audience: local_ui; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Exact presentation links below render supplied external/documentation, fragment, or application URLs; they do not read or mutate application state. Meaningful server reads producing resource URLs are inventoried separately.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated operator; credential current connection authority; project resource project read/write grant where scoped; ownership current organization; resolve identifiers within the authorized project.
- Application: web/conversation/src/app/components/ComposerContextStrip.tsx; return existing navigation targets/URLs from the authorized application read instead of requiring browser scraping
- Extraction: None for link presentation; producing application reads retain typed identifiers/URLs.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / shared application read/command for this frontend source
- Availability: hosted_dedicated / github,native / shared application read/command for this frontend source
- Availability: hosted_shared / github,native / shared application read/command for this frontend source
- Confirmation: read or ordinary non-destructive write → none

Sources: [web/conversation/src/app/components/ComposerContextStrip.tsx:129](../web/conversation/src/app/components/ComposerContextStrip.tsx#L129)
## frontend.web_conversation_src_app_components_ComposerPromptEditor_tsx.local_ui

ComposerPromptEditor.tsx local ui

- Audience: local_ui; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Exact local ui definitions below are local browser behavior without an application command.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated operator; credential current connection authority; project resource project read/write grant where scoped; ownership current organization; resolve identifiers within the authorized project.
- Application: web/conversation/src/app/components/ComposerPromptEditor.tsx; browser dialog/editor/local storage/rendering; no server mutation
- Extraction: None; exact client/protocol sites do not own an operator application command.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Availability: hosted_dedicated / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Availability: hosted_shared / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Confirmation: read or ordinary non-destructive write → none

Sources: [web/conversation/src/app/components/ComposerPromptEditor.tsx:392](../web/conversation/src/app/components/ComposerPromptEditor.tsx#L392), [web/conversation/src/app/components/ComposerPromptEditor.tsx:137](../web/conversation/src/app/components/ComposerPromptEditor.tsx#L137), [web/conversation/src/app/components/ComposerPromptEditor.tsx:148](../web/conversation/src/app/components/ComposerPromptEditor.tsx#L148), [web/conversation/src/app/components/ComposerPromptEditor.tsx:251](../web/conversation/src/app/components/ComposerPromptEditor.tsx#L251), [web/conversation/src/app/components/ComposerPromptEditor.tsx:296](../web/conversation/src/app/components/ComposerPromptEditor.tsx#L296)
## frontend.web_conversation_src_app_entry_api_ts.http_adapter

api.ts http adapter

- Audience: transport; status: **excluded**; owner: digitaldrywood/detent#3339.
- Decision: Exact http adapter definitions below are client/transport/identity plumbing; application requests and meaningful results have separate decisions.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated operator; credential current connection authority; project resource project read/write grant where scoped; ownership current organization; resolve identifiers within the authorized project.
- Application: web/conversation/src/app/entry/api.ts; bounded same-origin transport adapter; concrete requests are separately inventoried
- Extraction: None; exact client/protocol sites do not own an operator application command.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Availability: hosted_dedicated / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Availability: hosted_shared / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Confirmation: read or ordinary non-destructive write → none

Sources: [web/conversation/src/app/entry/api.ts:196](../web/conversation/src/app/entry/api.ts#L196), [web/conversation/src/app/entry/api.ts:200](../web/conversation/src/app/entry/api.ts#L200), [web/conversation/src/app/entry/api.ts:230](../web/conversation/src/app/entry/api.ts#L230), [web/conversation/src/app/entry/api.ts:215](../web/conversation/src/app/entry/api.ts#L215)
## frontend.web_conversation_src_app_fleet_RunnersSection_tsx.navigation_results

RunnersSection.tsx navigation results

- Audience: local_ui; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Exact presentation links below render supplied external/documentation, fragment, or application URLs; they do not read or mutate application state. Meaningful server reads producing resource URLs are inventoried separately.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated operator; credential current connection authority; project resource project read/write grant where scoped; ownership current organization; resolve identifiers within the authorized project.
- Application: web/conversation/src/app/fleet/RunnersSection.tsx; return existing navigation targets/URLs from the authorized application read instead of requiring browser scraping
- Extraction: None for link presentation; producing application reads retain typed identifiers/URLs.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / shared application read/command for this frontend source
- Availability: hosted_dedicated / github,native / shared application read/command for this frontend source
- Availability: hosted_shared / github,native / shared application read/command for this frontend source
- Confirmation: read or ordinary non-destructive write → none

Sources: [web/conversation/src/app/fleet/RunnersSection.tsx:266](../web/conversation/src/app/fleet/RunnersSection.tsx#L266), [web/conversation/src/app/fleet/RunnersSection.tsx:262](../web/conversation/src/app/fleet/RunnersSection.tsx#L262)
## frontend.web_conversation_src_app_usage_adapter_ts.http_adapter

adapter.ts http adapter

- Audience: transport; status: **excluded**; owner: digitaldrywood/detent#3339.
- Decision: Exact http adapter definitions below are client/transport/identity plumbing; application requests and meaningful results have separate decisions.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated operator; credential current connection authority; project resource project read/write grant where scoped; ownership current organization; resolve identifiers within the authorized project.
- Application: web/conversation/src/app/usage/adapter.ts; bounded same-origin transport adapter; concrete requests are separately inventoried
- Extraction: None; exact client/protocol sites do not own an operator application command.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Availability: hosted_dedicated / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Availability: hosted_shared / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Confirmation: read or ordinary non-destructive write → none

Sources: [web/conversation/src/app/usage/adapter.ts:229](../web/conversation/src/app/usage/adapter.ts#L229)
## frontend.web_conversation_src_app_work_lib_workHttp_ts.application

Request a governed pull-request action

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3347.
- Decision: Pending parity: return meaningful structured navigation/application results for these explicit browser sites; never scrape HTML or proxy arbitrary HTTP.
- Tool: `changes_artifacts.pull_request_action` — project_id, work_item_id, validated PR action enum and idempotency key; no arbitrary command; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → Typed correlated action receipt, PR identifier/URL and outcome; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role authenticated operator; credential current connection authority; project resource project read/write grant where scoped; ownership current organization; resolve identifiers within the authorized project.
- Application: WorkHttp pullRequestAction; no matching hub route is currently registered
- Extraction: Extract/reuse the existing workspace/forge governed PR application command; this client request currently lacks a registered hub handler. #3347 must provide a shared command and safe unavailable behavior before claiming parity.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=true; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / shared application read/command for this frontend source
- Availability: hosted_dedicated / github,native / shared application read/command for this frontend source
- Availability: hosted_shared / github,native / shared application read/command for this frontend source
- Confirmation: material PR/forge action → operator

Sources: [web/conversation/src/app/work/lib/workHttp.ts:653](../web/conversation/src/app/work/lib/workHttp.ts#L653)
## frontend.web_conversation_src_app_work_lib_workHttp_ts.http_adapter

workHttp.ts http adapter

- Audience: transport; status: **excluded**; owner: digitaldrywood/detent#3339.
- Decision: Exact http adapter definitions below are client/transport/identity plumbing; application requests and meaningful results have separate decisions.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated operator; credential current connection authority; project resource project read/write grant where scoped; ownership current organization; resolve identifiers within the authorized project.
- Application: web/conversation/src/app/work/lib/workHttp.ts; bounded same-origin transport adapter; concrete requests are separately inventoried
- Extraction: None; exact client/protocol sites do not own an operator application command.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Availability: hosted_dedicated / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Availability: hosted_shared / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Confirmation: read or ordinary non-destructive write → none

Sources: [web/conversation/src/app/work/lib/workHttp.ts:461](../web/conversation/src/app/work/lib/workHttp.ts#L461), [web/conversation/src/app/work/lib/workHttp.ts:505](../web/conversation/src/app/work/lib/workHttp.ts#L505), [web/conversation/src/app/work/lib/workHttp.ts:430](../web/conversation/src/app/work/lib/workHttp.ts#L430)
## frontend.web_conversation_src_components_ChatMarkdown_tsx.navigation_results

ChatMarkdown.tsx navigation results

- Audience: local_ui; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Exact presentation links below render supplied external/documentation, fragment, or application URLs; they do not read or mutate application state. Meaningful server reads producing resource URLs are inventoried separately.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated operator; credential current connection authority; project resource project read/write grant where scoped; ownership current organization; resolve identifiers within the authorized project.
- Application: web/conversation/src/components/ChatMarkdown.tsx; return existing navigation targets/URLs from the authorized application read instead of requiring browser scraping
- Extraction: None for link presentation; producing application reads retain typed identifiers/URLs.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / shared application read/command for this frontend source
- Availability: hosted_dedicated / github,native / shared application read/command for this frontend source
- Availability: hosted_shared / github,native / shared application read/command for this frontend source
- Confirmation: read or ordinary non-destructive write → none

Sources: [web/conversation/src/components/ChatMarkdown.tsx:2529](../web/conversation/src/components/ChatMarkdown.tsx#L2529), [web/conversation/src/components/ChatMarkdown.tsx:2077](../web/conversation/src/components/ChatMarkdown.tsx#L2077), [web/conversation/src/components/ChatMarkdown.tsx:2803](../web/conversation/src/components/ChatMarkdown.tsx#L2803)
## frontend.web_conversation_src_components_Icons_tsx.local_ui

Icons.tsx local ui

- Audience: local_ui; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Exact local ui definitions below are local browser behavior without an application command.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated operator; credential current connection authority; project resource project read/write grant where scoped; ownership current organization; resolve identifiers within the authorized project.
- Application: web/conversation/src/components/Icons.tsx; browser dialog/editor/local storage/rendering; no server mutation
- Extraction: None; exact client/protocol sites do not own an operator application command.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Availability: hosted_dedicated / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Availability: hosted_shared / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Confirmation: read or ordinary non-destructive write → none

Sources: [web/conversation/src/components/Icons.tsx:654](../web/conversation/src/components/Icons.tsx#L654), [web/conversation/src/components/Icons.tsx:70](../web/conversation/src/components/Icons.tsx#L70), [web/conversation/src/components/Icons.tsx:71](../web/conversation/src/components/Icons.tsx#L71)
## frontend.web_conversation_src_components_chat_AssistantCitationCommentEditor_tsx.local_ui

AssistantCitationCommentEditor.tsx local ui

- Audience: local_ui; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Exact local ui definitions below are local browser behavior without an application command.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated operator; credential current connection authority; project resource project read/write grant where scoped; ownership current organization; resolve identifiers within the authorized project.
- Application: web/conversation/src/components/chat/AssistantCitationCommentEditor.tsx; browser dialog/editor/local storage/rendering; no server mutation
- Extraction: None; exact client/protocol sites do not own an operator application command.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Availability: hosted_dedicated / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Availability: hosted_shared / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Confirmation: read or ordinary non-destructive write → none

Sources: [web/conversation/src/components/chat/AssistantCitationCommentEditor.tsx:66](../web/conversation/src/components/chat/AssistantCitationCommentEditor.tsx#L66)
## frontend.web_conversation_src_components_chat_PierreEntryIcon_tsx.local_ui

PierreEntryIcon.tsx local ui

- Audience: local_ui; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Exact local ui definitions below are local browser behavior without an application command.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated operator; credential current connection authority; project resource project read/write grant where scoped; ownership current organization; resolve identifiers within the authorized project.
- Application: web/conversation/src/components/chat/PierreEntryIcon.tsx; browser dialog/editor/local storage/rendering; no server mutation
- Extraction: None; exact client/protocol sites do not own an operator application command.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Availability: hosted_dedicated / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Availability: hosted_shared / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Confirmation: read or ordinary non-destructive write → none

Sources: [web/conversation/src/components/chat/PierreEntryIcon.tsx:92](../web/conversation/src/components/chat/PierreEntryIcon.tsx#L92)
## frontend.web_conversation_src_components_media_OpenMediaLink_tsx.navigation_results

OpenMediaLink.tsx navigation results

- Audience: local_ui; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Exact presentation links below render supplied external/documentation, fragment, or application URLs; they do not read or mutate application state. Meaningful server reads producing resource URLs are inventoried separately.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated operator; credential current connection authority; project resource project read/write grant where scoped; ownership current organization; resolve identifiers within the authorized project.
- Application: web/conversation/src/components/media/OpenMediaLink.tsx; return existing navigation targets/URLs from the authorized application read instead of requiring browser scraping
- Extraction: None for link presentation; producing application reads retain typed identifiers/URLs.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / shared application read/command for this frontend source
- Availability: hosted_dedicated / github,native / shared application read/command for this frontend source
- Availability: hosted_shared / github,native / shared application read/command for this frontend source
- Confirmation: read or ordinary non-destructive write → none

Sources: [web/conversation/src/components/media/OpenMediaLink.tsx:34](../web/conversation/src/components/media/OpenMediaLink.tsx#L34)
## frontend.web_conversation_src_components_media_mediaContent_ts.application

mediaContent.ts application

- Audience: transport; status: **excluded**; owner: digitaldrywood/detent#3347.
- Decision: Only these exact grant-bound artifact/media fetch and stream-reader sites are client display transport. Typed artifact references/access grants and change-review commands remain in separate pending rows.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated operator; credential current connection authority; project resource project read/write grant where scoped; ownership current organization; resolve identifiers within the authorized project.
- Application: web/conversation/src/components/media/mediaContent.ts; extract the application operation behind these exact request/action sites; retain current handler/service validation
- Extraction: None for byte streaming; share the application artifact/change read authorization in #3347.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / shared application read/command for this frontend source
- Availability: hosted_dedicated / github,native / shared application read/command for this frontend source
- Availability: hosted_shared / github,native / shared application read/command for this frontend source
- Confirmation: read or ordinary non-destructive write → none

Sources: [web/conversation/src/components/media/mediaContent.ts:13](../web/conversation/src/components/media/mediaContent.ts#L13)
## frontend.web_conversation_src_hooks_useLocalStorage_ts.local_ui

useLocalStorage.ts local ui

- Audience: local_ui; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Exact local ui definitions below are local browser behavior without an application command.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated operator; credential current connection authority; project resource project read/write grant where scoped; ownership current organization; resolve identifiers within the authorized project.
- Application: web/conversation/src/hooks/useLocalStorage.ts; browser dialog/editor/local storage/rendering; no server mutation
- Extraction: None; exact client/protocol sites do not own an operator application command.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Availability: hosted_dedicated / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Availability: hosted_shared / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Confirmation: read or ordinary non-destructive write → none

Sources: [web/conversation/src/hooks/useLocalStorage.ts:60](../web/conversation/src/hooks/useLocalStorage.ts#L60), [web/conversation/src/hooks/useLocalStorage.ts:107](../web/conversation/src/hooks/useLocalStorage.ts#L107)
## frontend.web_conversation_src_lib_previewAnnotation_ts.local_ui

previewAnnotation.ts local ui

- Audience: local_ui; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Exact local ui definitions below are local browser behavior without an application command.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated operator; credential current connection authority; project resource project read/write grant where scoped; ownership current organization; resolve identifiers within the authorized project.
- Application: web/conversation/src/lib/previewAnnotation.ts; browser dialog/editor/local storage/rendering; no server mutation
- Extraction: None; exact client/protocol sites do not own an operator application command.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Availability: hosted_dedicated / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Availability: hosted_shared / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Confirmation: read or ordinary non-destructive write → none

Sources: [web/conversation/src/lib/previewAnnotation.ts:106](../web/conversation/src/lib/previewAnnotation.ts#L106)
## frontend.web_conversation_src_runtime_bootstrap_ts.http_adapter

bootstrap.ts http adapter

- Audience: transport; status: **excluded**; owner: digitaldrywood/detent#3339.
- Decision: Exact http adapter definitions below are client/transport/identity plumbing; application requests and meaningful results have separate decisions.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated operator; credential current connection authority; project resource project read/write grant where scoped; ownership current organization; resolve identifiers within the authorized project.
- Application: web/conversation/src/runtime/bootstrap.ts; bounded same-origin transport adapter; concrete requests are separately inventoried
- Extraction: None; exact client/protocol sites do not own an operator application command.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Availability: hosted_dedicated / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Availability: hosted_shared / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Confirmation: read or ordinary non-destructive write → none

Sources: [web/conversation/src/runtime/bootstrap.ts:109](../web/conversation/src/runtime/bootstrap.ts#L109)
## frontend.web_conversation_src_runtime_rpc_http_ts.http_adapter

http.ts http adapter

- Audience: transport; status: **excluded**; owner: digitaldrywood/detent#3339.
- Decision: Exact http adapter definitions below are client/transport/identity plumbing; application requests and meaningful results have separate decisions.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated operator; credential current connection authority; project resource project read/write grant where scoped; ownership current organization; resolve identifiers within the authorized project.
- Application: web/conversation/src/runtime/rpc/http.ts; bounded same-origin transport adapter; concrete requests are separately inventoried
- Extraction: None; exact client/protocol sites do not own an operator application command.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Availability: hosted_dedicated / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Availability: hosted_shared / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Confirmation: read or ordinary non-destructive write → none

Sources: [web/conversation/src/runtime/rpc/http.ts:275](../web/conversation/src/runtime/rpc/http.ts#L275), [web/conversation/src/runtime/rpc/http.ts:331](../web/conversation/src/runtime/rpc/http.ts#L331), [web/conversation/src/runtime/rpc/http.ts:248](../web/conversation/src/runtime/rpc/http.ts#L248)
## frontend.web_conversation_src_runtime_rpc_sse_ts.http_adapter

sse.ts http adapter

- Audience: transport; status: **excluded**; owner: digitaldrywood/detent#3339.
- Decision: Exact http adapter definitions below are client/transport/identity plumbing; application requests and meaningful results have separate decisions.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated operator; credential current connection authority; project resource project read/write grant where scoped; ownership current organization; resolve identifiers within the authorized project.
- Application: web/conversation/src/runtime/rpc/sse.ts; bounded same-origin transport adapter; concrete requests are separately inventoried
- Extraction: None; exact client/protocol sites do not own an operator application command.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Availability: hosted_dedicated / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Availability: hosted_shared / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Confirmation: read or ordinary non-destructive write → none

Sources: [web/conversation/src/runtime/rpc/sse.ts:136](../web/conversation/src/runtime/rpc/sse.ts#L136)
## frontend.web_conversation_src_runtime_rpc_sse_ts.transport_frames

sse.ts transport frames

- Audience: transport; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Exact transport frames definitions below are client/transport/identity plumbing; application requests and meaningful results have separate decisions.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated operator; credential current connection authority; project resource project read/write grant where scoped; ownership current organization; resolve identifiers within the authorized project.
- Application: web/conversation/src/runtime/rpc/sse.ts; existing workspace relay or conversation SSE framing; retain runner/terminal grant authority
- Extraction: None; exact client/protocol sites do not own an operator application command.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Availability: hosted_dedicated / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Availability: hosted_shared / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Confirmation: read or ordinary non-destructive write → none

Sources: [web/conversation/src/runtime/rpc/sse.ts:85](../web/conversation/src/runtime/rpc/sse.ts#L85)
## frontend.web_conversation_src_runtime_state_drafts_ts.local_ui

drafts.ts local ui

- Audience: local_ui; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Exact local ui definitions below are local browser behavior without an application command.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated operator; credential current connection authority; project resource project read/write grant where scoped; ownership current organization; resolve identifiers within the authorized project.
- Application: web/conversation/src/runtime/state/drafts.ts; browser dialog/editor/local storage/rendering; no server mutation
- Extraction: None; exact client/protocol sites do not own an operator application command.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Availability: hosted_dedicated / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Availability: hosted_shared / github,native / shared application read/command for this frontend source — unavailable: This source is a browser asset; no standalone MCP transport or tool service.
- Confirmation: read or ordinary non-destructive write → none

Sources: [web/conversation/src/runtime/state/drafts.ts:166](../web/conversation/src/runtime/state/drafts.ts#L166), [web/conversation/src/runtime/state/drafts.ts:114](../web/conversation/src/runtime/state/drafts.ts#L114), [web/conversation/src/runtime/state/drafts.ts:127](../web/conversation/src/runtime/state/drafts.ts#L127)
## hubserver.accept_hosted_invitation

Accept hosted invitation

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3344.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `organization.accept_hosted_invitation` — Bounded acceptHostedInvitationRequest: organization; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → acceptHostedInvitationResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role hosted owner/admin; owner-only restrictions for owner membership/role changes; invitation acceptance bound to invited identity; credential hosted browser session; bearer credentials refused by session-only APIs; CSRF for mutation; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.acceptHostedInvitationJSON; s.acceptHostedInvitationToken, s.hostedEntryOwned
- Extraction: Extract hubserver.acceptHostedInvitationJSON application inputs/results and validation from Echo; reuse s.acceptHostedInvitationToken, s.hostedEntryOwned. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.; Current handler authority checks: s.hostedInvitationAcceptedBySession(c, request.Token); s.hostedSession(c); err != nil
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=false; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_dedicated / github,native / hub application service
- Availability: hosted_shared / github,native / hub application service
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [POST /api/v2/organizations/:organization/invitations/accept](../internal/hubserver/hosted_org_api.go#L44), [POST /organization/join](../internal/hubserver/hosted_ui.go#L42), [internal/web/templates/hosted.templ:170](../internal/web/templates/hosted.templ#L170), [internal/web/templates/hosted.templ:170](../internal/web/templates/hosted.templ#L170), [internal/web/templates/hosted.templ:31](../internal/web/templates/hosted.templ#L31), [web/conversation/src/app/account/api.ts:204](../web/conversation/src/app/account/api.ts#L204)
## hubserver.accept_hosted_shared_invitation

Accept hosted shared invitation

- Audience: transport; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Exact transport/protocol plumbing site. Application payload operations are inventoried separately; do not expose an HTTP or relay proxy tool.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role hosted owner/admin; owner-only restrictions for owner membership/role changes; invitation acceptance bound to invited identity; credential hosted browser session; bearer credentials refused by session-only APIs; CSRF for mutation; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.acceptHostedSharedInvitation; s.acceptHostedInvitationFor, s.hostedMutationMu.Lock, s.hostedMutationMu.Unlock
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_dedicated / github,native / hub application service
- Availability: hosted_shared / github,native / hub application service
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: this non-operator source site → not_applicable

Sources: [POST /internal/v1/invitations/accept](../internal/hubserver/hosted_shared.go#L190)
## hubserver.advance_git_hub_import

Advance git hub import

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3342.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `projects.advance_git_hub_import` — Bounded advanceGitHubImportRequest: organization, project, import; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → advanceGitHubImportResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.advanceGitHubImport; s.config.now, s.database.checkHostedGrowth, s.database.db.BeginTx, s.database.hostedConsumption, s.fetchImportPage
- Extraction: Extract hubserver.advanceGitHubImport application inputs/results and validation from Echo; reuse s.config.now, s.database.checkHostedGrowth, s.database.db.BeginTx, s.database.hostedConsumption, s.fetchImportPage. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=false; idempotent=false; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [POST /api/v2/organizations/:organization/projects/:project/imports/:import/advance](../internal/hubserver/integration.go#L155)
## hubserver.app_bootstrap_payload

App bootstrap payload

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3336.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `connection.app_bootstrap_payload` — Bounded appBootstrapPayloadRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → appBootstrapPayloadResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.appBootstrapPayload; s.appBootstrapFeature, s.appBootstrapPlan, s.appBootstrapPreferences, s.conversations.coordinator.Available, s.database.db.QueryRowContext, s.hostedAllRunnerGrants, s.hostedBase, s.hostedCredential, s.hostedOrganizationChoices, s.hostedPageCSRF, s.hostedReadableProjects
- Extraction: Extract hubserver.appBootstrapPayload application inputs/results and validation from Echo; reuse s.appBootstrapFeature, s.appBootstrapPlan, s.appBootstrapPreferences, s.conversations.coordinator.Available, s.database.db.QueryRowContext, s.hostedAllRunnerGrants, s.hostedBase, s.hostedCredential, s.hostedOrganizationChoices, s.hostedPageCSRF, s.hostedReadableProjects. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.; Current handler authority checks: s.hostedCredential(c)
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_dedicated / github,native / hub application service
- Availability: hosted_shared / github,native / hub application service
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /app/bootstrap](../internal/hubserver/app_ui.go#L62), [GET /chat/bootstrap](../internal/hubserver/app_ui.go#L63), [web/conversation/src/runtime/rpc/http.ts:384](../web/conversation/src/runtime/rpc/http.ts#L384), [web/conversation/src/app/App.tsx:1008](../web/conversation/src/app/App.tsx#L1008)
## hubserver.app_shell

App shell

- Audience: transport; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Exact transport/protocol plumbing site. Application payload operations are inventoried separately; do not expose an HTTP or relay proxy tool.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.appShell; s.hostedBase
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Current handler authority checks: s.hostedSession(c); err != nil
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_dedicated / github,native / hub application service
- Availability: hosted_shared / github,native / hub application service
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: this non-operator source site → not_applicable

Sources: [Any /*](../internal/hubserver/app_ui.go#L67), [GET /login](../internal/hubserver/hosted_ui.go#L24)
## hubserver.app_updates

App updates

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3336.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `connection.app_updates` — Bounded appUpdatesRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → appUpdatesResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.appUpdates; s.config.now, s.database.db.QueryContext, s.hostedAllRunnerGrants, s.hostedCredential
- Extraction: Extract hubserver.appUpdates application inputs/results and validation from Echo; reuse s.config.now, s.database.db.QueryContext, s.hostedAllRunnerGrants, s.hostedCredential. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.; Current handler authority checks: s.hostedCredential(c)
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_dedicated / github,native / hub application service
- Availability: hosted_shared / github,native / hub application service
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /app/updates](../internal/hubserver/app_ui.go#L64), [web/conversation/src/app/adapters/detentUpdates.ts:36](../web/conversation/src/app/adapters/detentUpdates.ts#L36)
## hubserver.append_native_run_event

Append native run event

- Audience: worker; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Worker protocol only: preserve worker credential, producer/lease and runner ownership; organization operator tools cannot impersonate a worker.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated worker/runner; credential worker scope; runner credential/lease where applicable; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.appendNativeRunEvent; s.nativeMutation, s.usagePrices
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: this non-operator source site → not_applicable

Sources: [POST /api/v2/organizations/:organization/projects/:project/work-items/:item/events](../internal/hubserver/native_api.go#L146)
## hubserver.append_work_item_event

Append work item event

- Audience: worker; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Worker protocol only: preserve worker credential, producer/lease and runner ownership; organization operator tools cannot impersonate a worker.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated worker/runner; credential worker scope; runner credential/lease where applicable; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.appendWorkItemEvent; s.config.now, s.database.appendEvent
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / hub application service
- Availability: hosted_dedicated / github,native / hub application service
- Availability: hosted_shared / github,native / hub application service
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: this non-operator source site → not_applicable

Sources: [POST /api/v1/work-items/:id/events](../internal/hubserver/api_http.go#L67)
## hubserver.approve_change_review_policy

Approve change review policy

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3347.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `changes_artifacts.approve_change_review_policy` — Bounded approveChangeReviewPolicyRequest: organization, project; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → approveChangeReviewPolicyResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.approveChangeReviewPolicy; s.nativeMutation
- Extraction: Extract hubserver.approveChangeReviewPolicy application inputs/results and validation from Echo; reuse s.nativeMutation. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=true; idempotent=false; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: ordinary non-destructive edit → none
- Confirmation: arguments remove data, alter access or create material external effects → operator

Sources: [PUT /api/v2/organizations/:organization/projects/:project/change-review-policy](../internal/hubserver/changes.go#L75)
## hubserver.approve_project_policy

Approve project policy

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3342.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `projects.approve_project_policy` — Bounded approveProjectPolicyRequest: owner, repo; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → approveProjectPolicyResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role instance admin in unhosted hub; hosted owner/admin through requireHostedAdministration; credential admin; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.approveProjectPolicy; s.database.approvePolicy, s.policyScope
- Extraction: Extract hubserver.approveProjectPolicy application inputs/results and validation from Echo; reuse s.database.approvePolicy, s.policyScope. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=true; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / hub application service
- Availability: hosted_dedicated / github,native / hub application service
- Availability: hosted_shared / github,native / hub application service
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: ordinary non-destructive edit → none
- Confirmation: arguments remove data, alter access or create material external effects → operator

Sources: [PUT /api/v1/repositories/:owner/:repo/policy](../internal/hubserver/api_http.go#L58), [PUT /api/v2/organizations/:organization/projects/:project/policy](../internal/hubserver/native_api.go#L111), [PUT /api/v2/organizations/:organization/projects/:project/onboarding/policy](../internal/hubserver/onboarding.go#L29), [web/conversation/src/app/account/ProjectSettings.tsx:254](../web/conversation/src/app/account/ProjectSettings.tsx#L254)
## hubserver.archive_native_issue

Archive native issue

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3341.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `work_commands.archive_native_issue` — Bounded archiveNativeIssueRequest: organization, project, item; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → archiveNativeIssueResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.archiveNativeIssue; s.setNativeArchive
- Extraction: Extract hubserver.archiveNativeIssue application inputs/results and validation from Echo; reuse s.setNativeArchive. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=false; idempotent=false; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [POST /api/v2/organizations/:organization/projects/:project/work-items/:item/archive](../internal/hubserver/native_api.go#L129), [web/conversation/src/app/work/lib/workHttp.ts:576](../web/conversation/src/app/work/lib/workHttp.ts#L576)
## hubserver.artifact_read_grant

Artifact read grant

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3347.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `changes_artifacts.artifact_read_grant` — Bounded artifactReadGrantRequest: organization, project, item, artifact; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → artifactReadGrantResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role hosted owner/admin; owner-only restrictions for owner membership/role changes; invitation acceptance bound to invited identity; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.artifactReadGrant; s.config.now, s.database.db.BeginTx, s.database.db.QueryRowContext
- Extraction: Extract hubserver.artifactReadGrant application inputs/results and validation from Echo; reuse s.config.now, s.database.db.BeginTx, s.database.db.QueryRowContext. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=true; idempotent=false; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: mutation of access, billing, deletion, cancellation or material external state → operator

Sources: [POST /api/v2/organizations/:organization/projects/:project/work-items/:item/artifacts/:artifact/access](../internal/hubserver/artifacts.go#L25)
## hubserver.artifact_receipt

Artifact receipt

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3347.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `changes_artifacts.artifact_receipt` — Bounded artifactReceiptRequest: organization, project, service; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → artifactReceiptResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.artifactReceipt; s.artifactPublisher, s.config.now, s.database.checkHostedGrowth, s.database.db.BeginTx, s.database.db.QueryRowContext, s.database.hostedConsumption
- Extraction: Extract hubserver.artifactReceipt application inputs/results and validation from Echo; reuse s.artifactPublisher, s.config.now, s.database.checkHostedGrowth, s.database.db.BeginTx, s.database.db.QueryRowContext, s.database.hostedConsumption. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=false; idempotent=false; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [POST /api/v2/organizations/:organization/projects/:project/artifact-services/:service/receipts](../internal/hubserver/artifacts.go#L21)
## hubserver.artifact_references

Artifact references

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3347.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `changes_artifacts.artifact_references` — Bounded artifactReferencesRequest: organization, project, item; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → artifactReferencesResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.artifactReferences; s.config.now, s.database.db.QueryContext
- Extraction: Extract hubserver.artifactReferences application inputs/results and validation from Echo; reuse s.config.now, s.database.db.QueryContext. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v2/organizations/:organization/projects/:project/work-items/:item/artifacts](../internal/hubserver/artifacts.go#L24)
## hubserver.artifact_services

Artifact services

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3347.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `changes_artifacts.artifact_services` — Bounded artifactServicesRequest: organization, project; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → artifactServicesResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.artifactServices; s.database.db.QueryContext
- Extraction: Extract hubserver.artifactServices application inputs/results and validation from Echo; reuse s.database.db.QueryContext. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v2/organizations/:organization/projects/:project/artifact-services](../internal/hubserver/artifacts.go#L20)
## hubserver.authorize_artifact_read

Authorize artifact read

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3347.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `changes_artifacts.authorize_artifact_read` — Bounded authorizeArtifactReadRequest: organization, project, service; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → authorizeArtifactReadResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.authorizeArtifactRead; s.artifactPublisher, s.authorizeHostedArtifactRead, s.config.now, s.database.db.QueryRowContext
- Extraction: Extract hubserver.authorizeArtifactRead application inputs/results and validation from Echo; reuse s.artifactPublisher, s.authorizeHostedArtifactRead, s.config.now, s.database.db.QueryRowContext. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=false; idempotent=false; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [POST /api/v2/organizations/:organization/projects/:project/artifact-services/:service/authorize](../internal/hubserver/artifacts.go#L22)
## hubserver.authorize_artifact_upload

Authorize artifact upload

- Audience: worker; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Worker protocol only: preserve worker credential, producer/lease and runner ownership; organization operator tools cannot impersonate a worker.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated worker/runner; credential worker scope; runner credential/lease where applicable; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.authorizeArtifactUpload; s.config.now, s.database.db.BeginTx
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: this non-operator source site → not_applicable

Sources: [POST /api/v2/organizations/:organization/projects/:project/work-items/:item/artifact-authority](../internal/hubserver/artifacts.go#L23)
## hubserver.bind_artifact_service

Bind artifact service

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3340.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `work_reads.bind_artifact_service` — Bounded bindArtifactServiceRequest: organization, project, service; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → bindArtifactServiceResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.bindArtifactService; s.database.db.ExecContext, s.database.db.QueryRowContext
- Extraction: Extract hubserver.bindArtifactService application inputs/results and validation from Echo; reuse s.database.db.ExecContext, s.database.db.QueryRowContext. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=true; idempotent=false; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: ordinary non-destructive edit → none
- Confirmation: arguments remove data, alter access or create material external effects → operator

Sources: [PUT /api/v2/organizations/:organization/projects/:project/artifact-services/:service](../internal/hubserver/artifacts.go#L19), [PUT /api/v2/organizations/:organization/projects/:project/onboarding/artifact-services/:service](../internal/hubserver/onboarding.go#L30)
## hubserver.bind_native_repository

Bind native repository

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3342.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `projects.bind_native_repository` — Bounded bindNativeRepositoryRequest: organization, project; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → bindNativeRepositoryResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role instance admin in unhosted hub; hosted owner/admin through requireHostedAdministration; credential admin; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.bindNativeRepository; s.bindRunnerCheckoutRepository, s.config.ReconcileBackend.Reconcile, s.nativeMutation
- Extraction: Extract hubserver.bindNativeRepository application inputs/results and validation from Echo; reuse s.bindRunnerCheckoutRepository, s.config.ReconcileBackend.Reconcile, s.nativeMutation. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=false; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [POST /api/v2/organizations/:organization/projects/:project/integration/repository](../internal/hubserver/integration.go#L150), [POST /api/v2/organizations/:organization/projects/:project/onboarding/repository](../internal/hubserver/onboarding.go#L26)
## hubserver.bind_workspace_worker

Bind workspace worker

- Audience: worker; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Worker protocol only: preserve worker credential, producer/lease and runner ownership; organization operator tools cannot impersonate a worker.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated worker/runner; credential worker scope; runner credential/lease where applicable; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.bindWorkspaceWorker; s.hubTransact, s.requireWorkspaces, service.bind, service.checkoutFor, service.committed, service.loadWorkspaceForWorker
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Current handler authority checks: s.requireWorkspaces()
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: this non-operator source site → not_applicable

Sources: [POST /api/v2/organizations/:organization/projects/:project/workspaces/:workspace/worker/bind](../internal/hubserver/native_api.go#L165)
## hubserver.bootstrap_hosted_shared_owner

Bootstrap hosted shared owner

- Audience: transport; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Exact transport/protocol plumbing site. Application payload operations are inventoried separately; do not expose an HTTP or relay proxy tool.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential hosted browser session; bearer credentials refused by session-only APIs; CSRF for mutation; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.bootstrapHostedSharedOwner; s.database.db.QueryRowContext, s.hostedMembership, s.hostedMutationMu.Lock, s.hostedMutationMu.Unlock, s.hostedProviderOrganization, s.storeHostedMember
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_dedicated / github,native / hub application service
- Availability: hosted_shared / github,native / hub application service
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: this non-operator source site → not_applicable

Sources: [POST /internal/v1/owner/bootstrap](../internal/hubserver/hosted_shared.go#L192)
## hubserver.change_hosted_grant

Change hosted grant

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3344.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `organization.change_hosted_grant` — Bounded changeHostedGrantRequest: organization, member; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → changeHostedGrantResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role hosted owner/admin; owner-only restrictions for owner membership/role changes; invitation acceptance bound to invited identity; credential hosted browser session; bearer credentials refused by session-only APIs; CSRF for mutation; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.changeHostedGrantJSON; s.hostedAdministrator, s.hostedGrant, s.hostedMemberByID
- Extraction: Extract hubserver.changeHostedGrantJSON application inputs/results and validation from Echo; reuse s.hostedAdministrator, s.hostedGrant, s.hostedMemberByID. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=true; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_dedicated / github,native / hub application service
- Availability: hosted_shared / github,native / hub application service
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: mutation of access, billing, deletion, cancellation or material external state → operator

Sources: [PUT /api/v2/organizations/:organization/members/:member/grants](../internal/hubserver/hosted_org_api.go#L39), [POST /organization/grants](../internal/hubserver/hosted_ui.go#L47), [internal/web/templates/hosted.templ:257](../internal/web/templates/hosted.templ#L257), [internal/web/templates/hosted.templ:257](../internal/web/templates/hosted.templ#L257), [web/conversation/src/app/account/api.ts:191](../web/conversation/src/app/account/api.ts#L191)
## hubserver.change_hosted_role

Change hosted role

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3344.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `organization.change_hosted_role` — Bounded changeHostedRoleRequest: organization, member; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → changeHostedRoleResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role hosted owner/admin; owner-only restrictions for owner membership/role changes; invitation acceptance bound to invited identity; credential hosted browser session; bearer credentials refused by session-only APIs; CSRF for mutation; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.changeHostedRoleJSON; s.config.Hosted.Provider.SetMembershipRole, s.config.now, s.database.db.ExecContext, s.hostedAdministrator, s.hostedManagedMember
- Extraction: Extract hubserver.changeHostedRoleJSON application inputs/results and validation from Echo; reuse s.config.Hosted.Provider.SetMembershipRole, s.config.now, s.database.db.ExecContext, s.hostedAdministrator, s.hostedManagedMember. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=true; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_dedicated / github,native / hub application service
- Availability: hosted_shared / github,native / hub application service
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: mutation of access, billing, deletion, cancellation or material external state → operator

Sources: [PUT /api/v2/organizations/:organization/members/:member/role](../internal/hubserver/hosted_org_api.go#L38), [POST /organization/members/:member/role](../internal/hubserver/hosted_ui.go#L46), [internal/web/templates/hosted.templ:239](../internal/web/templates/hosted.templ#L239), [internal/web/templates/hosted.templ:239](../internal/web/templates/hosted.templ#L239), [web/conversation/src/app/account/api.ts:179](../web/conversation/src/app/account/api.ts#L179)
## hubserver.change_native_dependency

Change native dependency

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3341.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `work_commands.change_native_dependency` — Bounded changeNativeDependencyRequest: organization, project, item; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → changeNativeDependencyResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.changeNativeDependency; s.nativeMutation
- Extraction: Extract hubserver.changeNativeDependency application inputs/results and validation from Echo; reuse s.nativeMutation. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=false; idempotent=false; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [POST /api/v2/organizations/:organization/projects/:project/work-items/:item/dependencies](../internal/hubserver/native_api.go#L133), [web/conversation/src/app/work/lib/workHttp.ts:581](../web/conversation/src/app/work/lib/workHttp.ts#L581)
## hubserver.change_viewed_files

Change viewed files

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3347.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `changes_artifacts.change_viewed_files` — Bounded changeViewedFilesRequest: organization, project, item, change, version; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → changeViewedFilesResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.changeViewedFiles; s.database.db.QueryContext
- Extraction: Extract hubserver.changeViewedFiles application inputs/results and validation from Echo; reuse s.database.db.QueryContext. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v2/organizations/:organization/projects/:project/work-items/:item/changes/:change/versions/:version/viewed-files](../internal/hubserver/changes.go#L82)
## hubserver.change_work_item_dependency

Change work item dependency

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3341.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `work_commands.change_work_item_dependency` — Bounded changeWorkItemDependencyRequest: id; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → changeWorkItemDependencyResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential hub worker/operator/admin per registration; native-only credentials cannot call legacy v1; hosted sessions cannot call outside nativeBase; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.changeWorkItemDependency; s.database.changeDependency
- Extraction: Extract hubserver.changeWorkItemDependency application inputs/results and validation from Echo; reuse s.database.changeDependency. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=false; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / hub application service
- Availability: hosted_dedicated / github,native / hub application service — unavailable: Current requireAPIScope refuses hosted-session calls outside nativeBase and rejects global credentials; use the corresponding native/hosted application operation. This row remains pending parity work.
- Availability: hosted_shared / github,native / hub application service — unavailable: Current requireAPIScope refuses hosted-session calls outside nativeBase and rejects global credentials; use the corresponding native/hosted application operation. This row remains pending parity work.
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [POST /api/v1/work-items/:id/dependencies](../internal/hubserver/api_http.go#L69)
## hubserver.change_work_item_order

Change work item order

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3341.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `work_commands.change_work_item_order` — Bounded changeWorkItemOrderRequest: id; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → changeWorkItemOrderResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential hub worker/operator/admin per registration; native-only credentials cannot call legacy v1; hosted sessions cannot call outside nativeBase; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.changeWorkItemOrder; s.database.changeQueueOrder
- Extraction: Extract hubserver.changeWorkItemOrder application inputs/results and validation from Echo; reuse s.database.changeQueueOrder. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=false; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / hub application service
- Availability: hosted_dedicated / github,native / hub application service — unavailable: Current requireAPIScope refuses hosted-session calls outside nativeBase and rejects global credentials; use the corresponding native/hosted application operation. This row remains pending parity work.
- Availability: hosted_shared / github,native / hub application service — unavailable: Current requireAPIScope refuses hosted-session calls outside nativeBase and rejects global credentials; use the corresponding native/hosted application operation. This row remains pending parity work.
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [POST /api/v1/work-items/:id/order](../internal/hubserver/api_http.go#L71)
## hubserver.change_work_item_priority

Change work item priority

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3341.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `work_commands.change_work_item_priority` — Bounded changeWorkItemPriorityRequest: id; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → changeWorkItemPriorityResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential hub worker/operator/admin per registration; native-only credentials cannot call legacy v1; hosted sessions cannot call outside nativeBase; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.changeWorkItemPriority; s.commitOutbox, s.database.workItemRepositoryID
- Extraction: Extract hubserver.changeWorkItemPriority application inputs/results and validation from Echo; reuse s.commitOutbox, s.database.workItemRepositoryID. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=false; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / hub application service
- Availability: hosted_dedicated / github,native / hub application service — unavailable: Current requireAPIScope refuses hosted-session calls outside nativeBase and rejects global credentials; use the corresponding native/hosted application operation. This row remains pending parity work.
- Availability: hosted_shared / github,native / hub application service — unavailable: Current requireAPIScope refuses hosted-session calls outside nativeBase and rejects global credentials; use the corresponding native/hosted application operation. This row remains pending parity work.
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [POST /api/v1/work-items/:id/priority](../internal/hubserver/api_http.go#L70)
## hubserver.change_work_item_workflow

Change work item workflow

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3341.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `work_commands.change_work_item_workflow` — Bounded changeWorkItemWorkflowRequest: id; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → changeWorkItemWorkflowResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential hub worker/operator/admin per registration; native-only credentials cannot call legacy v1; hosted sessions cannot call outside nativeBase; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.changeWorkItemWorkflow; s.ChangeWorkflowState, s.database.workItemRepositoryID
- Extraction: Extract hubserver.changeWorkItemWorkflow application inputs/results and validation from Echo; reuse s.ChangeWorkflowState, s.database.workItemRepositoryID. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Request governed transition; orchestrator alone writes tracker lane state (INV-1); Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=true; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / hub application service
- Availability: hosted_dedicated / github,native / hub application service — unavailable: Current requireAPIScope refuses hosted-session calls outside nativeBase and rejects global credentials; use the corresponding native/hosted application operation. This row remains pending parity work.
- Availability: hosted_shared / github,native / hub application service — unavailable: Current requireAPIScope refuses hosted-session calls outside nativeBase and rejects global credentials; use the corresponding native/hosted application operation. This row remains pending parity work.
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: non-terminal lane request → none
- Confirmation: terminal/destructive lane target → operator

Sources: [POST /api/v1/work-items/:id/workflow](../internal/hubserver/api_http.go#L68)
## hubserver.claim_native_issue

Claim native issue

- Audience: worker; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Worker protocol only: preserve worker credential, producer/lease and runner ownership; organization operator tools cannot impersonate a worker.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated worker/runner; credential worker scope; runner credential/lease where applicable; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.claimNativeIssue; s.database.claimNext, s.respondNativeLease
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: this non-operator source site → not_applicable

Sources: [POST /api/v2/organizations/:organization/projects/:project/claims](../internal/hubserver/native_api.go#L141)
## hubserver.claim_work_item

Claim work item

- Audience: worker; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Worker protocol only: preserve worker credential, producer/lease and runner ownership; organization operator tools cannot impersonate a worker.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated worker/runner; credential worker scope; runner credential/lease where applicable; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.claimWorkItem; s.database.claimNext, s.database.leasePolicyID
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / hub application service
- Availability: hosted_dedicated / github,native / hub application service
- Availability: hosted_shared / github,native / hub application service
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: this non-operator source site → not_applicable

Sources: [POST /api/v1/claims](../internal/hubserver/api_http.go#L64)
## hubserver.command_git_hub_batch

Command git hub batch

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3340.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `work_reads.command_git_hub_batch` — Bounded commandGitHubBatchRequest: organization, project; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → commandGitHubBatchResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.commandGitHubBatch; s.nativeMutation
- Extraction: Extract hubserver.commandGitHubBatch application inputs/results and validation from Echo; reuse s.nativeMutation. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=false; idempotent=false; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [POST /api/v2/organizations/:organization/projects/:project/onboarding/issue-intake](../internal/hubserver/onboarding.go#L21)
## hubserver.complete_hosted_login

Complete hosted login

- Audience: authentication; status: **excluded**; owner: digitaldrywood/detent#3336.
- Decision: Identity-provider login exchange belongs to connection setup, not model-controlled tool arguments. Meaningful organization/session commands are separate rows.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role public identity exchange/asset; credential none until connection authentication; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.completeHostedLogin; s.acceptHostedInvitationFor, s.bootstrapHostedMember, s.config.Hosted.Provider.Exchange, s.config.now, s.consumeHostedTransaction, s.database.db.ExecContext, s.hostedDenied, s.hostedMutationMu.Lock, s.hostedMutationMu.Unlock
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Current handler authority checks: s.hostedSession(c)
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_dedicated / github,native / hub application service
- Availability: hosted_shared / github,native / hub application service
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: connection authentication → connection

Sources: [GET /auth/oidc/callback](../internal/hubserver/hosted_ui.go#L26)
## hubserver.create_api_token

Create a p i token

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3344.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `organization.create_api_token` — Bounded createAPITokenRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → createAPITokenResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role instance admin in unhosted hub; hosted owner/admin through requireHostedAdministration; credential hub worker/operator/admin per registration; native-only credentials cannot call legacy v1; hosted sessions cannot call outside nativeBase; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.createAPIToken; s.config.generateToken, s.config.newTokenID, s.database.currentTime, s.database.db.ExecContext
- Extraction: Extract hubserver.createAPIToken application inputs/results and validation from Echo; reuse s.config.generateToken, s.config.newTokenID, s.database.currentTime, s.database.db.ExecContext. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=false; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / hub application service
- Availability: hosted_dedicated / github,native / hub application service — unavailable: Current requireAPIScope refuses hosted-session calls outside nativeBase and rejects global credentials; use the corresponding native/hosted application operation. This row remains pending parity work.
- Availability: hosted_shared / github,native / hub application service — unavailable: Current requireAPIScope refuses hosted-session calls outside nativeBase and rejects global credentials; use the corresponding native/hosted application operation. This row remains pending parity work.
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [POST /api/v1/tokens](../internal/hubserver/api_http.go#L76)
## hubserver.create_api_token_maintenance

Create a p i token maintenance

- Audience: staff; status: **excluded**; owner: digitaldrywood/detent#3344.
- Decision: Platform/instance staff authority is stricter than organization operator authority. Preserve this restriction; no organization-operator tool grants staff powers.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role private loopback credential-maintenance instance admin; no hosted member, runner, support or native-only credential; credential staff session or dedicated private instance-admin credential; not an organization operator credential; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.createAPIToken; s.config.generateToken, s.config.newTokenID, s.database.currentTime, s.database.db.ExecContext
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_dedicated / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / hub application service
- Confirmation: this non-operator source site → not_applicable

Sources: [POST /api/v1/tokens](../internal/hubserver/credential_maintenance.go#L24)
## hubserver.create_change

Create change

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3347.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `changes_artifacts.create_change` — Bounded createChangeRequest: organization, project, item; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → createChangeResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.createChange; s.nativeMutation
- Extraction: Extract hubserver.createChange application inputs/results and validation from Echo; reuse s.nativeMutation. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=false; idempotent=false; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [POST /api/v2/organizations/:organization/projects/:project/work-items/:item/changes](../internal/hubserver/changes.go#L77)
## hubserver.create_conversation

Create conversation

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3346.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `conversations_workspaces.create_conversation` — Bounded createConversationRequest: organization, project; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → createConversationResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.createConversation; s.requireLinkAvailable
- Extraction: Extract hubserver.createConversation application inputs/results and validation from Echo; reuse s.requireLinkAvailable. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.; Current handler authority checks: s.requireLinkAvailable(ctx, tx, *record); err != nil
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=false; idempotent=false; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [POST /api/v2/organizations/:organization/projects/:project/conversations](../internal/hubserver/conversation_api.go#L37), [web/conversation/src/runtime/rpc/http.ts:425](../web/conversation/src/runtime/rpc/http.ts#L425)
## hubserver.create_hosted_organization

Create hosted organization

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3344.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `organization.create_hosted_organization` — Bounded createHostedOrganizationRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → createHostedOrganizationResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential hosted browser session; bearer credentials refused by session-only APIs; CSRF for mutation; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.createHostedOrganization; s.config.Hosted.Provider.CreateMembership, s.config.Hosted.Provider.CreateOrganization, s.database.db.ExecContext, s.database.db.QueryRowContext, s.hostedProviderOrganization, s.storeHostedMember
- Extraction: Extract hubserver.createHostedOrganization application inputs/results and validation from Echo; reuse s.config.Hosted.Provider.CreateMembership, s.config.Hosted.Provider.CreateOrganization, s.database.db.ExecContext, s.database.db.QueryRowContext, s.hostedProviderOrganization, s.storeHostedMember. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.; Current handler authority checks: s.hostedSession(c)
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=true; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_dedicated / github,native / hub application service
- Availability: hosted_shared / github,native / hub application service
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: create an organization with external identity or billing effects → operator

Sources: [POST /organization/create](../internal/hubserver/hosted_ui.go#L41), [internal/web/templates/hosted.templ:162](../internal/web/templates/hosted.templ#L162), [internal/web/templates/hosted.templ:162](../internal/web/templates/hosted.templ#L162)
## hubserver.create_hosted_project

Create hosted project

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3342.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `projects.create_hosted_project` — Bounded createHostedProjectRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → createHostedProjectResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential hosted browser session; bearer credentials refused by session-only APIs; CSRF for mutation; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.createHostedProject; s.createHostedProjectRecord, s.hostedAdministrator
- Extraction: Extract hubserver.createHostedProject application inputs/results and validation from Echo; reuse s.createHostedProjectRecord, s.hostedAdministrator. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=false; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_dedicated / github,native / hub application service
- Availability: hosted_shared / github,native / hub application service
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [POST /projects](../internal/hubserver/hosted_ui.go#L48), [internal/web/templates/board.templ:183](../internal/web/templates/board.templ#L183), [internal/web/templates/board.templ:181](../internal/web/templates/board.templ#L181), [internal/web/templates/hosted.templ:204](../internal/web/templates/hosted.templ#L204), [internal/web/templates/hosted.templ:204](../internal/web/templates/hosted.templ#L204), [internal/web/templates/native_work.templ:13](../internal/web/templates/native_work.templ#L13), [web/conversation/src/app/account/api.ts:219](../web/conversation/src/app/account/api.ts#L219), [web/conversation/src/app/account/api.ts:221](../web/conversation/src/app/account/api.ts#L221)
## hubserver.create_hosted_project_json

Create hosted project j s o n

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3342.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `projects.create_hosted_project_json` — Bounded createHostedProjectJSONRequest: organization; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → createHostedProjectJSONResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role instance admin in unhosted hub; hosted owner/admin through requireHostedAdministration; credential hosted browser session; bearer credentials refused by session-only APIs; CSRF for mutation; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.createHostedProjectJSON; s.createHostedProjectRecord
- Extraction: Extract hubserver.createHostedProjectJSON application inputs/results and validation from Echo; reuse s.createHostedProjectRecord. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=false; idempotent=false; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [POST /api/v2/organizations/:organization/projects](../internal/hubserver/native_api.go#L118), [web/conversation/src/app/account/Organization.tsx:123](../web/conversation/src/app/account/Organization.tsx#L123), [web/conversation/src/app/account/Organization.tsx:121](../web/conversation/src/app/account/Organization.tsx#L121), [web/conversation/src/app/projects/NewProject.tsx:148](../web/conversation/src/app/projects/NewProject.tsx#L148), [web/conversation/src/app/projects/NewProject.tsx:145](../web/conversation/src/app/projects/NewProject.tsx#L145)
## hubserver.create_native_comment

Create native comment

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3341.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `work_commands.create_native_comment` — Bounded createNativeCommentRequest: organization, project, item; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → createNativeCommentResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.createNativeComment; s.nativeMutation
- Extraction: Extract hubserver.createNativeComment application inputs/results and validation from Echo; reuse s.nativeMutation. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=false; idempotent=false; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [POST /api/v2/organizations/:organization/projects/:project/work-items/:item/comments](../internal/hubserver/native_api.go#L135), [web/conversation/src/app/work/components/ActivityFeed.tsx:114](../web/conversation/src/app/work/components/ActivityFeed.tsx#L114), [web/conversation/src/app/work/components/ActivityFeed.tsx:112](../web/conversation/src/app/work/components/ActivityFeed.tsx#L112), [web/conversation/src/app/work/lib/workHttp.ts:602](../web/conversation/src/app/work/lib/workHttp.ts#L602)
## hubserver.create_native_issue

Create native issue

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3341.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `work_commands.create_native_issue` — Bounded createNativeIssueRequest: organization, project; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → createNativeIssueResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.createNativeIssue; s.nativeMutation
- Extraction: Extract hubserver.createNativeIssue application inputs/results and validation from Echo; reuse s.nativeMutation. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=false; idempotent=false; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [POST /api/v2/organizations/:organization/projects/:project/work-items](../internal/hubserver/native_api.go#L126), [web/conversation/src/app/account/api.ts:313](../web/conversation/src/app/account/api.ts#L313), [web/conversation/src/app/components/HandoffForm.tsx:161](../web/conversation/src/app/components/HandoffForm.tsx#L161), [web/conversation/src/app/components/HandoffForm.tsx:158](../web/conversation/src/app/components/HandoffForm.tsx#L158), [web/conversation/src/app/work/NewIssue.tsx:137](../web/conversation/src/app/work/NewIssue.tsx#L137), [web/conversation/src/app/work/NewIssue.tsx:134](../web/conversation/src/app/work/NewIssue.tsx#L134)
## hubserver.create_native_organization

Create native organization

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3344.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `organization.create_native_organization` — Bounded createNativeOrganizationRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → createNativeOrganizationResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role instance admin in unhosted hub; hosted owner/admin through requireHostedAdministration; credential admin; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.createNativeOrganization; s.config.now, s.database.db.ExecContext
- Extraction: Extract hubserver.createNativeOrganization application inputs/results and validation from Echo; reuse s.config.now, s.database.db.ExecContext. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=false; idempotent=false; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [POST /api/v2/organizations](../internal/hubserver/native_api.go#L116)
## hubserver.create_native_project

Create native project

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3342.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `projects.create_native_project` — Bounded createNativeProjectRequest: organization; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → createNativeProjectResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role instance admin in unhosted hub; hosted owner/admin through requireHostedAdministration; credential admin; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.createNativeProject; s.nativeMutation
- Extraction: Extract hubserver.createNativeProject application inputs/results and validation from Echo; reuse s.nativeMutation. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=false; idempotent=false; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [POST /api/v2/organizations/:organization/projects](../internal/hubserver/native_api.go#L120)
## hubserver.create_project_action

Create project action

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3346.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `conversations_workspaces.create_project_action` — Bounded createProjectActionRequest: organization, project; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → createProjectActionResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.createProjectAction; s.nativeMutationStatus
- Extraction: Extract hubserver.createProjectAction application inputs/results and validation from Echo; reuse s.nativeMutationStatus. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=false; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [POST /api/v2/organizations/:organization/projects/:project/actions](../internal/hubserver/native_api.go#L170), [web/conversation/src/app/work/lib/workHttp.ts:698](../web/conversation/src/app/work/lib/workHttp.ts#L698), [web/conversation/src/components/ProjectScriptsControl.tsx:303](../web/conversation/src/components/ProjectScriptsControl.tsx#L303)
## hubserver.create_project_action_run

Create project action run

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3346.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `conversations_workspaces.create_project_action_run` — Bounded createProjectActionRunRequest: organization, project, action; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → createProjectActionRunResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.createProjectActionRun; s.nativeMutationStatus, s.requireWorkspaces, service.queueActionRun
- Extraction: Extract hubserver.createProjectActionRun application inputs/results and validation from Echo; reuse s.nativeMutationStatus, s.requireWorkspaces, service.queueActionRun. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.; Current handler authority checks: s.requireWorkspaces()
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=false; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [POST /api/v2/organizations/:organization/projects/:project/actions/:action/runs](../internal/hubserver/native_api.go#L174), [web/conversation/src/app/work/lib/workHttp.ts:718](../web/conversation/src/app/work/lib/workHttp.ts#L718)
## hubserver.create_runner_enrollment

Create runner enrollment

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3343.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `runs_fleet.create_runner_enrollment` — Bounded createRunnerEnrollmentRequest: organization; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → createRunnerEnrollmentResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role instance admin in unhosted hub; hosted owner/admin through requireHostedAdministration; credential admin; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.createRunnerEnrollment; s.config.generateToken, s.runnerTransaction
- Extraction: Extract hubserver.createRunnerEnrollment application inputs/results and validation from Echo; reuse s.config.generateToken, s.runnerTransaction. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=false; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / hub application service
- Availability: hosted_dedicated / github,native / hub application service
- Availability: hosted_shared / github,native / hub application service
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [POST /api/v2/organizations/:organization/runner-enrollments](../internal/hubserver/runner_enrollment.go#L25), [web/conversation/src/app/account/api.ts:339](../web/conversation/src/app/account/api.ts#L339), [web/conversation/src/app/fleet/RunnersSection.tsx:166](../web/conversation/src/app/fleet/RunnersSection.tsx#L166), [web/conversation/src/app/fleet/RunnersSection.tsx:166](../web/conversation/src/app/fleet/RunnersSection.tsx#L166), [web/conversation/src/app/fleet/RunnersSection.tsx:166](../web/conversation/src/app/fleet/RunnersSection.tsx#L166)
## hubserver.create_workspace

Create workspace

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3346.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `conversations_workspaces.create_workspace` — Bounded createWorkspaceRequest: organization, project; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → createWorkspaceResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.createWorkspace; s.nativeMutationStatus, s.requireWorkspaces, service.committed, service.openWorkspace
- Extraction: Extract hubserver.createWorkspace application inputs/results and validation from Echo; reuse s.nativeMutationStatus, s.requireWorkspaces, service.committed, service.openWorkspace. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.; Current handler authority checks: s.requireWorkspaces()
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=false; idempotent=false; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [POST /api/v2/organizations/:organization/projects/:project/workspaces](../internal/hubserver/native_api.go#L157), [web/conversation/src/app/work/lib/workHttp.ts:676](../web/conversation/src/app/work/lib/workHttp.ts#L676)
## hubserver.cutover_project

Cutover project

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3342.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `projects.cutover_project` — Bounded cutoverProjectRequest: organization, project; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → cutoverProjectResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role instance admin in unhosted hub; hosted owner/admin through requireHostedAdministration; credential admin; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.cutoverProject; s.nativeMutation
- Extraction: Extract hubserver.cutoverProject application inputs/results and validation from Echo; reuse s.nativeMutation. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=true; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: mutation of access, billing, deletion, cancellation or material external state → operator

Sources: [POST /api/v2/organizations/:organization/projects/:project/integration/cutover](../internal/hubserver/integration.go#L151)
## hubserver.delete_conversation_attachment

Delete conversation attachment

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3346.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `conversations_workspaces.delete_conversation_attachment` — Bounded deleteConversationAttachmentRequest: organization, project, conversation, attachment; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → deleteConversationAttachmentResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.deleteConversationAttachment; service.loadConversation, service.requireActorAuthority, service.store.deleteAttachment, service.store.readAttachment, service.transact
- Extraction: Extract hubserver.deleteConversationAttachment application inputs/results and validation from Echo; reuse service.loadConversation, service.requireActorAuthority, service.store.deleteAttachment, service.store.readAttachment, service.transact. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=true; idempotent=false; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: mutation of access, billing, deletion, cancellation or material external state → operator

Sources: [DELETE /api/v2/organizations/:organization/projects/:project/conversations/:conversation/attachments/:attachment](../internal/hubserver/conversation_api.go#L52), [web/conversation/src/runtime/rpc/http.ts:496](../web/conversation/src/runtime/rpc/http.ts#L496)
## hubserver.delete_project_action

Delete project action

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3346.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `conversations_workspaces.delete_project_action` — Bounded deleteProjectActionRequest: organization, project, action; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → deleteProjectActionResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.deleteProjectAction; s.hubTransact, s.recheckHostedMutation
- Extraction: Extract hubserver.deleteProjectAction application inputs/results and validation from Echo; reuse s.hubTransact, s.recheckHostedMutation. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=true; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: mutation of access, billing, deletion, cancellation or material external state → operator

Sources: [DELETE /api/v2/organizations/:organization/projects/:project/actions/:action](../internal/hubserver/native_api.go#L172), [web/conversation/src/app/work/lib/workHttp.ts:715](../web/conversation/src/app/work/lib/workHttp.ts#L715)
## hubserver.delete_workspace

Delete workspace

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3346.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `conversations_workspaces.delete_workspace` — Bounded deleteWorkspaceRequest: organization, project, workspace; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → deleteWorkspaceResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.deleteWorkspace; s.hubTransact, s.recheckHostedMutation, s.requireWorkspaces, service.committed, service.endWorkspace, service.readWorkspaceForActor
- Extraction: Extract hubserver.deleteWorkspace application inputs/results and validation from Echo; reuse s.hubTransact, s.recheckHostedMutation, s.requireWorkspaces, service.committed, service.endWorkspace, service.readWorkspaceForActor. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.; Current handler authority checks: s.requireWorkspaces()
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=true; idempotent=false; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: mutation of access, billing, deletion, cancellation or material external state → operator

Sources: [DELETE /api/v2/organizations/:organization/projects/:project/workspaces/:workspace](../internal/hubserver/native_api.go#L160), [web/conversation/src/app/work/lib/workHttp.ts:686](../web/conversation/src/app/work/lib/workHttp.ts#L686)
## hubserver.discuss_change

Discuss change

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3347.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `changes_artifacts.discuss_change` — Bounded discussChangeRequest: organization, project, item, change; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → discussChangeResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.discussChange; s.nativeMutation
- Extraction: Extract hubserver.discussChange application inputs/results and validation from Echo; reuse s.nativeMutation. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=false; idempotent=false; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [POST /api/v2/organizations/:organization/projects/:project/work-items/:item/changes/:change/discussion](../internal/hubserver/changes.go#L80), [web/conversation/src/app/work/lib/workHttp.ts:638](../web/conversation/src/app/work/lib/workHttp.ts#L638)
## hubserver.get_attempt_diff

Get attempt diff

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3340.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `work_reads.get_attempt_diff` — Bounded getAttemptDiffRequest: organization, project, attempt; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → getAttemptDiffResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.getAttemptDiff; handler-owned application validation/read/command
- Extraction: Extract hubserver.getAttemptDiff application inputs/results and validation from Echo; reuse the current handler-owned service logic. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v2/organizations/:organization/projects/:project/attempts/:attempt/diff](../internal/hubserver/native_api.go#L151), [web/conversation/src/app/work/lib/workHttp.ts:616](../web/conversation/src/app/work/lib/workHttp.ts#L616)
## hubserver.get_change

Get change

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3347.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `changes_artifacts.get_change` — Bounded getChangeRequest: organization, project, item, change; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → getChangeResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.getChange; s.config.now, s.database.db.BeginTx
- Extraction: Extract hubserver.getChange application inputs/results and validation from Echo; reuse s.config.now, s.database.db.BeginTx. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v2/organizations/:organization/projects/:project/work-items/:item/changes/:change](../internal/hubserver/changes.go#L78), [web/conversation/src/app/work/lib/workHttp.ts:610](../web/conversation/src/app/work/lib/workHttp.ts#L610), [web/conversation/src/app/components/surfaces/DiffSurface.tsx:426](../web/conversation/src/app/components/surfaces/DiffSurface.tsx#L426), [web/conversation/src/app/work/ChangeRequestPage.tsx:384](../web/conversation/src/app/work/ChangeRequestPage.tsx#L384)
## hubserver.get_change_review_policy

Get change review policy

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3347.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `changes_artifacts.get_change_review_policy` — Bounded getChangeReviewPolicyRequest: organization, project; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → getChangeReviewPolicyResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.getChangeReviewPolicy; handler-owned application validation/read/command
- Extraction: Extract hubserver.getChangeReviewPolicy application inputs/results and validation from Echo; reuse the current handler-owned service logic. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v2/organizations/:organization/projects/:project/change-review-policy](../internal/hubserver/changes.go#L74)
## hubserver.get_conversation

Get conversation

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3346.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `conversations_workspaces.get_conversation` — Bounded getConversationRequest: organization, project, conversation; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → getConversationResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.getConversation; service.loadConversation, service.store.listMessages, service.store.listSnapshotQuestions, service.transact
- Extraction: Extract hubserver.getConversation application inputs/results and validation from Echo; reuse service.loadConversation, service.store.listMessages, service.store.listSnapshotQuestions, service.transact. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v2/organizations/:organization/projects/:project/conversations/:conversation](../internal/hubserver/conversation_api.go#L40), [web/conversation/src/runtime/rpc/http.ts:408](../web/conversation/src/runtime/rpc/http.ts#L408)
## hubserver.get_conversation_attachment

Get conversation attachment

- Audience: worker; status: **excluded**; owner: digitaldrywood/detent#3346.
- Decision: Worker-only attachment-byte endpoint registered with requireConversationScope(apiScopeWorker); operator upload/delete and typed attachment references remain separate.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role worker; credential worker conversation scope; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.getConversationAttachment; service.loadConversation, service.store.readAttachment, service.store.readAttachmentContent
- Extraction: Extract hubserver.getConversationAttachment application inputs/results and validation from Echo; reuse service.loadConversation, service.store.readAttachment, service.store.readAttachmentContent. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v2/organizations/:organization/projects/:project/conversations/:conversation/attachments/:attachment](../internal/hubserver/conversation_api.go#L51)
## hubserver.get_cutover_receipt

Get cutover receipt

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3342.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `projects.get_cutover_receipt` — Bounded getCutoverReceiptRequest: organization, project; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → getCutoverReceiptResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.getCutoverReceipt; s.database.db.QueryRowContext
- Extraction: Extract hubserver.getCutoverReceipt application inputs/results and validation from Echo; reuse s.database.db.QueryRowContext. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v2/organizations/:organization/projects/:project/integration/cutover](../internal/hubserver/integration.go#L152)
## hubserver.get_git_hub_batch

Get git hub batch

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3340.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `work_reads.get_git_hub_batch` — Bounded getGitHubBatchRequest: organization, project; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → getGitHubBatchResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.getGitHubBatch; handler-owned application validation/read/command
- Extraction: Extract hubserver.getGitHubBatch application inputs/results and validation from Echo; reuse the current handler-owned service logic. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v2/organizations/:organization/projects/:project/onboarding/issue-intake](../internal/hubserver/onboarding.go#L20), [web/conversation/src/app/account/IssueIntake.tsx:65](../web/conversation/src/app/account/IssueIntake.tsx#L65)
## hubserver.get_git_hub_import

Get git hub import

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3342.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `projects.get_git_hub_import` — Bounded getGitHubImportRequest: organization, project, import; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → getGitHubImportResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.getGitHubImport; handler-owned application validation/read/command
- Extraction: Extract hubserver.getGitHubImport application inputs/results and validation from Echo; reuse the current handler-owned service logic. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v2/organizations/:organization/projects/:project/imports/:import](../internal/hubserver/integration.go#L154)
## hubserver.get_native_issue

Get native issue

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3340.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `work_reads.get_native_issue` — Bounded getNativeIssueRequest: organization, project, item; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → getNativeIssueResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.getNativeIssue; handler-owned application validation/read/command
- Extraction: Extract hubserver.getNativeIssue application inputs/results and validation from Echo; reuse the current handler-owned service logic. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v2/organizations/:organization/projects/:project/work-items/:item](../internal/hubserver/native_api.go#L127), [web/conversation/src/app/work/lib/workHttp.ts:552](../web/conversation/src/app/work/lib/workHttp.ts#L552), [web/conversation/src/app/components/TimelineCards.tsx:153](../web/conversation/src/app/components/TimelineCards.tsx#L153), [web/conversation/src/app/work/IssuePage.tsx:989](../web/conversation/src/app/work/IssuePage.tsx#L989)
## hubserver.get_native_project

Get native project

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3342.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `projects.get_native_project` — Bounded getNativeProjectRequest: organization, project; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → getNativeProjectResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.getNativeProject; handler-owned application validation/read/command
- Extraction: Extract hubserver.getNativeProject application inputs/results and validation from Echo; reuse the current handler-owned service logic. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v2/organizations/:organization/projects/:project](../internal/hubserver/native_api.go#L123), [web/conversation/src/app/work/lib/workHttp.ts:535](../web/conversation/src/app/work/lib/workHttp.ts#L535)
## hubserver.get_native_version

Get native version

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3340.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `work_reads.get_native_version` — Bounded getNativeVersionRequest: organization, project, item, comment, revision; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → getNativeVersionResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.getNativeVersion; s.database.db.QueryRowContext
- Extraction: Extract hubserver.getNativeVersion application inputs/results and validation from Echo; reuse s.database.db.QueryRowContext. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v2/organizations/:organization/projects/:project/work-items/:item/comments/:comment/versions/:revision](../internal/hubserver/native_api.go#L140), [GET /api/v2/organizations/:organization/projects/:project/work-items/:item/versions/:revision](../internal/hubserver/native_api.go#L139)
## hubserver.get_onboarding

Get onboarding

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3342.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `projects.get_onboarding` — Bounded getOnboardingRequest: organization, project; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → getOnboardingResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.getOnboarding; s.projectOnboarding
- Extraction: Extract hubserver.getOnboarding application inputs/results and validation from Echo; reuse s.projectOnboarding. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v2/organizations/:organization/projects/:project/onboarding](../internal/hubserver/onboarding.go#L27)
## hubserver.get_project_action_run

Get project action run

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3346.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `conversations_workspaces.get_project_action_run` — Bounded getProjectActionRunRequest: organization, project, action, run; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → getProjectActionRunResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.getProjectActionRun; s.readActionRunForRequest
- Extraction: Extract hubserver.getProjectActionRun application inputs/results and validation from Echo; reuse s.readActionRunForRequest. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v2/organizations/:organization/projects/:project/actions/:action/runs/:run](../internal/hubserver/native_api.go#L175), [web/conversation/src/app/work/lib/workHttp.ts:725](../web/conversation/src/app/work/lib/workHttp.ts#L725)
## hubserver.get_project_action_run_output

Get project action run output

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3346.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `conversations_workspaces.get_project_action_run_output` — Bounded getProjectActionRunOutputRequest: organization, project, action, run; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → getProjectActionRunOutputResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.getProjectActionRunOutput; s.readActionRunForRequest
- Extraction: Extract hubserver.getProjectActionRunOutput application inputs/results and validation from Echo; reuse s.readActionRunForRequest. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v2/organizations/:organization/projects/:project/actions/:action/runs/:run/output](../internal/hubserver/native_api.go#L176)
## hubserver.get_project_integration

Get project integration

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3342.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `projects.get_project_integration` — Bounded getProjectIntegrationRequest: organization, project; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → getProjectIntegrationResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.getProjectIntegration; handler-owned application validation/read/command
- Extraction: Extract hubserver.getProjectIntegration application inputs/results and validation from Echo; reuse the current handler-owned service logic. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v2/organizations/:organization/projects/:project/integration](../internal/hubserver/integration.go#L148), [web/conversation/src/app/account/api.ts:235](../web/conversation/src/app/account/api.ts#L235)
## hubserver.get_project_policy

Get project policy

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3342.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `projects.get_project_policy` — Bounded getProjectPolicyRequest: owner, repo; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → getProjectPolicyResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential hub worker/operator/admin per registration; native-only credentials cannot call legacy v1; hosted sessions cannot call outside nativeBase; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.getProjectPolicy; s.policyScope
- Extraction: Extract hubserver.getProjectPolicy application inputs/results and validation from Echo; reuse s.policyScope. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / hub application service
- Availability: hosted_dedicated / github,native / hub application service — unavailable: Current requireAPIScope refuses hosted-session calls outside nativeBase and rejects global credentials; use the corresponding native/hosted application operation. This row remains pending parity work.
- Availability: hosted_shared / github,native / hub application service — unavailable: Current requireAPIScope refuses hosted-session calls outside nativeBase and rejects global credentials; use the corresponding native/hosted application operation. This row remains pending parity work.
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v1/repositories/:owner/:repo/policy](../internal/hubserver/api_http.go#L57), [GET /api/v2/organizations/:organization/projects/:project/policy](../internal/hubserver/native_api.go#L110), [web/conversation/src/app/account/api.ts:257](../web/conversation/src/app/account/api.ts#L257)
## hubserver.get_runner_identity

Get runner identity

- Audience: worker; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Worker protocol only: preserve worker credential, producer/lease and runner ownership; organization operator tools cannot impersonate a worker.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated worker/runner; credential worker scope; runner credential/lease where applicable; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.getRunnerIdentity; handler-owned application validation/read/command
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / hub application service
- Availability: hosted_dedicated / github,native / hub application service
- Availability: hosted_shared / github,native / hub application service
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: this non-operator source site → not_applicable

Sources: [GET /api/v2/organizations/:organization/runners/:runner](../internal/hubserver/runner_enrollment.go#L28)
## hubserver.get_runner_routing

Get runner routing

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3343.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `runs_fleet.get_runner_routing` — Bounded getRunnerRoutingRequest: organization, runner; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → getRunnerRoutingResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.getRunnerRouting; s.config.now
- Extraction: Extract hubserver.getRunnerRouting application inputs/results and validation from Echo; reuse s.config.now. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / hub application service
- Availability: hosted_dedicated / github,native / hub application service
- Availability: hosted_shared / github,native / hub application service
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v2/organizations/:organization/runners/:runner/routing](../internal/hubserver/runner_enrollment.go#L33)
## hubserver.get_work_item

Get work item

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3340.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `work_reads.get_work_item` — Bounded getWorkItemRequest: id; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → getWorkItemResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential hub worker/operator/admin per registration; native-only credentials cannot call legacy v1; hosted sessions cannot call outside nativeBase; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.getWorkItem; s.applyRepositorySyncHealth, s.database.latestWorkpad, s.database.workItemBody, s.database.workItemTimeline, s.tracker.GetWorkItems
- Extraction: Extract hubserver.getWorkItem application inputs/results and validation from Echo; reuse s.applyRepositorySyncHealth, s.database.latestWorkpad, s.database.workItemBody, s.database.workItemTimeline, s.tracker.GetWorkItems. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / hub application service
- Availability: hosted_dedicated / github,native / hub application service — unavailable: Current requireAPIScope refuses hosted-session calls outside nativeBase and rejects global credentials; use the corresponding native/hosted application operation. This row remains pending parity work.
- Availability: hosted_shared / github,native / hub application service — unavailable: Current requireAPIScope refuses hosted-session calls outside nativeBase and rejects global credentials; use the corresponding native/hosted application operation. This row remains pending parity work.
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v1/work-items/:id](../internal/hubserver/api_http.go#L63)
## hubserver.get_work_item_diff

Get work item diff

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3340.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `work_reads.get_work_item_diff` — Bounded getWorkItemDiffRequest: organization, project, item; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → getWorkItemDiffResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.getWorkItemDiff; handler-owned application validation/read/command
- Extraction: Extract hubserver.getWorkItemDiff application inputs/results and validation from Echo; reuse the current handler-owned service logic. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v2/organizations/:organization/projects/:project/work-items/:item/diff](../internal/hubserver/native_api.go#L152), [web/conversation/src/app/work/lib/workHttp.ts:590](../web/conversation/src/app/work/lib/workHttp.ts#L590), [web/conversation/src/app/adapters/surfaces.ts:256](../web/conversation/src/app/adapters/surfaces.ts#L256), [web/conversation/src/app/adapters/surfaces.ts:287](../web/conversation/src/app/adapters/surfaces.ts#L287)
## hubserver.get_workspace

Get workspace

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3346.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `conversations_workspaces.get_workspace` — Bounded getWorkspaceRequest: organization, project, workspace; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → getWorkspaceResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.getWorkspace; s.requireWorkspaces, service.presentWorkspace, service.readWorkspaceForActor
- Extraction: Extract hubserver.getWorkspace application inputs/results and validation from Echo; reuse s.requireWorkspaces, service.presentWorkspace, service.readWorkspaceForActor. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.; Current handler authority checks: s.requireWorkspaces()
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v2/organizations/:organization/projects/:project/workspaces/:workspace](../internal/hubserver/native_api.go#L159), [web/conversation/src/app/work/lib/workHttp.ts:674](../web/conversation/src/app/work/lib/workHttp.ts#L674)
## hubserver.get_workspace_terminal_recording

Get workspace terminal recording

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3346.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `conversations_workspaces.get_workspace_terminal_recording` — Bounded getWorkspaceTerminalRecordingRequest: organization, project, workspace, recording; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → getWorkspaceTerminalRecordingResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership recording creator or owner/admin; user-isolation recordings owners only; workspace belongs to project.
- Application: s.getWorkspaceTerminalRecording; s.requireWorkspaces, service.readWorkspaceForActor
- Extraction: Extract hubserver.getWorkspaceTerminalRecording application inputs/results and validation from Echo; reuse s.requireWorkspaces, service.readWorkspaceForActor. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.; Current handler authority checks: s.requireWorkspaces()
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v2/organizations/:organization/projects/:project/workspaces/:workspace/terminal-recordings/:recording](../internal/hubserver/native_api.go#L183)
## hubserver.github_request_counts

Github request counts

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3343.
- Decision: Expose the current instance-admin request-count read under the same administration authority; ordinary operators cannot invoke it.
- Tool: `runs_fleet.github_request_counts` — Authorized instance/organization context; bounded window; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → Typed bounded request counts with timestamps/freshness; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role requireInstanceAdmin: unhosted instance admin; hosted owner/admin through requireHostedAdministration; credential staff session or dedicated private instance-admin credential; not an organization operator credential; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.githubRequestCounts; s.config.GitHubRequestCounts
- Extraction: Extract githubRequestCounts authorized application read from HTTP and retain current request-accounting service.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / hub application service
- Availability: hosted_dedicated / github,native / hub application service
- Availability: hosted_shared / github,native / hub application service
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: this non-operator source site → not_applicable

Sources: [GET /api/v2/github/requests](../internal/hubserver/integration.go#L144)
## hubserver.github_webhook

Github webhook

- Audience: transport; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Exact transport/protocol plumbing site. Application payload operations are inventoried separately; do not expose an HTTP or relay proxy tool.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.githubWebhook; s.config.now, s.database.processWebhook, s.database.recordWebhook
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / hub application service
- Availability: hosted_dedicated / github,native / hub application service
- Availability: hosted_shared / github,native / hub application service
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: this non-operator source site → not_applicable

Sources: [POST /api/v1/webhooks/github](../internal/hubserver/api_http.go#L79)
## hubserver.grant_native_token

Grant native token

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3344.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `organization.grant_native_token` — Bounded grantNativeTokenRequest: id; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → grantNativeTokenResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role instance admin in unhosted hub; hosted owner/admin through requireHostedAdministration; credential admin; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.grantNativeToken; s.database.db.BeginTx
- Extraction: Extract hubserver.grantNativeToken application inputs/results and validation from Echo; reuse s.database.db.BeginTx. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=true; idempotent=false; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: mutation of access, billing, deletion, cancellation or material external state → operator

Sources: [POST /api/v2/tokens/:id/grants](../internal/hubserver/native_api.go#L122)
## hubserver.grant_native_token_maintenance

Grant native token maintenance

- Audience: staff; status: **excluded**; owner: digitaldrywood/detent#3344.
- Decision: Platform/instance staff authority is stricter than organization operator authority. Preserve this restriction; no organization-operator tool grants staff powers.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role private loopback credential-maintenance instance admin; no hosted member, runner, support or native-only credential; credential staff session or dedicated private instance-admin credential; not an organization operator credential; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.grantNativeToken; s.database.db.BeginTx
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_dedicated / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / hub application service
- Confirmation: this non-operator source site → not_applicable

Sources: [POST /api/v2/tokens/:id/grants](../internal/hubserver/credential_maintenance.go#L27)
## hubserver.health

Health

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3340.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `work_reads.health` — Bounded healthRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → healthResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential hub worker/operator/admin per registration; native-only credentials cannot call legacy v1; hosted sessions cannot call outside nativeBase; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.health; s.OutboxHealth, s.config.now, s.database.health, s.database.repositoryFreshness, s.ready.Load
- Extraction: Extract hubserver.health application inputs/results and validation from Echo; reuse s.OutboxHealth, s.config.now, s.database.health, s.database.repositoryFreshness, s.ready.Load. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / hub application service
- Availability: hosted_dedicated / github,native / hub application service — unavailable: Current requireAPIScope refuses hosted-session calls outside nativeBase and rejects global credentials; use the corresponding native/hosted application operation. This row remains pending parity work.
- Availability: hosted_shared / github,native / hub application service — unavailable: Current requireAPIScope refuses hosted-session calls outside nativeBase and rejects global credentials; use the corresponding native/hosted application operation. This row remains pending parity work.
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /health](../internal/hubserver/api_http.go#L61)
## hubserver.heartbeat_machine

Heartbeat machine

- Audience: worker; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Worker protocol only: preserve worker credential, producer/lease and runner ownership; organization operator tools cannot impersonate a worker.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated worker/runner; credential worker scope; runner credential/lease where applicable; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.heartbeatMachine; s.database.currentTime, s.database.db.ExecContext, s.database.machine
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / hub application service
- Availability: hosted_dedicated / github,native / hub application service
- Availability: hosted_shared / github,native / hub application service
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: this non-operator source site → not_applicable

Sources: [POST /api/v1/machines/:id/heartbeat](../internal/hubserver/api_http.go#L73)
## hubserver.heartbeat_native_machine

Heartbeat native machine

- Audience: worker; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Worker protocol only: preserve worker credential, producer/lease and runner ownership; organization operator tools cannot impersonate a worker.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated worker/runner; credential worker scope; runner credential/lease where applicable; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.heartbeatNativeMachine; s.runnerTransaction
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: this non-operator source site → not_applicable

Sources: [POST /api/v2/organizations/:organization/projects/:project/machines/:machine/heartbeat](../internal/hubserver/runner_enrollment.go#L37)
## hubserver.heartbeat_workspace_worker

Heartbeat workspace worker

- Audience: worker; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Worker protocol only: preserve worker credential, producer/lease and runner ownership; organization operator tools cannot impersonate a worker.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated worker/runner; credential worker scope; runner credential/lease where applicable; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.heartbeatWorkspaceWorker; s.hubTransact, s.requireWorkspaces, service.committed, service.heartbeat, service.loadWorkspaceForWorker
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Current handler authority checks: s.requireWorkspaces()
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: this non-operator source site → not_applicable

Sources: [POST /api/v2/organizations/:organization/projects/:project/workspaces/:workspace/worker/heartbeat](../internal/hubserver/native_api.go#L166)
## hubserver.hosted_artifact_allowances

Hosted artifact allowances

- Audience: staff; status: **excluded**; owner: digitaldrywood/detent#3345.
- Decision: Requires entitlementAdministrator/current configured platform administration identity. Organization owners/operators do not receive this staff-only power.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role configured entitlement administrator; not organization owner/admin; credential staff session or dedicated private instance-admin credential; not an organization operator credential; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.hostedArtifactAllowances; s.authenticateAPIRequest, s.config.now, s.database.db.BeginTx, s.database.hostedEntitlement
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / hub application service — unavailable: Hosted organization/billing/identity service is absent in an unhosted hub.
- Availability: hosted_dedicated / github,native / hub application service
- Availability: hosted_shared / github,native / hub application service
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: this non-operator source site → not_applicable

Sources: [POST /api/v2/organizations/:organization/artifact-allowances/:service](../internal/hubserver/hosted_ui.go#L54)
## hubserver.hosted_billing

Hosted billing

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3345.
- Decision: Expose the owner-only billing operation using hostedBillingOwner and shared billing intent/idempotency services.
- Tool: `billing_usage.hosted_billing` — Typed billing context and validated price/idempotency key for checkout; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → Typed bounded billing report or checkout/portal URL and request correlation; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role hosted organization owner only; reject support impersonation; credential staff session or dedicated private instance-admin credential; not an organization operator credential; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.hostedBilling; s.database.hostedMetadata, s.hostedBillingOwner, s.hostedUsage
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.; Current handler authority checks: s.hostedBillingOwner(c)
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / hub application service — unavailable: Hosted organization/billing/identity service is absent in an unhosted hub.
- Availability: hosted_dedicated / github,native / hub application service
- Availability: hosted_shared / github,native / hub application service
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: this non-operator source site → not_applicable

Sources: [GET /api/cloud/billing](../internal/hubserver/hosted_ui.go#L51), [internal/web/templates/hosted.templ:348](../internal/web/templates/hosted.templ#L348), [internal/web/templates/hosted_billing.templ:62](../internal/web/templates/hosted_billing.templ#L62)
## hubserver.hosted_billing_checkout

Hosted billing checkout

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3345.
- Decision: Expose the owner-only billing operation using hostedBillingOwner and shared billing intent/idempotency services.
- Tool: `billing_usage.hosted_billing_checkout` — Bounded hostedBillingCheckoutRequest: organization; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → hostedBillingCheckoutResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role hosted organization owner only; reject support impersonation; credential hosted browser session; bearer credentials refused by session-only APIs; CSRF for mutation; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.hostedBillingCheckout; s.database.readHostedBilling, s.ensureHostedCustomer, s.hostedBillingDestination, s.hostedBillingFailure, s.hostedBillingOwner, s.hostedBillingReturn, s.prepareHostedCheckout, s.saveHostedCheckout
- Extraction: Extract hubserver.hostedBillingCheckout application inputs/results and validation from Echo; reuse s.database.readHostedBilling, s.ensureHostedCustomer, s.hostedBillingDestination, s.hostedBillingFailure, s.hostedBillingOwner, s.hostedBillingReturn, s.prepareHostedCheckout, s.saveHostedCheckout. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.; Current handler authority checks: s.hostedBillingOwner(c); s.hostedBillingOwner(c); err != nil
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=true; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / hub application service — unavailable: Hosted organization/billing/identity service is absent in an unhosted hub.
- Availability: hosted_dedicated / github,native / hub application service
- Availability: hosted_shared / github,native / hub application service
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: mutation of access, billing, deletion, cancellation or material external state → operator

Sources: [POST /api/v2/organizations/:organization/billing/checkout](../internal/hubserver/hosted_ui.go#L37), [POST /organization/billing/checkout](../internal/hubserver/hosted_ui.go#L34), [internal/web/templates/hosted_billing.templ:32](../internal/web/templates/hosted_billing.templ#L32), [internal/web/templates/hosted_billing.templ:32](../internal/web/templates/hosted_billing.templ#L32), [web/conversation/src/app/account/api.ts:416](../web/conversation/src/app/account/api.ts#L416)
## hubserver.hosted_billing_export

Hosted billing export

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3345.
- Decision: Return the meaningful owner-only billing export as a typed result, using the same hostedBillingOwner and hostedBillingReport paths.
- Tool: `billing_usage.billing_export` — Authenticated organization context; bounded export selection; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → Typed hostedBillingReport with subscription, audit, identifiers and freshness; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role hosted organization owner; no support impersonation; credential hosted browser session; bearer credentials refused by session-only APIs; CSRF for mutation; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.hostedBillingExport; s.hostedBillingOwner, s.hostedBillingReport
- Extraction: Extract the authorized hostedBillingReport application read shared by export, JSON and browser handlers.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.; Current handler authority checks: s.hostedBillingOwner(c)
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / hub application service — unavailable: Hosted organization/billing/identity service is absent in an unhosted hub.
- Availability: hosted_dedicated / github,native / hub application service
- Availability: hosted_shared / github,native / hub application service
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: billing export read → none

Sources: [GET /api/cloud/billing/subscription](../internal/hubserver/hosted_ui.go#L40), [internal/web/templates/hosted_billing.templ:61](../internal/web/templates/hosted_billing.templ#L61)
## hubserver.hosted_billing_page

Hosted billing page

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3345.
- Decision: Expose the owner-only billing operation using hostedBillingOwner and shared billing intent/idempotency services.
- Tool: `billing_usage.hosted_billing_page` — Bounded hostedBillingPageRequest: organization; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → hostedBillingPageResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role hosted organization owner only; reject support impersonation; credential hosted browser session; bearer credentials refused by session-only APIs; CSRF for mutation; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.hostedBillingJSON; s.config.now, s.database.db.QueryRowContext, s.database.hostedBillingBinding, s.hostedBillingOwner, s.hostedBillingReport, s.hostedPriceLabel
- Extraction: Extract hubserver.hostedBillingJSON application inputs/results and validation from Echo; reuse s.config.now, s.database.db.QueryRowContext, s.database.hostedBillingBinding, s.hostedBillingOwner, s.hostedBillingReport, s.hostedPriceLabel. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.; Current handler authority checks: s.hostedBillingOwner(c)
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / hub application service — unavailable: Hosted organization/billing/identity service is absent in an unhosted hub.
- Availability: hosted_dedicated / github,native / hub application service
- Availability: hosted_shared / github,native / hub application service
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v2/organizations/:organization/billing](../internal/hubserver/hosted_ui.go#L36), [GET /organization/billing](../internal/hubserver/hosted_ui.go#L33), [internal/web/templates/hosted.templ:185](../internal/web/templates/hosted.templ#L185), [internal/web/templates/hosted.templ:347](../internal/web/templates/hosted.templ#L347), [web/conversation/src/app/account/api.ts:414](../web/conversation/src/app/account/api.ts#L414)
## hubserver.hosted_billing_portal

Hosted billing portal

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3345.
- Decision: Expose the owner-only billing operation using hostedBillingOwner and shared billing intent/idempotency services.
- Tool: `billing_usage.hosted_billing_portal` — Bounded hostedBillingPortalRequest: organization; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → hostedBillingPortalResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role hosted organization owner only; reject support impersonation; credential hosted browser session; bearer credentials refused by session-only APIs; CSRF for mutation; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.hostedBillingPortal; s.database.hostedBillingBinding, s.hostedBillingDestination, s.hostedBillingFailure, s.hostedBillingOwner, s.hostedBillingReturn, s.recordBillingAction
- Extraction: Extract hubserver.hostedBillingPortal application inputs/results and validation from Echo; reuse s.database.hostedBillingBinding, s.hostedBillingDestination, s.hostedBillingFailure, s.hostedBillingOwner, s.hostedBillingReturn, s.recordBillingAction. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.; Current handler authority checks: s.hostedBillingOwner(c)
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=true; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / hub application service — unavailable: Hosted organization/billing/identity service is absent in an unhosted hub.
- Availability: hosted_dedicated / github,native / hub application service
- Availability: hosted_shared / github,native / hub application service
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: mutation of access, billing, deletion, cancellation or material external state → operator

Sources: [POST /api/v2/organizations/:organization/billing/portal](../internal/hubserver/hosted_ui.go#L38), [POST /organization/billing/portal](../internal/hubserver/hosted_ui.go#L35), [internal/web/templates/hosted_billing.templ:43](../internal/web/templates/hosted_billing.templ#L43), [internal/web/templates/hosted_billing.templ:43](../internal/web/templates/hosted_billing.templ#L43), [web/conversation/src/app/account/api.ts:421](../web/conversation/src/app/account/api.ts#L421)
## hubserver.hosted_events

Hosted events

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3340.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `work_reads.hosted_events` — Bounded hostedEventsRequest: project; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → hostedEventsResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential hosted browser session; bearer credentials refused by session-only APIs; CSRF for mutation; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.hostedEvents; s.database.db.QueryRowContext, s.hostedCredential, s.requireHostedProject
- Extraction: Extract hubserver.hostedEvents application inputs/results and validation from Echo; reuse s.database.db.QueryRowContext, s.hostedCredential, s.requireHostedProject. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.; Current handler authority checks: s.hostedCredential(c); s.requireHostedProject(c.Request().Context(), s.database.db, initialScope, false); err != nil; s.requireHostedProject(c.Request().Context(), s.database.db, scope, false); err != nil
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / hub application service — unavailable: Hosted organization/billing/identity service is absent in an unhosted hub.
- Availability: hosted_dedicated / github,native / hub application service
- Availability: hosted_shared / github,native / hub application service
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /projects/:project/events](../internal/hubserver/hosted_ui.go#L49)
## hubserver.hosted_fleet

Hosted fleet

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3343.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `runs_fleet.hosted_fleet` — Bounded hostedFleetRequest: organization; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → hostedFleetResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential hosted browser session; bearer credentials refused by session-only APIs; CSRF for mutation; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.hostedFleet; s.database.db.QueryRowContext, s.hostedAllRunnerGrants, s.hostedCredential, s.hostedFleetRunners, s.hostedFleetUsage, s.hostedReadableProjects, s.hostedVisibleReservations
- Extraction: Extract hubserver.hostedFleet application inputs/results and validation from Echo; reuse s.database.db.QueryRowContext, s.hostedAllRunnerGrants, s.hostedCredential, s.hostedFleetRunners, s.hostedFleetUsage, s.hostedReadableProjects, s.hostedVisibleReservations. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.; Current handler authority checks: s.hostedCredential(c)
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / hub application service — unavailable: Hosted organization/billing/identity service is absent in an unhosted hub.
- Availability: hosted_dedicated / github,native / hub application service
- Availability: hosted_shared / github,native / hub application service
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v2/organizations/:organization/fleet](../internal/hubserver/hosted_org_api.go#L41)
## hubserver.hosted_landing

Hosted landing

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3340.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `work_reads.hosted_landing` — Bounded hostedLandingRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → hostedLandingResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential hosted browser session; bearer credentials refused by session-only APIs; CSRF for mutation; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.hostedLanding; s.appShell, s.hostedCredential, s.hostedHome
- Extraction: Extract hubserver.hostedLanding application inputs/results and validation from Echo; reuse s.appShell, s.hostedCredential, s.hostedHome. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.; Current handler authority checks: s.hostedCredential(c); err == nil
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / hub application service — unavailable: Hosted organization/billing/identity service is absent in an unhosted hub.
- Availability: hosted_dedicated / github,native / hub application service
- Availability: hosted_shared / github,native / hub application service
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /](../internal/hubserver/hosted_ui.go#L23)
## hubserver.hosted_metadata

Hosted metadata

- Audience: staff; status: **excluded**; owner: digitaldrywood/detent#3340.
- Decision: Platform/instance staff authority is stricter than organization operator authority. Preserve this restriction; no organization-operator tool grants staff powers.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role bootstrap admin token or configured staff email; reject support impersonation; credential staff session or dedicated private instance-admin credential; not an organization operator credential; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.hostedMetadata; s.authenticateAPIRequest, s.database.hostedMetadata, s.hostedUsage
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Current handler authority checks: s.hostedSession(c)
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / hub application service — unavailable: Hosted organization/billing/identity service is absent in an unhosted hub.
- Availability: hosted_dedicated / github,native / hub application service
- Availability: hosted_shared / github,native / hub application service
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: this non-operator source site → not_applicable

Sources: [GET /api/cloud/metadata](../internal/hubserver/hosted_ui.go#L50)
## hubserver.hosted_plan_page

Hosted plan page

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3345.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `billing_usage.hosted_plan_page` — Bounded hostedPlanPageRequest: organization; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → hostedPlanPageResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential hosted browser session; bearer credentials refused by session-only APIs; CSRF for mutation; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.hostedPlanJSON; s.config.now, s.database.hostedPlanUsage, s.hostedAdministrator, s.hostedCredential
- Extraction: Extract hubserver.hostedPlanJSON application inputs/results and validation from Echo; reuse s.config.now, s.database.hostedPlanUsage, s.hostedAdministrator, s.hostedCredential. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.; Current handler authority checks: s.hostedCredential(c); err != nil
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / hub application service — unavailable: Hosted organization/billing/identity service is absent in an unhosted hub.
- Availability: hosted_dedicated / github,native / hub application service
- Availability: hosted_shared / github,native / hub application service
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v2/organizations/:organization/plan](../internal/hubserver/hosted_org_api.go#L42), [GET /organization/plan](../internal/hubserver/hosted_ui.go#L32), [internal/web/templates/hosted.templ:188](../internal/web/templates/hosted.templ#L188), [internal/web/templates/hosted_billing.templ:60](../internal/web/templates/hosted_billing.templ#L60), [web/conversation/src/app/account/api.ts:413](../web/conversation/src/app/account/api.ts#L413)
## hubserver.hosted_plan_report

Hosted plan report

- Audience: staff; status: **excluded**; owner: digitaldrywood/detent#3345.
- Decision: Requires entitlementAdministrator/current configured platform administration identity. Organization owners/operators do not receive this staff-only power.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role configured entitlement administrator; not organization owner/admin; credential hosted browser session; bearer credentials refused by session-only APIs; CSRF for mutation; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.hostedPlanReport; s.database.hostedEntitlementReport, s.entitlementAdministrator
- Extraction: Extract hubserver.hostedPlanReport application inputs/results and validation from Echo; reuse s.database.hostedEntitlementReport, s.entitlementAdministrator. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Current handler authority checks: s.entitlementAdministrator(c)
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / hub application service — unavailable: Hosted organization/billing/identity service is absent in an unhosted hub.
- Availability: hosted_dedicated / github,native / hub application service
- Availability: hosted_shared / github,native / hub application service
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v2/organizations/:organization/entitlements](../internal/hubserver/hosted_ui.go#L52)
## hubserver.hosted_shared_billing_binding

Hosted shared billing binding

- Audience: transport; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Exact transport/protocol plumbing site. Application payload operations are inventoried separately; do not expose an HTTP or relay proxy tool.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role organization owner/admin for billing; current membership for plan visibility; credential hosted browser session; bearer credentials refused by session-only APIs; CSRF for mutation; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.hostedSharedBillingBinding; s.billing.mu.Lock, s.billing.mu.Unlock, s.billing.reconcile, s.config.now, s.database.db.QueryRowContext, s.database.hostedBillingBinding, s.database.readHostedBilling
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / hub application service — unavailable: Hosted organization/billing/identity service is absent in an unhosted hub.
- Availability: hosted_dedicated / github,native / hub application service
- Availability: hosted_shared / github,native / hub application service
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: this non-operator source site → not_applicable

Sources: [POST /internal/v1/billing/binding](../internal/hubserver/hosted_shared.go#L193)
## hubserver.hosted_shared_billing_event

Hosted shared billing event

- Audience: transport; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Exact transport/protocol plumbing site. Application payload operations are inventoried separately; do not expose an HTTP or relay proxy tool.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role organization owner/admin for billing; current membership for plan visibility; credential hosted browser session; bearer credentials refused by session-only APIs; CSRF for mutation; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.hostedSharedBillingEvent; s.config.now, s.database.db.ExecContext, s.database.hostedBillingBinding
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / hub application service — unavailable: Hosted organization/billing/identity service is absent in an unhosted hub.
- Availability: hosted_dedicated / github,native / hub application service
- Availability: hosted_shared / github,native / hub application service
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: this non-operator source site → not_applicable

Sources: [POST /internal/v1/billing/events](../internal/hubserver/hosted_shared.go#L194)
## hubserver.hosted_shared_health

Hosted shared health

- Audience: transport; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Exact transport/protocol plumbing site. Application payload operations are inventoried separately; do not expose an HTTP or relay proxy tool.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential hosted browser session; bearer credentials refused by session-only APIs; CSRF for mutation; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.hostedSharedHealth; s.database.health, s.ready.Load
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / hub application service — unavailable: Hosted organization/billing/identity service is absent in an unhosted hub.
- Availability: hosted_dedicated / github,native / hub application service
- Availability: hosted_shared / github,native / hub application service
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: this non-operator source site → not_applicable

Sources: [POST /internal/v1/health](../internal/hubserver/hosted_shared.go#L191)
## hubserver.hosted_stripe_webhook

Hosted stripe webhook

- Audience: transport; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Exact transport/protocol plumbing site. Application payload operations are inventoried separately; do not expose an HTTP or relay proxy tool.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential hosted browser session; bearer credentials refused by session-only APIs; CSRF for mutation; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.hostedStripeWebhook; s.config.Hosted.Billing.mode, s.config.now, s.database.db.ExecContext
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / hub application service — unavailable: Hosted organization/billing/identity service is absent in an unhosted hub.
- Availability: hosted_dedicated / github,native / hub application service
- Availability: hosted_shared / github,native / hub application service
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: this non-operator source site → not_applicable

Sources: [POST /webhooks/stripe](../internal/hubserver/hosted_ui.go#L39)
## hubserver.hosted_support_page

Hosted support page

- Audience: staff; status: **excluded**; owner: digitaldrywood/detent#3344.
- Decision: Platform/instance staff authority is stricter than organization operator authority. Preserve this restriction; no organization-operator tool grants staff powers.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role platform staff/support allowlist or entitlement administrator; credential-maintenance instance admin if applicable; credential staff session or dedicated private instance-admin credential; not an organization operator credential; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.hostedSupportPage; s.renderHosted
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Current handler authority checks: s.hostedSession(c)
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / hub application service — unavailable: Hosted organization/billing/identity service is absent in an unhosted hub.
- Availability: hosted_dedicated / github,native / hub application service
- Availability: hosted_shared / github,native / hub application service
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: this non-operator source site → not_applicable

Sources: [GET /support](../internal/hubserver/hosted_ui.go#L30), [internal/web/templates/hosted.templ:128](../internal/web/templates/hosted.templ#L128)
## hubserver.hosted_usage_report

Hosted usage report

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3345.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `billing_usage.hosted_usage_report` — Bounded hostedUsageReportRequest: organization; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → hostedUsageReportResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential hosted browser session; bearer credentials refused by session-only APIs; CSRF for mutation; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.hostedUsageReport; s.chatUsageRows, s.config.now, s.database.chatUsageSummary, s.hostedCredential, s.usageLimits, s.usagePrices, s.usageReadableProjects, s.usageRows, s.usageRunnerCapacity
- Extraction: Extract hubserver.hostedUsageReport application inputs/results and validation from Echo; reuse s.chatUsageRows, s.config.now, s.database.chatUsageSummary, s.hostedCredential, s.usageLimits, s.usagePrices, s.usageReadableProjects, s.usageRows, s.usageRunnerCapacity. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.; Current handler authority checks: s.hostedCredential(c); s.usageReadableProjects(ctx, credential)
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / hub application service — unavailable: Hosted organization/billing/identity service is absent in an unhosted hub.
- Availability: hosted_dedicated / github,native / hub application service
- Availability: hosted_shared / github,native / hub application service
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v2/organizations/:organization/usage](../internal/hubserver/hosted_usage_api.go#L43), [web/conversation/src/app/usage/adapter.ts:232](../web/conversation/src/app/usage/adapter.ts#L232)
## hubserver.intake_linked_issue

Intake linked issue

- Audience: worker; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Worker protocol only: preserve worker credential, producer/lease and runner ownership; organization operator tools cannot impersonate a worker.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated worker/runner; credential worker scope; runner credential/lease where applicable; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.intakeLinkedIssue; s.nativeMutation
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: this non-operator source site → not_applicable

Sources: [POST /api/v2/organizations/:organization/projects/:project/work-items/:item/source-intake](../internal/hubserver/native_api.go#L131)
## hubserver.invite_hosted_member

Invite hosted member

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3344.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `organization.invite_hosted_member` — Bounded inviteHostedMemberRequest: organization; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → inviteHostedMemberResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role hosted owner/admin; owner-only restrictions for owner membership/role changes; invitation acceptance bound to invited identity; credential hosted browser session; bearer credentials refused by session-only APIs; CSRF for mutation; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.inviteHostedMemberJSON; s.abandonHostedCommand, s.claimHostedCommand, s.completeHostedCommand, s.config.Hosted.Provider.Invite, s.config.now, s.database.db.ExecContext, s.hostedAdministrator, s.releaseFailedHostedInvitation, s.reserveHostedInvitationSeat
- Extraction: Extract hubserver.inviteHostedMemberJSON application inputs/results and validation from Echo; reuse s.abandonHostedCommand, s.claimHostedCommand, s.completeHostedCommand, s.config.Hosted.Provider.Invite, s.config.now, s.database.db.ExecContext, s.hostedAdministrator, s.releaseFailedHostedInvitation, s.reserveHostedInvitationSeat. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=true; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_dedicated / github,native / hub application service
- Availability: hosted_shared / github,native / hub application service
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: mutation of access, billing, deletion, cancellation or material external state → operator

Sources: [POST /api/v2/organizations/:organization/members/invitations](../internal/hubserver/hosted_org_api.go#L35), [POST /organization/invite](../internal/hubserver/hosted_ui.go#L44), [internal/web/templates/hosted.templ:223](../internal/web/templates/hosted.templ#L223), [internal/web/templates/hosted.templ:223](../internal/web/templates/hosted.templ#L223), [web/conversation/src/app/account/Organization.tsx:337](../web/conversation/src/app/account/Organization.tsx#L337), [web/conversation/src/app/account/Organization.tsx:335](../web/conversation/src/app/account/Organization.tsx#L335), [web/conversation/src/app/account/api.ts:162](../web/conversation/src/app/account/api.ts#L162)
## hubserver.land_change

Land change

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3347.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `changes_artifacts.land_change` — Bounded landChangeRequest: organization, project, item, change, version; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → landChangeResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.landChange; s.nativeMutation
- Extraction: Extract hubserver.landChange application inputs/results and validation from Echo; reuse s.nativeMutation. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=true; idempotent=false; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: mutation of access, billing, deletion, cancellation or material external state → operator

Sources: [POST /api/v2/organizations/:organization/projects/:project/work-items/:item/changes/:change/versions/:version/landing](../internal/hubserver/changes.go#L85)
## hubserver.link_conversation

Link conversation

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3346.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `conversations_workspaces.link_conversation` — Bounded linkConversationRequest: organization, project, conversation; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → linkConversationResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.linkConversation; s.nativeMutation, service.appendMessage, service.authorizeWrite, service.committed, service.coordinator.Hold, service.loadConversation, service.queryMessages, service.store.readConversation, service.store.recordAudience, service.updateExecution, service.updateMessage
- Extraction: Extract hubserver.linkConversation application inputs/results and validation from Echo; reuse s.nativeMutation, service.appendMessage, service.authorizeWrite, service.committed, service.coordinator.Hold, service.loadConversation, service.queryMessages, service.store.readConversation, service.store.recordAudience, service.updateExecution, service.updateMessage. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=false; idempotent=false; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [POST /api/v2/organizations/:organization/projects/:project/conversations/:conversation/link](../internal/hubserver/conversation_api.go#L44), [web/conversation/src/runtime/rpc/http.ts:445](../web/conversation/src/runtime/rpc/http.ts#L445)
## hubserver.list_changes

List changes

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3347.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `changes_artifacts.list_changes` — Bounded listChangesRequest: organization, project, item; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → listChangesResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.listChanges; handler-owned application validation/read/command
- Extraction: Extract hubserver.listChanges application inputs/results and validation from Echo; reuse the current handler-owned service logic. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v2/organizations/:organization/projects/:project/work-items/:item/changes](../internal/hubserver/changes.go#L76), [web/conversation/src/app/work/lib/workHttp.ts:608](../web/conversation/src/app/work/lib/workHttp.ts#L608), [web/conversation/src/app/work/ChangesPage.tsx:140](../web/conversation/src/app/work/ChangesPage.tsx#L140)
## hubserver.list_conversation_messages

List conversation messages

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3346.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `conversations_workspaces.list_conversation_messages` — Bounded listConversationMessagesRequest: organization, project, conversation; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → listConversationMessagesResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.listConversationMessages; s.conversations.loadConversation, s.conversations.store.listMessages
- Extraction: Extract hubserver.listConversationMessages application inputs/results and validation from Echo; reuse s.conversations.loadConversation, s.conversations.store.listMessages. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v2/organizations/:organization/projects/:project/conversations/:conversation/messages](../internal/hubserver/conversation_api.go#L41), [web/conversation/src/runtime/rpc/http.ts:416](../web/conversation/src/runtime/rpc/http.ts#L416), [web/conversation/src/components/chat/MessagesTimeline.tsx:1542](../web/conversation/src/components/chat/MessagesTimeline.tsx#L1542), [web/conversation/src/components/chat/MessagesTimeline.tsx:3514](../web/conversation/src/components/chat/MessagesTimeline.tsx#L3514)
## hubserver.list_git_hub_import_records

List git hub import records

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3342.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `projects.list_git_hub_import_records` — Bounded listGitHubImportRecordsRequest: organization, project, import; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → listGitHubImportRecordsResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.listGitHubImportRecords; s.database.db.QueryContext, s.nativePage
- Extraction: Extract hubserver.listGitHubImportRecords application inputs/results and validation from Echo; reuse s.database.db.QueryContext, s.nativePage. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v2/organizations/:organization/projects/:project/imports/:import/records](../internal/hubserver/integration.go#L156)
## hubserver.list_hosted_members

List hosted members

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3344.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `organization.list_hosted_members` — Bounded listHostedMembersRequest: organization; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → listHostedMembersResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role current organization member; ordinary members see themselves; owner/admin sees all members/invitations; credential hosted browser session; bearer credentials refused by session-only APIs; CSRF for mutation; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.listHostedMembers; s.config.Hosted.Provider.Memberships, s.hostedCredential, s.hostedMemberEmails, s.hostedMemberGrants, s.hostedPendingInvitations; ordinary members see their own row, owner/admin sees the complete organization list
- Extraction: Extract hubserver.listHostedMembers application inputs/results and validation from Echo; reuse s.config.Hosted.Provider.Memberships, s.hostedCredential, s.hostedMemberEmails, s.hostedMemberGrants, s.hostedPendingInvitations. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.; Current handler authority checks: s.hostedCredential(c)
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_dedicated / github,native / hub application service
- Availability: hosted_shared / github,native / hub application service
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v2/organizations/:organization/members](../internal/hubserver/hosted_org_api.go#L34), [GET /organization](../internal/hubserver/hosted_ui.go#L31), [internal/web/templates/hosted.templ:57](../internal/web/templates/hosted.templ#L57), [internal/web/templates/hosted.templ:83](../internal/web/templates/hosted.templ#L83), [internal/web/templates/hosted.templ:116](../internal/web/templates/hosted.templ#L116), [internal/web/templates/hosted.templ:345](../internal/web/templates/hosted.templ#L345), [web/conversation/src/app/main.tsx:47](../web/conversation/src/app/main.tsx#L47), [web/conversation/src/app/account/api.ts:160](../web/conversation/src/app/account/api.ts#L160)
## hubserver.list_hosted_projects

List hosted projects

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3342.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `projects.list_hosted_projects` — Bounded listHostedProjectsRequest: organization; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → listHostedProjectsResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential hosted browser session; bearer credentials refused by session-only APIs; CSRF for mutation; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.listHostedProjects; s.hostedCredential, s.hostedReadableProjects, s.projectOnboarding
- Extraction: Extract hubserver.listHostedProjects application inputs/results and validation from Echo; reuse s.hostedCredential, s.hostedReadableProjects, s.projectOnboarding. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.; Current handler authority checks: s.hostedCredential(c)
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_dedicated / github,native / hub application service
- Availability: hosted_shared / github,native / hub application service
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v2/organizations/:organization/projects](../internal/hubserver/hosted_org_api.go#L40), [internal/web/templates/hosted.templ:182](../internal/web/templates/hosted.templ#L182), [internal/web/templates/hosted.templ:122](../internal/web/templates/hosted.templ#L122), [internal/web/templates/hosted.templ:199](../internal/web/templates/hosted.templ#L199)
## hubserver.list_native_attempts

List native attempts

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3340.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `work_reads.list_native_attempts` — Bounded listNativeAttemptsRequest: organization, project, item; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → listNativeAttemptsResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.listNativeAttempts; s.config.now, s.database.db.QueryContext, s.nativePage
- Extraction: Extract hubserver.listNativeAttempts application inputs/results and validation from Echo; reuse s.config.now, s.database.db.QueryContext, s.nativePage. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v2/organizations/:organization/projects/:project/work-items/:item/attempts](../internal/hubserver/native_api.go#L138), [web/conversation/src/app/work/lib/workHttp.ts:588](../web/conversation/src/app/work/lib/workHttp.ts#L588)
## hubserver.list_native_comments

List native comments

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3341.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `work_commands.list_native_comments` — Bounded listNativeCommentsRequest: organization, project, item; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → listNativeCommentsResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.listNativeComments; s.nativePage
- Extraction: Extract hubserver.listNativeComments application inputs/results and validation from Echo; reuse s.nativePage. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v2/organizations/:organization/projects/:project/work-items/:item/comments](../internal/hubserver/native_api.go#L134), [web/conversation/src/app/work/lib/workHttp.ts:597](../web/conversation/src/app/work/lib/workHttp.ts#L597)
## hubserver.list_native_history

List native history

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3340.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `work_reads.list_native_history` — Bounded listNativeHistoryRequest: organization, project, item; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → listNativeHistoryResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.listNativeHistory; s.database.db.QueryContext, s.database.db.QueryRowContext, s.nativePage
- Extraction: Extract hubserver.listNativeHistory application inputs/results and validation from Echo; reuse s.database.db.QueryContext, s.database.db.QueryRowContext, s.nativePage. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v2/organizations/:organization/projects/:project/work-items/:item/history](../internal/hubserver/native_api.go#L137), [web/conversation/src/app/work/lib/workHttp.ts:592](../web/conversation/src/app/work/lib/workHttp.ts#L592), [web/conversation/src/app/work/lib/useWork.ts:369](../web/conversation/src/app/work/lib/useWork.ts#L369)
## hubserver.list_native_issues

List native issues

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3340.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `work_reads.list_native_issues` — Bounded listNativeIssuesRequest: organization, project; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → listNativeIssuesResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.listNativeIssues; s.nativePage
- Extraction: Extract hubserver.listNativeIssues application inputs/results and validation from Echo; reuse s.nativePage. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v2/organizations/:organization/projects/:project/work-items](../internal/hubserver/native_api.go#L125), [web/conversation/src/app/work/lib/workHttp.ts:537](../web/conversation/src/app/work/lib/workHttp.ts#L537)
## hubserver.list_native_labels

List native labels

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3340.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `work_reads.list_native_labels` — Bounded listNativeLabelsRequest: organization, project; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → listNativeLabelsResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.listNativeLabels; handler-owned application validation/read/command
- Extraction: Extract hubserver.listNativeLabels application inputs/results and validation from Echo; reuse the current handler-owned service logic. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v2/organizations/:organization/projects/:project/labels](../internal/hubserver/native_api.go#L124), [web/conversation/src/app/work/lib/workHttp.ts:554](../web/conversation/src/app/work/lib/workHttp.ts#L554)
## hubserver.list_organization_conversations

List organization conversations

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3346.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `conversations_workspaces.list_organization_conversations` — Bounded listOrganizationConversationsRequest: organization; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → listOrganizationConversationsResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.listOrganizationConversations; s.listConversationsPage, s.readableConversationProjects
- Extraction: Extract hubserver.listOrganizationConversations application inputs/results and validation from Echo; reuse s.listConversationsPage, s.readableConversationProjects. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / hub application service
- Availability: hosted_dedicated / github,native / hub application service
- Availability: hosted_shared / github,native / hub application service
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v2/organizations/:organization/conversations](../internal/hubserver/conversation_api.go#L39), [web/conversation/src/runtime/rpc/http.ts:386](../web/conversation/src/runtime/rpc/http.ts#L386)
## hubserver.list_project_action_runs

List project action runs

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3346.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `conversations_workspaces.list_project_action_runs` — Bounded listProjectActionRunsRequest: organization, project, action; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → listProjectActionRunsResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.listProjectActionRuns; handler-owned application validation/read/command
- Extraction: Extract hubserver.listProjectActionRuns application inputs/results and validation from Echo; reuse the current handler-owned service logic. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v2/organizations/:organization/projects/:project/actions/:action/runs](../internal/hubserver/native_api.go#L173), [web/conversation/src/app/work/lib/workHttp.ts:731](../web/conversation/src/app/work/lib/workHttp.ts#L731)
## hubserver.list_project_actions

List project actions

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3346.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `conversations_workspaces.list_project_actions` — Bounded listProjectActionsRequest: organization, project; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → listProjectActionsResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.listProjectActions; handler-owned application validation/read/command
- Extraction: Extract hubserver.listProjectActions application inputs/results and validation from Echo; reuse the current handler-owned service logic. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v2/organizations/:organization/projects/:project/actions](../internal/hubserver/native_api.go#L169), [web/conversation/src/app/work/lib/workHttp.ts:696](../web/conversation/src/app/work/lib/workHttp.ts#L696)
## hubserver.list_project_conversations

List project conversations

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3346.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `conversations_workspaces.list_project_conversations` — Bounded listProjectConversationsRequest: organization, project; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → listProjectConversationsResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.listProjectConversations; s.listConversationsPage
- Extraction: Extract hubserver.listProjectConversations application inputs/results and validation from Echo; reuse s.listConversationsPage. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v2/organizations/:organization/projects/:project/conversations](../internal/hubserver/conversation_api.go#L38), [web/conversation/src/runtime/rpc/http.ts:397](../web/conversation/src/runtime/rpc/http.ts#L397)
## hubserver.list_runner_routing

List runner routing

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3343.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `runs_fleet.list_runner_routing` — Bounded listRunnerRoutingRequest: organization; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → listRunnerRoutingResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role instance admin in unhosted hub; hosted owner/admin through requireHostedAdministration; credential admin; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.listRunnerRouting; s.config.now, s.database.db.QueryContext
- Extraction: Extract hubserver.listRunnerRouting application inputs/results and validation from Echo; reuse s.config.now, s.database.db.QueryContext. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / hub application service
- Availability: hosted_dedicated / github,native / hub application service
- Availability: hosted_shared / github,native / hub application service
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v2/organizations/:organization/runners](../internal/hubserver/runner_enrollment.go#L32), [web/conversation/src/app/account/api.ts:348](../web/conversation/src/app/account/api.ts#L348)
## hubserver.list_work_item_pull_requests

List work item pull requests

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3340.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `work_reads.list_work_item_pull_requests` — Bounded listWorkItemPullRequestsRequest: organization, project, item; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → listWorkItemPullRequestsResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.listWorkItemPullRequests; s.changePullRequestView, s.config.now, s.pullRequests.allowRefresh, s.pullRequests.get, s.pullRequests.set, s.requestPullRequestHydration
- Extraction: Extract hubserver.listWorkItemPullRequests application inputs/results and validation from Echo; reuse s.changePullRequestView, s.config.now, s.pullRequests.allowRefresh, s.pullRequests.get, s.pullRequests.set, s.requestPullRequestHydration. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v2/organizations/:organization/projects/:project/work-items/:item/pull-requests](../internal/hubserver/native_api.go#L153), [web/conversation/src/app/work/lib/workHttp.ts:651](../web/conversation/src/app/work/lib/workHttp.ts#L651), [web/conversation/src/app/adapters/issuePullRequest.ts:75](../web/conversation/src/app/adapters/issuePullRequest.ts#L75), [web/conversation/src/components/Sidebar.tsx:1460](../web/conversation/src/components/Sidebar.tsx#L1460)
## hubserver.list_work_item_references

List work item references

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3346.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `conversations_workspaces.list_work_item_references` — Bounded listWorkItemReferencesRequest: organization, project, item; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → listWorkItemReferencesResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.listWorkItemReferences; handler-owned application validation/read/command
- Extraction: Extract hubserver.listWorkItemReferences application inputs/results and validation from Echo; reuse the current handler-owned service logic. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v2/organizations/:organization/projects/:project/work-items/:item/references](../internal/hubserver/conversation_api.go#L46)
## hubserver.list_work_items

List work items

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3340.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `work_reads.list_work_items` — Bounded listWorkItemsRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → listWorkItemsResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential hub worker/operator/admin per registration; native-only credentials cannot call legacy v1; hosted sessions cannot call outside nativeBase; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.listWorkItems; s.allWorkItems
- Extraction: Extract hubserver.listWorkItems application inputs/results and validation from Echo; reuse s.allWorkItems. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / hub application service
- Availability: hosted_dedicated / github,native / hub application service — unavailable: Current requireAPIScope refuses hosted-session calls outside nativeBase and rejects global credentials; use the corresponding native/hosted application operation. This row remains pending parity work.
- Availability: hosted_shared / github,native / hub application service — unavailable: Current requireAPIScope refuses hosted-session calls outside nativeBase and rejects global credentials; use the corresponding native/hosted application operation. This row remains pending parity work.
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v1/work-items](../internal/hubserver/api_http.go#L62)
## hubserver.list_workspace_terminal_recordings

List workspace terminal recordings

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3346.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `conversations_workspaces.list_workspace_terminal_recordings` — Bounded listWorkspaceTerminalRecordingsRequest: organization, project, workspace; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → listWorkspaceTerminalRecordingsResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership recording creator or owner/admin; user-isolation recordings owners only; workspace belongs to project.
- Application: s.listWorkspaceTerminalRecordings; s.requireWorkspaces, service.readWorkspaceForActor
- Extraction: Extract hubserver.listWorkspaceTerminalRecordings application inputs/results and validation from Echo; reuse s.requireWorkspaces, service.readWorkspaceForActor. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.; Current handler authority checks: s.requireWorkspaces()
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v2/organizations/:organization/projects/:project/workspaces/:workspace/terminal-recordings](../internal/hubserver/native_api.go#L182)
## hubserver.list_workspaces

List workspaces

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3346.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `conversations_workspaces.list_workspaces` — Bounded listWorkspacesRequest: organization, project; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → listWorkspacesResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.listWorkspaces; s.requireWorkspaces, service.presentWorkspace
- Extraction: Extract hubserver.listWorkspaces application inputs/results and validation from Echo; reuse s.requireWorkspaces, service.presentWorkspace. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.; Current handler authority checks: s.requireWorkspaces()
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v2/organizations/:organization/projects/:project/workspaces](../internal/hubserver/native_api.go#L158), [web/conversation/src/app/work/lib/workHttp.ts:665](../web/conversation/src/app/work/lib/workHttp.ts#L665), [web/conversation/src/app/adapters/workspaces.ts:289](../web/conversation/src/app/adapters/workspaces.ts#L289)
## hubserver.logout_hosted

Logout hosted

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3336.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `connection.logout_hosted` — Bounded logoutHostedRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → logoutHostedResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential hosted browser session; bearer credentials refused by session-only APIs; CSRF for mutation; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.logoutHosted; s.config.now, s.database.db.ExecContext, s.database.db.QueryRowContext, s.hostedDenied
- Extraction: Extract hubserver.logoutHosted application inputs/results and validation from Echo; reuse s.config.now, s.database.db.ExecContext, s.database.db.QueryRowContext, s.hostedDenied. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.; Current handler authority checks: s.hostedSession(c)
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=false; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_dedicated / github,native / hub application service
- Availability: hosted_shared / github,native / hub application service
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [POST /logout](../internal/hubserver/hosted_ui.go#L28), [internal/web/templates/hosted.templ:13](../internal/web/templates/hosted.templ#L13), [internal/web/templates/hosted.templ:99](../internal/web/templates/hosted.templ#L99), [internal/web/templates/hosted.templ:13](../internal/web/templates/hosted.templ#L13), [internal/web/templates/hosted.templ:99](../internal/web/templates/hosted.templ#L99), [web/conversation/src/app/account/api.ts:216](../web/conversation/src/app/account/api.ts#L216), [web/conversation/src/app/entry/EntryScreens.tsx:98](../web/conversation/src/app/entry/EntryScreens.tsx#L98), [web/conversation/src/app/entry/EntryScreens.tsx:98](../web/conversation/src/app/entry/EntryScreens.tsx#L98)
## hubserver.mint_workspace_relay_ticket

Mint workspace relay ticket

- Audience: transport; status: **excluded**; owner: digitaldrywood/detent#3346.
- Decision: Exact authenticated transport entry for existing governed application services. No MCP raw HTTP/relay/tool-forwarding proxy; meaningful typed application operations and the five existing read tools are separate rows.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.mintWorkspaceRelayTicket; s.hubTransact, s.relayPrincipalFor, s.requireRelayOrigin, s.requireWorkspaces, service.readWorkspaceForActor
- Extraction: No raw transport command extraction; share authorization with the typed payload operations.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.; Current handler authority checks: s.requireRelayOrigin(c); err != nil; s.requireWorkspaces()
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: transport authority establishment → connection

Sources: [POST /api/v2/organizations/:organization/projects/:project/workspaces/:workspace/relay-tickets](../internal/hubserver/native_api.go#L162), [web/conversation/src/app/work/lib/workHttp.ts:689](../web/conversation/src/app/work/lib/workHttp.ts#L689)
## hubserver.native_capabilities

Native capabilities

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3340.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `work_reads.native_capabilities` — Bounded nativeCapabilitiesRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → nativeCapabilitiesResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.nativeCapabilities; s.database.db.QueryRowContext
- Extraction: Extract hubserver.nativeCapabilities application inputs/results and validation from Echo; reuse s.database.db.QueryRowContext. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v2/capabilities](../internal/hubserver/native_api.go#L114)
## hubserver.native_organizations

Native organizations

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3344.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `organization.native_organizations` — Bounded nativeOrganizationsRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → nativeOrganizationsResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role instance admin in unhosted hub; hosted owner/admin through requireHostedAdministration; credential admin; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.nativeOrganizations; s.database.db.QueryContext
- Extraction: Extract hubserver.nativeOrganizations application inputs/results and validation from Echo; reuse s.database.db.QueryContext. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v2/organizations](../internal/hubserver/native_api.go#L115), [web/conversation/src/app/account/Support.tsx:127](../web/conversation/src/app/account/Support.tsx#L127), [web/conversation/src/app/account/api.ts:199](../web/conversation/src/app/account/api.ts#L199)
## hubserver.observe_project_policy

Observe project policy

- Audience: worker; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Worker protocol only: preserve worker credential, producer/lease and runner ownership; organization operator tools cannot impersonate a worker.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated worker/runner; credential worker scope; runner credential/lease where applicable; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.observeProjectPolicy; s.config.now, s.database.db.ExecContext, s.policyScope
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: this non-operator source site → not_applicable

Sources: [POST /api/v2/organizations/:organization/projects/:project/policy/observed](../internal/hubserver/native_api.go#L113)
## hubserver.open_workspace_relay

Open workspace relay

- Audience: transport; status: **excluded**; owner: digitaldrywood/detent#3346.
- Decision: Exact authenticated transport entry for existing governed application services. No MCP raw HTTP/relay/tool-forwarding proxy; meaningful typed application operations and the five existing read tools are separate rows.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.openWorkspaceRelay; s.hubTransact, s.relayActor, s.relayPrincipalFor, s.requireRelayOrigin, s.requireWorkspaces, service.readWorkspaceForActor, service.servePerson
- Extraction: No raw transport command extraction; share authorization with the typed payload operations.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.; Current handler authority checks: s.requireRelayOrigin(c); err != nil; s.requireWorkspaces()
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: transport authority establishment → connection

Sources: [GET /api/v2/organizations/:organization/projects/:project/workspaces/:workspace/relay](../internal/hubserver/native_api.go#L163)
## hubserver.open_workspace_worker_relay

Open workspace worker relay

- Audience: worker; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Worker protocol only: preserve worker credential, producer/lease and runner ownership; organization operator tools cannot impersonate a worker.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated worker/runner; credential worker scope; runner credential/lease where applicable; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.openWorkspaceWorkerRelay; s.hubTransact, s.requireWorkspaces, service.closeIfEndedSinceUpgrade, service.closeRelaySocket, service.loadWorkspaceForWorker, service.readRunner, service.relay.attachRunner, service.relay.detachRunner, service.relay.failWorkspaceExecRuns
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Current handler authority checks: s.requireWorkspaces()
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: this non-operator source site → not_applicable

Sources: [GET /api/v2/organizations/:organization/projects/:project/workspaces/:workspace/worker/relay](../internal/hubserver/native_api.go#L164)
## hubserver.outbox_health

Outbox health

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3340.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `work_reads.outbox_health` — Bounded outboxHealthRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → outboxHealthResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential hub worker/operator/admin per registration; native-only credentials cannot call legacy v1; hosted sessions cannot call outside nativeBase; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.outboxHealth; s.OutboxHealth
- Extraction: Extract hubserver.outboxHealth application inputs/results and validation from Echo; reuse s.OutboxHealth. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / hub application service
- Availability: hosted_dedicated / github,native / hub application service — unavailable: Current requireAPIScope refuses hosted-session calls outside nativeBase and rejects global credentials; use the corresponding native/hosted application operation. This row remains pending parity work.
- Availability: hosted_shared / github,native / hub application service — unavailable: Current requireAPIScope refuses hosted-session calls outside nativeBase and rejects global credentials; use the corresponding native/hosted application operation. This row remains pending parity work.
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v1/outbox/health](../internal/hubserver/api_http.go#L75)
## hubserver.patch_conversation

Patch conversation

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3346.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `conversations_workspaces.patch_conversation` — Bounded patchConversationRequest: organization, project, conversation; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → patchConversationResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.patchConversation; s.conversationDefaultModel, s.conversationModelChoices, s.conversations.writeAgentOverride, s.hasLunaCoordinator, s.saveConversationChange
- Extraction: Extract hubserver.patchConversation application inputs/results and validation from Echo; reuse s.conversationDefaultModel, s.conversationModelChoices, s.conversations.writeAgentOverride, s.hasLunaCoordinator, s.saveConversationChange. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=true; idempotent=false; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: ordinary non-destructive edit → none
- Confirmation: arguments remove data, alter access or create material external effects → operator

Sources: [PATCH /api/v2/organizations/:organization/projects/:project/conversations/:conversation](../internal/hubserver/conversation_api.go#L45), [web/conversation/src/runtime/rpc/http.ts:459](../web/conversation/src/runtime/rpc/http.ts#L459), [web/conversation/src/runtime/rpc/http.ts:468](../web/conversation/src/runtime/rpc/http.ts#L468)
## hubserver.patch_project_action

Patch project action

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3346.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `conversations_workspaces.patch_project_action` — Bounded patchProjectActionRequest: organization, project, action; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → patchProjectActionResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.patchProjectAction; s.nativeMutation
- Extraction: Extract hubserver.patchProjectAction application inputs/results and validation from Echo; reuse s.nativeMutation. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=true; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: ordinary non-destructive edit → none
- Confirmation: arguments remove data, alter access or create material external effects → operator

Sources: [PATCH /api/v2/organizations/:organization/projects/:project/actions/:action](../internal/hubserver/native_api.go#L171), [web/conversation/src/app/work/lib/workHttp.ts:705](../web/conversation/src/app/work/lib/workHttp.ts#L705), [web/conversation/src/components/projectScriptEditor.tsx:272](../web/conversation/src/components/projectScriptEditor.tsx#L272), [web/conversation/src/components/projectScriptEditor.tsx:272](../web/conversation/src/components/projectScriptEditor.tsx#L272)
## hubserver.post_attempt_diff

Post attempt diff

- Audience: worker; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Worker protocol only: preserve worker credential, producer/lease and runner ownership; organization operator tools cannot impersonate a worker.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated worker/runner; credential worker scope; runner credential/lease where applicable; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.postAttemptDiff; s.hubTransact, s.recheckHostedMutation
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: this non-operator source site → not_applicable

Sources: [POST /api/v2/organizations/:organization/projects/:project/attempts/:attempt/diff](../internal/hubserver/native_api.go#L150)
## hubserver.post_conversation_command

Post conversation command

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3346.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `conversations_workspaces.post_conversation_command` — Bounded postConversationCommandRequest: organization, project, conversation; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → postConversationCommandResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.postConversationCommand; service.acceptCommand, service.authorizeWrite, service.committed, service.loadConversation, service.logStaleExecution, service.recordReceipt, service.requireActorAuthority, service.store.reserveCommand, service.transact
- Extraction: Extract hubserver.postConversationCommand application inputs/results and validation from Echo; reuse service.acceptCommand, service.authorizeWrite, service.committed, service.loadConversation, service.logStaleExecution, service.recordReceipt, service.requireActorAuthority, service.store.reserveCommand, service.transact. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=false; idempotent=false; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [POST /api/v2/organizations/:organization/projects/:project/conversations/:conversation/commands](../internal/hubserver/conversation_api.go#L43), [web/conversation/src/app/App.tsx:1224](../web/conversation/src/app/App.tsx#L1224), [web/conversation/src/app/App.tsx:558](../web/conversation/src/app/App.tsx#L558), [web/conversation/src/app/App.tsx:1241](../web/conversation/src/app/App.tsx#L1241), [web/conversation/src/app/components/Composer.tsx:394](../web/conversation/src/app/components/Composer.tsx#L394), [web/conversation/src/app/components/Composer.tsx:640](../web/conversation/src/app/components/Composer.tsx#L640), [web/conversation/src/app/components/Composer.tsx:390](../web/conversation/src/app/components/Composer.tsx#L390), [web/conversation/src/app/components/Composer.tsx:396](../web/conversation/src/app/components/Composer.tsx#L396), [web/conversation/src/app/work/components/IssueComposer.tsx:66](../web/conversation/src/app/work/components/IssueComposer.tsx#L66), [web/conversation/src/components/GitActionsControl.tsx:410](../web/conversation/src/components/GitActionsControl.tsx#L410), [web/conversation/src/components/GitActionsControl.tsx:410](../web/conversation/src/components/GitActionsControl.tsx#L410), [web/conversation/src/components/chat/AssistantCitationChip.tsx:158](../web/conversation/src/components/chat/AssistantCitationChip.tsx#L158), [web/conversation/src/runtime/rpc/http.ts:436](../web/conversation/src/runtime/rpc/http.ts#L436)
## hubserver.preview_provider_candidates

Preview provider candidates

- Audience: worker; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Worker protocol only: preserve worker credential, producer/lease and runner ownership; organization operator tools cannot impersonate a worker.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated worker/runner; credential worker scope; runner credential/lease where applicable; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.previewProviderCandidates; s.runnerTransaction
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: this non-operator source site → not_applicable

Sources: [POST /api/v2/organizations/:organization/projects/:project/claims/preview](../internal/hubserver/native_api.go#L142)
## hubserver.project_native_summary

Project native summary

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3342.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `projects.project_native_summary` — Bounded projectNativeSummaryRequest: organization, project, item; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → projectNativeSummaryResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.projectNativeSummary; s.nativeMutation
- Extraction: Extract hubserver.projectNativeSummary application inputs/results and validation from Echo; reuse s.nativeMutation. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=false; idempotent=false; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [POST /api/v2/organizations/:organization/projects/:project/work-items/:item/projection](../internal/hubserver/integration.go#L157)
## hubserver.project_secret_metadata

Project secret metadata

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3342.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `projects.project_secret_metadata` — Bounded projectSecretMetadataRequest: organization, project, kind; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → projectSecretMetadataResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.projectSecretMetadata; handler-owned application validation/read/command
- Extraction: Extract hubserver.projectSecretMetadata application inputs/results and validation from Echo; reuse the current handler-owned service logic. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v2/organizations/:organization/projects/:project/secrets/:kind](../internal/hubserver/project_secrets.go#L80), [web/conversation/src/app/account/api.ts:229](../web/conversation/src/app/account/api.ts#L229)
## hubserver.publish_change_version

Publish change version

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3347.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `changes_artifacts.publish_change_version` — Bounded publishChangeVersionRequest: organization, project, item, change; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → publishChangeVersionResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.publishChangeVersion; s.nativeMutation
- Extraction: Extract hubserver.publishChangeVersion application inputs/results and validation from Echo; reuse s.nativeMutation. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=false; idempotent=false; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [POST /api/v2/organizations/:organization/projects/:project/work-items/:item/changes/:change/versions](../internal/hubserver/changes.go#L79)
## hubserver.redeem_runner_enrollment

Redeem runner enrollment

- Audience: worker; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Worker protocol only: preserve worker credential, producer/lease and runner ownership; organization operator tools cannot impersonate a worker.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated worker/runner; credential worker scope; runner credential/lease where applicable; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.redeemRunnerEnrollment; s.runnerTransaction
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / hub application service
- Availability: hosted_dedicated / github,native / hub application service
- Availability: hosted_shared / github,native / hub application service
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: this non-operator source site → not_applicable

Sources: [POST /api/v2/organizations/:organization/runner-enrollments/redeem](../internal/hubserver/runner_enrollment.go#L27)
## hubserver.register_machine

Register machine

- Audience: worker; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Worker protocol only: preserve worker credential, producer/lease and runner ownership; organization operator tools cannot impersonate a worker.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated worker/runner; credential worker scope; runner credential/lease where applicable; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.registerMachine; s.database.currentTime, s.database.db.ExecContext, s.database.machine
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / hub application service
- Availability: hosted_dedicated / github,native / hub application service
- Availability: hosted_shared / github,native / hub application service
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: this non-operator source site → not_applicable

Sources: [POST /api/v1/machines/register](../internal/hubserver/api_http.go#L72)
## hubserver.register_native_machine

Register native machine

- Audience: worker; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Worker protocol only: preserve worker credential, producer/lease and runner ownership; organization operator tools cannot impersonate a worker.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated worker/runner; credential worker scope; runner credential/lease where applicable; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.registerNativeMachine; s.config.now, s.database.db.ExecContext, s.runnerTransaction
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: this non-operator source site → not_applicable

Sources: [POST /api/v2/organizations/:organization/projects/:project/machines/register](../internal/hubserver/native_api.go#L145)
## hubserver.release_lease

Release lease

- Audience: worker; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Worker protocol only: preserve worker credential, producer/lease and runner ownership; organization operator tools cannot impersonate a worker.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated worker/runner; credential worker scope; runner credential/lease where applicable; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.releaseLease; s.tracker.Release
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / hub application service
- Availability: hosted_dedicated / github,native / hub application service
- Availability: hosted_shared / github,native / hub application service
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: this non-operator source site → not_applicable

Sources: [POST /api/v1/leases/:id/release](../internal/hubserver/api_http.go#L66)
## hubserver.release_native_lease

Release native lease

- Audience: worker; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Worker protocol only: preserve worker credential, producer/lease and runner ownership; organization operator tools cannot impersonate a worker.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated worker/runner; credential worker scope; runner credential/lease where applicable; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.releaseNativeLease; s.conversations.leaseReleased, s.database.Release, s.requireNativeLease
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Current handler authority checks: s.requireNativeLease(c); err != nil
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: this non-operator source site → not_applicable

Sources: [POST /api/v2/organizations/:organization/projects/:project/leases/:lease/release](../internal/hubserver/native_api.go#L144)
## hubserver.remove_project_secret

Remove project secret

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3342.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `projects.remove_project_secret` — Bounded removeProjectSecretRequest: organization, project, kind; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → removeProjectSecretResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role hosted owner/admin for provider secrets; unhosted native operator/admin per project-secret administration guard; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.removeProjectSecret; s.config.now, s.secretMutation
- Extraction: Extract hubserver.removeProjectSecret application inputs/results and validation from Echo; reuse s.config.now, s.secretMutation. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=true; idempotent=false; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: mutation of access, billing, deletion, cancellation or material external state → operator

Sources: [DELETE /api/v2/organizations/:organization/projects/:project/secrets/:kind](../internal/hubserver/project_secrets.go#L82), [web/conversation/src/app/account/api.ts:233](../web/conversation/src/app/account/api.ts#L233)
## hubserver.renew_lease

Renew lease

- Audience: worker; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Worker protocol only: preserve worker credential, producer/lease and runner ownership; organization operator tools cannot impersonate a worker.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated worker/runner; credential worker scope; runner credential/lease where applicable; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.renewLease; s.database.leasePolicyID, s.database.renew
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / hub application service
- Availability: hosted_dedicated / github,native / hub application service
- Availability: hosted_shared / github,native / hub application service
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: this non-operator source site → not_applicable

Sources: [POST /api/v1/leases/:id/renew](../internal/hubserver/api_http.go#L65)
## hubserver.renew_native_lease

Renew native lease

- Audience: worker; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Worker protocol only: preserve worker credential, producer/lease and runner ownership; organization operator tools cannot impersonate a worker.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated worker/runner; credential worker scope; runner credential/lease where applicable; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.renewNativeLease; s.database.renew, s.requireNativeLease, s.respondNativeLease
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Current handler authority checks: s.requireNativeLease(c); err != nil
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: this non-operator source site → not_applicable

Sources: [POST /api/v2/organizations/:organization/projects/:project/leases/:lease/renew](../internal/hubserver/native_api.go#L143)
## hubserver.renew_runner_identity

Renew runner identity

- Audience: worker; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Worker protocol only: preserve worker credential, producer/lease and runner ownership; organization operator tools cannot impersonate a worker.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated worker/runner; credential worker scope; runner credential/lease where applicable; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.renewRunnerIdentity; s.changeRunnerCredential
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / hub application service
- Availability: hosted_dedicated / github,native / hub application service
- Availability: hosted_shared / github,native / hub application service
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: this non-operator source site → not_applicable

Sources: [POST /api/v2/organizations/:organization/runners/:runner/renew](../internal/hubserver/runner_enrollment.go#L29)
## hubserver.report_git_hub_batch

Report git hub batch

- Audience: worker; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Worker protocol only: preserve worker credential, producer/lease and runner ownership; organization operator tools cannot impersonate a worker.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated worker/runner; credential worker scope; runner credential/lease where applicable; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.reportGitHubBatch; s.nativeMutation
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: this non-operator source site → not_applicable

Sources: [POST /api/v2/organizations/:organization/projects/:project/onboarding/issue-intake/result](../internal/hubserver/onboarding.go#L22)
## hubserver.report_workspace_action_run

Report workspace action run

- Audience: worker; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Worker protocol only: preserve worker credential, producer/lease and runner ownership; organization operator tools cannot impersonate a worker.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated worker/runner; credential worker scope; runner credential/lease where applicable; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.reportWorkspaceActionRun; s.hubTransact, s.requireWorkspaces, service.loadWorkspaceForWorker, service.recordActionRunReport
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Current handler authority checks: s.requireWorkspaces()
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: this non-operator source site → not_applicable

Sources: [POST /api/v2/organizations/:organization/projects/:project/workspaces/:workspace/worker/action-runs](../internal/hubserver/native_api.go#L168)
## hubserver.repository_freshness

Repository freshness

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3342.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `projects.repository_freshness` — Bounded repositoryFreshnessRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → repositoryFreshnessResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential hub worker/operator/admin per registration; native-only credentials cannot call legacy v1; hosted sessions cannot call outside nativeBase; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.repositoryFreshness; s.config.now, s.database.repositoryFreshness
- Extraction: Extract hubserver.repositoryFreshness application inputs/results and validation from Echo; reuse s.config.now, s.database.repositoryFreshness. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / hub application service
- Availability: hosted_dedicated / github,native / hub application service — unavailable: Current requireAPIScope refuses hosted-session calls outside nativeBase and rejects global credentials; use the corresponding native/hosted application operation. This row remains pending parity work.
- Availability: hosted_shared / github,native / hub application service — unavailable: Current requireAPIScope refuses hosted-session calls outside nativeBase and rejects global credentials; use the corresponding native/hosted application operation. This row remains pending parity work.
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v1/repositories/freshness](../internal/hubserver/api_http.go#L74)
## hubserver.restore_native_issue

Restore native issue

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3341.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `work_commands.restore_native_issue` — Bounded restoreNativeIssueRequest: organization, project, item; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → restoreNativeIssueResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.restoreNativeIssue; s.setNativeArchive
- Extraction: Extract hubserver.restoreNativeIssue application inputs/results and validation from Echo; reuse s.setNativeArchive. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=false; idempotent=false; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [POST /api/v2/organizations/:organization/projects/:project/work-items/:item/restore](../internal/hubserver/native_api.go#L130)
## hubserver.review_change

Review change

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3347.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `changes_artifacts.review_change` — Bounded reviewChangeRequest: organization, project, item, change, version; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → reviewChangeResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role authorized change reviewer; approval policy and no self-approval; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.reviewChange; s.nativeMutation
- Extraction: Extract hubserver.reviewChange application inputs/results and validation from Echo; reuse s.nativeMutation. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Current version, reviewer identity and change review policy; reject self-approval; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=false; idempotent=false; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [POST /api/v2/organizations/:organization/projects/:project/work-items/:item/changes/:change/versions/:version/reviews](../internal/hubserver/changes.go#L81), [web/conversation/src/app/work/lib/workHttp.ts:622](../web/conversation/src/app/work/lib/workHttp.ts#L622)
## hubserver.revoke_api_token

Revoke a p i token

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3344.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `organization.revoke_api_token` — Bounded revokeAPITokenRequest: id; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → revokeAPITokenResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role instance admin in unhosted hub; hosted owner/admin through requireHostedAdministration; credential hub worker/operator/admin per registration; native-only credentials cannot call legacy v1; hosted sessions cannot call outside nativeBase; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.revokeAPIToken; s.database.currentTime, s.database.db.ExecContext
- Extraction: Extract hubserver.revokeAPIToken application inputs/results and validation from Echo; reuse s.database.currentTime, s.database.db.ExecContext. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=true; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / hub application service
- Availability: hosted_dedicated / github,native / hub application service — unavailable: Current requireAPIScope refuses hosted-session calls outside nativeBase and rejects global credentials; use the corresponding native/hosted application operation. This row remains pending parity work.
- Availability: hosted_shared / github,native / hub application service — unavailable: Current requireAPIScope refuses hosted-session calls outside nativeBase and rejects global credentials; use the corresponding native/hosted application operation. This row remains pending parity work.
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: mutation of access, billing, deletion, cancellation or material external state → operator

Sources: [DELETE /api/v1/tokens/:id](../internal/hubserver/api_http.go#L78)
## hubserver.revoke_api_token_maintenance

Revoke a p i token maintenance

- Audience: staff; status: **excluded**; owner: digitaldrywood/detent#3344.
- Decision: Platform/instance staff authority is stricter than organization operator authority. Preserve this restriction; no organization-operator tool grants staff powers.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role private loopback credential-maintenance instance admin; no hosted member, runner, support or native-only credential; credential staff session or dedicated private instance-admin credential; not an organization operator credential; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.revokeAPIToken; s.database.currentTime, s.database.db.ExecContext
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_dedicated / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / hub application service
- Confirmation: this non-operator source site → not_applicable

Sources: [DELETE /api/v1/tokens/:id](../internal/hubserver/credential_maintenance.go#L26)
## hubserver.revoke_hosted_invitation_json

Revoke hosted invitation j s o n

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3344.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `organization.revoke_hosted_invitation_json` — Bounded revokeHostedInvitationJSONRequest: organization, invitation; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → revokeHostedInvitationJSONResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role hosted owner/admin; owner-only restrictions for owner membership/role changes; invitation acceptance bound to invited identity; credential hosted browser session; bearer credentials refused by session-only APIs; CSRF for mutation; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.revokeHostedInvitationJSON; s.database.db.QueryRowContext, s.hostedAdministrator, s.releaseHostedInvitation
- Extraction: Extract hubserver.revokeHostedInvitationJSON application inputs/results and validation from Echo; reuse s.database.db.QueryRowContext, s.hostedAdministrator, s.releaseHostedInvitation. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=true; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_dedicated / github,native / hub application service
- Availability: hosted_shared / github,native / hub application service
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: mutation of access, billing, deletion, cancellation or material external state → operator

Sources: [DELETE /api/v2/organizations/:organization/members/invitations/:invitation](../internal/hubserver/hosted_org_api.go#L36), [web/conversation/src/app/account/api.ts:168](../web/conversation/src/app/account/api.ts#L168)
## hubserver.revoke_hosted_member

Revoke hosted member

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3344.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `organization.revoke_hosted_member` — Bounded revokeHostedMemberRequest: organization, member; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → revokeHostedMemberResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role hosted owner/admin; owner-only restrictions for owner membership/role changes; invitation acceptance bound to invited identity; credential hosted browser session; bearer credentials refused by session-only APIs; CSRF for mutation; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.revokeHostedMemberJSON; s.config.Hosted.Provider.RevokeMembership, s.hostedAdministrator, s.hostedManagedMember, s.revokeHostedMemberLocally
- Extraction: Extract hubserver.revokeHostedMemberJSON application inputs/results and validation from Echo; reuse s.config.Hosted.Provider.RevokeMembership, s.hostedAdministrator, s.hostedManagedMember, s.revokeHostedMemberLocally. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=true; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_dedicated / github,native / hub application service
- Availability: hosted_shared / github,native / hub application service
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: mutation of access, billing, deletion, cancellation or material external state → operator

Sources: [DELETE /api/v2/organizations/:organization/members/:member](../internal/hubserver/hosted_org_api.go#L37), [POST /organization/members/:member/revoke](../internal/hubserver/hosted_ui.go#L45), [internal/web/templates/hosted.templ:249](../internal/web/templates/hosted.templ#L249), [internal/web/templates/hosted.templ:249](../internal/web/templates/hosted.templ#L249), [web/conversation/src/app/account/api.ts:175](../web/conversation/src/app/account/api.ts#L175)
## hubserver.revoke_hosted_shared_sessions

Revoke hosted shared sessions

- Audience: transport; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Exact transport/protocol plumbing site. Application payload operations are inventoried separately; do not expose an HTTP or relay proxy tool.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential hosted browser session; bearer credentials refused by session-only APIs; CSRF for mutation; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.revokeHostedSharedSessions; s.config.now, s.revokeHostedSharedBinding
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_dedicated / github,native / hub application service
- Availability: hosted_shared / github,native / hub application service
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: this non-operator source site → not_applicable

Sources: [POST /internal/v1/sessions/revoke](../internal/hubserver/hosted_shared.go#L189)
## hubserver.revoke_project_policy

Revoke project policy

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3342.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `projects.revoke_project_policy` — Bounded revokeProjectPolicyRequest: owner, repo; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → revokeProjectPolicyResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role instance admin in unhosted hub; hosted owner/admin through requireHostedAdministration; credential admin; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.revokeProjectPolicy; s.database.db.ExecContext, s.policyScope
- Extraction: Extract hubserver.revokeProjectPolicy application inputs/results and validation from Echo; reuse s.database.db.ExecContext, s.policyScope. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=true; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / hub application service
- Availability: hosted_dedicated / github,native / hub application service
- Availability: hosted_shared / github,native / hub application service
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: mutation of access, billing, deletion, cancellation or material external state → operator

Sources: [DELETE /api/v1/repositories/:owner/:repo/policy](../internal/hubserver/api_http.go#L59), [DELETE /api/v2/organizations/:organization/projects/:project/policy](../internal/hubserver/native_api.go#L112)
## hubserver.revoke_runner_enrollment

Revoke runner enrollment

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3343.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `runs_fleet.revoke_runner_enrollment` — Bounded revokeRunnerEnrollmentRequest: organization, enrollment; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → revokeRunnerEnrollmentResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role instance admin in unhosted hub; hosted owner/admin through requireHostedAdministration; credential admin; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.revokeRunnerEnrollment; s.runnerTransaction
- Extraction: Extract hubserver.revokeRunnerEnrollment application inputs/results and validation from Echo; reuse s.runnerTransaction. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=true; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / hub application service
- Availability: hosted_dedicated / github,native / hub application service
- Availability: hosted_shared / github,native / hub application service
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: mutation of access, billing, deletion, cancellation or material external state → operator

Sources: [DELETE /api/v2/organizations/:organization/runner-enrollments/:enrollment](../internal/hubserver/runner_enrollment.go#L26)
## hubserver.revoke_runner_identity

Revoke runner identity

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3343.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `runs_fleet.revoke_runner_identity` — Bounded revokeRunnerIdentityRequest: organization, runner; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → revokeRunnerIdentityResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role instance admin in unhosted hub; hosted owner/admin through requireHostedAdministration; credential admin; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.revokeRunnerIdentity; s.runnerTransaction
- Extraction: Extract hubserver.revokeRunnerIdentity application inputs/results and validation from Echo; reuse s.runnerTransaction. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=true; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / hub application service
- Availability: hosted_dedicated / github,native / hub application service
- Availability: hosted_shared / github,native / hub application service
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: mutation of access, billing, deletion, cancellation or material external state → operator

Sources: [DELETE /api/v2/organizations/:organization/runners/:runner](../internal/hubserver/runner_enrollment.go#L31)
## hubserver.rotate_api_token

Rotate a p i token

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3344.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `organization.rotate_api_token` — Bounded rotateAPITokenRequest: id; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → rotateAPITokenResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role instance admin in unhosted hub; hosted owner/admin through requireHostedAdministration; credential hub worker/operator/admin per registration; native-only credentials cannot call legacy v1; hosted sessions cannot call outside nativeBase; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.rotateAPIToken; s.config.generateToken, s.database.currentTime, s.database.db.ExecContext, s.database.db.QueryRowContext
- Extraction: Extract hubserver.rotateAPIToken application inputs/results and validation from Echo; reuse s.config.generateToken, s.database.currentTime, s.database.db.ExecContext, s.database.db.QueryRowContext. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=true; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / hub application service
- Availability: hosted_dedicated / github,native / hub application service — unavailable: Current requireAPIScope refuses hosted-session calls outside nativeBase and rejects global credentials; use the corresponding native/hosted application operation. This row remains pending parity work.
- Availability: hosted_shared / github,native / hub application service — unavailable: Current requireAPIScope refuses hosted-session calls outside nativeBase and rejects global credentials; use the corresponding native/hosted application operation. This row remains pending parity work.
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: mutation of access, billing, deletion, cancellation or material external state → operator

Sources: [POST /api/v1/tokens/:id/rotate](../internal/hubserver/api_http.go#L77)
## hubserver.rotate_api_token_maintenance

Rotate a p i token maintenance

- Audience: staff; status: **excluded**; owner: digitaldrywood/detent#3344.
- Decision: Platform/instance staff authority is stricter than organization operator authority. Preserve this restriction; no organization-operator tool grants staff powers.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role private loopback credential-maintenance instance admin; no hosted member, runner, support or native-only credential; credential staff session or dedicated private instance-admin credential; not an organization operator credential; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.rotateAPIToken; s.config.generateToken, s.database.currentTime, s.database.db.ExecContext, s.database.db.QueryRowContext
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_dedicated / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / hub application service
- Confirmation: this non-operator source site → not_applicable

Sources: [POST /api/v1/tokens/:id/rotate](../internal/hubserver/credential_maintenance.go#L25)
## hubserver.rotate_runner_identity

Rotate runner identity

- Audience: worker; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Worker protocol only: preserve worker credential, producer/lease and runner ownership; organization operator tools cannot impersonate a worker.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated worker/runner; credential worker scope; runner credential/lease where applicable; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.rotateRunnerIdentity; s.changeRunnerCredential
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / hub application service
- Availability: hosted_dedicated / github,native / hub application service
- Availability: hosted_shared / github,native / hub application service
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: this non-operator source site → not_applicable

Sources: [POST /api/v2/organizations/:organization/runners/:runner/rotate](../internal/hubserver/runner_enrollment.go#L30)
## hubserver.router_bind

Router.bind

- Audience: worker; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Worker protocol only: preserve worker credential, producer/lease and runner ownership; organization operator tools cannot impersonate a worker.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated worker/runner; credential worker scope; runner credential/lease where applicable; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: router.bind; handler-owned application validation/read/command
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: this non-operator source site → not_applicable

Sources: [POST /api/v2/organizations/:organization/projects/:project/work-items/:item/conversation/bind](../internal/hubserver/conversation_worker.go#L89)
## hubserver.router_controls

Router.controls

- Audience: worker; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Worker protocol only: preserve worker credential, producer/lease and runner ownership; organization operator tools cannot impersonate a worker.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated worker/runner; credential worker scope; runner credential/lease where applicable; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: router.controls; handler-owned application validation/read/command
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: this non-operator source site → not_applicable

Sources: [GET /api/v2/organizations/:organization/projects/:project/conversations/:conversation/controls](../internal/hubserver/conversation_worker.go#L91)
## hubserver.router_turn_events

Router.turn events

- Audience: worker; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Worker protocol only: preserve worker credential, producer/lease and runner ownership; organization operator tools cannot impersonate a worker.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated worker/runner; credential worker scope; runner credential/lease where applicable; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: router.turnEvents; handler-owned application validation/read/command
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: this non-operator source site → not_applicable

Sources: [POST /api/v2/organizations/:organization/projects/:project/conversations/:conversation/turn-events](../internal/hubserver/conversation_worker.go#L92)
## hubserver.router_unbind

Router.unbind

- Audience: worker; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Worker protocol only: preserve worker credential, producer/lease and runner ownership; organization operator tools cannot impersonate a worker.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated worker/runner; credential worker scope; runner credential/lease where applicable; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: router.unbind; handler-owned application validation/read/command
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: this non-operator source site → not_applicable

Sources: [POST /api/v2/organizations/:organization/projects/:project/work-items/:item/conversation/unbind](../internal/hubserver/conversation_worker.go#L90)
## hubserver.save_onboarding

Save onboarding

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3342.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `projects.save_onboarding` — Bounded saveOnboardingRequest: organization, project; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → saveOnboardingResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.saveOnboarding; s.nativeMutation
- Extraction: Extract hubserver.saveOnboarding application inputs/results and validation from Echo; reuse s.nativeMutation. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=true; idempotent=false; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: ordinary non-destructive edit → none
- Confirmation: arguments remove data, alter access or create material external effects → operator

Sources: [PUT /api/v2/organizations/:organization/projects/:project/onboarding](../internal/hubserver/onboarding.go#L28)
## hubserver.set_project_secret

Set project secret

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3342.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `projects.set_project_secret` — Bounded setProjectSecretRequest: organization, project, kind; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → setProjectSecretResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role hosted owner/admin for provider secrets; unhosted native operator/admin per project-secret administration guard; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.setProjectSecret; s.config.SecretKeys.Seal, s.config.SecretKeys.Version, s.config.now, s.secretMutation
- Extraction: Extract hubserver.setProjectSecret application inputs/results and validation from Echo; reuse s.config.SecretKeys.Seal, s.config.SecretKeys.Version, s.config.now, s.secretMutation. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=true; idempotent=false; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: mutation of access, billing, deletion, cancellation or material external state → operator

Sources: [PUT /api/v2/organizations/:organization/projects/:project/secrets/:kind](../internal/hubserver/project_secrets.go#L81), [web/conversation/src/app/account/SpritesCard.tsx:50](../web/conversation/src/app/account/SpritesCard.tsx#L50), [web/conversation/src/app/account/SpritesCard.tsx:50](../web/conversation/src/app/account/SpritesCard.tsx#L50), [web/conversation/src/app/account/api.ts:231](../web/conversation/src/app/account/api.ts#L231)
## hubserver.start_git_hub_import

Start git hub import

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3342.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `projects.start_git_hub_import` — Bounded startGitHubImportRequest: organization, project; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → startGitHubImportResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.startGitHubImport; s.nativeMutation
- Extraction: Extract hubserver.startGitHubImport application inputs/results and validation from Echo; reuse s.nativeMutation. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=false; idempotent=false; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [POST /api/v2/organizations/:organization/projects/:project/imports](../internal/hubserver/integration.go#L153)
## hubserver.start_hosted_invitation

Start hosted invitation

- Audience: authentication; status: **excluded**; owner: digitaldrywood/detent#3336.
- Decision: Identity-provider login exchange belongs to connection setup, not model-controlled tool arguments. Meaningful organization/session commands are separate rows.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role hosted owner/admin; owner-only restrictions for owner membership/role changes; invitation acceptance bound to invited identity; credential none until connection authentication; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.startHostedInvitation; s.config.Hosted.Provider.Invitation, s.database.db.ExecContext, s.database.db.QueryRowContext, s.hostedDenied, s.hostedProviderOrganization, s.newHostedTransaction
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_dedicated / github,native / hub application service
- Availability: hosted_shared / github,native / hub application service
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: connection authentication → connection

Sources: [GET /invite](../internal/hubserver/hosted_ui.go#L27)
## hubserver.start_hosted_login

Start hosted login

- Audience: authentication; status: **excluded**; owner: digitaldrywood/detent#3336.
- Decision: Identity-provider login exchange belongs to connection setup, not model-controlled tool arguments. Meaningful organization/session commands are separate rows.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role public identity exchange/asset; credential none until connection authentication; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.startHostedLogin; s.hostedDenied, s.hostedProviderOrganization, s.newHostedTransaction
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_dedicated / github,native / hub application service
- Availability: hosted_shared / github,native / hub application service
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: connection authentication → connection

Sources: [GET /auth/oidc/start](../internal/hubserver/hosted_ui.go#L25), [internal/web/templates/hosted.templ:30](../internal/web/templates/hosted.templ#L30), [internal/web/templates/hosted.templ:58](../internal/web/templates/hosted.templ#L58), [internal/web/templates/hosted.templ:118](../internal/web/templates/hosted.templ#L118)
## hubserver.start_hosted_support

Start hosted support

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3344.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `organization.start_hosted_support` — Bounded startHostedSupportRequest: organization; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → startHostedSupportResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential hosted browser session; bearer credentials refused by session-only APIs; CSRF for mutation; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.startHostedSupportJSON; s.beginHostedSupport, s.hostedEntryOwned
- Extraction: Extract hubserver.startHostedSupportJSON application inputs/results and validation from Echo; reuse s.beginHostedSupport, s.hostedEntryOwned. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.; Current handler authority checks: s.hostedSession(c); err != nil
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=false; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_dedicated / github,native / hub application service
- Availability: hosted_shared / github,native / hub application service
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [POST /api/v2/organizations/:organization/support/start](../internal/hubserver/hosted_org_api.go#L45), [POST /support/start](../internal/hubserver/hosted_ui.go#L29), [internal/web/templates/hosted.templ:294](../internal/web/templates/hosted.templ#L294), [internal/web/templates/hosted.templ:294](../internal/web/templates/hosted.templ#L294), [web/conversation/src/app/account/api.ts:214](../web/conversation/src/app/account/api.ts#L214), [web/conversation/src/app/entry/PlatformConsole.tsx:97](../web/conversation/src/app/entry/PlatformConsole.tsx#L97), [web/conversation/src/app/entry/PlatformConsole.tsx:97](../web/conversation/src/app/entry/PlatformConsole.tsx#L97)
## hubserver.static_assets

Static assets

- Audience: asset; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Static application assets carry no application operation; no MCP asset-serving tool.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role public identity exchange/asset; credential none until connection authentication; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: echo.WrapHandler(http.StripPrefix("/static/", http.FileServerFS(detent.StaticFS()))); handler-owned application validation/read/command
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_dedicated / github,native / hub application service
- Availability: hosted_shared / github,native / hub application service
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: this non-operator source site → not_applicable

Sources: [GET /static/*](../internal/hubserver/hosted_ui.go#L22)
## hubserver.stream_conversation_events

Stream conversation events

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3346.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `conversations_workspaces.stream_conversation_events` — Bounded streamConversationEventsRequest: organization, project, conversation; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → streamConversationEventsResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.streamConversationEvents; s.reauthorizeConversationStream, service.broker.subscribe, service.loadConversation
- Extraction: Extract hubserver.streamConversationEvents application inputs/results and validation from Echo; reuse s.reauthorizeConversationStream, service.broker.subscribe, service.loadConversation. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v2/organizations/:organization/projects/:project/conversations/:conversation/events](../internal/hubserver/conversation_api.go#L42), [web/conversation/src/runtime/rpc/sse.ts:150](../web/conversation/src/runtime/rpc/sse.ts#L150)
## hubserver.submit_change_check

Submit change check

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3347.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `changes_artifacts.submit_change_check` — Bounded submitChangeCheckRequest: organization, project, item, change, version; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → submitChangeCheckResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.submitChangeCheck; s.nativeMutation
- Extraction: Extract hubserver.submitChangeCheck application inputs/results and validation from Echo; reuse s.nativeMutation. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=false; idempotent=false; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [POST /api/v2/organizations/:organization/projects/:project/work-items/:item/changes/:change/versions/:version/checks](../internal/hubserver/changes.go#L84)
## hubserver.switch_hosted_organization

Switch hosted organization

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3336.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `organization.switch_hosted_organization` — Bounded switchHostedOrganizationRequest: organization; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → switchHostedOrganizationResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential hosted browser session; bearer credentials refused by session-only APIs; CSRF for mutation; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.switchHostedOrganizationJSON; s.hostedEntryOwned, s.hostedSwitchDestination
- Extraction: Extract hubserver.switchHostedOrganizationJSON application inputs/results and validation from Echo; reuse s.hostedEntryOwned, s.hostedSwitchDestination. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.; Current handler authority checks: s.hostedSession(c); err != nil
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=false; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_dedicated / github,native / hub application service
- Availability: hosted_shared / github,native / hub application service
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [POST /api/v2/organizations/:organization/switch](../internal/hubserver/hosted_org_api.go#L43), [POST /organization/switch](../internal/hubserver/hosted_ui.go#L43), [internal/web/templates/hosted.templ:141](../internal/web/templates/hosted.templ#L141), [internal/web/templates/hosted.templ:141](../internal/web/templates/hosted.templ#L141), [web/conversation/src/app/account/api.ts:209](../web/conversation/src/app/account/api.ts#L209)
## hubserver.transition_native_issue

Transition native issue

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3341.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `work_commands.transition_native_issue` — Bounded transitionNativeIssueRequest: organization, project, item; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → transitionNativeIssueResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.transitionNativeIssue; s.nativeMutation
- Extraction: Extract hubserver.transitionNativeIssue application inputs/results and validation from Echo; reuse s.nativeMutation. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Request governed transition; orchestrator alone writes tracker lane state (INV-1); Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=true; idempotent=false; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: non-terminal lane request → none
- Confirmation: terminal/destructive lane target → operator

Sources: [POST /api/v2/organizations/:organization/projects/:project/work-items/:item/workflow](../internal/hubserver/native_api.go#L132), [web/conversation/src/app/work/lib/workHttp.ts:566](../web/conversation/src/app/work/lib/workHttp.ts#L566)
## hubserver.unbind_workspace_worker

Unbind workspace worker

- Audience: worker; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Worker protocol only: preserve worker credential, producer/lease and runner ownership; organization operator tools cannot impersonate a worker.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated worker/runner; credential worker scope; runner credential/lease where applicable; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.unbindWorkspaceWorker; s.hubTransact, s.requireWorkspaces, service.committed, service.endWorkspace, service.loadWorkspaceForWorker
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Current handler authority checks: s.requireWorkspaces()
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: this non-operator source site → not_applicable

Sources: [POST /api/v2/organizations/:organization/projects/:project/workspaces/:workspace/worker/unbind](../internal/hubserver/native_api.go#L167)
## hubserver.update_hosted_plan

Update hosted plan

- Audience: staff; status: **excluded**; owner: digitaldrywood/detent#3345.
- Decision: Requires entitlementAdministrator/current configured platform administration identity. Organization owners/operators do not receive this staff-only power.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role configured entitlement administrator; not organization owner/admin; credential staff session or dedicated private instance-admin credential; not an organization operator credential; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.updateHostedPlan; s.database.applyHostedPlanCommand, s.entitlementAdministrator
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Current handler authority checks: s.entitlementAdministrator(c)
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_dedicated / github,native / hub application service
- Availability: hosted_shared / github,native / hub application service
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: this non-operator source site → not_applicable

Sources: [POST /api/v2/organizations/:organization/entitlements](../internal/hubserver/hosted_ui.go#L53)
## hubserver.update_native_comment

Update native comment

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3341.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `work_commands.update_native_comment` — Bounded updateNativeCommentRequest: organization, project, item, comment; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → updateNativeCommentResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.updateNativeComment; s.nativeMutation
- Extraction: Extract hubserver.updateNativeComment application inputs/results and validation from Echo; reuse s.nativeMutation. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=true; idempotent=false; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: ordinary non-destructive edit → none
- Confirmation: arguments remove data, alter access or create material external effects → operator

Sources: [PATCH /api/v2/organizations/:organization/projects/:project/work-items/:item/comments/:comment](../internal/hubserver/native_api.go#L136)
## hubserver.update_native_issue

Update native issue

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3341.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `work_commands.update_native_issue` — Bounded updateNativeIssueRequest: organization, project, item; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → updateNativeIssueResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.updateNativeIssue; s.nativeMutation
- Extraction: Extract hubserver.updateNativeIssue application inputs/results and validation from Echo; reuse s.nativeMutation. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=true; idempotent=false; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: ordinary non-destructive edit → none
- Confirmation: arguments remove data, alter access or create material external effects → operator

Sources: [PATCH /api/v2/organizations/:organization/projects/:project/work-items/:item](../internal/hubserver/native_api.go#L128), [web/conversation/src/app/work/lib/workHttp.ts:556](../web/conversation/src/app/work/lib/workHttp.ts#L556)
## hubserver.update_project_integration

Update project integration

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3342.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `projects.update_project_integration` — Bounded updateProjectIntegrationRequest: organization, project; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → updateProjectIntegrationResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role instance admin in unhosted hub; hosted owner/admin through requireHostedAdministration; credential admin; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.updateProjectIntegration; s.nativeMutation
- Extraction: Extract hubserver.updateProjectIntegration application inputs/results and validation from Echo; reuse s.nativeMutation. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=true; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: ordinary non-destructive edit → none
- Confirmation: arguments remove data, alter access or create material external effects → operator

Sources: [PUT /api/v2/organizations/:organization/projects/:project/integration](../internal/hubserver/integration.go#L149), [PUT /api/v2/organizations/:organization/projects/:project/onboarding/integration](../internal/hubserver/onboarding.go#L25), [web/conversation/src/app/account/api.ts:250](../web/conversation/src/app/account/api.ts#L250)
## hubserver.update_runner_host

Update runner host

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3343.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `runs_fleet.update_runner_host` — Bounded updateRunnerHostRequest: organization, machine; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → updateRunnerHostResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role instance admin in unhosted hub; hosted owner/admin through requireHostedAdministration; credential admin; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.updateRunnerHost; s.runnerTransaction
- Extraction: Extract hubserver.updateRunnerHost application inputs/results and validation from Echo; reuse s.runnerTransaction. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=true; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / hub application service
- Availability: hosted_dedicated / github,native / hub application service
- Availability: hosted_shared / github,native / hub application service
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: ordinary non-destructive edit → none
- Confirmation: arguments remove data, alter access or create material external effects → operator

Sources: [PUT /api/v2/organizations/:organization/machines/:machine/routing](../internal/hubserver/runner_enrollment.go#L35)
## hubserver.update_runner_routing

Update runner routing

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3343.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `runs_fleet.update_runner_routing` — Bounded updateRunnerRoutingRequest: organization, runner; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → updateRunnerRoutingResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role instance admin in unhosted hub; hosted owner/admin through requireHostedAdministration; credential admin; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.updateRunnerRouting; s.runnerTransaction
- Extraction: Extract hubserver.updateRunnerRouting application inputs/results and validation from Echo; reuse s.runnerTransaction. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=true; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / hub application service
- Availability: hosted_dedicated / github,native / hub application service
- Availability: hosted_shared / github,native / hub application service
- Availability: credential_maintenance / github,native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: ordinary non-destructive edit → none
- Confirmation: arguments remove data, alter access or create material external effects → operator

Sources: [PUT /api/v2/organizations/:organization/runners/:runner/routing](../internal/hubserver/runner_enrollment.go#L34), [web/conversation/src/app/account/api.ts:368](../web/conversation/src/app/account/api.ts#L368)
## hubserver.upload_conversation_attachment

Upload conversation attachment

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3346.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `conversations_workspaces.upload_conversation_attachment` — Bounded uploadConversationAttachmentRequest: organization, project, conversation; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → uploadConversationAttachmentResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.uploadConversationAttachment; s.database.checkHostedGrowth, s.database.hostedConsumption, service.authorizeWrite, service.loadConversation, service.requireActorAuthority, service.sameAttachmentUpload, service.store.insertAttachment, service.store.readAttachmentByRef, service.transact
- Extraction: Extract hubserver.uploadConversationAttachment application inputs/results and validation from Echo; reuse s.database.checkHostedGrowth, s.database.hostedConsumption, service.authorizeWrite, service.loadConversation, service.requireActorAuthority, service.sameAttachmentUpload, service.store.insertAttachment, service.store.readAttachmentByRef, service.transact. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=false; idempotent=false; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [POST /api/v2/organizations/:organization/projects/:project/conversations/:conversation/attachments](../internal/hubserver/conversation_api.go#L47), [web/conversation/src/runtime/rpc/http.ts:486](../web/conversation/src/runtime/rpc/http.ts#L486)
## hubserver.validate_runner_lease

Validate runner lease

- Audience: worker; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Worker protocol only: preserve worker credential, producer/lease and runner ownership; organization operator tools cannot impersonate a worker.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated worker/runner; credential worker scope; runner credential/lease where applicable; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.validateRunnerLease; s.runnerTransaction
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: this non-operator source site → not_applicable

Sources: [POST /api/v2/organizations/:organization/projects/:project/leases/:lease/validate](../internal/hubserver/runner_enrollment.go#L36)
## hubserver.view_change_file

View change file

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3347.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `changes_artifacts.view_change_file` — Bounded viewChangeFileRequest: organization, project, item, change, version; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → viewChangeFileResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role organization member/viewer for reads; owner/admin/operator or explicit project grant for writes; credential worker or operator (native project); exact registration middleware retained in source; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.viewChangeFile; s.nativeMutation
- Extraction: Extract hubserver.viewChangeFile application inputs/results and validation from Echo; reuse s.nativeMutation. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=false; idempotent=false; openWorld=false. Authorization/confirmation still apply.
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [POST /api/v2/organizations/:organization/projects/:project/work-items/:item/changes/:change/versions/:version/viewed-files](../internal/hubserver/changes.go#L83)
## hubserver.workspace_for_work_item

Workspace for work item

- Audience: worker; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Worker protocol only: preserve worker credential, producer/lease and runner ownership; organization operator tools cannot impersonate a worker.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role authenticated worker/runner; credential worker scope; runner credential/lease where applicable; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.workspaceForWorkItem; s.hubTransact, s.requireWorkspaces
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Current handler authority checks: s.requireWorkspaces(); err != nil
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / native / hub application service
- Availability: hosted_dedicated / native / hub application service
- Availability: hosted_shared / native / hub application service
- Availability: credential_maintenance / native / hub application service — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: this non-operator source site → not_applicable

Sources: [GET /api/v2/organizations/:organization/projects/:project/work-items/:item/workspace](../internal/hubserver/native_api.go#L161)
## web.ai_debug_prompt

Ai debug prompt

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3340.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `work_reads.ai_debug_prompt` — Bounded aiDebugPromptRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → aiDebugPromptResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local operator (dashboard authentication when configured); credential read/write/admin (project scope where route supplies project); project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.aiDebugPrompt; s.aiDebugProjection
- Extraction: Extract web.aiDebugPrompt application inputs/results and validation from Echo; reuse s.aiDebugProjection. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v1/ai-debug](../internal/web/server.go#L544), [internal/web/templates/ai_debug.templ:9](../internal/web/templates/ai_debug.templ#L9), [internal/web/templates/ai_debug.templ:192](../internal/web/templates/ai_debug.templ#L192)
## web.analytics_dashboard

Analytics dashboard

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3340.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `work_reads.analytics_dashboard` — Bounded analyticsDashboardRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → analyticsDashboardResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local operator (dashboard authentication when configured); credential read; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.analyticsDashboard; s.analyticsDashboardData, s.demoAnalyticsDashboard, s.latestSnapshot
- Extraction: Extract web.analyticsDashboard application inputs/results and validation from Echo; reuse s.analyticsDashboardData, s.demoAnalyticsDashboard, s.latestSnapshot. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /analytics](../internal/web/server.go#L469), [internal/web/templates/analytics.templ:78](../internal/web/templates/analytics.templ#L78), [internal/web/templates/analytics.templ:80](../internal/web/templates/analytics.templ#L80), [internal/web/templates/analytics.templ:79](../internal/web/templates/analytics.templ#L79)
## web.api_board_activity

Api board activity

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3340.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `work_reads.api_board_activity` — Bounded apiBoardActivityRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → apiBoardActivityResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local operator (dashboard authentication when configured); credential read/write/admin (project scope where route supplies project); project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.apiBoardActivity; s.boardActivityData, s.demoDashboardData, s.latestSnapshot
- Extraction: Extract web.apiBoardActivity application inputs/results and validation from Echo; reuse s.boardActivityData, s.demoDashboardData, s.latestSnapshot. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v1/board/activity](../internal/web/server.go#L549), [internal/web/templates/activity.templ:131](../internal/web/templates/activity.templ#L131), [internal/web/templates/activity.templ:80](../internal/web/templates/activity.templ#L80), [internal/web/templates/activity.templ:47](../internal/web/templates/activity.templ#L47), [internal/web/templates/activity.templ:95](../internal/web/templates/activity.templ#L95), [internal/web/templates/activity.templ:276](../internal/web/templates/activity.templ#L276)
## web.api_board_activity_events

Api board activity events

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3340.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `work_reads.api_board_activity_events` — Bounded apiBoardActivityEventsRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → apiBoardActivityEventsResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local operator (dashboard authentication when configured); credential read/write/admin (project scope where route supplies project); project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.apiBoardActivityEvents; s.hub.Subscribe, s.latestSnapshot, s.sendBoardActivity
- Extraction: Extract web.apiBoardActivityEvents application inputs/results and validation from Echo; reuse s.hub.Subscribe, s.latestSnapshot, s.sendBoardActivity. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v1/board/activity/events](../internal/web/server.go#L550)
## web.api_board_card

Api board card

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3340.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `work_reads.api_board_card` — Bounded apiBoardCardRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → apiBoardCardResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local operator (dashboard authentication when configured); credential read/write/admin (project scope where route supplies project); project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.apiBoardCard; s.boardCardDashboardData, s.kanbanConversationShellData, s.loadBoardAttemptCosts
- Extraction: Extract web.apiBoardCard application inputs/results and validation from Echo; reuse s.boardCardDashboardData, s.kanbanConversationShellData, s.loadBoardAttemptCosts. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v1/board/card](../internal/web/server.go#L545), [internal/web/templates/sheet.templ:355](../internal/web/templates/sheet.templ#L355), [internal/web/templates/sheet.templ:357](../internal/web/templates/sheet.templ#L357)
## web.api_board_card_core

Api board card core

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3340.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `work_reads.api_board_card_core` — Bounded apiBoardCardCoreRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → apiBoardCardCoreResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local operator (dashboard authentication when configured); credential read/write/admin (project scope where route supplies project); project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.apiBoardCardCore; s.boardCardDashboardData
- Extraction: Extract web.apiBoardCardCore application inputs/results and validation from Echo; reuse s.boardCardDashboardData. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v1/board/card/core](../internal/web/server.go#L546), [internal/web/templates/sheet.templ:142](../internal/web/templates/sheet.templ#L142)
## web.api_board_conversation

Api board conversation

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3346.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `conversations_workspaces.api_board_conversation` — Bounded apiBoardConversationRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → apiBoardConversationResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local operator (dashboard authentication when configured); credential read/write/admin (project scope where route supplies project); project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.apiBoardConversation; s.boardCardDashboardData, s.hydrateKanbanIssueConversation, s.hydrateKanbanPRConversation, s.kanbanConversationShellData
- Extraction: Extract web.apiBoardConversation application inputs/results and validation from Echo; reuse s.boardCardDashboardData, s.hydrateKanbanIssueConversation, s.hydrateKanbanPRConversation, s.kanbanConversationShellData. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v1/board/conversation](../internal/web/server.go#L548), [internal/web/templates/comments.templ:86](../internal/web/templates/comments.templ#L86), [internal/web/templates/comments.templ:67](../internal/web/templates/comments.templ#L67), [internal/web/templates/comments.templ:18](../internal/web/templates/comments.templ#L18), [internal/web/templates/comments.templ:153](../internal/web/templates/comments.templ#L153), [internal/web/templates/comments.templ:177](../internal/web/templates/comments.templ#L177), [internal/web/templates/comments.templ:137](../internal/web/templates/comments.templ#L137)
## web.api_board_receipt

Api board receipt

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3340.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `work_reads.api_board_receipt` — Bounded apiBoardReceiptRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → apiBoardReceiptResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local operator (dashboard authentication when configured); credential read/write/admin (project scope where route supplies project); project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.apiBoardReceipt; s.boardCardDashboardData, s.store.EfficiencyReceipt
- Extraction: Extract web.apiBoardReceipt application inputs/results and validation from Echo; reuse s.boardCardDashboardData, s.store.EfficiencyReceipt. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v1/board/receipt](../internal/web/server.go#L547), [internal/web/templates/sheet.templ:298](../internal/web/templates/sheet.templ#L298), [internal/web/templates/sheet.templ:327](../internal/web/templates/sheet.templ#L327)
## web.api_board_session

Api board session

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3340.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `work_reads.api_board_session` — Bounded apiBoardSessionRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → apiBoardSessionResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local operator (dashboard authentication when configured); credential read/write/admin (project scope where route supplies project); project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.apiBoardSession; s.demoDashboardData, s.latestSnapshot
- Extraction: Extract web.apiBoardSession application inputs/results and validation from Echo; reuse s.demoDashboardData, s.latestSnapshot. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v1/board/session](../internal/web/server.go#L551), [internal/web/templates/activity.templ:148](../internal/web/templates/activity.templ#L148), [internal/web/templates/activity.templ:231](../internal/web/templates/activity.templ#L231), [internal/web/templates/activity.templ:206](../internal/web/templates/activity.templ#L206), [internal/web/templates/activity.templ:20](../internal/web/templates/activity.templ#L20)
## web.api_board_session_events

Api board session events

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3340.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `work_reads.api_board_session_events` — Bounded apiBoardSessionEventsRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → apiBoardSessionEventsResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local operator (dashboard authentication when configured); credential read/write/admin (project scope where route supplies project); project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.apiBoardSessionEvents; s.activity.Subscribe, s.latestSnapshot
- Extraction: Extract web.apiBoardSessionEvents application inputs/results and validation from Echo; reuse s.activity.Subscribe, s.latestSnapshot. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v1/board/session/events](../internal/web/server.go#L552)
## web.api_board_session_history

Api board session history

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3340.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `work_reads.api_board_session_history` — Bounded apiBoardSessionHistoryRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → apiBoardSessionHistoryResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local operator (dashboard authentication when configured); credential read/write/admin (project scope where route supplies project); project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.apiBoardSessionHistory; s.history.Page, s.latestSnapshot
- Extraction: Extract web.apiBoardSessionHistory application inputs/results and validation from Echo; reuse s.history.Page, s.latestSnapshot. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v1/board/session/history](../internal/web/server.go#L553)
## web.api_budget_override_clear

Api budget override clear

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3345.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `billing_usage.api_budget_override_clear` — Bounded apiBudgetOverrideClearRequest: project_id; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → apiBudgetOverrideClearResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local operator (dashboard authentication when configured); credential project write or global write/admin; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.apiBudgetOverrideClear; s.renderProjectBudgetPanel
- Extraction: Extract web.apiBudgetOverrideClear application inputs/results and validation from Echo; reuse s.renderProjectBudgetPanel. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=true; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: mutation of access, billing, deletion, cancellation or material external state → operator

Sources: [DELETE /api/v1/projects/:project_id/budget/override](../internal/web/server.go#L517)
## web.api_budget_override_set

Api budget override set

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3345.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `billing_usage.api_budget_override_set` — Bounded apiBudgetOverrideSetRequest: project_id; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → apiBudgetOverrideSetResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local operator (dashboard authentication when configured); credential project write or global write/admin; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.apiBudgetOverrideSet; s.registry.Get, s.renderProjectBudgetPanel
- Extraction: Extract web.apiBudgetOverrideSet application inputs/results and validation from Echo; reuse s.registry.Get, s.renderProjectBudgetPanel. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=true; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: mutation of access, billing, deletion, cancellation or material external state → operator

Sources: [POST /api/v1/projects/:project_id/budget/override](../internal/web/server.go#L516), [internal/web/templates/project.templ:158](../internal/web/templates/project.templ#L158), [internal/web/templates/project.templ:142](../internal/web/templates/project.templ#L142), [internal/web/templates/project.templ:142](../internal/web/templates/project.templ#L142)
## web.api_capacity_clear

Api capacity clear

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3343.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `runs_fleet.api_capacity_clear` — Bounded apiCapacityClearRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → apiCapacityClearResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local administrator; credential admin; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.apiCapacityClear; s.registry.Get, s.registry.List
- Extraction: Extract web.apiCapacityClear application inputs/results and validation from Echo; reuse s.registry.Get, s.registry.List. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=false; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [POST /api/v1/capacity/clear](../internal/web/server.go#L535), [internal/web/templates/dashboard.templ:337](../internal/web/templates/dashboard.templ#L337), [internal/web/templates/dashboard.templ:337](../internal/web/templates/dashboard.templ#L337)
## web.api_chat_confirm

Api chat confirm

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3337.
- Decision: Reuse real operator approval/rejection. Model-originated confirmation calls cannot approve their own pending material action.
- Tool: `conversations_workspaces.api_chat_confirm` — Bounded apiChatConfirmRequest: action_id; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → apiChatConfirmResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local operator (dashboard authentication when configured); credential global write/admin; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.apiChatConfirm; s.authorizeChatAction, s.chat.Confirm, s.chatContext
- Extraction: Extract web.apiChatConfirm application inputs/results and validation from Echo; reuse s.authorizeChatAction, s.chat.Confirm, s.chatContext. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.; Real operator approval/rejection is required at the existing approval surface; an MCP model cannot approve its own action by invoking a confirmation tool.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=true; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: real operator confirms the pending material action → operator

Sources: [POST /api/v1/chat/actions/:action_id/confirm](../internal/web/server.go#L556), [internal/web/templates/chat.templ:98](../internal/web/templates/chat.templ#L98)
## web.api_chat_message

Api chat message

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3346.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `conversations_workspaces.api_chat_message` — Bounded apiChatMessageRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → apiChatMessageResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local operator (dashboard authentication when configured); credential global write/admin; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.apiChatMessage; s.chat.Conversation, s.chat.Send, s.chatContext
- Extraction: Extract web.apiChatMessage application inputs/results and validation from Echo; reuse s.chat.Conversation, s.chat.Send, s.chatContext. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=false; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [POST /api/v1/chat/messages](../internal/web/server.go#L555), [internal/web/templates/chat.templ:35](../internal/web/templates/chat.templ#L35), [internal/web/templates/chat.templ:33](../internal/web/templates/chat.templ#L33)
## web.api_chat_panel

Api chat panel

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3346.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `conversations_workspaces.api_chat_panel` — Bounded apiChatPanelRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → apiChatPanelResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local operator (dashboard authentication when configured); credential read/write/admin (project scope where route supplies project); project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.apiChatPanel; s.chat.Conversation
- Extraction: Extract web.apiChatPanel application inputs/results and validation from Echo; reuse s.chat.Conversation. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v1/chat](../internal/web/server.go#L554), [internal/web/templates/shell.templ:259](../internal/web/templates/shell.templ#L259)
## web.api_chat_reject

Api chat reject

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3337.
- Decision: Reuse real operator approval/rejection. Model-originated confirmation calls cannot approve their own pending material action.
- Tool: `conversations_workspaces.api_chat_reject` — Bounded apiChatRejectRequest: action_id; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → apiChatRejectResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local operator (dashboard authentication when configured); credential global write/admin; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.apiChatReject; s.chat.Reject
- Extraction: Extract web.apiChatReject application inputs/results and validation from Echo; reuse s.chat.Reject. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.; Real operator approval/rejection is required at the existing approval surface; an MCP model cannot approve its own action by invoking a confirmation tool.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=false; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [POST /api/v1/chat/actions/:action_id/reject](../internal/web/server.go#L557), [internal/web/templates/chat.templ:97](../internal/web/templates/chat.templ#L97)
## web.api_create_work_item

Api create work item

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3341.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `work_commands.api_create_work_item` — Bounded apiCreateWorkItemRequest: project_id; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → apiCreateWorkItemResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local operator (dashboard authentication when configured); credential project write or global write/admin; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.apiCreateWorkItem; s.registry.Get, s.requestKanbanRefresh
- Extraction: Extract web.apiCreateWorkItem application inputs/results and validation from Echo; reuse s.registry.Get, s.requestKanbanRefresh. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=false; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [POST /api/v1/projects/:project_id/work-items](../internal/web/server.go#L514)
## web.api_demo_scenarios

Api demo scenarios

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3340.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `work_reads.api_demo_scenarios` — Bounded apiDemoScenariosRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → apiDemoScenariosResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local operator (dashboard authentication when configured); credential read/write/admin (project scope where route supplies project); project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.apiDemoScenarios; handler-owned application validation/read/command
- Extraction: Extract web.apiDemoScenarios application inputs/results and validation from Echo; reuse the current handler-owned service logic. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v1/demo/scenarios](../internal/web/server.go#L510)
## web.api_failure_breaker_canary

Api failure breaker canary

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3343.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `runs_fleet.api_failure_breaker_canary` — Bounded apiFailureBreakerCanaryRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → apiFailureBreakerCanaryResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local administrator; credential admin; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.apiFailureBreakerCanary; s.registry.Get, s.registry.List
- Extraction: Extract web.apiFailureBreakerCanary application inputs/results and validation from Echo; reuse s.registry.Get, s.registry.List. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=false; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [POST /api/v1/failure-breaker/canary](../internal/web/server.go#L538), [internal/web/templates/dashboard.templ:227](../internal/web/templates/dashboard.templ#L227), [internal/web/templates/dashboard.templ:227](../internal/web/templates/dashboard.templ#L227)
## web.api_forge_availability_clear

Api forge availability clear

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3343.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `runs_fleet.api_forge_availability_clear` — Bounded apiForgeAvailabilityClearRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → apiForgeAvailabilityClearResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local administrator; credential admin; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.apiForgeAvailabilityClear; s.registry.Get, s.registry.List
- Extraction: Extract web.apiForgeAvailabilityClear application inputs/results and validation from Echo; reuse s.registry.Get, s.registry.List. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=false; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [POST /api/v1/forge/availability/clear](../internal/web/server.go#L537)
## web.api_issue

Api issue

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3340.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `work_reads.api_issue` — Bounded apiIssueRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → apiIssueResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local operator (dashboard authentication when configured); credential read/write/admin (project scope where route supplies project); project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.apiIssue; s.cachedEnrichedSnapshot, s.hub.Latest
- Extraction: Extract web.apiIssue application inputs/results and validation from Echo; reuse s.cachedEnrichedSnapshot, s.hub.Latest. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v1/*](../internal/web/server.go#L566), [internal/web/templates/dashboard.templ:4245](../internal/web/templates/dashboard.templ#L4245)
## web.api_issue_explanation

Api issue explanation

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3345.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `billing_usage.api_issue_explanation` — Bounded apiIssueExplanationRequest: project_id; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → apiIssueExplanationResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local operator (dashboard authentication when configured); credential read/write/admin (project scope where route supplies project); project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.apiIssueExplanation; s.issueExplanation
- Extraction: Extract web.apiIssueExplanation application inputs/results and validation from Echo; reuse s.issueExplanation. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v1/projects/:project_id/issues/explanation](../internal/web/server.go#L519)
## web.api_issue_park_acknowledgement

Api issue park acknowledgement

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3341.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `work_commands.api_issue_park_acknowledgement` — Bounded apiIssueParkAcknowledgementRequest: project_id; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → apiIssueParkAcknowledgementResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local operator (dashboard authentication when configured); credential project write or global write/admin; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.apiIssueParkAcknowledgement; s.issueExplanation
- Extraction: Extract web.apiIssueParkAcknowledgement application inputs/results and validation from Echo; reuse s.issueExplanation. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=false; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [POST /api/v1/projects/:project_id/issues/explanation](../internal/web/server.go#L520)
## web.api_issue_priority

Api issue priority

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3341.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `work_commands.api_issue_priority` — Bounded apiIssuePriorityRequest: project_id, issue_id; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → apiIssuePriorityResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local operator (dashboard authentication when configured); credential project write or global write/admin; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.apiIssuePriority; s.requestKanbanRefresh, s.setIssuePriority
- Extraction: Extract web.apiIssuePriority application inputs/results and validation from Echo; reuse s.requestKanbanRefresh, s.setIssuePriority. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=false; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [POST /api/v1/projects/:project_id/issues/:issue_id/priority](../internal/web/server.go#L558)
## web.api_issue_progress_credit

Api issue progress credit

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3343.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `runs_fleet.api_issue_progress_credit` — Bounded apiIssueProgressCreditRequest: project_id; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → apiIssueProgressCreditResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local operator (dashboard authentication when configured); credential project write or global write/admin; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.apiIssueProgressCredit; s.issueExplanation, s.now
- Extraction: Extract web.apiIssueProgressCredit application inputs/results and validation from Echo; reuse s.issueExplanation, s.now. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=false; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [POST /api/v1/projects/:project_id/issues/progress-credit](../internal/web/server.go#L521)
## web.api_kanban_comment

Api kanban comment

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3341.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `work_commands.api_kanban_comment` — Bounded apiKanbanCommentRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → apiKanbanCommentResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local operator (dashboard authentication when configured); credential read/write/admin (project scope where route supplies project); project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.apiKanbanCommentDialog; s.kanbanCommentDialogData
- Extraction: Extract web.apiKanbanCommentDialog application inputs/results and validation from Echo; reuse s.kanbanCommentDialogData. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=false; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v1/kanban/comment](../internal/web/server.go#L562), [POST /api/v1/kanban/comment](../internal/web/server.go#L563), [internal/web/templates/sheet.templ:369](../internal/web/templates/sheet.templ#L369)
## web.api_kanban_comment_delete

Api kanban comment delete

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3341.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `work_commands.api_kanban_comment_delete` — Bounded apiKanbanCommentDeleteRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → apiKanbanCommentDeleteResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local operator (dashboard authentication when configured); credential project write or global write/admin; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.apiKanbanCommentDelete; s.kanbanActionTarget, s.kanbanCommentCanMutate, s.kanbanMutations.WithLock, s.requestKanbanRefresh
- Extraction: Extract web.apiKanbanCommentDelete application inputs/results and validation from Echo; reuse s.kanbanActionTarget, s.kanbanCommentCanMutate, s.kanbanMutations.WithLock, s.requestKanbanRefresh. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=true; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: mutation of access, billing, deletion, cancellation or material external state → operator

Sources: [DELETE /api/v1/kanban/comment](../internal/web/server.go#L565), [internal/web/templates/comments.templ:103](../internal/web/templates/comments.templ#L103), [internal/web/templates/comments.templ:103](../internal/web/templates/comments.templ#L103), [internal/web/templates/dashboard.templ:3190](../internal/web/templates/dashboard.templ#L3190), [internal/web/templates/dashboard.templ:3190](../internal/web/templates/dashboard.templ#L3190), [internal/web/templates/comments.templ:185](../internal/web/templates/comments.templ#L185), [internal/web/templates/comments.templ:185](../internal/web/templates/comments.templ#L185)
## web.api_kanban_comment_edit

Api kanban comment edit

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3341.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `work_commands.api_kanban_comment_edit` — Bounded apiKanbanCommentEditRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → apiKanbanCommentEditResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local operator (dashboard authentication when configured); credential project write or global write/admin; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.apiKanbanCommentEdit; s.kanbanActionTarget, s.kanbanCommentCanMutate, s.kanbanMutations.WithLock, s.requestKanbanRefresh
- Extraction: Extract web.apiKanbanCommentEdit application inputs/results and validation from Echo; reuse s.kanbanActionTarget, s.kanbanCommentCanMutate, s.kanbanMutations.WithLock, s.requestKanbanRefresh. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=false; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [POST /api/v1/kanban/comment/edit](../internal/web/server.go#L564), [internal/web/templates/comments.templ:203](../internal/web/templates/comments.templ#L203), [internal/web/templates/comments.templ:203](../internal/web/templates/comments.templ#L203)
## web.api_kanban_move

Api kanban move

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3341.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `work_commands.api_kanban_move` — Bounded apiKanbanMoveRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → apiKanbanMoveResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local operator (dashboard authentication when configured); credential read/write/admin (project scope where route supplies project); project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.apiKanbanMoveDialog; s.kanbanMoveDialogData
- Extraction: Extract web.apiKanbanMoveDialog application inputs/results and validation from Echo; reuse s.kanbanMoveDialogData. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=false; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v1/kanban/move](../internal/web/server.go#L559), [POST /api/v1/kanban/move](../internal/web/server.go#L560), [internal/web/templates/board.templ:721](../internal/web/templates/board.templ#L721), [internal/web/templates/board.templ:721](../internal/web/templates/board.templ#L721), [internal/web/templates/dashboard.templ:3139](../internal/web/templates/dashboard.templ#L3139), [internal/web/templates/dashboard.templ:3139](../internal/web/templates/dashboard.templ#L3139), [internal/web/templates/sheet.templ:86](../internal/web/templates/sheet.templ#L86), [internal/web/templates/board.templ:647](../internal/web/templates/board.templ#L647)
## web.api_kanban_remove

Api kanban remove

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3341.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `work_commands.api_kanban_remove` — Bounded apiKanbanRemoveRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → apiKanbanRemoveResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local operator (dashboard authentication when configured); credential project write or global write/admin; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.apiKanbanRemove; s.kanbanActionTarget, s.kanbanCardFresh, s.kanbanMutations.NoteCardRemoved, s.kanbanMutations.WithLock, s.kanbanRemoveSuccess
- Extraction: Extract web.apiKanbanRemove application inputs/results and validation from Echo; reuse s.kanbanActionTarget, s.kanbanCardFresh, s.kanbanMutations.NoteCardRemoved, s.kanbanMutations.WithLock, s.kanbanRemoveSuccess. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=true; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: mutation of access, billing, deletion, cancellation or material external state → operator

Sources: [POST /api/v1/kanban/remove](../internal/web/server.go#L561), [internal/web/templates/sheet.templ:99](../internal/web/templates/sheet.templ#L99), [internal/web/templates/sheet.templ:99](../internal/web/templates/sheet.templ#L99)
## web.api_keys_create

Api keys create

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3344.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `organization.api_keys_create` — Bounded apiKeysCreateRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → apiKeysCreateResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local administrator; credential admin; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.apiKeysCreate; s.apiKeys.Create, s.apiKeysData
- Extraction: Extract web.apiKeysCreate application inputs/results and validation from Echo; reuse s.apiKeys.Create, s.apiKeysData. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=false; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [POST /api/v1/keys](../internal/web/server.go#L529)
## web.api_keys_list

Api keys list

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3344.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `organization.api_keys_list` — Bounded apiKeysListRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → apiKeysListResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local administrator; credential admin; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.apiKeysList; s.apiKeysData, s.store.ListAPIKeys
- Extraction: Extract web.apiKeysList application inputs/results and validation from Echo; reuse s.apiKeysData, s.store.ListAPIKeys. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v1/keys](../internal/web/server.go#L528), [internal/web/templates/api_keys.templ:161](../internal/web/templates/api_keys.templ#L161), [internal/web/templates/api_keys.templ:30](../internal/web/templates/api_keys.templ#L30), [internal/web/templates/api_keys.templ:46](../internal/web/templates/api_keys.templ#L46), [internal/web/templates/api_keys.templ:149](../internal/web/templates/api_keys.templ#L149), [internal/web/templates/api_keys.templ:111](../internal/web/templates/api_keys.templ#L111), [internal/web/templates/api_keys.templ:190](../internal/web/templates/api_keys.templ#L190), [internal/web/templates/api_keys.templ:260](../internal/web/templates/api_keys.templ#L260), [internal/web/templates/api_keys.templ:190](../internal/web/templates/api_keys.templ#L190), [internal/web/templates/api_keys.templ:260](../internal/web/templates/api_keys.templ#L260)
## web.api_keys_page

Api keys page

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3344.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `organization.api_keys_page` — Bounded apiKeysPageRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → apiKeysPageResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local operator (dashboard authentication when configured); credential read; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.apiKeysPage; s.apiKeysData
- Extraction: Extract web.apiKeysPage application inputs/results and validation from Echo; reuse s.apiKeysData. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api-keys](../internal/web/server.go#L481)
## web.api_keys_revoke

Api keys revoke

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3344.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `organization.api_keys_revoke` — Bounded apiKeysRevokeRequest: id; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → apiKeysRevokeResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local administrator; credential admin; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.apiKeysRevoke; s.apiKeys.Revoke, s.apiKeysData
- Extraction: Extract web.apiKeysRevoke application inputs/results and validation from Echo; reuse s.apiKeys.Revoke, s.apiKeysData. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=true; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: mutation of access, billing, deletion, cancellation or material external state → operator

Sources: [DELETE /api/v1/keys/:id](../internal/web/server.go#L532)
## web.api_keys_rotate

Api keys rotate

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3344.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `organization.api_keys_rotate` — Bounded apiKeysRotateRequest: id; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → apiKeysRotateResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local administrator; credential admin; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.apiKeysRotateDialog; s.apiKeysData
- Extraction: Extract web.apiKeysRotateDialog application inputs/results and validation from Echo; reuse s.apiKeysData. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=false; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v1/keys/:id/rotate](../internal/web/server.go#L530), [POST /api/v1/keys/:id/rotate](../internal/web/server.go#L531)
## web.api_operations

Api operations

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3343.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `runs_fleet.api_operations` — Bounded apiOperationsRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → apiOperationsResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local operator (dashboard authentication when configured); credential read/write/admin (project scope where route supplies project); project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.apiOperations; s.operationsReport
- Extraction: Extract web.apiOperations application inputs/results and validation from Echo; reuse s.operationsReport. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v1/operations](../internal/web/server.go#L509)
## web.api_operator_tool

Api operator tool

- Audience: transport; status: **excluded**; owner: digitaldrywood/detent#3340.
- Decision: Exact authenticated transport entry for existing governed application services. No MCP raw HTTP/relay/tool-forwarding proxy; meaningful typed application operations and the five existing read tools are separate rows.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role local operator (dashboard authentication when configured); credential read/write/admin (project scope where route supplies project); project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.apiOperatorTool; s.chatContext, s.operatorTools.Execute
- Extraction: No raw transport command extraction; share authorization with the typed payload operations.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: transport authority establishment → connection

Sources: [POST /api/v1/operator-tools/:tool_name](../internal/web/server.go#L512)
## web.api_project

Api project

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3342.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `projects.api_project` — Bounded apiProjectRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → apiProjectResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local operator (dashboard authentication when configured); credential read/write/admin (project scope where route supplies project); project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.apiProject; s.apiProjectState, s.apiProjectTimeSeries
- Extraction: Extract web.apiProject application inputs/results and validation from Echo; reuse s.apiProjectState, s.apiProjectTimeSeries. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v1/projects/*](../internal/web/server.go#L527), [internal/web/templates/dashboard.templ:2398](../internal/web/templates/dashboard.templ#L2398)
## web.api_refresh

Api refresh

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3343.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `runs_fleet.api_refresh` — Bounded apiRefreshRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → apiRefreshResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local operator (dashboard authentication when configured); credential global write/admin; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.apiRefresh; s.demoRefresh, s.refreshRefusal, s.refresher.RequestRefresh
- Extraction: Extract web.apiRefresh application inputs/results and validation from Echo; reuse s.demoRefresh, s.refreshRefusal, s.refresher.RequestRefresh. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=false; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [POST /api/v1/refresh](../internal/web/server.go#L533)
## web.api_security_audit_disposition

Api security audit disposition

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3341.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `work_commands.api_security_audit_disposition` — Bounded apiSecurityAuditDispositionRequest: project_id; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → apiSecurityAuditDispositionResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local operator (dashboard authentication when configured); credential audit disposition authority for project; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.apiSecurityAuditDisposition; s.now, s.registry.Get
- Extraction: Extract web.apiSecurityAuditDisposition application inputs/results and validation from Echo; reuse s.now, s.registry.Get. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=false; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [POST /api/v1/projects/:project_id/security-audits/dispositions](../internal/web/server.go#L515)
## web.api_staleness_warning_acknowledgement

Api staleness warning acknowledgement

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3343.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `runs_fleet.api_staleness_warning_acknowledgement` — Bounded apiStalenessWarningAcknowledgementRequest: project_id, warning_id; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → apiStalenessWarningAcknowledgementResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local operator (dashboard authentication when configured); credential project write or global write/admin; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.apiStalenessWarningAcknowledgement; s.acknowledgeStalenessWarnings
- Extraction: Extract web.apiStalenessWarningAcknowledgement application inputs/results and validation from Echo; reuse s.acknowledgeStalenessWarnings. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=false; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [POST /api/v1/projects/:project_id/staleness-warnings/:warning_id/acknowledge](../internal/web/server.go#L523), [internal/web/templates/board.templ:265](../internal/web/templates/board.templ#L265), [internal/web/templates/board.templ:263](../internal/web/templates/board.templ#L263)
## web.api_staleness_warnings_acknowledgement

Api staleness warnings acknowledgement

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3343.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `runs_fleet.api_staleness_warnings_acknowledgement` — Bounded apiStalenessWarningsAcknowledgementRequest: project_id; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → apiStalenessWarningsAcknowledgementResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local operator (dashboard authentication when configured); credential project write or global write/admin; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.apiStalenessWarningsAcknowledgement; s.acknowledgeStalenessWarnings
- Extraction: Extract web.apiStalenessWarningsAcknowledgement application inputs/results and validation from Echo; reuse s.acknowledgeStalenessWarnings. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=false; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [POST /api/v1/projects/:project_id/staleness-warnings/acknowledge](../internal/web/server.go#L522)
## web.api_state

Api state

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3340.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `work_reads.api_state` — Bounded apiStateRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → apiStateResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local operator (dashboard authentication when configured); credential read/write/admin (project scope where route supplies project); project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.apiState; s.hub.Latest, s.instanceName, s.now, s.stateEndpoints.begin, s.stateSnapshot, s.withManualRefresh
- Extraction: Extract web.apiState application inputs/results and validation from Echo; reuse s.hub.Latest, s.instanceName, s.now, s.stateEndpoints.begin, s.stateSnapshot, s.withManualRefresh. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v1/state](../internal/web/server.go#L508), [internal/web/templates/dashboard.templ:2396](../internal/web/templates/dashboard.templ#L2396)
## web.api_stop_run

Api stop run

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3343.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `runs_fleet.api_stop_run` — Bounded apiStopRunRequest: project_id, attempt; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → apiStopRunResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local operator (dashboard authentication when configured); credential read/write/admin (project scope where route supplies project); project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.apiStopRunDialog; s.latestSnapshot, s.stopRunDemoScenario
- Extraction: Extract web.apiStopRunDialog application inputs/results and validation from Echo; reuse s.latestSnapshot, s.stopRunDemoScenario. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=false; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v1/projects/:project_id/runs/:attempt/stop](../internal/web/server.go#L525), [POST /api/v1/projects/:project_id/runs/:attempt/stop](../internal/web/server.go#L526), [internal/web/templates/stop_run.templ:42](../internal/web/templates/stop_run.templ#L42), [internal/web/templates/stop_run.templ:42](../internal/web/templates/stop_run.templ#L42), [internal/web/templates/activity.templ:175](../internal/web/templates/activity.templ#L175), [internal/web/templates/fleet.templ:199](../internal/web/templates/fleet.templ#L199)
## web.api_time_series

Api time series

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3340.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `work_reads.api_time_series` — Bounded apiTimeSeriesRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → apiTimeSeriesResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local operator (dashboard authentication when configured); credential read/write/admin (project scope where route supplies project); project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.apiTimeSeries; s.latestSnapshot, s.projectSmallMultiples
- Extraction: Extract web.apiTimeSeries application inputs/results and validation from Echo; reuse s.latestSnapshot, s.projectSmallMultiples. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v1/timeseries](../internal/web/server.go#L511), [internal/web/templates/dashboard.templ:2028](../internal/web/templates/dashboard.templ#L2028), [static/js/dashboard-charts.js:73](../static/js/dashboard-charts.js#L73)
## web.api_tracker_availability_clear

Api tracker availability clear

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3343.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `runs_fleet.api_tracker_availability_clear` — Bounded apiTrackerAvailabilityClearRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → apiTrackerAvailabilityClearResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local administrator; credential admin; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.apiTrackerAvailabilityClear; s.registry.Get, s.registry.List
- Extraction: Extract web.apiTrackerAvailabilityClear application inputs/results and validation from Echo; reuse s.registry.Get, s.registry.List. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=false; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [POST /api/v1/tracker/availability/clear](../internal/web/server.go#L536)
## web.api_update_apply

Api update apply

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3343.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `runs_fleet.api_update_apply` — Bounded apiUpdateApplyRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → apiUpdateApplyResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local administrator; credential admin; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.apiUpdateApply; s.updateApplier.ApplyPending
- Extraction: Extract web.apiUpdateApply application inputs/results and validation from Echo; reuse s.updateApplier.ApplyPending. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=true; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: mutation of access, billing, deletion, cancellation or material external state → operator

Sources: [POST /api/v1/update/apply](../internal/web/server.go#L534), [internal/web/templates/update_pending.templ:23](../internal/web/templates/update_pending.templ#L23), [internal/web/templates/update_pending.templ:23](../internal/web/templates/update_pending.templ#L23)
## web.api_usage

Api usage

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3345.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `billing_usage.api_usage` — Bounded apiUsageRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → apiUsageResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local operator (dashboard authentication when configured); credential read/write/admin (project scope where route supplies project); project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.apiUsage; s.store.UsageReport
- Extraction: Extract web.apiUsage application inputs/results and validation from Echo; reuse s.store.UsageReport. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v1/usage](../internal/web/server.go#L542)
## web.api_work_attempt_receipt

Api work attempt receipt

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3340.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `work_reads.api_work_attempt_receipt` — Bounded apiWorkAttemptReceiptRequest: project_id, attempt_id; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → apiWorkAttemptReceiptResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local operator (dashboard authentication when configured); credential read/write/admin (project scope where route supplies project); project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.apiWorkAttemptReceipt; s.recovery.WorkAttemptReceipt
- Extraction: Extract web.apiWorkAttemptReceipt application inputs/results and validation from Echo; reuse s.recovery.WorkAttemptReceipt. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v1/projects/:project_id/work-attempts/:attempt_id](../internal/web/server.go#L518), [internal/web/templates/dashboard.templ:3907](../internal/web/templates/dashboard.templ#L3907)
## web.api_work_attempt_recovery

Api work attempt recovery

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3343.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `runs_fleet.api_work_attempt_recovery` — Bounded apiWorkAttemptRecoveryRequest: project_id, attempt_id; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → apiWorkAttemptRecoveryResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local operator (dashboard authentication when configured); credential project write or global write/admin; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.apiWorkAttemptRecovery; s.recovery.RecoverWorkAttempt
- Extraction: Extract web.apiWorkAttemptRecovery application inputs/results and validation from Echo; reuse s.recovery.RecoverWorkAttempt. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=false; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [POST /api/v1/projects/:project_id/work-attempts/:attempt_id/recovery](../internal/web/server.go#L524), [internal/web/templates/dashboard.templ:3909](../internal/web/templates/dashboard.templ#L3909), [internal/web/templates/dashboard.templ:3909](../internal/web/templates/dashboard.templ#L3909), [internal/web/templates/dashboard.templ:3910](../internal/web/templates/dashboard.templ#L3910)
## web.api_workflow_timeline

Api workflow timeline

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3341.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `work_commands.api_workflow_timeline` — Bounded apiWorkflowTimelineRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → apiWorkflowTimelineResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local operator (dashboard authentication when configured); credential read/write/admin (project scope where route supplies project); project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.apiWorkflowTimeline; s.store.IssueWorkflowTimeline
- Extraction: Extract web.apiWorkflowTimeline application inputs/results and validation from Echo; reuse s.store.IssueWorkflowTimeline. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /api/v1/workflow/timeline](../internal/web/server.go#L543)
## web.artifact_access

Artifact access

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3347.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `changes_artifacts.artifact_access` — Bounded artifactAccessRequest: project_id, issue_ref, artifact; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → artifactAccessResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local operator (dashboard authentication when configured); credential write; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.artifactAccess; s.nativeFormData
- Extraction: Extract web.artifactAccess application inputs/results and validation from Echo; reuse s.nativeFormData. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=false; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [POST /projects/:project_id/issues/:issue_ref/artifacts/:artifact/access](../internal/web/server.go#L478)
## web.board

Board

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3340.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `work_reads.board` — Bounded boardRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → boardResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local operator (dashboard authentication when configured); credential read; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.board; s.boardFirstPaintData, s.demoBoard, s.latestBoardSnapshot, s.withKanbanRefreshFeedback
- Extraction: Extract web.board application inputs/results and validation from Echo; reuse s.boardFirstPaintData, s.demoBoard, s.latestBoardSnapshot, s.withKanbanRefreshFeedback. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /](../internal/web/server.go#L462), [internal/web/templates/board.templ:1912](../internal/web/templates/board.templ#L1912), [internal/web/templates/board.templ:230](../internal/web/templates/board.templ#L230), [internal/web/templates/board.templ:1914](../internal/web/templates/board.templ#L1914), [internal/web/templates/board.templ:247](../internal/web/templates/board.templ#L247), [internal/web/templates/shell.templ:101](../internal/web/templates/shell.templ#L101), [internal/web/templates/shell.templ:192](../internal/web/templates/shell.templ#L192), [internal/web/templates/shell.templ:151](../internal/web/templates/shell.templ#L151)
## web.board_live_session_page

Board live session page

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3340.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `work_reads.board_live_session_page` — Bounded boardLiveSessionPageRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → boardLiveSessionPageResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local operator (dashboard authentication when configured); credential read; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.boardLiveSessionPage; s.boardData, s.demoDashboardData, s.latestSnapshot
- Extraction: Extract web.boardLiveSessionPage application inputs/results and validation from Echo; reuse s.boardData, s.demoDashboardData, s.latestSnapshot. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /live-session](../internal/web/server.go#L463), [internal/web/templates/activity.templ:156](../internal/web/templates/activity.templ#L156), [internal/web/templates/activity.templ:186](../internal/web/templates/activity.templ#L186), [internal/web/templates/activity.templ:204](../internal/web/templates/activity.templ#L204)
## web.change_detail

Change detail

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3347.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `changes_artifacts.change_detail` — Bounded changeDetailRequest: project_id, issue_ref, change; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → changeDetailResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local operator (dashboard authentication when configured); credential read; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.changeDetail; s.changePageData, s.loadArtifacts
- Extraction: Extract web.changeDetail application inputs/results and validation from Echo; reuse s.changePageData, s.loadArtifacts. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /projects/:project_id/issues/:issue_ref/changes/:change](../internal/web/server.go#L475), [internal/web/templates/change.templ:50](../internal/web/templates/change.templ#L50), [internal/web/templates/change_files.templ:13](../internal/web/templates/change_files.templ#L13), [internal/web/templates/change.templ:8](../internal/web/templates/change.templ#L8), [internal/web/templates/change.templ:22](../internal/web/templates/change.templ#L22), [internal/web/templates/change.templ:138](../internal/web/templates/change.templ#L138)
## web.change_review_action

Change review action

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3347.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `changes_artifacts.change_review_action` — Bounded changeReviewActionRequest: project_id, issue_ref, change, version; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → changeReviewActionResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role authorized change reviewer; approval policy and no self-approval; credential write; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.changeReviewAction; s.nativeFormData
- Extraction: Extract web.changeReviewAction application inputs/results and validation from Echo; reuse s.nativeFormData. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Current version, reviewer identity and change review policy; reject self-approval; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=false; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [POST /projects/:project_id/issues/:issue_ref/changes/:change/versions/:version/review](../internal/web/server.go#L476), [internal/web/templates/change.templ:200](../internal/web/templates/change.templ#L200), [internal/web/templates/change_files.templ:30](../internal/web/templates/change_files.templ#L30), [internal/web/templates/change_files.templ:30](../internal/web/templates/change_files.templ#L30), [internal/web/templates/change_files.templ:67](../internal/web/templates/change_files.templ#L67), [internal/web/templates/change_files.templ:66](../internal/web/templates/change_files.templ#L66), [internal/web/templates/change_files.templ:65](../internal/web/templates/change_files.templ#L65), [internal/web/templates/change_files.templ:43](../internal/web/templates/change_files.templ#L43)
## web.complete_o_i_d_c

Complete o i d c

- Audience: authentication; status: **excluded**; owner: digitaldrywood/detent#3336.
- Decision: Identity-provider login exchange belongs to connection setup, not model-controlled tool arguments. Meaningful organization/session commands are separate rows.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role public identity exchange/asset; credential none until connection authentication; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.completeOIDC; s.identityAllowlist.Allows, s.identityProvider.Exchange, s.oidcTransaction, s.renderAuthPage
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: connection authentication → connection

Sources: [GET /auth/oidc/callback](../internal/web/server.go#L449)
## web.consume_magic_link

Consume magic link

- Audience: authentication; status: **excluded**; owner: digitaldrywood/detent#3336.
- Decision: Identity-provider login exchange belongs to connection setup, not model-controlled tool arguments. Meaningful organization/session commands are separate rows.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role public identity exchange/asset; credential none until connection authentication; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.consumeMagicLink; s.magicLinks.ConsumeLink, s.renderAuthPage
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: connection authentication → connection

Sources: [GET /auth/magic-link](../internal/web/server.go#L445)
## web.dashboard

Dashboard

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3343.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `runs_fleet.dashboard` — Bounded dashboardRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → dashboardResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local operator (dashboard authentication when configured); credential read; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.dashboard; s.dashboardFirstPaintData, s.demoDashboard, s.latestBoardSnapshot
- Extraction: Extract web.dashboard application inputs/results and validation from Echo; reuse s.dashboardFirstPaintData, s.demoDashboard, s.latestBoardSnapshot. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /fleet](../internal/web/server.go#L464), [internal/web/templates/runner_fleet.templ:21](../internal/web/templates/runner_fleet.templ#L21), [internal/web/templates/runner_fleet.templ:145](../internal/web/templates/runner_fleet.templ#L145), [internal/web/templates/runner_fleet.templ:145](../internal/web/templates/runner_fleet.templ#L145), [web/conversation/src/app/account/api.ts:412](../web/conversation/src/app/account/api.ts#L412), [internal/web/templates/dashboard.templ:1757](../internal/web/templates/dashboard.templ#L1757), [internal/web/templates/dashboard.templ:2665](../internal/web/templates/dashboard.templ#L2665), [internal/web/templates/dashboard.templ:2513](../internal/web/templates/dashboard.templ#L2513), [internal/web/templates/dashboard.templ:2640](../internal/web/templates/dashboard.templ#L2640), [internal/web/templates/dashboard.templ:3083](../internal/web/templates/dashboard.templ#L3083), [internal/web/templates/dashboard.templ:89](../internal/web/templates/dashboard.templ#L89), [internal/web/templates/dashboard.templ:249](../internal/web/templates/dashboard.templ#L249), [internal/web/templates/dashboard.templ:2233](../internal/web/templates/dashboard.templ#L2233), [internal/web/templates/dashboard.templ:4251](../internal/web/templates/dashboard.templ#L4251), [internal/web/templates/dashboard.templ:4258](../internal/web/templates/dashboard.templ#L4258)
## web.diagnostics_dashboard

Diagnostics dashboard

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3343.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `runs_fleet.diagnostics_dashboard` — Bounded diagnosticsDashboardRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → diagnosticsDashboardResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local operator (dashboard authentication when configured); credential read; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.diagnosticsDashboard; s.diagnosticsDashboardData, s.latestSnapshot
- Extraction: Extract web.diagnosticsDashboard application inputs/results and validation from Echo; reuse s.diagnosticsDashboardData, s.latestSnapshot. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /diagnostics](../internal/web/server.go#L468), [internal/web/templates/dashboard.templ:2228](../internal/web/templates/dashboard.templ#L2228)
## web.echo__wrap_handler(s_mcp_h_t_t_p)

Echo. wrap handler(s.mcp h t t p)

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3340.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `work_reads.echo__wrap_handler(s_mcp_h_t_t_p)` — Bounded echo.WrapHandler(s.mcpHTTP)Request: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → echo.WrapHandler(s.mcpHTTP)Result: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local operator (dashboard authentication when configured); credential write; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: echo.WrapHandler(s.mcpHTTP); handler-owned application validation/read/command
- Extraction: Extract web.echo.WrapHandler(s.mcpHTTP) application inputs/results and validation from Echo; reuse the current handler-owned service logic. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=false; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [Any /mcp](../internal/web/server.go#L513)
## web.events

Events

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3340.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `work_reads.events` — Bounded eventsRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → eventsResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local operator (dashboard authentication when configured); credential read; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.events; s.cachedEnrichedSnapshot, s.dashboardData, s.demoEvents, s.hub.Subscribe, s.now, s.projectDashboardData, s.snapshotMissingRequiredChecks, s.sseSnapshotComponent, s.withManualRefresh
- Extraction: Extract web.events application inputs/results and validation from Echo; reuse s.cachedEnrichedSnapshot, s.dashboardData, s.demoEvents, s.hub.Subscribe, s.now, s.projectDashboardData, s.snapshotMissingRequiredChecks, s.sseSnapshotComponent, s.withManualRefresh. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /events](../internal/web/server.go#L484)
## web.github_webhook

Github webhook

- Audience: transport; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Exact transport/protocol plumbing site. Application payload operations are inventoried separately; do not expose an HTTP or relay proxy tool.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role local operator (dashboard authentication when configured); credential write; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.githubWebhook; s.githubWebhookRoutes, s.requestWebhookRefresh
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: this non-operator source site → not_applicable

Sources: [POST /api/v1/webhooks/github](../internal/web/server.go#L539)
## web.health

Health

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3340.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `work_reads.health` — Bounded healthRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → healthResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local operator (dashboard authentication when configured); credential read; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.health; s.connectorName, s.enforcedBudgets, s.healthNotifications.Failures, s.hub.Latest, s.now, s.orphanedAgentProcesses, s.projectHealth, s.startupLifecycle.State, s.stateEndpoints.health, s.tickLiveness.TickLiveness, s.workflowSources
- Extraction: Extract web.health application inputs/results and validation from Echo; reuse s.connectorName, s.enforcedBudgets, s.healthNotifications.Failures, s.hub.Latest, s.now, s.orphanedAgentProcesses, s.projectHealth, s.startupLifecycle.State, s.stateEndpoints.health, s.tickLiveness.TickLiveness, s.workflowSources. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /health](../internal/web/server.go#L438)
## web.health_dashboard

Health dashboard

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3343.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `runs_fleet.health_dashboard` — Bounded healthDashboardRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → healthDashboardResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local operator (dashboard authentication when configured); credential read; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.healthDashboard; s.demoHealthDashboard, s.healthDashboardData, s.latestSnapshot
- Extraction: Extract web.healthDashboard application inputs/results and validation from Echo; reuse s.demoHealthDashboard, s.healthDashboardData, s.latestSnapshot. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /health/ui](../internal/web/server.go#L467), [internal/web/templates/work.templ:70](../internal/web/templates/work.templ#L70), [internal/web/templates/health.templ:74](../internal/web/templates/health.templ#L74)
## web.intake_webhook

Intake webhook

- Audience: transport; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Exact transport/protocol plumbing site. Application payload operations are inventoried separately; do not expose an HTTP or relay proxy tool.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role local operator (dashboard authentication when configured); credential write; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.intakeWebhook; s.registry.Get, s.requestIntakeRefresh, s.resolveGitHubWebhookSecret
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: this non-operator source site → not_applicable

Sources: [POST /api/v1/intake/:project_id/:source](../internal/web/server.go#L540)
## web.issue_detail

Issue detail

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3340.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `work_reads.issue_detail` — Bounded issueDetailRequest: project_id, issue_ref; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → issueDetailResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local operator (dashboard authentication when configured); credential read; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.issueDetail; s.latestSnapshot, s.loadNativeWork, s.projectDashboardData, s.registry.Get
- Extraction: Extract web.issueDetail application inputs/results and validation from Echo; reuse s.latestSnapshot, s.loadNativeWork, s.projectDashboardData, s.registry.Get. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /projects/:project_id/issues/:issue_ref](../internal/web/server.go#L474), [internal/web/templates/issue_detail.templ:11](../internal/web/templates/issue_detail.templ#L11), [internal/web/templates/native_work.templ:146](../internal/web/templates/native_work.templ#L146)
## web.library

Library

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3340.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `work_reads.library` — Bounded libraryRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → libraryResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local operator (dashboard authentication when configured); credential read; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.library; s.libraryData
- Extraction: Extract web.library application inputs/results and validation from Echo; reuse s.libraryData. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /library](../internal/web/server.go#L470), [internal/web/templates/library.templ:20](../internal/web/templates/library.templ#L20), [internal/web/templates/library.templ:58](../internal/web/templates/library.templ#L58), [internal/web/templates/library.templ:20](../internal/web/templates/library.templ#L20), [internal/web/templates/library.templ:127](../internal/web/templates/library.templ#L127), [internal/web/templates/library.templ:117](../internal/web/templates/library.templ#L117), [internal/web/templates/library.templ:122](../internal/web/templates/library.templ#L122)
## web.login_page

Login page

- Audience: authentication; status: **excluded**; owner: digitaldrywood/detent#3336.
- Decision: Identity-provider login exchange belongs to connection setup, not model-controlled tool arguments. Meaningful organization/session commands are separate rows.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role public identity exchange/asset; credential none until connection authentication; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.loginPage; s.renderAuthPage
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: connection authentication → connection

Sources: [GET /login](../internal/web/server.go#L441), [internal/web/templates/auth.templ:30](../internal/web/templates/auth.templ#L30), [internal/web/templates/auth.templ:33](../internal/web/templates/auth.templ#L33), [internal/web/templates/auth.templ:36](../internal/web/templates/auth.templ#L36)
## web.method_not_allowed

Method not allowed

- Audience: transport; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Exact transport/protocol plumbing site. Application payload operations are inventoried separately; do not expose an HTTP or relay proxy tool.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role local operator (dashboard authentication when configured); credential read/write/admin (project scope where route supplies project); project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.methodNotAllowed; handler-owned application validation/read/command
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: this non-operator source site → not_applicable

Sources: [GET /api/v1/refresh](../internal/web/server.go#L541), [internal/web/templates/dashboard.templ:474](../internal/web/templates/dashboard.templ#L474), [internal/web/templates/dashboard.templ:2910](../internal/web/templates/dashboard.templ#L2910), [internal/web/templates/dashboard.templ:2910](../internal/web/templates/dashboard.templ#L2910), [internal/web/templates/dashboard.templ:474](../internal/web/templates/dashboard.templ#L474)
## web.native_issue_export

Native issue export

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3340.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `work_reads.native_issue_export` — Bounded nativeIssueExportRequest: project_id, issue_ref; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → nativeIssueExportResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local operator (dashboard authentication when configured); credential read; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.nativeIssueExport; s.nativeFormData
- Extraction: Extract web.nativeIssueExport application inputs/results and validation from Echo; reuse s.nativeFormData. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /projects/:project_id/issues/:issue_ref/export](../internal/web/server.go#L473)
## web.native_issue_submit

Native issue submit

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3340.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `work_reads.native_issue_submit` — Bounded nativeIssueSubmitRequest: project_id, issue_ref; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → nativeIssueSubmitResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local operator (dashboard authentication when configured); credential read; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.nativeIssueForm; s.nativeFormData
- Extraction: Extract web.nativeIssueForm application inputs/results and validation from Echo; reuse s.nativeFormData. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Form action discriminates create, edit, transition, dependency, comment, comment_edit and change; preserve variant validation and lane ownership; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=true; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: create/edit/comment/dependency ordinary request → none
- Confirmation: terminal transition, destructive edit or material change action → operator

Sources: [GET /projects/:project_id/issues/:issue_ref/edit](../internal/web/server.go#L472), [GET /projects/:project_id/issues/new](../internal/web/server.go#L471), [POST /projects/:project_id/issues/:issue_ref/edit](../internal/web/server.go#L505), [POST /projects/:project_id/issues/new](../internal/web/server.go#L504), [internal/web/templates/artifacts.templ:18](../internal/web/templates/artifacts.templ#L18), [internal/web/templates/artifacts.templ:18](../internal/web/templates/artifacts.templ#L18), [internal/web/templates/change.templ:16](../internal/web/templates/change.templ#L16), [internal/web/templates/change.templ:209](../internal/web/templates/change.templ#L209), [internal/web/templates/change.templ:98](../internal/web/templates/change.templ#L98), [internal/web/templates/native_work.templ:19](../internal/web/templates/native_work.templ#L19), [internal/web/templates/native_work.templ:168](../internal/web/templates/native_work.templ#L168), [internal/web/templates/native_work.templ:159](../internal/web/templates/native_work.templ#L159), [internal/web/templates/native_work.templ:114](../internal/web/templates/native_work.templ#L114), [internal/web/templates/native_work.templ:111](../internal/web/templates/native_work.templ#L111), [internal/web/templates/native_work.templ:21](../internal/web/templates/native_work.templ#L21), [internal/web/templates/native_work.templ:106](../internal/web/templates/native_work.templ#L106), [internal/web/templates/native_work.templ:86](../internal/web/templates/native_work.templ#L86), [internal/web/templates/native_work.templ:21](../internal/web/templates/native_work.templ#L21), [internal/web/templates/work.templ:85](../internal/web/templates/work.templ#L85), [internal/web/templates/native_work.templ:22](../internal/web/templates/native_work.templ#L22)
## web.native_run_detail

Native run detail

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3340.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `work_reads.native_run_detail` — Bounded nativeRunDetailRequest: project_id, issue_ref, attempt; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → nativeRunDetailResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local operator (dashboard authentication when configured); credential read; project native project read grant; writes need write grant; runner/terminal access additionally needs runner grant; ownership resolve project, issue, attempt, comment, change, artifact, workspace and runner under current organization; author/audience restrictions remain.
- Application: s.nativeRunDetail; s.changePageData, s.loadArtifacts
- Extraction: Extract web.nativeRunDetail application inputs/results and validation from Echo; reuse s.changePageData, s.loadArtifacts. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /projects/:project_id/issues/:issue_ref/runs/:attempt](../internal/web/server.go#L477), [internal/web/templates/change.templ:102](../internal/web/templates/change.templ#L102), [internal/web/templates/native_work.templ:126](../internal/web/templates/native_work.templ#L126)
## web.onboarding

Onboarding

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3342.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `projects.onboarding` — Bounded onboardingRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → onboardingResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local operator (dashboard authentication when configured); credential read; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.onboarding; s.onboardingData
- Extraction: Extract web.onboarding application inputs/results and validation from Echo; reuse s.onboardingData. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /onboarding](../internal/web/server.go#L453), [internal/web/templates/onboarding.templ:193](../internal/web/templates/onboarding.templ#L193), [internal/web/templates/onboarding.templ:211](../internal/web/templates/onboarding.templ#L211), [internal/web/templates/onboarding.templ:213](../internal/web/templates/onboarding.templ#L213)
## web.onboarding_agent

Onboarding agent

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3342.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `projects.onboarding_agent` — Bounded onboardingAgentRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → onboardingAgentResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local operator (dashboard authentication when configured); credential write; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.onboardingAgent; s.renderOnboardingStep
- Extraction: Extract web.onboardingAgent application inputs/results and validation from Echo; reuse s.renderOnboardingStep. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=false; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [POST /onboarding/agent](../internal/web/server.go#L457), [POST /onboarding/agent](../internal/web/server.go#L489), [internal/web/templates/onboarding.templ:355](../internal/web/templates/onboarding.templ#L355), [internal/web/templates/onboarding.templ:355](../internal/web/templates/onboarding.templ#L355)
## web.onboarding_credentials

Onboarding credentials

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3342.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `projects.onboarding_credentials` — Bounded onboardingCredentialsRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → onboardingCredentialsResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local operator (dashboard authentication when configured); credential write; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.onboardingCredentials; s.renderOnboardingStep
- Extraction: Extract web.onboardingCredentials application inputs/results and validation from Echo; reuse s.renderOnboardingStep. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=false; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [POST /onboarding/credentials](../internal/web/server.go#L455), [POST /onboarding/credentials](../internal/web/server.go#L487), [internal/web/templates/onboarding.templ:286](../internal/web/templates/onboarding.templ#L286), [internal/web/templates/onboarding.templ:408](../internal/web/templates/onboarding.templ#L408), [internal/web/templates/onboarding.templ:286](../internal/web/templates/onboarding.templ#L286)
## web.onboarding_project

Onboarding project

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3342.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `projects.onboarding_project` — Bounded onboardingProjectRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → onboardingProjectResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local operator (dashboard authentication when configured); credential write; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.onboardingProject; s.renderOnboardingStep
- Extraction: Extract web.onboardingProject application inputs/results and validation from Echo; reuse s.renderOnboardingStep. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=false; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [POST /onboarding/project](../internal/web/server.go#L456), [POST /onboarding/project](../internal/web/server.go#L488), [internal/web/templates/onboarding.templ:311](../internal/web/templates/onboarding.templ#L311), [internal/web/templates/onboarding.templ:311](../internal/web/templates/onboarding.templ#L311)
## web.onboarding_tracker

Onboarding tracker

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3342.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `projects.onboarding_tracker` — Bounded onboardingTrackerRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → onboardingTrackerResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local operator (dashboard authentication when configured); credential write; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.onboardingTracker; s.renderOnboardingStep
- Extraction: Extract web.onboardingTracker application inputs/results and validation from Echo; reuse s.renderOnboardingStep. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=false; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [POST /onboarding/tracker](../internal/web/server.go#L454), [POST /onboarding/tracker](../internal/web/server.go#L486), [internal/web/templates/onboarding.templ:261](../internal/web/templates/onboarding.templ#L261), [internal/web/templates/onboarding.templ:346](../internal/web/templates/onboarding.templ#L346), [internal/web/templates/onboarding.templ:261](../internal/web/templates/onboarding.templ#L261)
## web.onboarding_write

Onboarding write

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3342.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `projects.onboarding_write` — Bounded onboardingWriteRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → onboardingWriteResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local operator (dashboard authentication when configured); credential write; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.onboardingWrite; s.onboardingCloseoutSnapshot, s.renderOnboardingStep, s.runOnboardingCloseout
- Extraction: Extract web.onboardingWrite application inputs/results and validation from Echo; reuse s.onboardingCloseoutSnapshot, s.renderOnboardingStep, s.runOnboardingCloseout. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=false; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [POST /onboarding/write](../internal/web/server.go#L458), [POST /onboarding/write](../internal/web/server.go#L490), [internal/web/templates/onboarding.templ:460](../internal/web/templates/onboarding.templ#L460), [internal/web/templates/onboarding.templ:481](../internal/web/templates/onboarding.templ#L481), [internal/web/templates/onboarding.templ:481](../internal/web/templates/onboarding.templ#L481), [internal/web/templates/onboarding.templ:460](../internal/web/templates/onboarding.templ#L460)
## web.open_api

Open a p i

- Audience: transport; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Exact transport/protocol plumbing site. Application payload operations are inventoried separately; do not expose an HTTP or relay proxy tool.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role local operator (dashboard authentication when configured); credential read; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.openAPI; handler-owned application validation/read/command
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: this non-operator source site → not_applicable

Sources: [GET /api/v1/openapi.yaml](../internal/web/server.go#L439)
## web.operations_page

Operations page

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3343.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `runs_fleet.operations_page` — Bounded operationsPageRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → operationsPageResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local operator (dashboard authentication when configured); credential read; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.operationsPage; s.analyticsDashboardData, s.latestSnapshot, s.operationsReport
- Extraction: Extract web.operationsPage application inputs/results and validation from Echo; reuse s.analyticsDashboardData, s.latestSnapshot, s.operationsReport. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /operations](../internal/web/server.go#L483), [internal/web/templates/operations.templ:22](../internal/web/templates/operations.templ#L22), [internal/web/templates/operations.templ:113](../internal/web/templates/operations.templ#L113), [internal/web/templates/operations.templ:135](../internal/web/templates/operations.templ#L135), [internal/web/templates/operations.templ:124](../internal/web/templates/operations.templ#L124)
## web.project_dashboard

Project dashboard

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3342.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `projects.project_dashboard` — Bounded projectDashboardRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → projectDashboardResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local operator (dashboard authentication when configured); credential read; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.projectDashboard; s.demoProjectDashboard, s.latestBoardSnapshot, s.projectFirstPaintData, s.projectPageTitle, s.settingsData, s.withKanbanRefreshFeedback
- Extraction: Extract web.projectDashboard application inputs/results and validation from Echo; reuse s.demoProjectDashboard, s.latestBoardSnapshot, s.projectFirstPaintData, s.projectPageTitle, s.settingsData, s.withKanbanRefreshFeedback. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /projects/*](../internal/web/server.go#L479), [internal/web/templates/project.templ:33](../internal/web/templates/project.templ#L33), [internal/web/templates/project.templ:17](../internal/web/templates/project.templ#L17)
## web.redirect_to_board

Redirect to board

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3340.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `work_reads.redirect_to_board` — Bounded redirectToBoardRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → redirectToBoardResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local operator (dashboard authentication when configured); credential read; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.redirectToBoard; handler-owned application validation/read/command
- Extraction: Extract web.redirectToBoard application inputs/results and validation from Echo; reuse the current handler-owned service logic. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /kanban](../internal/web/server.go#L466)
## web.redirect_to_dashboard

Redirect to dashboard

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3340.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `work_reads.redirect_to_dashboard` — Bounded redirectToDashboardRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → redirectToDashboardResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local operator (dashboard authentication when configured); credential read; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.redirectToDashboard; s.demoOnboarding
- Extraction: Extract web.redirectToDashboard application inputs/results and validation from Echo; reuse s.demoOnboarding. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /onboarding](../internal/web/server.go#L485), [internal/web/templates/onboarding.templ:302](../internal/web/templates/onboarding.templ#L302), [web/conversation/src/app/account/api.ts:278](../web/conversation/src/app/account/api.ts#L278), [web/conversation/src/app/account/api.ts:279](../web/conversation/src/app/account/api.ts#L279), [web/conversation/src/app/account/api.ts:280](../web/conversation/src/app/account/api.ts#L280), [web/conversation/src/app/account/api.ts:295](../web/conversation/src/app/account/api.ts#L295), [web/conversation/src/app/account/api.ts:270](../web/conversation/src/app/account/api.ts#L270), [web/conversation/src/app/account/api.ts:387](../web/conversation/src/app/account/api.ts#L387), [web/conversation/src/app/account/api.ts:399](../web/conversation/src/app/account/api.ts#L399)
## web.redirect_to_onboarding

Redirect to onboarding

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3342.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `projects.redirect_to_onboarding` — Bounded redirectToOnboardingRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → redirectToOnboardingResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local operator (dashboard authentication when configured); credential read; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.redirectToOnboarding; handler-owned application validation/read/command
- Extraction: Extract web.redirectToOnboarding application inputs/results and validation from Echo; reuse the current handler-owned service logic. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /](../internal/web/server.go#L452)
## web.reports

Reports

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3340.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `work_reads.reports` — Bounded reportsRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → reportsResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local operator (dashboard authentication when configured); credential read; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.reports; s.demoReports, s.reportsData
- Extraction: Extract web.reports application inputs/results and validation from Echo; reuse s.demoReports, s.reportsData. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /reports](../internal/web/server.go#L482), [internal/web/templates/reportsv2.templ:33](../internal/web/templates/reportsv2.templ#L33)
## web.request_magic_link

Request magic link

- Audience: authentication; status: **excluded**; owner: digitaldrywood/detent#3336.
- Decision: Identity-provider login exchange belongs to connection setup, not model-controlled tool arguments. Meaningful organization/session commands are separate rows.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role public identity exchange/asset; credential none until connection authentication; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.requestMagicLink; s.magicLinks.RequestLink, s.renderAuthPage
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: connection authentication → connection

Sources: [POST /login](../internal/web/server.go#L444), [internal/web/templates/auth.templ:48](../internal/web/templates/auth.templ#L48), [internal/web/templates/auth.templ:48](../internal/web/templates/auth.templ#L48)
## web.runner_fleet_page

Runner fleet page

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3343.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `runs_fleet.runner_fleet_page` — Bounded runnerFleetPageRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → runnerFleetPageResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local operator (dashboard authentication when configured); credential read; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.runnerFleetPage; s.apiKeyDashboardManagementToken, s.dashboardFirstPaintData, s.instanceName, s.latestSnapshot, s.runnerFleet.Fleet, s.runnerFleet.ProjectEligibility
- Extraction: Extract web.runnerFleetPage application inputs/results and validation from Echo; reuse s.apiKeyDashboardManagementToken, s.dashboardFirstPaintData, s.instanceName, s.latestSnapshot, s.runnerFleet.Fleet, s.runnerFleet.ProjectEligibility. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /fleet/runners](../internal/web/server.go#L465), [internal/web/templates/fleet.templ:36](../internal/web/templates/fleet.templ#L36), [internal/web/templates/project.templ:21](../internal/web/templates/project.templ#L21), [internal/web/templates/runner_fleet.templ:24](../internal/web/templates/runner_fleet.templ#L24), [internal/web/templates/runner_fleet.templ:19](../internal/web/templates/runner_fleet.templ#L19), [internal/web/templates/runner_fleet.templ:26](../internal/web/templates/runner_fleet.templ#L26), [internal/web/templates/runner_fleet.templ:63](../internal/web/templates/runner_fleet.templ#L63), [internal/web/templates/runner_fleet.templ:117](../internal/web/templates/runner_fleet.templ#L117), [internal/web/templates/runner_fleet.templ:117](../internal/web/templates/runner_fleet.templ#L117)
## web.settings

Settings

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3340.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `work_reads.settings` — Bounded settingsRequest: organization context; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → settingsResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local operator (dashboard authentication when configured); credential read; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.settings; s.demoSettings, s.settingsData
- Extraction: Extract web.settings application inputs/results and validation from Echo; reuse s.demoSettings, s.settingsData. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [GET /settings](../internal/web/server.go#L480), [internal/web/templates/settings.templ:66](../internal/web/templates/settings.templ#L66), [internal/web/templates/settings.templ:14](../internal/web/templates/settings.templ#L14)
## web.start_o_i_d_c

Start o i d c

- Audience: authentication; status: **excluded**; owner: digitaldrywood/detent#3336.
- Decision: Identity-provider login exchange belongs to connection setup, not model-controlled tool arguments. Meaningful organization/session commands are separate rows.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role public identity exchange/asset; credential none until connection authentication; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.startOIDC; s.renderAuthPage, s.sealOIDCTransaction
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: connection authentication → connection

Sources: [GET /auth/oidc/start](../internal/web/server.go#L448), [internal/web/templates/auth.templ:39](../internal/web/templates/auth.templ#L39)
## web.static_assets

Static assets

- Audience: asset; status: **excluded**; owner: digitaldrywood/detent#3335.
- Decision: Static application assets carry no application operation; no MCP asset-serving tool.
- Tool: `boundary.no_tool` — not applicable → explicit source decision
- Authority: role public identity exchange/asset; credential none until connection authentication; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.assets.serve; handler-owned application validation/read/command
- Extraction: None for this protocol/authority boundary; no operator command extraction.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: this non-operator source site → not_applicable

Sources: [GET /static/*](../internal/web/server.go#L437)
## web.update_fleet_host

Update fleet host

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3343.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `runs_fleet.update_fleet_host` — Bounded updateFleetHostRequest: machine; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → updateFleetHostResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local administrator; credential admin; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.updateFleetHost; s.runnerFleet.UpdateHost
- Extraction: Extract web.updateFleetHost application inputs/results and validation from Echo; reuse s.runnerFleet.UpdateHost. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=false; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [POST /fleet/hosts/:machine](../internal/web/server.go#L507)
## web.update_fleet_runner

Update fleet runner

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3343.
- Decision: Expose the meaningful application operation through a typed tool using shared dashboard authority and commands.
- Tool: `runs_fleet.update_fleet_runner` — Bounded updateFleetRunnerRequest: runner; pagination/cursor where listing; existing validated fields only; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → updateFleetRunnerResult: bounded application data or command receipt with identifiers/URLs, outcome and freshness where relevant; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role local administrator; credential admin; project not project-scoped; organization/instance authority still enforced; ownership current organization/account; no cross-organization resource lookup.
- Application: s.updateFleetRunner; s.runnerFleet.UpdateRunner
- Extraction: Extract web.updateFleetRunner application inputs/results and validation from Echo; reuse s.runnerFleet.UpdateRunner. The HTTP handler and MCP must delegate to this same application operation.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=false; destructive=false; idempotent=false; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / dashboard daemon
- Availability: hosted_dedicated / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: hosted_shared / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Availability: credential_maintenance / github,native / dashboard daemon — unavailable: This route is not registered in this deployment; use the corresponding deployment application operation, where present.
- Confirmation: read or ordinary non-destructive write → none

Sources: [POST /fleet/runners/:runner](../internal/web/server.go#L506)
## workspace.file_read

Read a workspace file through the existing relay file channel

- Audience: operator; status: **pending**; owner: digitaldrywood/detent#3346.
- Decision: Pending parity: return meaningful structured navigation/application results for these explicit browser sites; never scrape HTML or proxy arbitrary HTTP.
- Tool: `conversations_workspaces.workspace_file_read` — project_id, workspace_id, confined relative path (1–4096 bytes), bounded offset/length; no shell command; bounds: identifiers 1–256 bytes, pagination 1–200, request at most 64 KiB, action fields use existing application validation → Typed file metadata/content chunk with workspace identity, byte bounds and freshness; at most 256 KiB, pagination 1–200, opaque service-unavailable errors
- Authority: role authenticated operator; credential current connection authority; project resource project read/write grant where scoped; ownership current organization; resolve identifiers within the authorized project.
- Application: workspace relay file channel read/list/stat and workspacesession file validation; FilesSurface uses relay.read
- Extraction: Extract bounded typed workspace file reads from the existing file-channel dispatch; preserve path confinement and runner-grant/ownership authorization. No arbitrary shell or raw relay forwarding.
- Preconditions: Current authenticated principal and organization; current role, scope, project grant and ownership at execution; Underlying deployment service must be installed/enabled; otherwise return an opaque safe unavailable result without credentials or sensitive payloads.; Mutations reuse the shared audit/retry contract (#3338); authentication and YOLO belong to connection authority (#3336/#3337), never arguments.
- Coverage: Source-derived inventory; execution authorization and parity regressions belong to the owner child
- Proposed hints: readOnly=true; destructive=false; idempotent=true; openWorld=true. Authorization/confirmation still apply.
- Availability: self_hosted / github,native / shared application read/command for this frontend source
- Availability: hosted_dedicated / github,native / shared application read/command for this frontend source
- Availability: hosted_shared / github,native / shared application read/command for this frontend source
- Confirmation: read or ordinary non-destructive write → none

Sources: [web/conversation/src/app/components/surfaces/FilesSurface.tsx:264](../web/conversation/src/app/components/surfaces/FilesSurface.tsx#L264), [web/conversation/src/app/components/surfaces/FilesSurface.tsx:57](../web/conversation/src/app/components/surfaces/FilesSurface.tsx#L57), [web/conversation/src/app/adapters/workspaceRelay.ts:396](../web/conversation/src/app/adapters/workspaceRelay.ts#L396)
