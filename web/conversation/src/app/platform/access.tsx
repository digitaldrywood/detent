import React from "react";

import type { AccountError } from "../account/api.ts";
import type { PlatformOrganizations } from "../entry/api.ts";

/**
 * What the signed-in staff member may do. It comes from the live
 * organizations listing, which the entry already answers with 403 to anyone
 * who is not staff, so no page decides access on its own.
 */
export interface PlatformAccess {
  readonly email: string;
  readonly csrf: string;
  readonly canSupport: boolean;
  readonly canGrant: boolean;
}

export type AccessState =
  | { readonly kind: "loading" }
  | { readonly kind: "signed-out" }
  | { readonly kind: "forbidden" }
  | { readonly kind: "failed"; readonly message: string }
  | { readonly kind: "ready"; readonly access: PlatformAccess };

export function accessState(
  value: PlatformOrganizations | undefined,
  error: AccountError | null,
): AccessState {
  if (value !== undefined) {
    return {
      kind: "ready",
      access: {
        email: value.email,
        csrf: value.csrf,
        canSupport: value.can_support,
        canGrant: value.can_grant === true,
      },
    };
  }
  if (error === null) return { kind: "loading" };
  if (error.status === 401) return { kind: "signed-out" };
  if (error.status === 403) return { kind: "forbidden" };
  return { kind: "failed", message: error.message };
}

const PlatformAccessContext = React.createContext<PlatformAccess | null>(null);

export const PlatformAccessProvider = PlatformAccessContext.Provider;

export function usePlatformAccess(): PlatformAccess {
  const access = React.useContext(PlatformAccessContext);
  if (access === null) throw new Error("usePlatformAccess is used outside the platform console.");
  return access;
}
