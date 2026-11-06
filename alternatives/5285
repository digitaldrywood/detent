export type UserSort = "email" | "role" | "organizations" | "last_sign_in";
export type UserSegment = "all" | "staff" | "no_organization" | "invited";
export type UserSearch = {
  q?: string;
  segment?: UserSegment;
  organization?: string;
  role?: string;
  sort?: UserSort;
  direction?: "asc" | "desc";
  page?: number;
};

export function validateUserSearch(search: Record<string, unknown>): UserSearch {
  const page = Number(search.page);
  return {
    q: typeof search.q === "string" ? search.q : undefined,
    segment: search.segment === "staff" || search.segment === "no_organization" || search.segment === "invited" ? search.segment : undefined,
    organization: typeof search.organization === "string" ? search.organization : undefined,
    role: typeof search.role === "string" ? search.role : undefined,
    sort: search.sort === "email" || search.sort === "role" || search.sort === "organizations" || search.sort === "last_sign_in" ? search.sort : undefined,
    direction: search.direction === "asc" || search.direction === "desc" ? search.direction : undefined,
    page: Number.isSafeInteger(page) && page > 1 ? page : undefined,
  };
}
