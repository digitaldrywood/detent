// The organization client's view of the shared entry. Behind the shared
// origin every organization lives under `/organizations/ORG`, and only the
// entry knows the account's other organizations, so the switcher reads the
// entry's list and switching is a navigation, not an API call.
import React from "react";

import { basePath } from "../../runtime/basePath.ts";
import { makeEntryApi } from "./api.ts";

export interface SharedOrganization {
  readonly id: string;
  readonly name: string;
  readonly url: string;
  readonly current: boolean;
}

/** True when this client runs inside an organization behind the shared entry. */
export function behindSharedEntry(prefix: string = basePath()): boolean {
  return prefix.startsWith("/organizations/");
}

export function currentSharedOrganization(prefix: string = basePath()): string {
  return behindSharedEntry(prefix) ? decodeURIComponent(prefix.slice("/organizations/".length)) : "";
}

/** The account's organizations from the entry, or null outside the shared entry. */
export function useSharedOrganizations(): readonly SharedOrganization[] | null {
  const [value, setValue] = React.useState<readonly SharedOrganization[] | null>(null);
  React.useEffect(() => {
    if (!behindSharedEntry()) return;
    let live = true;
    const current = currentSharedOrganization();
    makeEntryApi()
      .organizations()
      .then((listing) => {
        if (!live) return;
        setValue(
          listing.organizations.map((organization) => ({
            ...organization,
            current: organization.id === current,
          })),
        );
      })
      .catch(() => {
        if (live) setValue([]);
      });
    return () => {
      live = false;
    };
  }, []);
  return value;
}

export const ENTRY_ORGANIZATIONS = "/organizations";
export const ENTRY_CREATE_ORGANIZATION = "/organizations/new";
