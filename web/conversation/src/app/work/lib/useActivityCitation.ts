import React from "react";

export function useActivityCitation(workItemId: string): string | null {
  const [highlightedId, setHighlightedId] = React.useState<string | null>(null);
  React.useEffect(() => {
    const highlight = () => {
      const fragment = window.location.hash.slice(1);
      if (/^issue-(history|comment|attempt)-[a-zA-Z0-9_-]+$/.test(fragment))
        setHighlightedId(fragment);
    };
    const click = (event: MouseEvent) => {
      const target = event.target;
      if (!(target instanceof Element)) return;
      const anchor = target.closest("a[href]");
      if (!(anchor instanceof HTMLAnchorElement)) return;
      const url = new URL(anchor.href, window.location.href);
      if (
        url.origin !== window.location.origin ||
        url.pathname !== window.location.pathname ||
        !/^#issue-(history|comment|attempt)-[a-zA-Z0-9_-]+$/.test(url.hash)
      )
        return;
      event.preventDefault();
      event.stopPropagation();
      window.history.replaceState(null, "", url.hash);
      highlight();
      document
        .getElementById(url.hash.slice(1))
        ?.scrollIntoView?.({ behavior: "smooth", block: "center" });
    };
    setHighlightedId(null);
    highlight();
    document.addEventListener("click", click, true);
    window.addEventListener("hashchange", highlight);
    return () => {
      document.removeEventListener("click", click, true);
      window.removeEventListener("hashchange", highlight);
    };
  }, [workItemId]);
  return highlightedId;
}
