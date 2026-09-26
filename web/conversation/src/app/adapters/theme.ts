import React from "react";

export type ResolvedTheme = "light" | "dark";

function readResolvedTheme(): ResolvedTheme {
  const root = globalThis.document?.documentElement;
  const attribute = root?.dataset.theme;
  if (attribute === "dark" || attribute === "light") return attribute;
  return globalThis.matchMedia?.("(prefers-color-scheme: dark)").matches ? "dark" : "light";
}

export function useTheme(): { resolvedTheme: ResolvedTheme } {
  const [resolvedTheme, setResolvedTheme] = React.useState<ResolvedTheme>(readResolvedTheme);
  React.useEffect(() => {
    const query = globalThis.matchMedia?.("(prefers-color-scheme: dark)");
    if (query === undefined) return;
    const onChange = () => setResolvedTheme(readResolvedTheme());
    query.addEventListener("change", onChange);
    return () => query.removeEventListener("change", onChange);
  }, []);
  return { resolvedTheme };
}
