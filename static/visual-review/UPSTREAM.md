# Vendored visual-review viewer

`viewer.js`, `schema.js`, and `style.css` are copied byte-for-byte from
`michaelhvisser/ai` commit `eb0603070bbdba76d44d1497b9500870a59c4210`, under
`plugins/workflow/skills/visual-review/assets/`.

Detent owns `host.html`, `host.css`, and `host-adapter.js`. They adapt the unchanged local
viewer to authenticated, node-local HTTP persistence. Update the pinned files
only by comparing them against a named upstream commit and updating this note.
