/* Framework-free viewer: a host may supply load/save through VisualReviewHost. */
(function () {
  'use strict';
  const $ = id => document.getElementById(id);
  const el = (tag, text, cls) => {const n=document.createElement(tag);if(text!==undefined)n.textContent=text;if(cls)n.className=cls;return n;};
  const notice = (text,error=false) => { $('notice').textContent=text;$('notice').className=error?'error':''; $('modal-notice').textContent=text;$('modal-notice').className=error?'warning':''; };
  const key = m => `visual-review:1:${m.repository}:${m.pr}:${m.head_sha}:${m.capture_id}`;
  const localHost = {
    load: () => globalThis.VISUAL_REVIEW_PACKAGE.manifest,
    readDraft: m => JSON.parse(localStorage.getItem(key(m)) || 'null'),
    saveDraft: (m,f) => localStorage.setItem(key(m),JSON.stringify(f))
  };
  const host = globalThis.VisualReviewHost || localHost;
  let m, f, change, asset, selection=null, start=null;
  const mediaFailures=new Set();
  function mediaFailure(a){mediaFailures.add(a.id);$('recommendation').querySelector('[value="recommend-approval"]').disabled=true;if($('recommendation').value==='recommend-approval')$('recommendation').value='draft';notice(`Could not display ${a.label}. Restore the missing/unsupported media and reload before recommending approval.`,true);}
  const drafts = new Map();
  const toolCursors=Object.fromEntries(Object.entries({
    pin:'<circle cx="20" cy="19" r="5"/><path d="M20 24v6"/>',
    rectangle:'<rect x="13" y="13" width="16" height="13" rx="1"/>',
    ellipse:'<ellipse cx="21" cy="20" rx="8" ry="6"/>',
    arrow:'<path d="M13 28L28 13M18 13h10v10"/>',
    pen:'<path d="M13 28l2-7L25 11l5 5-10 10-7 2zM23 13l5 5"/>'
  }).map(([kind,shape])=>{const marks='<path d="M6 1v10M1 6h10"/>'+shape;const svg=`<svg xmlns="http://www.w3.org/2000/svg" width="32" height="32" viewBox="0 0 32 32"><g fill="none" stroke="white" stroke-width="4" stroke-linejoin="round">${marks}</g><g fill="none" stroke="#172b33" stroke-width="2" stroke-linejoin="round">${marks}</g></svg>`;return[kind,`url("data:image/svg+xml,${encodeURIComponent(svg)}") 6 6, crosshair`];}));
  function approvalControl(id){
    const label=el('label',undefined,'asset-approval'),input=el('input'),caption=el('span');input.type='checkbox';input.dataset.approval=id;input.setAttribute('aria-label',`Approve ${m.assets.find(a=>a.id===id).label}`);
    input.onchange=()=>{f.asset_approvals=(f.asset_approvals||[]).filter(a=>a.asset_id!==id);if(input.checked)f.asset_approvals.push({asset_id:id,author:$('author').value.trim()||'Anonymous reviewer',approved_at:new Date().toISOString()});save();};label.append(input,caption);return label;
  }
  function refreshApprovals(){
    const approved=new Set((f.asset_approvals||[]).map(a=>a.asset_id));
    document.querySelectorAll('[data-approval]').forEach(input=>{input.checked=approved.has(input.dataset.approval);input.disabled=mediaFailures.has(input.dataset.approval);input.nextElementSibling.textContent=input.checked?'Approved':'Approve';});
    $('approval-progress').textContent=`${approved.size} of ${m.assets.length} approved`;
    $('approval-meter').max=Math.max(1,m.assets.length);$('approval-meter').value=approved.size;
    document.querySelectorAll('#changes button').forEach(b=>{const c=m.changes.find(c=>c.id===b.dataset.id);b.querySelector('span').textContent=c.asset_ids.length?`${c.asset_ids.filter(id=>approved.has(id)).length} / ${c.asset_ids.length} approved · ${c.status}`:c.status;});
    const disabled=approved.size!==m.assets.length||m.changes.some(c=>c.status==='blocked')||mediaFailures.size>0;
    $('recommendation').querySelector('[value="recommend-approval"]').disabled=disabled;
    if(disabled&&$('recommendation').value==='recommend-approval')$('recommendation').value='draft';
  }
  const undoStack=[],redoStack=[];
  function checkpoint(){undoStack.push(JSON.stringify(f.annotations));redoStack.length=0;}
  function undo(redo=false){const from=redo?redoStack:undoStack,to=redo?undoStack:redoStack;if(!from.length)return;to.push(JSON.stringify(f.annotations));f.annotations=JSON.parse(from.pop());save();renderNotes();renderMarks();}
  function openAsset(id) {
    showAsset(id);
    $('modal-title').textContent=asset.label;
    $('modal-notice').textContent='Choose a markup tool, mark the image, then add your feedback. Text uses your feedback as its label.';
    if(!$('lightbox').open)$('lightbox').showModal();
  }
  function feedback() {
    return {...f,author:$('author').value.trim()||'Anonymous reviewer',recommendation:$('recommendation').value,exported_at:new Date().toISOString(),drafts:Object.fromEntries(drafts)};
  }
  function save() {
    refreshApprovals();
    f=feedback();
    try {host.saveDraft(m,f);}catch {notice('Browser storage is unavailable. Export feedback before closing this page.',true);}
  }
  function renderNotes() {
    const notes=f.annotations.filter(a=>a.change_id===change.id&&a.asset_id===asset?.id);
    $('annotations').replaceChildren();$('count').textContent=`(${notes.length})`;
    for(const a of notes){
      const li=el('li',a.text);li.id=`annotation-${a.id}`;
      li.append(el('small',`${a.author||f.author} · ${a.asset_id} · ${a.kind}${a.kind==='timestamp'?` at ${a.time}s`:''}`));
      const remove=el('button','Remove','secondary');remove.type='button';remove.onclick=()=>{checkpoint();f.annotations=f.annotations.filter(x=>x.id!==a.id);save();renderNotes();renderMarks();};li.append(remove);$('annotations').append(li);
    }
  }
  function renderMarks() {
    $('stage').querySelectorAll('.mark,.selection,.drawing').forEach(n=>n.remove());
    if(!asset || asset.kind==='video')return;
    const notes=f.annotations.filter(a=>a.asset_id===asset.id&&['pin','rectangle','ellipse','arrow','pen','text'].includes(a.kind));
    notes.forEach((a,i)=>{
      if(['arrow','pen','text'].includes(a.kind)){draw(a,false);return;}
      const b=el('button',String(i+1),`mark ${a.kind}`);b.type='button';b.setAttribute('aria-label',`Annotation ${i+1}: ${a.text}`);
      b.style.borderColor=a.color||'#913800';b.style.color=a.color||'#913800';b.style.borderWidth=`${a.stroke_width||2}px`;
      b.style.left=`${a.x*100}%`;b.style.top=`${a.y*100}%`;
      if(['rectangle','ellipse'].includes(a.kind)){b.style.width=`${a.width*100}%`;b.style.height=`${a.height*100}%`;}
      b.tabIndex=-1;b.style.pointerEvents='none';$('stage').append(b);
    });
    if(selection){if(selection.kind==='text')draw({...selection,text:$('comment').value||'Type your text…'},true);else if(['arrow','pen'].includes(selection.kind))draw(selection,true);else{const box=el('div',undefined,selection.width?'selection':'selection selected-pin');Object.assign(box.style,{left:`${selection.x*100}%`,top:`${selection.y*100}%`,borderColor:$('ink').value,...(selection.width?{width:`${selection.width*100}%`,height:`${selection.height*100}%`}:{}),...(selection.kind==='ellipse'?{borderRadius:'50%'}:{})});$('stage').append(box);}}
    $('undo').disabled=!undoStack.length;$('redo').disabled=!redoStack.length;
  }
  function draw(a,preview){
    const ns='http://www.w3.org/2000/svg',svg=document.createElementNS(ns,'svg');svg.classList.add('drawing');svg.setAttribute('viewBox',`0 0 ${asset.width} ${asset.height}`);svg.setAttribute('aria-label',preview?'Draft markup':a.text);
    const color=a.color||$('ink').value,stroke=a.stroke_width||Number($('stroke').value);
    const shape=(tag,attrs)=>{const n=document.createElementNS(ns,tag);for(const[k,v]of Object.entries(attrs))n.setAttribute(k,String(v));svg.append(n);return n;};
    if(a.kind==='text'){const n=shape('text',{x:a.x*asset.width,y:a.y*asset.height,fill:color,'font-size':Math.max(16,asset.width/45),'dominant-baseline':'hanging'});n.textContent=a.text;}
    else{
      const points=a.kind==='pen'?a.points:[{x:a.x,y:a.y},{x:a.end_x,y:a.end_y}];
      shape('polyline',{points:points.map(p=>`${p.x*asset.width},${p.y*asset.height}`).join(' '),fill:'none',stroke:color,'stroke-width':stroke,'vector-effect':'non-scaling-stroke','stroke-linecap':'round','stroke-linejoin':'round'});
      if(a.kind==='arrow'){const x=a.end_x*asset.width,y=a.end_y*asset.height,angle=Math.atan2(y-a.y*asset.height,x-a.x*asset.width),len=Math.max(12,asset.width/60);shape('polyline',{points:`${x-len*Math.cos(angle-.5)},${y-len*Math.sin(angle-.5)} ${x},${y} ${x-len*Math.cos(angle+.5)},${y-len*Math.sin(angle+.5)}`,fill:'none',stroke:color,'stroke-width':stroke,'vector-effect':'non-scaling-stroke'});}
    }
    $('stage').append(svg);
  }
  function showAsset(id) {
    asset=m.assets.find(a=>a.id===id);selection=null;$('stage').replaceChildren();
    if(!asset)return;
    $('stage').classList.remove('actual-size');$('zoom').textContent='Actual size';$('modal-title').textContent=asset.label;
    $('asset').value=id;
    const media=el(asset.kind==='video'?'video':'img');media.src=asset.path;
    if(asset.kind==='video'){media.controls=true;media.preload='metadata';media.ontimeupdate=()=>{$('time').value=media.currentTime.toFixed(1);};}
    else {media.alt=asset.label;media.width=asset.width;media.height=asset.height;media.draggable=false;}
    media.onerror=()=>mediaFailure(asset);
    $('stage').append(media);$('observed').textContent=asset.observed;$('source').textContent=JSON.stringify(asset.source,null,2);
    $('zoom').disabled=asset.kind==='video';$('image-help').hidden=asset.kind==='video';
    $('kind').value=asset.kind==='video'?'timestamp':'pin';
    for(const o of $('kind').options)o.disabled=asset.kind==='video'?!['note','timestamp'].includes(o.value):o.value==='timestamp';
    document.querySelectorAll('[data-markup-tool]').forEach(b=>b.disabled=asset.kind==='video');
    $('modal-approval').replaceChildren(approvalControl(id));refreshApprovals();fields();renderMarks();renderNotes();
  }
  function fields(){const kind=$('kind').value;$('stage').style.cursor=kind==='text'?'text':(toolCursors[kind]||'default');for(const n of ['x','y','width','height','time']){$(n).parentElement.hidden=!(['pin','rectangle','ellipse','text'].includes(kind)&&['x','y'].includes(n)||['rectangle','ellipse'].includes(kind)&&['width','height'].includes(n)||kind==='timestamp'&&n==='time');}document.querySelectorAll('[data-markup-tool]').forEach(b=>b.setAttribute('aria-pressed',String(b.dataset.markupTool===kind)));}
  function showChange(id) {
    if(change)drafts.set(change.id,$('comment').value);
    change=m.changes.find(c=>c.id===id);
    document.querySelectorAll('#changes button').forEach(b=>{b.classList.toggle('active',b.dataset.id===id);b.setAttribute('aria-current',b.dataset.id===id?'true':'false');});
    $('change-id').textContent=change.id;$('change-title').textContent=change.title;$('change-status').textContent=change.status;
    $('description').textContent=change.description;$('reason').hidden=!change.reason;$('reason').textContent=change.reason||'';
    $('comment').value=drafts.get(id)||'';$('evidence').replaceChildren();$('asset').replaceChildren();
    const assets=change.asset_ids.map(id=>m.assets.find(a=>a.id===id));
    if(!assets.some(a=>a.kind==='before'))$('evidence').append(el('p','Before evidence is not available for this change.','muted'));
    const grid=el('div',undefined,'evidence-grid');
    for(const a of assets){
      const o=el('option',a.label);o.value=a.id;$('asset').append(o);
      const card=el('button',undefined,'evidence-card');card.type='button';card.append(el('strong',a.kind.toUpperCase()));
      if(a.kind!=='video'){const img=el('img');img.src=a.path;img.alt=a.label;img.loading='lazy';img.onerror=()=>mediaFailure(a);card.append(img);}
      card.append(el('span',a.label),el('small','Click to open & annotate'));card.setAttribute('aria-label',`Open and annotate ${a.label}`);card.onclick=()=>openAsset(a.id);const item=el('div',undefined,'evidence-item');item.append(card,approvalControl(a.id));grid.append(item);
    }
    $('evidence').append(grid);$('annotation-panel').hidden=!assets.length;
    if(assets.length)showAsset((assets.find(a=>a.kind==='after')||assets[0]).id);else asset=null;
    renderNotes();refreshApprovals();
  }
  function point(e){const r=$('stage').getBoundingClientRect();return {x:Math.max(0,Math.min(1,(e.clientX-r.left)/r.width)),y:Math.max(0,Math.min(1,(e.clientY-r.top)/r.height))};}
  async function boot(){
    m=globalThis.VisualReviewSchema.manifest(await host.load());
    const progress=el('p',undefined,'muted');progress.id='approval-progress';$('coverage').after(progress);
    const meter=el('progress',undefined,'approval-meter');meter.id='approval-meter';meter.setAttribute('aria-label','Evidence approval progress');progress.after(meter);
    const modalApproval=el('div');modalApproval.id='modal-approval';$('close-zoom').before(modalApproval);
    const panel=$('annotation-panel'), canvas=el('div',undefined,'canvas-scroll'), sidebar=el('div',undefined,'annotation-sidebar');
    for(const value of ['ellipse','arrow','pen','text']){const o=el('option',value[0].toUpperCase()+value.slice(1));o.value=value;$('kind').append(o);}
    const toolbar=el('div',undefined,'markup-toolbar');toolbar.setAttribute('role','group');toolbar.setAttribute('aria-label','Markup tools');
    for(const [kind,label]of [['pin','● Pin'],['rectangle','□ Rectangle'],['ellipse','◯ Ellipse'],['arrow','↗ Arrow'],['pen','✎ Draw'],['text','T Text']]){const b=el('button',label,'secondary');b.type='button';b.dataset.markupTool=kind;b.onclick=()=>{$('kind').value=kind;selection=null;fields();renderMarks();};toolbar.append(b);}
    const ink=el('input');ink.type='color';ink.id='ink';ink.value='#e25a30';ink.setAttribute('aria-label','Markup color');toolbar.append(ink);
    const stroke=el('select');stroke.id='stroke';stroke.setAttribute('aria-label','Line width');for(const n of [2,4,8]){const o=el('option',`${n} px`);o.value=n;stroke.append(o);}toolbar.append(stroke);
    for(const [id,label]of [['undo','↶ Undo'],['redo','↷ Redo']]){const b=el('button',label,'secondary');b.id=id;b.type='button';b.onclick=()=>undo(id==='redo');toolbar.append(b);}
    $('lightbox').insertBefore(toolbar,$('modal-notice'));
    canvas.append($('stage'));sidebar.append($('annotation-form'),$('annotations').parentElement);
    panel.querySelector('h3').remove();$('asset').parentElement.hidden=true;sidebar.querySelector('.form-row').hidden=true;
    $('image-help').parentElement.hidden=true;toolbar.append($('zoom'));
    panel.style.gridTemplateRows='minmax(0,1fr)';canvas.style.gridRow='1';sidebar.style.gridRow='1';
    panel.append(canvas,sidebar);$('full-media').append(panel);
    const observed=$('observed'), details=$('source').parentElement;
    sidebar.append(observed,details);
    for (const name of ['x','y','width','height']) $(name).step='any';
    f={schema_version:1,repository:m.repository,pr:m.pr,capture_id:m.capture_id,head_sha:m.head_sha,authenticated:false,author:'Anonymous reviewer',recommendation:'draft',exported_at:new Date().toISOString(),annotations:[]};
    try{const draft=await host.readDraft(m);if(draft){if(draft.recommendation==='recommend-approval'&&(draft.asset_approvals||[]).length!==m.assets.length)draft.recommendation='draft';f=globalThis.VisualReviewSchema.feedback(draft,m);}}catch{notice('Saved draft could not be read. Import an exported feedback file to recover it.',true);}
    $('title').textContent=m.title;document.title=`Visual review · ${m.title}`;$('identity').textContent=`${m.repository} · PR #${m.pr} · ${m.head_sha.slice(0,10)} · ${new Date(m.captured_at).toLocaleString()}`;
    $('summary').textContent=m.summary;$('coverage-notes').textContent=m.coverage_notes;$('revision').textContent=`Capture: ${m.capture_id}\nHead: ${m.head_sha}\nBase: ${m.base_sha}`;
    const captured=m.changes.filter(c=>c.status==='captured').length,blocked=m.changes.filter(c=>c.status==='blocked').length;
    $('coverage').textContent=`${captured} captured · ${blocked} ${blocked===1?'gap':'gaps'} · ${m.changes.length-captured-blocked} non-UI`;
    $('recommendation').querySelector('[value="recommend-approval"]').disabled=blocked>0;
    $('approval-help').textContent=blocked?`Recommend approval is unavailable: ${blocked} ${blocked===1?'change has':'changes have'} missing evidence.`:'Approve each screenshot or video before recommending approval. These local approvals do not change Detent or GitHub.';
    if(blocked){const gaps=el('ul');for(const c of m.changes.filter(c=>c.status==='blocked')){const li=el('li'),b=el('button',c.title,'secondary');b.onclick=()=>{showChange(c.id);$('change-title').scrollIntoView({block:'start',behavior:'smooth'});};li.append(b);gaps.append(li);}$('approval-help').append(gaps);}
    $('author').value=f.author==='Anonymous reviewer'?'':f.author;$('recommendation').value=f.recommendation;
    for(const [k,v] of Object.entries(f.drafts||{}))drafts.set(k,v);
    for(const file of m.changed_files)$('files').append(el('li',file));
    for(const c of m.changes){const b=el('button',c.title);b.type='button';b.dataset.id=c.id;b.append(el('span',c.status));b.onclick=()=>showChange(c.id);$('changes').append(b);}
    const prior=globalThis.VISUAL_REVIEW_PACKAGE?.previous_review;
    if(prior){
      globalThis.VisualReviewSchema.feedback(prior.feedback,prior.manifest);
      globalThis.VisualReviewSchema.responses(prior.responses,prior.feedback,m);
      const panel=document.querySelector('.review-panel');panel.append(el('h3','Previous round'));
      panel.append(el('p',`Original capture: ${prior.manifest.capture_id}. Keep the original package to inspect its media. Agent responses below are not reviewer acceptance.`,'muted'));
      for(const a of prior.feedback.annotations){
        const r=prior.responses.items.find(r=>r.annotation_id===a.id),box=el('div',undefined,'summary-box');
        box.append(el('strong',a.text),el('p',`${r.status}: ${r.explanation}`),el('small',`Original reference: ${a.id} · ${a.asset_id}`));
        for(const id of r.asset_ids){const target=m.changes.find(c=>c.asset_ids.includes(id));if(target){const b=el('button','View new evidence','secondary');b.onclick=()=>{showChange(target.id);openAsset(id);};box.append(b);}}
        panel.append(box);
      }
    }
    $('asset').onchange=()=>showAsset($('asset').value);$('kind').onchange=()=>{selection=null;fields();renderMarks();};
    $('author').onchange=save;$('recommendation').onchange=save;
    $('comment').oninput=()=>{drafts.set(change.id,$('comment').value);save();if(selection?.kind==='text')renderMarks();};
    $('stage').onpointerdown=e=>{if(!asset||asset.kind==='video'||e.target.closest('button')||e.button!==0)return;start=point(e);selection={...start,kind:$('kind').value,points:[start]};$('stage').setPointerCapture(e.pointerId);};
    $('stage').onpointermove=e=>{if(!start)return;const p=point(e),kind=$('kind').value;if(kind==='pen'){if(selection.points.length<2000)selection.points.push(p);}else if(kind==='arrow')selection={...start,kind,end_x:p.x,end_y:p.y};else selection={kind,x:Math.min(start.x,p.x),y:Math.min(start.y,p.y),width:Math.abs(p.x-start.x),height:Math.abs(p.y-start.y)};renderMarks();};
    $('stage').onpointercancel=()=>{start=null;selection=null;renderMarks();};
    $('stage').onpointerup=e=>{
      if(!start)return;const p=point(e),kind=$('kind').value;
      if(['pen','arrow'].includes(kind)){if(kind==='arrow')selection={...start,kind,end_x:p.x,end_y:p.y};else if(selection.points.length<2000)selection.points.push(p);}
      else selection=['rectangle','ellipse'].includes(kind)?{kind,x:Math.min(p.x,start.x),y:Math.min(p.y,start.y),width:Math.abs(p.x-start.x),height:Math.abs(p.y-start.y)}:{kind,...p};
      for(const n of ['x','y','width','height'])if(selection[n]!==undefined)$(n).value=(selection[n]*100).toFixed(3);start=null;fields();renderMarks();$('comment').focus();
    };
    $('annotation-form').onsubmit=e=>{
      e.preventDefault();if(!$('comment').value.trim()){notice('Write feedback before adding the annotation.',true);return;}
      const a={id:`a-${crypto.randomUUID()}`,change_id:change.id,asset_id:asset.id,kind:$('kind').value,text:$('comment').value.trim(),author:$('author').value.trim()||'Anonymous reviewer',created_at:new Date().toISOString()};
      a.color=$('ink').value;a.stroke_width=Number($('stroke').value);
      if(['pin','rectangle','ellipse','text'].includes(a.kind)){a.x=Number($('x').value)/100;a.y=Number($('y').value)/100;}
      if(['rectangle','ellipse'].includes(a.kind)){a.width=Number($('width').value)/100;a.height=Number($('height').value)/100;}
      if(['arrow','pen'].includes(a.kind)){if(!selection||selection.kind!==a.kind){notice('Draw on the image before adding this annotation.',true);return;}Object.assign(a,selection);}
      if(a.kind==='timestamp')a.time=Number($('time').value);
      try{globalThis.VisualReviewSchema.feedback({...feedback(),annotations:[...f.annotations,a]},m);}catch(err){notice(err.message,true);return;}
      checkpoint();f.annotations.push(a);$('comment').value='';drafts.delete(change.id);selection=null;save();renderNotes();renderMarks();notice('Annotation added. Export feedback when you are ready to return it.');
    };
    $('export').onclick=()=>{
      try{if([...drafts.values()].some(v=>v.trim()))throw Error('Add or clear your unsent annotation drafts before exporting.');save();if(mediaFailures.size&&f.recommendation==='recommend-approval')throw Error('Unavailable media prevents recommendation of approval.');globalThis.VisualReviewSchema.feedback(f,m);const url=URL.createObjectURL(new Blob([JSON.stringify(f,null,2)+'\n'],{type:'application/json'}));const a=el('a');a.href=url;a.download=`feedback-${m.capture_id}.json`;a.click();setTimeout(()=>URL.revokeObjectURL(url),1000);notice('Feedback exported. Return the JSON file to the agent; keep this capture for reference.');}catch(err){notice(err.message,true);}
    };
    $('import').onchange=async()=>{
      const file=$('import').files[0];if(!file)return;
      try{
        if(file.size>5*1024*1024)throw Error('Feedback file exceeds 5 MB');
        const incoming=globalThis.VisualReviewSchema.feedback(JSON.parse(await file.text()),m);
        const merged=new Map(f.annotations.map(a=>[a.id,a]));
        for(const a of incoming.annotations){if(merged.has(a.id)&&JSON.stringify(merged.get(a.id))!==JSON.stringify(a))throw Error('Conflicting annotation ID; import into a clean package or reconcile files first');merged.set(a.id,a);}
        const combined={...incoming,annotations:[...merged.values()]};globalThis.VisualReviewSchema.feedback(combined,m);f=combined;
        $('author').value=f.author;$('recommendation').value=f.recommendation;save();renderNotes();renderMarks();notice('Feedback imported for this exact capture. Existing annotations were preserved.');
      }catch(err){notice(err.message,true);}finally{$('import').value='';}
    };
    $('zoom').onclick=()=>{const actual=$('stage').classList.toggle('actual-size');$('zoom').textContent=actual?'Fit image':'Actual size';};
    $('close-zoom').onclick=()=>{$('lightbox').close();save();};
    $('lightbox').addEventListener('close',()=>{start=null;save();});showChange(m.changes[0].id);
  }
  boot().catch(err=>{notice(`Review could not load: ${err.message}`,true);$('title').textContent='Review unavailable';});
})();
