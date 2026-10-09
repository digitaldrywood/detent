export const plainSearchOptions = {
  parseSearch: (search: string): Record<string, string> =>
    Object.fromEntries(new URLSearchParams(search)),
  stringifySearch: (search: Record<string, unknown>): string => {
    const params = new URLSearchParams();
    for (const [key, value] of Object.entries(search)) {
      if (value !== undefined) params.set(key, String(value));
    }
    const query = params.toString();
    return query === "" ? "" : `?${query}`;
  },
};
