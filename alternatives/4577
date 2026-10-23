// The gallery as an application of its own.
//
// Two hosts mount it:
//
// - the dev server, from `src/app/main.tsx`, for any `/design-system` path.
//   The gallery needs no hub, so it skips the bootstrap request and opens
//   with `npm run dev` alone. Specimens that embed a real app route still
//   need the mock hub (`npm run dev:mock`) and say so when it is not running.
// - the static build (`npm run build:gallery`, `gallery.html`), which a
//   designer opens from any static file server. It routes on the URL hash so
//   every entry is a deep link without a server-side fallback.
//
// Neither host reaches the production bundle: the dev import is guarded by
// `import.meta.env.DEV`, and the static build has its own entry.
import { RegistryProvider } from "@effect/atom-react";
import {
  createHashHistory,
  createRootRoute,
  createRoute,
  createRouter,
  Outlet,
  redirect,
  RouterProvider,
  type AnyRoute,
} from "@tanstack/react-router";
import React from "react";

import { routerBasePath } from "../../runtime/basePath.ts";
import { FrameRoute, GalleryRoute } from "./Gallery";
import { isStaticGallery } from "./frame";

function galleryRouter() {
  const root = createRootRoute({ component: Outlet });
  const routes = [
    createRoute({
      getParentRoute: () => root,
      path: "/",
      beforeLoad: () => {
        throw redirect({ to: "/design-system" as never });
      },
    }),
    createRoute({ getParentRoute: () => root, path: "/design-system", component: GalleryRoute }),
    createRoute({ getParentRoute: () => root, path: "/design-system/$entryId", component: GalleryRoute }),
    createRoute({ getParentRoute: () => root, path: "/design-system/frame/$entryId/$specimenId", component: FrameRoute }),
  ] as unknown as AnyRoute[];
  const routeTree = root.addChildren(routes);
  return isStaticGallery()
    ? createRouter({ routeTree, history: createHashHistory() })
    : createRouter({ routeTree, basepath: routerBasePath() });
}

/**
 * A frame document takes its theme from `?theme=` before React renders, so
 * code highlighting and anything else that reads the theme on mount sees the
 * frame's theme rather than the operating system's.
 */
export function applyFrameThemeFromLocation(location: Location = globalThis.location): void {
  const hash = location.hash.startsWith("#/") ? location.hash.slice(1) : "";
  const [path = "", query = ""] = (hash !== "" ? hash : `${location.pathname}${location.search}`).split("?");
  if (!path.includes("/design-system/frame/")) return;
  const theme = new URLSearchParams(query).get("theme") === "light" ? "light" : "dark";
  const root = globalThis.document.documentElement;
  root.classList.toggle("dark", theme === "dark");
  root.dataset.theme = theme;
}

/** The gallery's element tree, for a host's `mount`. */
export function GalleryApp(): React.ReactElement {
  const [router] = React.useState(galleryRouter);
  return (
    <React.StrictMode>
      <RegistryProvider>
        <RouterProvider router={router as never} />
      </RegistryProvider>
    </React.StrictMode>
  );
}
