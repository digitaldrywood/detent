/** Plain query strings: a key given once is a string, a key repeated is a list, and a list writes one parameter per value. */
export const plainSearchOptions = {
  parseSearch: (search: string): Record<string, string | string[]> => {
    const out: Record<string, string | string[]> = {};
    for (const [key, value] of new URLSearchParams(search)) {
      const current = out[key];
      out[key] = current === undefined ? value : [...(Array.isArray(current) ? current : [current]), value];
    }
    return out;
  },
  stringifySearch: (search: Record<string, unknown>): string => {
    const params = new URLSearchParams();
    for (const [key, value] of Object.entries(search)) {
      if (Array.isArray(value)) for (const item of value) params.append(key, String(item));
      else if (value !== undefined) params.set(key, String(value));
    }
    const query = params.toString();
    return query === "" ? "" : `?${query}`;
  },
};
