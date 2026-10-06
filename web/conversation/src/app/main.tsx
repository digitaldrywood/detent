// Entry point. The bootstrap payload is fetched first: it carries the CSRF
// token and the API base every other request needs.
import { RegistryProvider } from "@effect/atom-react";
import React from "react";
import { createRoot } from "react-dom/client";
import { RouterProvider } from "@tanstack/react-router";

import {
  BootstrapRefused,
  loadBootstrap,
  makeClient,
} from "../runtime/bootstrap.ts";
import {
  SupportCard,
  supportCSRF,
  supportOrganization,
} from "./account/Support.tsx";
import { ClientContext } from "./client.ts";
import { ContentSecurity } from "./ContentSecurity.tsx";
import { isDesignSystemPath, makeRouter } from "./router.tsx";
import { isLoginPath, LoginCard } from "./routes.account.tsx";
import "./index.css";
import { hubPath, withoutBasePath } from "../runtime/basePath.ts";
import { isEntrySurface, makeEntryRouter } from "./entry/router.tsx";
import { formatPageTitle } from "./pageTitle.ts";

const container = document.getElementById("root");
if (container === null)
  throw new Error("The conversation client has no mount point.");
container.setAttribute("data-detent-conversation-root", "");

// Development only: `?theme=light|dark` sets the theme before the first
// render, so the design-system gallery can show a real route in either theme.
if (import.meta.env.DEV) {
  const theme = new URLSearchParams(globalThis.location?.search ?? "").get("theme");
  if (theme === "light" || theme === "dark") {
    document.documentElement.classList.toggle("dark", theme === "dark");
    document.documentElement.dataset.theme = theme;
  }
}

const root = createRoot(container);

function mount(node: React.ReactNode): void {
  root.render(<ContentSecurity>{node}</ContentSecurity>);
}

function Failure({ message }: { message: string }): React.ReactElement {
  return (
    <div
      className="flex h-full flex-col items-center justify-center gap-3 p-8 text-center text-muted-foreground text-sm"
      role="alert"
    >
      <p>{message}</p>
      <a
        className="text-primary underline-offset-4 hover:underline"
        href={hubPath("/organization")}
      >
        Back to Detent
      </a>
    </div>
  );
}

if (
  import.meta.env.DEV &&
  isDesignSystemPath(withoutBasePath(globalThis.location?.pathname ?? ""))
) {
  // The design-system gallery needs no hub: it skips the bootstrap so that
  // `npm run dev` alone serves it. The guard is a literal so the production
  // build drops this branch and the gallery chunk with it.
  void import("../design-system/gallery/standalone.tsx").then(({ applyFrameThemeFromLocation, GalleryApp }) => {
    applyFrameThemeFromLocation();
    mount(<GalleryApp />);
  });
} else if (isEntrySurface()) {
  mount(
    <React.StrictMode>
      <RouterProvider router={makeEntryRouter() as never} />
    </React.StrictMode>,
  );
} else {
  startOrganizationClient();
}

function startOrganizationClient(): void {
  const pathname = withoutBasePath(globalThis.location?.pathname ?? "");
  const search = globalThis.location?.search ?? "";
  if (isLoginPath(pathname)) {
    document.title = formatPageTitle("Sign in");
    mount(
      <React.StrictMode>
        <div className="flex h-full flex-col">
          <LoginCard error={new URLSearchParams(search).get("error")} />
        </div>
      </React.StrictMode>,
    );
    return;
  }

  void loadBootstrap()
    .then((bootstrap) => {
      const client = makeClient({ bootstrap });
      const router = makeRouter();
      mount(
        <React.StrictMode>
          <RegistryProvider>
            <ClientContext.Provider value={client}>
              <RouterProvider router={router} />
            </ClientContext.Provider>
          </RegistryProvider>
        </React.StrictMode>,
      );
    })
    .catch((cause: unknown) => {
      const pathname = withoutBasePath(globalThis.location?.pathname ?? "");
      const search = globalThis.location?.search ?? "";
      // Support can be opened without organization access. A refused
      // bootstrap there is expected; other routes need one and say so.
      if (isSupportPath(pathname)) {
        document.title = formatPageTitle("Support");
        mount(
          <React.StrictMode>
            <div className="flex h-full flex-col">
              <SupportCard
                organization={supportOrganization({
                  pathname,
                  search,
                  body:
                    cause instanceof BootstrapRefused ? cause.body : undefined,
                })}
                csrfToken={
                  cause instanceof BootstrapRefused
                    ? supportCSRF(cause.body)
                    : null
                }
              />
            </div>
          </React.StrictMode>,
        );
        return;
      }
      mount(
        <Failure
          message={
            cause instanceof Error ? cause.message : "The chat could not start."
          }
        />,
      );
    });
}

/** True for the support screen, with or without an organization segment. */
function isSupportPath(pathname: string): boolean {
  return pathname === "/support" || pathname.startsWith("/support/");
}
