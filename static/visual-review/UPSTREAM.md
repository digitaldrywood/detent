# Vendored visual-review viewer

`viewer.js`, `schema.js`, and `style.css` are copied byte-for-byte from
`michaelhvisser/ai` commit `5db518a4212c7b53c666e6507f5beeccd6289a63`, under
`plugins/workflow/skills/visual-review/assets/`.

Detent owns `host.html`, `host.css`, and `host-adapter.js`. They adapt the unchanged local
viewer to authenticated, node-local HTTP persistence. Update the pinned files
only by comparing them against a named upstream commit and updating this note.
