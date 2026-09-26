// `/login`.
//
// The artifact's wizard card (screen 5) with one job instead of four: a 640px
// panel on a dimmed shell, the Detent Cloud logo, the two ways in, and the
// invitation token for somebody who was sent one. There is no sidebar — the
// reader is not signed in, so there is nothing to navigate.
//
// The hub serves the application shell for `/login` without a session
// (decisions.md §12, "Serving"), which is why this route never reads the
// bootstrap payload: it has to render for a reader the hub could not identify.
import React from "react";

import { Button } from "../../components/ui/button.tsx";
import { Input } from "../../components/ui/input.tsx";
import { Label } from "../../components/ui/label.tsx";
import { cn } from "../../lib/utils.ts";
import { useAccountApi, useAccountBootstrap } from "./context.ts";
import { newKey } from "./idempotency.ts";
import { useMutation } from "./useResource.ts";
import { hubPath } from "../../runtime/basePath.ts";

/** Where the browser goes to start a sign-in. */
export const OIDC_START = "/auth/oidc/start";
/**
 * The same start, unscoped: the reader is joining an organization they were
 * invited to rather than signing in to one they already belong to, so the hub
 * must not scope the authorization request to the organization this host
 * serves.
 */
export const OIDC_START_UNSCOPED = "/auth/oidc/start?unscoped=1";

/**
 * The friendly half of `?error=`. The hub puts a code in the query string
 * after a failed callback; anything it has not named is still worth saying
 * plainly rather than echoing a raw code at the reader.
 */
export function loginErrorMessage(code: string | null): string | null {
  if (code === null || code.trim().length === 0) return null;
  switch (code) {
    case "access_denied":
      return "That sign-in was cancelled before it finished.";
    case "invalid_state":
    case "state_expired":
      return "That sign-in link had expired. Start again from this page.";
    case "no_membership":
      return "That account is not a member of this organization. Ask an owner for an invitation.";
    case "invitation_expired":
      return "That invitation has expired. Ask whoever invited you for a new one.";
    case "session_expired":
      return "Your session expired. Sign in again.";
    default:
      return "That sign-in did not complete. Try again.";
  }
}

export function DetentCloudLogo(): React.ReactElement {
  return (
    <div className="flex items-center gap-2 text-[22px] font-bold tracking-[-0.02em]">
      <span
        aria-hidden="true"
        className="grid size-7 place-items-center rounded-lg bg-primary text-[15px] font-semibold text-primary-foreground"
      >
        D
      </span>
      <span>
        Detent <span className="font-light text-muted-foreground">Cloud</span>
      </span>
    </div>
  );
}

/**
 * The card itself, with no data access of its own so it can be rendered by the
 * route, by the no-session entry path, and by a test.
 */
export function LoginCard({
  error,
  onAcceptInvitation,
  accepting = false,
  acceptError = null,
  className,
}: {
  readonly error?: string | null;
  /** Absent where no session exists to accept an invitation with. */
  readonly onAcceptInvitation?: ((token: string) => void) | null;
  readonly accepting?: boolean;
  readonly acceptError?: string | null;
  readonly className?: string;
}): React.ReactElement {
  const [token, setToken] = React.useState("");
  const message = typeof error === "string" ? loginErrorMessage(error) : null;

  return (
    <div className="grid flex-1 place-items-center p-10">
      <div
        className={cn(
          "w-full max-w-[640px] overflow-hidden rounded-2xl border border-border bg-card shadow-[0_30px_80px_rgb(0_0_0/45%)]",
          className,
        )}
      >
        <div className="border-b border-border/60 px-7 pt-[26px] pb-[18px]">
          <DetentCloudLogo />
        </div>
        <div className="px-7 pt-6 pb-[26px]">
          <h1 className="mb-2 text-2xl font-semibold tracking-[-0.02em] text-balance">
            Sign in to Detent
          </h1>
          <p className="mb-[18px] text-sm text-muted-foreground">
            Detent runs your work on your own machines, with your own provider login. Sign in to
            reach your organization's board, runners and spend.
          </p>

          {message === null ? null : (
            <div
              role="alert"
              className="mb-4 rounded-[10px] border border-destructive/30 bg-destructive/8 px-3.5 py-3 text-[13px] text-destructive-foreground"
            >
              {message}
            </div>
          )}

          <div className="flex flex-col gap-2.5">
            <Button
              size="lg"
              className="w-full justify-center"
              render={<a href={hubPath(OIDC_START)}>Continue with WorkOS</a>}
            />
            <Button
              size="lg"
              variant="outline"
              className="w-full justify-center"
              render={<a href={hubPath(OIDC_START_UNSCOPED)}>Join with invitation</a>}
            />
          </div>

          <form
            className="mt-6 border-t border-border/60 pt-5"
            onSubmit={(event) => {
              event.preventDefault();
              const value = token.trim();
              if (value.length === 0) return;
              if (onAcceptInvitation != null) onAcceptInvitation(value);
              else globalThis.location?.assign(hubPath(`/invite?token=${encodeURIComponent(value)}`));
            }}
          >
            <Label htmlFor="login-invitation-token" className="text-sm font-medium">
              Have an invitation token?
            </Label>
            <p className="mt-1 mb-2.5 text-[13px] text-muted-foreground">
              Paste the token from your invitation email to join the organization it names.
            </p>
            <div className="flex flex-col gap-2 sm:flex-row">
              <Input
                id="login-invitation-token"
                name="token"
                autoComplete="one-time-code"
                placeholder="inv_…"
                value={token}
                onChange={(event) => setToken(event.currentTarget.value)}
                className="flex-1"
              />
              <Button type="submit" variant="secondary" disabled={accepting}>
                {accepting ? "Joining…" : "Join"}
              </Button>
            </div>
            {acceptError === null ? null : (
              <p role="alert" className="mt-2 text-[13px] text-destructive-foreground">
                {acceptError}
              </p>
            )}
          </form>
        </div>
      </div>
    </div>
  );
}

/** Reads `?error=` from the browser's own location. */
export function locationError(search?: string): string | null {
  const raw = search ?? globalThis.location?.search ?? "";
  if (raw.length === 0) return null;
  return new URLSearchParams(raw).get("error");
}

/**
 * The route. Where a session already exists the invitation token is accepted
 * through the API and the browser follows the `next` the hub names — it is
 * always a fresh sign-in against whichever organization now owns the session,
 * so the client never guesses the destination (§12, "Organization").
 */
export function LoginRoute(): React.ReactElement {
  const api = useAccountApi();
  const bootstrap = useAccountBootstrap();
  const accept = useMutation(async (token: string) => {
    const result = await api.acceptInvitation({ token, key: newKey() });
    globalThis.location?.assign(result.next);
    return result;
  });

  return (
    <LoginCard
      error={locationError()}
      onAcceptInvitation={bootstrap === null ? null : (token) => void accept.call(token)}
      accepting={accept.pending}
      acceptError={accept.error?.message ?? null}
    />
  );
}
