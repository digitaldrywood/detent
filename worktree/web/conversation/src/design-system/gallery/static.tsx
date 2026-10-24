// Entry of the static gallery build (`npm run build:gallery`, see
// `vite.gallery.config.ts`). The production client never imports it.
import { createRoot } from "react-dom/client";

import "../../app/index.css";
import { ContentSecurity } from "../../app/ContentSecurity.tsx";
import { applyFrameThemeFromLocation, GalleryApp } from "./standalone";

applyFrameThemeFromLocation();

const container = document.getElementById("root");
if (container === null) throw new Error("The design-system gallery has no mount point.");

createRoot(container).render(
  <ContentSecurity>
    <GalleryApp />
  </ContentSecurity>,
);
