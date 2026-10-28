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
    const onChange = () => setResolvedTheme(readResolvedTheme());
    // The theme can also be set on the root after mount (the design-system
    // gallery themes each frame that way), so follow `data-theme` too.
    const root = globalThis.document?.documentElement;
    const observer =
      root !== undefined && typeof MutationObserver === "function" ? new MutationObserver(onChange) : null;
    observer?.observe(root as HTMLElement, { attributes: true, attributeFilter: ["data-theme"] });
    onChange();
    const query = globalThis.matchMedia?.("(prefers-color-scheme: dark)");
    query?.addEventListener("change", onChange);
    return () => {
      observer?.disconnect();
      query?.removeEventListener("change", onChange);
    };
  }, []);
  return { resolvedTheme };
}
