# Human wait lane verification

- Baseline `TestHumanWaitLaneReplay` reproduced Rework / Todo / Rework instead of Human Review / Human Review / Blocked; the existing Human Review case was unchanged.
- The final replay covers Todo, Rework, In Progress, and Merging; authorized answers and cleared human blockers return to the original lane after restart. The snapshot includes question text and elapsed age, or the human action.
- `TestHumanWaitFailureKeepsLane` verifies storage and tracker write errors do not become human blockers. Completion tests verify repeated questions and explicit human blockers do not become protected failure parks.
- Chrome DevTools inspected the production board renderer using an isolated ephemeral-port Go overlay preview. The snapshot was produced by the actual routing path. Human Review contains #2159, #2160, and unchanged #2129; Blocked contains #2007. In Comfy density the existing detail area displays both questions with `waiting 10h0m0s` and the hardware action. The preview intentionally uses last-known data and has no SSE backend.
- `board.png` records this final state. The overlay server exited successfully after the browser navigated away. No live instance was modified.
- Generation, all orchestrator and template package tests, and vet passed.

- `TestHumanWaitReplyReachesDispatchIssue` reproduced and fixed loss of authorized answer comments at the dispatch boundary. The final touched-package tests and vet pass.
