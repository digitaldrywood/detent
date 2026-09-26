import * as Cause from "effect/Cause";
import { AsyncResult, Atom } from "effect/unstable/reactivity";

import type { AssetCreateUrlResult, AssetImageDimensions, AssetResource } from "../contracts/ui.ts";

const NONE = Atom.make<string | null>(null).pipe(Atom.withLabel("detent-project-favicon:none"));

export function projectFaviconUrlAtom(_input: {
  environmentId: string;
  cwd: string;
  faviconPath?: string | null | undefined;
}): Atom.Atom<string | null> {
  return NONE;
}

/** Upstream's, verbatim. */
export function resolveAssetUrl(httpBaseUrl: string, relativeUrl: string): string | null {
  try {
    return new URL(relativeUrl, httpBaseUrl).toString();
  } catch {
    return null;
  }
}

/** Upstream's, verbatim. */
export const EMPTY_ASSET_URL_ATOM = Atom.make(AsyncResult.initial<never, never>(false)).pipe(
  Atom.withLabel("asset-url:empty"),
);

/** Upstream's, verbatim. */
export type AssetUrlState =
  | { readonly _tag: "Loading" }
  | { readonly _tag: "Failure" }
  | {
      readonly _tag: "Success";
      readonly url: string;
      /** The host path the server chose to serve, when it differs from what was asked for. */
      readonly sourcePath?: string;
      /** Pixel size from the image header, when the server could read one. */
      readonly imageDimensions?: AssetImageDimensions;
    };

/** Upstream's, verbatim. */
export function assetUrlStateFromResult(
  result: AsyncResult.AsyncResult<AssetCreateUrlResult, unknown>,
  httpBaseUrl: string | null,
): AssetUrlState {
  if (result._tag === "Failure") return { _tag: "Failure" };
  if (httpBaseUrl === null || result._tag !== "Success") return { _tag: "Loading" };
  const url = resolveAssetUrl(httpBaseUrl, result.value.relativeUrl);
  if (url === null) return { _tag: "Failure" };
  return {
    _tag: "Success",
    url,
    ...(result.value.sourcePath !== undefined ? { sourcePath: result.value.sourcePath } : {}),
    ...(result.value.imageDimensions !== undefined
      ? { imageDimensions: result.value.imageDimensions }
      : {}),
  };
}

/**
 * The message the reader sees when a transcript names a file this client
 * cannot fetch. Phrased as the limit it is, not as a fault.
 */
export const ASSET_UNAVAILABLE_MESSAGE =
  "This file lives in the runner's checkout and cannot be opened from the browser.";

const UNAVAILABLE_ASSET_URL_ATOM = Atom.make(
  AsyncResult.failure<AssetCreateUrlResult, Error>(Cause.fail(new Error(ASSET_UNAVAILABLE_MESSAGE))),
).pipe(Atom.withLabel("detent-asset-url:unavailable"));

// One atom per resource count, not per resource list: the copied
// `useAssetUrls` maps the results positionally, so the array has to be exactly
// as long as the request, and building a fresh atom on every render would
// churn the registry for a value that never changes.
const assetUrlListAtomsByLength = new Map<
  number,
  Atom.Atom<ReadonlyArray<AsyncResult.AsyncResult<AssetCreateUrlResult, Error>>>
>();

function assetUrlListAtom(
  length: number,
): Atom.Atom<ReadonlyArray<AsyncResult.AsyncResult<AssetCreateUrlResult, Error>>> {
  const cached = assetUrlListAtomsByLength.get(length);
  if (cached !== undefined) return cached;
  const created = Atom.make<ReadonlyArray<AsyncResult.AsyncResult<AssetCreateUrlResult, Error>>>(
    Array.from({ length }, () =>
      AsyncResult.failure<AssetCreateUrlResult, Error>(
        Cause.fail(new Error(ASSET_UNAVAILABLE_MESSAGE)),
      ),
    ),
  ).pipe(Atom.withLabel(`detent-asset-urls:unavailable:${length}`));
  assetUrlListAtomsByLength.set(length, created);
  return created;
}

export const assetEnvironment = {
  createUrl: (_target: {
    readonly environmentId: string;
    readonly input: { readonly resource: AssetResource };
  }): Atom.Atom<AsyncResult.AsyncResult<AssetCreateUrlResult, Error>> => UNAVAILABLE_ASSET_URL_ATOM,
  /**
   * The batched twin, which a mounted message row uses to ask for all of its
   * stored images at once. Detent signs none, so the list is empty and the
   * copied `assets/assetUrls.ts` maps every resource to `null` — the same
   * result it reaches when a connection is not prepared.
   */
  createUrls: (target: {
    readonly environmentId: string;
    readonly resources: ReadonlyArray<AssetResource>;
  }): Atom.Atom<ReadonlyArray<AsyncResult.AsyncResult<AssetCreateUrlResult, Error>>> =>
    assetUrlListAtom(target.resources.length),
};
