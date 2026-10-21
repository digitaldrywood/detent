---
name: deferred-form-handler-readiness
description: Diagnose native POST navigation caused by submitting server-rendered forms before deferred JavaScript installs their handlers.
when_to_use: Browser journeys lose their HTML page after a form submit, especially when traces show a POST to the document URL and an aborted script request.
---

Inspect the failed Playwright trace network records before changing timeouts. Compare the failed POST's URL, content type, response status, and navigation with a successful API submission. A POST to the current HTML URL plus a canceled deferred script can indicate that the browser performed the form's default action before its handler existed. Confirm the event sequence; the canceled request alone does not establish causality.

Reproduce on an isolated server by routing the responsible script through a promise barrier. Navigate with `waitUntil: 'commit'` so the test can interact with parsed HTML while the deferred script remains held. Fill and submit the form, and record the actual document POST response. Release the barrier in `finally`.

For JavaScript-only forms, render submit controls disabled and enable them only after the submit listener is installed. Preserve genuine non-JavaScript submission paths when they exist. Do not hide the product defect by adding a test-only page-load wait.

Turn the reproduction into a regression: hold the script, assert submission is disabled, release it, then assert the control becomes enabled and saving reaches the intended API and returns to readable HTML. Cover shared form families and retain the original journey. Use a barrier rather than an arbitrary delay.
