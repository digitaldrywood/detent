// The entry and every organization serve this client under
// `style-src 'self'`, which blocks the inline `<style>` elements Base UI
// renders by default. Base UI skips them under this provider; the rules they
// carried live in index.css instead.
import { CSPProvider } from "@base-ui/react/csp-provider";
import type React from "react";

export function ContentSecurity({
  children,
}: {
  children?: React.ReactNode;
}): React.ReactElement {
  return <CSPProvider disableStyleElements>{children}</CSPProvider>;
}
