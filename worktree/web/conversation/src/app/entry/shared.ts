// The organization client's view of the shared entry. Behind the shared
// origin every organization lives under `/organizations/ORG`, and only the
// entry knows the account's other organizations, so the switcher reads the
// entry's list and switching is a navigation, not an API call.
import { basePath } from "../../runtime/basePath.ts";
import { useResource } from "../account/useResource.ts";
import { makeEntryApi } from "./api.ts";

export interface SharedOrganization {
  readonly logo_url?: string | undefined;
  readonly id: string;
  readonly name: string;
  readonly url: string;
  readonly current: boolean;
  readonly role?: string;
}

/** True when this client runs inside an organization behind the shared entry. */
export function behindSharedEntry(prefix: string = basePath()): boolean {
  return prefix.startsWith("/organizations/");
}

export function currentSharedOrganization(prefix: string = basePath()): string {
  return behindSharedEntry(prefix) ? decodeURIComponent(prefix.slice("/organizations/".length)) : "";
}

export function useSharedOrganizations() {
  return useResource<readonly SharedOrganization[] | null>(async () => {
    if (!behindSharedEntry()) return null;
    const current = currentSharedOrganization();
    const listing = await makeEntryApi().organizations();
    return listing.organizations.map((organization) => ({
      ...organization,
      current: organization.id === current,
    }));
  }, []);
}

export { CHOOSE_ORGANIZATION as ENTRY_ORGANIZATIONS } from "./api.ts";
export const ENTRY_CREATE_ORGANIZATION = "/organizations/new";
