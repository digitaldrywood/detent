// Entry point. The bootstrap payload is fetched first: it carries the CSRF
// token and the API base every other request needs.
import { RegistryProvider } from "@effect/atom-react";
import React from "react";
import { createRoot } from "react-dom/client";
import { RouterProvider } from "@tanstack/react-router";

import { BootstrapRefused, loadBootstrap, makeClient } from "../runtime/bootstrap.ts";
import { SupportCard, supportOrganization } from "./account/Support.tsx";
import { ClientContext } from "./client.ts";
import { makeRouter } from "./router.tsx";
import { isLoginPath, LoginCard } from "./routes.account.tsx";
import "./index.css";
import { hubPath, withoutBasePath } from "../runtime/basePath.ts";
import { isEntrySurface, makeEntryRouter } from "./entry/router.tsx";

const container = document.getElementById("root");
if (container === null) throw new Error("The conversation client has no mount point.");
container.setAttribute("data-detent-conversation-root", "");

const root = createRoot(container);

function Failure({ message }: { message: string }): React.ReactElement {
  return (
    <div
      className="flex h-full flex-col items-center justify-center gap-3 p-8 text-center text-muted-foreground text-sm"
      role="alert"
    >
      <p>{message}</p>
      <a className="text-primary underline-offset-4 hover:underline" href={hubPath("/organization")}>
        Back to Detent
      </a>
    </div>
  );
}

if (isEntrySurface()) {
  root.render(
    <React.StrictMode>
      <RouterProvider router={makeEntryRouter() as never} />
    </React.StrictMode>,
  );
} else {
  startOrganizationClient();
}

function startOrganizationClient(): void {
  void loadBootstrap()
  .then((bootstrap) => {
    const client = makeClient({ bootstrap });
    const router = makeRouter();
    root.render(
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
    // `/login` and `/support` are the two paths the hub serves without
    // organization access (decisions.md §12, "Serving"), so a refused
    // bootstrap on either is the expected state rather than a failure. Every
    // other path needs one and says so.
    if (isLoginPath(pathname)) {
      root.render(
        <React.StrictMode>
          <div className="flex h-full flex-col">
            <LoginCard error={new URLSearchParams(search).get("error")} />
          </div>
        </React.StrictMode>,
      );
      return;
    }
    if (isSupportPath(pathname)) {
      root.render(
        <React.StrictMode>
          <div className="flex h-full flex-col">
            <SupportCard
              organization={supportOrganization({
                pathname,
                search,
                body: cause instanceof BootstrapRefused ? cause.body : undefined,
              })}
            />
          </div>
        </React.StrictMode>,
      );
      return;
    }
    root.render(
      <Failure message={cause instanceof Error ? cause.message : "The chat could not start."} />,
    );
  });
}

/** True for the support screen, with or without an organization segment. */
function isSupportPath(pathname: string): boolean {
  return pathname === "/support" || pathname.startsWith("/support/");
}
