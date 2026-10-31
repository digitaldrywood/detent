import React from "react";

import { Button } from "../../components/ui/button.tsx";
import { cn } from "../../lib/utils.ts";
import { hubPath } from "../../runtime/basePath.ts";

/** Where the browser goes to start a sign-in. */
export const OIDC_START = "/auth/oidc/start";
/** Starts the existing OIDC flow on the account creation screen. */
export const OIDC_SIGN_UP = "/auth/oidc/start?screen_hint=sign-up";

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

/** Public navigation mirrors detent.build and always leaves the Hub for the site. */
function SiteHeader(): React.ReactElement {
  const links = [
    ["How it works", "/how-it-works"],
    ["Why Detent", "/why-detent"],
    ["Dashboard", "/dashboard"],
    ["Install", "/install"],
    ["Docs", "/docs"],
    ["Videos", "/videos"],
    ["Open source", "/open-source"],
  ] as const;
  return (
    <header className="shrink-0 border-b border-border bg-background/85 backdrop-blur">
      <nav aria-label="Primary" className="mx-auto flex min-h-14 max-w-[1280px] flex-wrap items-center gap-x-6 gap-y-2 px-6 py-3 md:px-8 lg:h-14 lg:flex-nowrap lg:py-0">
        <a href="https://detent.build/" aria-label="detent.build home" className="flex shrink-0 items-center gap-2.5 rounded-sm">
          <svg viewBox="0 0 100 100" role="img" aria-label="Detent" className="size-6 text-[#4338ca] dark:text-[#818cf8]">
            <path fill="currentColor" fillRule="evenodd" d="M22 15 H50 C73 15 87 31 87 50 C87 69 73 85 50 85 H22 Z M41 32 H50 C63 32 70 41 70 50 C70 59 63 68 50 68 H41 Z" />
            <circle cx="51.5" cy="55.5" r="7.4" fill="currentColor" />
          </svg>
          <span className="font-mono text-[15px] font-semibold tracking-tight">detent<span className="text-primary">.</span>build</span>
        </a>
        <div className="order-last flex w-full gap-7 overflow-x-auto text-sm text-muted-foreground lg:order-none lg:ml-2 lg:w-auto">
          {links.map(([label, path]) => (
            <a key={path} href={`https://detent.build${path}`} className="inline-flex min-h-6 shrink-0 items-center whitespace-nowrap py-1 transition-colors hover:text-primary">{label}</a>
          ))}
        </div>
        <a href="https://github.com/digitaldrywood/detent" className="ml-auto shrink-0 rounded-md border border-border px-3 py-1.5 font-mono text-xs transition-colors hover:border-primary hover:text-primary">GitHub ↗</a>
      </nav>
    </header>
  );
}

/** Shared by the organization login route and the unauthenticated entry. */
export function LoginCard({
  error,
  className,
}: {
  readonly error?: string | null;
  readonly className?: string;
}): React.ReactElement {
  const message = typeof error === "string" ? loginErrorMessage(error) : null;
  return (
    <div className="detent-sign-in flex min-h-full flex-1 flex-col bg-background text-foreground">
      <SiteHeader />
      <main className="grid flex-1 place-items-center px-4 py-12 sm:px-6">
        <section aria-labelledby="login-title" className={cn("w-full max-w-[400px] rounded-xl border border-border bg-card p-7 shadow-lg", className)}>
          <h1 id="login-title" className="mb-2 text-2xl font-semibold tracking-tight">Sign in to Detent</h1>
          <p className="mb-6 text-sm text-muted-foreground">Open your workspace or create an account to get started.</p>
          {message === null ? null : (
            <div role="alert" className="mb-4 rounded-lg border border-destructive/30 bg-destructive/8 px-3.5 py-3 text-sm text-destructive-foreground">{message}</div>
          )}
          <div className="flex flex-col gap-2.5">
            <Button size="lg" className="w-full justify-center" render={<a href={hubPath(OIDC_START)}>Sign in</a>} />
            <Button size="lg" variant="outline" className="w-full justify-center" render={<a href={hubPath(OIDC_SIGN_UP)}>Create account</a>} />
          </div>
        </section>
      </main>
    </div>
  );
}

/** Reads `?error=` from the browser's own location. */
export function locationError(search?: string): string | null {
  const raw = search ?? globalThis.location?.search ?? "";
  if (raw.length === 0) return null;
  return new URLSearchParams(raw).get("error");
}

/** The login route needs no bootstrap or session to render. */
export function LoginRoute(): React.ReactElement {
  return <LoginCard error={locationError()} />;
}
