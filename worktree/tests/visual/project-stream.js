function dispatchHumanActionAnswered(workItemId) {
  for (const source of window.findingSources) source.dispatchEvent(new MessageEvent("human_action", {
    data: JSON.stringify({ entity_id: workItemId }),
  }));
}

module.exports = { dispatchHumanActionAnswered };
