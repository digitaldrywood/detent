import React from "react";

import { AccountError } from "../account/api.ts";
import { useResource, type Resource } from "../account/useResource.ts";
import { SIGN_IN_PLATFORM } from "./api.ts";

export function redirectPlatformSignIn(error: unknown): void {
  if (error instanceof AccountError && error.status === 401) {
    globalThis.location?.assign(SIGN_IN_PLATFORM);
  }
}

export function usePlatformResource<A>(read: () => Promise<A>, dependencies: readonly unknown[]): Resource<A> {
  const resource = useResource(read, dependencies);
  React.useEffect(() => {
    redirectPlatformSignIn(resource.error);
  }, [resource.error]);
  return resource;
}
