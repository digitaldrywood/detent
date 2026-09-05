(function () {
  'use strict';

  const match = window.location.pathname.match(/^\/projects\/([^/]+)\/visual-reviews\/([^/]+)\/?$/);
  let loaded;
  let revision = 0;
  let pending = null;
  let saving = false;
  let stopped = false;

  function endpoint(suffix) {
    if (!match) throw new Error('Invalid visual review URL');
    return `/api/v1/projects/${encodeURIComponent(decodeURIComponent(match[1]))}/visual-reviews/${encodeURIComponent(decodeURIComponent(match[2]))}${suffix || ''}`;
  }

  function status(text, kind) {
    let node = document.getElementById('detent-save-status');
    if (!node) {
      node = document.createElement('p');
      node.id = 'detent-save-status';
      node.setAttribute('role', 'status');
      node.setAttribute('aria-live', 'polite');
      document.getElementById('notice')?.after(node);
    }
    node.textContent = text;
    node.className = kind || '';
    const recommendation = document.getElementById('recommendation');
    if (recommendation) recommendation.disabled = saving || stopped || !loaded?.writable;
  }

  function enforceReadOnly() {
    if (loaded?.writable) return;
    document.querySelectorAll('#annotation-form input, #annotation-form textarea, #annotation-form select, #annotation-form button, #annotations button, #undo, #redo, #author, #recommendation, #import, [data-approval], [data-markup-tool]').forEach(control => {
      control.disabled = true;
    });
  }

  function renderRounds() {
    if (!loaded?.rounds?.length) return;
    const panel = document.querySelector('.round-help');
    const list = document.createElement('ol');
    list.id = 'detent-review-rounds';
    for (const round of loaded.rounds) {
      const item = document.createElement('li');
      const link = document.createElement('a');
      link.href = round.url;
      link.textContent = `${round.capture_id} · ${round.head_sha.slice(0, 10)} · ${new Date(round.captured_at).toLocaleString()}`;
      if (round.capture_id === loaded.manifest.capture_id) link.setAttribute('aria-current', 'page');
      item.append(link);
      list.append(item);
    }
    panel?.append(list);
  }

  async function flush() {
    if (saving || stopped || !pending) return;
    saving = true;
    const feedback = pending;
    pending = null;
    status('Saving review draft…');
    try {
      const response = await fetch(endpoint('/draft'), {
        method: 'PUT',
        credentials: 'same-origin',
        headers: {'Content-Type': 'application/json', 'HX-Request': 'true'},
        body: JSON.stringify({
          capture_id: loaded.manifest.capture_id,
          head_sha: loaded.manifest.head_sha,
          expected_revision: revision,
          feedback
        })
      });
      if (response.status === 409) {
        stopped = true;
        status('Conflict: this draft changed elsewhere or the PR head is stale. Export your work, then reload before continuing.', 'error');
        return;
      }
      if (!response.ok) throw new Error(`HTTP ${response.status}`);
      const result = await response.json();
      revision = result.revision;
    } catch (error) {
      stopped = true;
      if (!pending) pending = feedback;
      status(`Save failed (${error.message}). Export your work before leaving this page, then reload to retry.`, 'error');
      return;
    } finally {
      saving = false;
    }
    status(pending ? 'New edits waiting to save…' : `Saved on this Detent node at ${new Date().toLocaleTimeString()}`);
    if (pending) void flush();
  }

  globalThis.VisualReviewHost = {
    async load() {
      const response = await fetch(endpoint(''), {
        credentials: 'same-origin',
        headers: {'HX-Request': 'true'}
      });
      if (!response.ok) throw new Error(`Detent returned HTTP ${response.status}`);
      loaded = await response.json();
      revision = loaded.revision || 0;
      queueMicrotask(renderRounds);
      queueMicrotask(() => status(
        loaded.writable ? 'Draft is stored on this Detent node. Review assertions are not GitHub approval.' : loaded.read_only_reason,
        loaded.writable ? '' : 'warning'
      ));
      if (!loaded.writable) {
        const observer = new MutationObserver(enforceReadOnly);
        observer.observe(document.body, {childList: true, subtree: true});
        queueMicrotask(enforceReadOnly);
      }
      return loaded.manifest;
    },
    async readDraft() {
      return loaded?.feedback || null;
    },
    saveDraft(_manifest, feedback) {
      if (!loaded?.writable || stopped) return;
      pending = feedback;
      status(saving ? 'New edits waiting to save…' : 'Draft has unsaved edits…');
      void flush();
    }
  };

  window.addEventListener('beforeunload', event => {
    if (!pending && !saving && !stopped) return;
    event.preventDefault();
    event.returnValue = '';
  });
})();
