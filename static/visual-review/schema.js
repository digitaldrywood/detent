/* Shared contract for the local CLI and future hosted viewer. No DOM dependencies. */
(function (root) {
  'use strict';
  const fail = message => { throw new Error(message); };
  const text = (v, name) => typeof v === 'string' && v.trim() && v.length <= 20000 || fail(`${name}: expected nonempty text`);
  const list = (v, name) => Array.isArray(v) && v.length <= 2000 || fail(`${name}: expected bounded array`);
  const id = (v, name) => typeof v === 'string' && /^[a-zA-Z0-9][a-zA-Z0-9._-]{0,99}$/.test(v) || fail(`${name}: invalid ID`);
  const sha = v => typeof v === 'string' && /^(?:[a-f0-9]{40}|[a-f0-9]{64})$/.test(v) || fail('Invalid commit SHA');
  const num = (v, min, max, name) => Number.isFinite(v) && v >= min && v <= max || fail(`${name}: invalid number`);
  const date = v => typeof v === 'string' && Number.isFinite(Date.parse(v)) || fail('Invalid date');
  const unique = (items, name) => new Set(items).size === items.length || fail(`${name}: duplicate values`);
  function localPath(v) {
    text(v, 'asset.path');
    if (!/^[a-zA-Z0-9_./-]+$/.test(v) || v.startsWith('/') || v.split('/').some(p => !p || p === '.' || p === '..')) fail('Asset path must be a safe relative path');
    if (!v.startsWith('media/')) fail('Asset paths must be under media/');
  }
  function manifest(m) {
    if (!m || m.schema_version !== 1) fail('Unsupported manifest schema_version');
    id(m.capture_id, 'capture_id'); text(m.title, 'title'); text(m.repository, 'repository');
    if (!/^[\w.-]+\/[\w.-]+$/.test(m.repository)) fail('Invalid repository');
    num(m.pr, 1, Number.MAX_SAFE_INTEGER, 'pr'); if (!Number.isInteger(m.pr)) fail('Invalid PR');
    sha(m.head_sha); sha(m.base_sha); date(m.captured_at);
    text(m.summary, 'summary'); text(m.coverage_notes, 'coverage_notes');
    list(m.changed_files, 'changed_files'); unique(m.changed_files, 'changed_files');
    m.changed_files.forEach(f => text(f, 'changed file'));
    list(m.assets, 'assets'); list(m.changes, 'changes');
    if (!m.changes.length || !m.changed_files.length) fail('Inventory must not be empty');
    unique(m.assets.map(a => a.id), 'asset IDs'); unique(m.assets.map(a => a.path), 'asset paths'); unique(m.changes.map(c => c.id), 'change IDs');
    const assets = new Map(m.assets.map(a => [a.id, a]));
    for (const a of m.assets) {
      id(a.id, 'asset ID'); localPath(a.path); text(a.label, 'asset label'); text(a.observed, 'observed');
      if (!['before', 'after', 'detail', 'video'].includes(a.kind)) fail('Invalid asset kind');
      const ext = a.path.split('.').pop().toLowerCase();
      if (!(a.kind === 'video' ? ['mp4', 'webm'] : ['png', 'jpg', 'jpeg', 'webp']).includes(ext)) fail('Unsupported media extension');
      if (a.sha256 !== undefined && !/^[a-f0-9]{64}$/.test(a.sha256)) fail('Invalid media digest');
      num(a.width, 1, 50000, 'width'); num(a.height, 1, 100000, 'height');
      if (a.kind === 'video') num(a.duration, 0.01, 14400, 'duration');
      if (typeof a.inspected !== 'boolean') fail('Inspected must be explicit');
      const s = a.source;
      if (!s) fail('Missing asset source'); sha(s.commit); text(s.url, 'source.url');
      if (!/^https?:\/\//.test(s.url)) fail('Source URL must use HTTP(S)');
      text(s.provenance, 'provenance'); text(s.state, 'state'); text(s.role, 'role'); text(s.theme, 'theme'); text(s.conditions, 'conditions');
      num(s.viewport?.width, 1, 10000, 'viewport width'); num(s.viewport?.height, 1, 10000, 'viewport height');
      if (a.kind !== 'before' && s.commit !== m.head_sha) fail('After evidence does not match head SHA');
      if (a.parent_id) {
        const p = assets.get(a.parent_id);
        if (!p || p.id === a.id || p.kind === 'video' || p.parent_id || p.source.commit !== s.commit) fail('Invalid parent asset');
        const c = a.crop;
        if (!c) fail('Crop requires source rectangle');
        num(c.x, 0, p.width, 'crop x'); num(c.y, 0, p.height, 'crop y'); num(c.width, 1, p.width, 'crop width'); num(c.height, 1, p.height, 'crop height');
        if (c.x + c.width > p.width || c.y + c.height > p.height) fail('Crop outside parent');
      } else if (a.crop) fail('Crop requires parent_id');
    }
    const covered = new Set(), referencedAssets = new Set();
    for (const c of m.changes) {
      id(c.id, 'change ID'); text(c.title, 'change title'); text(c.description, 'change description');
      list(c.files, 'change files'); list(c.asset_ids, 'change assets'); unique(c.asset_ids, 'change assets');
      if (!c.files.length) fail('Change must identify files');
      c.files.forEach(f => { if (!m.changed_files.includes(f)) fail('Change references unknown file'); covered.add(f); });
      if (!['captured', 'blocked', 'not-ui'].includes(c.status)) fail('Invalid coverage status');
      c.asset_ids.forEach(a => { if (!assets.has(a)) fail('Unknown asset reference'); referencedAssets.add(a); });
      if (c.status === 'captured') {
        if (!c.asset_ids.some(a => ['after', 'detail'].includes(assets.get(a).kind))) fail('Captured change requires after screenshot');
        if (c.asset_ids.some(a => !assets.get(a).inspected)) fail('Captured evidence must be inspected');
      } else text(c.reason, 'coverage reason');
    }
    if (m.changed_files.some(f => !covered.has(f))) fail('Changed file missing from inventory');
    if (m.assets.some(a => !referencedAssets.has(a.id))) fail('Asset missing from change inventory');
    return m;
  }
  function feedback(f, m) {
    manifest(m);
    if (!f || f.schema_version !== 1 || f.capture_id !== m.capture_id || f.head_sha !== m.head_sha || f.repository !== m.repository || f.pr !== m.pr) fail('Feedback does not belong to this capture');
    if (!['draft', 'request-changes', 'recommend-approval'].includes(f.recommendation)) fail('Invalid recommendation');
    if (f.authenticated !== false) fail('Local feedback must be unauthenticated');
    text(f.author, 'author'); date(f.exported_at); list(f.annotations, 'annotations'); unique(f.annotations.map(a => a.id), 'annotation IDs');
    if (f.recommendation === 'recommend-approval' && m.changes.some(c => c.status === 'blocked')) fail('Incomplete coverage cannot recommend approval');
    const assets = new Map(m.assets.map(a => [a.id, a]));
    const approvals=f.asset_approvals||[];list(approvals,'asset approvals');unique(approvals.map(a=>a.asset_id),'asset approval IDs');
    for(const a of approvals){if(!assets.has(a.asset_id))fail('Unknown approved asset');text(a.author,'approval author');date(a.approved_at);}
    if(f.recommendation==='recommend-approval'&&approvals.length!==assets.size)fail('Every screenshot or video needs approval');
    for (const a of f.annotations) {
      id(a.id, 'annotation ID'); text(a.text, 'annotation text'); date(a.created_at);
      if (a.author !== undefined) text(a.author, 'annotation author');
      if (!m.changes.some(c => c.id === a.change_id)) fail('Unknown change in feedback');
      const asset = assets.get(a.asset_id);
      if (!asset || !m.changes.find(c => c.id === a.change_id).asset_ids.includes(a.asset_id)) fail('Unknown asset in feedback');
      if (!['note', 'pin', 'rectangle', 'ellipse', 'arrow', 'pen', 'text', 'timestamp'].includes(a.kind)) fail('Invalid annotation kind');
      if (a.color !== undefined && !/^#[a-fA-F0-9]{6}$/.test(a.color)) fail('Invalid annotation color');
      if (a.stroke_width !== undefined) num(a.stroke_width, 1, 12, 'stroke width');
      if (a.kind === 'timestamp') { if (asset.kind !== 'video') fail('Timestamp requires video'); num(a.time, 0, asset.duration, 'timestamp'); }
      if (['pin', 'rectangle', 'ellipse', 'arrow', 'pen', 'text'].includes(a.kind)) {
        if (asset.kind === 'video') fail('Coordinates require screenshot');
        num(a.x, 0, 1, 'x'); num(a.y, 0, 1, 'y');
        if (['rectangle','ellipse'].includes(a.kind)) {
          num(a.width, 0.001, 1, 'rectangle width'); num(a.height, 0.001, 1, 'rectangle height');
          if (a.x + a.width > 1.000001 || a.y + a.height > 1.000001) fail('Rectangle outside image');
        }
        if (a.kind === 'arrow') {num(a.end_x, 0, 1, 'arrow end x');num(a.end_y, 0, 1, 'arrow end y');}
        if (a.kind === 'pen') {
          list(a.points, 'pen points');if(a.points.length<2)fail('Pen needs at least two points');
          for(const p of a.points){num(p.x,0,1,'pen x');num(p.y,0,1,'pen y');}
        }
      }
    }
    if (f.drafts !== undefined) {
      if (!f.drafts || typeof f.drafts !== 'object' || Array.isArray(f.drafts)) fail('Invalid draft map');
      for (const [key, value] of Object.entries(f.drafts)) {
        if (!m.changes.some(c => c.id === key) || typeof value !== 'string' || value.length > 20000) fail('Invalid unsent draft');
      }
    }
    return f;
  }
  function responses(r, previous, current) {
    if (!r || r.schema_version !== 1 || r.capture_id !== current.capture_id || r.feedback_capture_id !== previous.capture_id) fail('Responses do not match captures');
    list(r.items, 'response items'); unique(r.items.map(i => i.annotation_id), 'response annotation IDs');
    const ids = previous.annotations.map(a => a.id), assets = new Map(current.assets.map(a => [a.id, a]));
    for (const item of r.items) {
      if (!ids.includes(item.annotation_id) || !['addressed', 'unresolved'].includes(item.status)) fail('Invalid feedback response');
      text(item.explanation, 'response explanation'); list(item.asset_ids, 'response assets');
      for (const id of item.asset_ids) if (!assets.has(id)) fail('Unknown response evidence');
      if (item.status === 'addressed' && (!item.asset_ids.length || !item.asset_ids.every(id => {
        const asset = assets.get(id);
        return asset.inspected === true && ['after', 'detail', 'video'].includes(asset.kind) && asset.source.commit === current.head_sha;
      }))) fail('Addressed response needs inspected current-result evidence');
    }
    if (ids.some(id => !r.items.some(i => i.annotation_id === id))) fail('Feedback annotation missing a response');
    return r;
  }
  const api = { manifest, feedback, responses };
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
  else root.VisualReviewSchema = api;
})(globalThis);
