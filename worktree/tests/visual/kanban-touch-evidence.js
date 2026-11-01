// Observe the gesture without changing its timers, events, or scroll behavior.
async function recordKanbanTouchEvents(page) {
  await page.evaluate(() => {
    const evidence = { events: [], truncated: false };
    window.__kanbanTouchEvidence = evidence;
    function record(event) {
      if (evidence.events.length >= 2000) {
        evidence.truncated = true;
        return;
      }
      const lanes = document.querySelector("#board-lanes");
      const target = document.querySelector('[data-kanban-drop-state="Todo"]');
      const box = target?.getBoundingClientRect();
      evidence.events.push({
        time: Date.now(),
        type: event.type,
        eventTarget: event.target?.id || event.target?.nodeName,
        trusted: event.isTrusted,
        x: event.clientX,
        y: event.clientY,
        touches: Array.from(event.touches || [], (touch) => ({
          id: touch.identifier,
          x: touch.clientX,
          y: touch.clientY,
        })),
        changedTouches: Array.from(event.changedTouches || [], (touch) => touch.identifier),
        scrollLeft: lanes?.scrollLeft,
        target: box ? { x: box.x, y: box.y, width: box.width, height: box.height } : null,
        dragging: !!document.querySelector('[data-kanban-dragging="true"]'),
        connection: document.documentElement.getAttribute("data-detent-connection"),
      });
    }
    for (const type of [
      "pointerdown", "pointermove", "pointercancel", "pointerup",
      "touchstart", "touchmove", "touchend", "touchcancel",
      "scroll", "scrollend", "htmx:afterSettle", "detent:connectionChange",
    ]) {
      document.addEventListener(type, record, { capture: true, passive: true });
    }
    window.addEventListener("blur", record, { passive: true });
  });
}

async function attachKanbanTouchEvidence(page, testInfo, laneSamples) {
  // Keep the original failure visible even if the page has already closed.
  const evidence = await page.evaluate(() => window.__kanbanTouchEvidence)
    .catch((error) => ({ captureError: error.message }));
  await testInfo.attach("kanban-touch-evidence", {
    body: JSON.stringify({ ...evidence, laneSamples }, null, 2),
    contentType: "application/json",
  });
}

module.exports = { recordKanbanTouchEvents, attachKanbanTouchEvidence };
