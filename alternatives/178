# #2745 browser verification

Chrome DevTools loaded the real Fleet handler at a random localhost port using
an overlay test with two draining attempts. The existing banner rendered
`draining for update: 2 active attempts`; its accessibility tree exposed a live
status region. The state API exposed `update.state=draining` and
`update.active_attempts=2`. The follow-up API regression also verifies top-level
`status=draining`. The overlay test passed after the browser tab closed.

No live dogfood process, tracker lane, or production database was changed.
See [drain-banner.png](drain-banner.png).
