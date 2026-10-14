// @vitest-environment jsdom
//
// The client is served under `style-src 'self'`, so any inline `<style>`
// element it renders is blocked and logged as a CSP violation.
import { cleanup, render } from "@testing-library/react";
import type React from "react";
import { afterEach, describe, expect, it } from "vitest";

import { ContentSecurity } from "../../src/app/ContentSecurity.tsx";
import { ScrollArea } from "../../src/components/ui/scroll-area.tsx";

afterEach(() => {
  cleanup();
  for (const element of document.querySelectorAll("style")) element.remove();
});

function inlineStyles(): string[] {
  return Array.from(document.querySelectorAll("style"), (element) => element.outerHTML);
}

describe("ContentSecurity", () => {
  const cases: { name: string; wrap: (node: React.ReactNode) => React.ReactElement; want: number }[] = [
    { name: "Base UI renders an inline style element by default", wrap: (node) => <>{node}</>, want: 1 },
    { name: "the provider suppresses Base UI inline style elements", wrap: (node) => <ContentSecurity>{node}</ContentSecurity>, want: 0 },
  ];

  for (const tc of cases) {
    it(tc.name, () => {
      render(tc.wrap(<ScrollArea>content</ScrollArea>));
      expect(inlineStyles()).toHaveLength(tc.want);
    });
  }
});
