---
name: provider-impersonation-browser-binding
aliases:
  - provider-fixture-account-continuity
description: Integrate provider-initiated support impersonation and preserve browser fixture identities across application-session changes.
when_to_use: Use when an identity provider redirects support impersonation directly to a callback without the ordinary application's state or PKCE transaction, or invitation, membership, or grant browser fixtures return to the wrong actor after sign-in.
---

# Provider-initiated impersonation

1. Verify the current provider token, session and dashboard documentation. Do not
   assume the access token has ordinary OIDC audience claims or that actor fields
   match across token, exchange and session representations. Bind the signature
   to the configured application and cross-check documented actor representations.
2. Keep normal login state and PKCE mandatory. A valid provider impersonator is
   not proof that the receiving browser requested that customer identity.
3. Require an authenticated ordinary support account, explicit support allowlist
   and CSRF-protected support-start action. Persist a one-use transaction bound to
   that browser, current staff session, actual actor and selected tenant.
4. Accept a provider-initiated callback only while that transaction and staff
   session remain valid. Consume intent atomically before exchange. Match the
   verified actual actor, effective customer, selected tenant and provider session
   metadata; never convert a normal callback with missing state into this flow.
5. Authorize the effective customer through current tenant membership and explicit
   resource permissions. Keep staff reporting credentials and instance/runner
   authority separate. Recheck before cached responses, cursor reuse, mutations
   and stream emission; scope cache identities to the effective session.
6. Preserve absolute provider expiry, forbid silent actor loss during refresh or
   organization switching, display temporary actual/effective identity, and provide
   local-first logout with provider revocation. Trusted persisted session identity
   may be used for logout after local expiry; it must not reopen resource access.
7. Audit actor identities, tenant, session start/end or expiry, a content-free reason
   code and route-template action metadata. Exclude bodies, query strings, provider
   errors and bearer capabilities from operator reports and logs.
8. Test real browser-bound callback behavior and fixtures for missing/replayed
   state, wrong actor/tenant, expired/revoked staff or customer sessions, ordinary
   staff access, effective viewer restrictions and CSRF header confusion. Inspect
   native form state in a browser; a render-only test can miss Boolean attribute
   semantics that silently change a selected permission.

## Preserve provider fixture account continuity

Use this case when invitation, membership, or grant browser evidence unexpectedly returns to a privileged account after provider sign-in.

- Model the provider's selected account separately from the application's session.
  A revoked or rotated app cookie must not select a default privileged actor.
- Keep explicit account selection restricted to known synthetic identities in
  the test preview. Continue through the real application's state, code exchange
  and session handling.
- Add table-driven cases for privileged and restricted actors: select the actor,
  invalidate the app session, sign in again and assert the returned identity.
- Verify permissions after invitation acceptance and grant changes. A successful
  page load alone does not prove which actor completed the journey.
- Use a fixture mailbox for provider messages. Access a running service through
  its owned interfaces instead of opening its exclusively owned database.
- Capture both denied access before a grant and allowed access afterward, while
  checking that privileged controls remain absent for the restricted actor.
- Exclude captures made under the wrong identity and regenerate them after the
  fixture correction. Keep all identities and messages synthetic.
