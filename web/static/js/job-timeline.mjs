// Paginated immutable job history. Provider adapters use this same view; this
// module never invents remote routes or treats a short run summary as a chat.
export function mergeHistory(existing, incoming, jobId) {
  const byId = new Map(existing.map(e => [e.id, e]));
  for (const e of incoming || []) {
    if (!e || typeof e.id !== 'string' || e.jobId !== jobId || !Number.isSafeInteger(e.sequence)) throw new Error('History did not match the selected job.');
    const old = byId.get(e.id);
    if (old && JSON.stringify(old) !== JSON.stringify(e)) throw new Error('An immutable history entry changed. Refresh to verify this history.');
    byId.set(e.id, e);
  }
  return [...byId.values()].sort((a,b) => a.sequence - b.sequence || a.id.localeCompare(b.id));
}
export function historyText(entry) {
  const r=entry.run;
  if(!r)return typeof entry.text==='string'?entry.text:'';
  const values=[entry.text,r.title,r.instructions,r.progress,typeof r.result==='string'?r.result:r.result?JSON.stringify(r.result,null,2):'',typeof r.error==='string'?r.error:r.error?JSON.stringify(r.error,null,2):''];
  return [...new Set(values.filter(v=>typeof v==='string'&&v.length))].join('\n\n');
}
export function createHistoryLoader(request, changed = () => {}) {
  let epoch = 0;
  let state = {jobId:'',entries:[],olderCursor:'',newerCursor:'',busy:false,error:'',boundary:'',legacy:false,hasMoreNewer:false};
  function emit() { changed({...state,entries:[...state.entries]}); }
  async function load(mode='latest') {
    if (!state.jobId || state.busy) return;
    const token = epoch, jobId = state.jobId;
    const cursor = mode === 'older' ? state.olderCursor : mode === 'newer' ? state.newerCursor : '';
    if (mode !== 'latest' && !cursor) return;
    state.busy = true; state.error = ''; emit();
    try {
      const query = new URLSearchParams({limit:'30'});
      if (cursor) query.set(mode === 'older' ? 'cursor' : 'after', cursor);
      const data = await request(`/api/v1/jobs/${encodeURIComponent(jobId)}/history?${query}`);
      if (token !== epoch) return;
      state.entries = mergeHistory(mode === 'latest' ? [] : state.entries, data.entries, jobId);
      if (mode !== 'newer') state.olderCursor = data.olderCursor || '';
      if (mode !== 'older' || !state.newerCursor) state.newerCursor = data.newerCursor || state.newerCursor;
      state.hasMoreNewer = mode === 'newer' && Boolean(data.hasMore);
      state.boundary = data.retentionBoundary || 'Only history actually retained by the service is shown.';
      state.legacy = data.legacyHistoryAvailable === true;
    } catch(e) {
      if (token === epoch) state.error = e?.status === 404 ? 'No retained timeline is available for this job.' : e?.message || 'History could not be refreshed. Loaded entries remain available.';
    } finally { if (token === epoch) { state.busy = false; emit(); } }
  }
  return {
    select(jobId) { epoch++; state = {jobId,entries:[],olderCursor:'',newerCursor:'',busy:false,error:'',boundary:'',legacy:false,hasMoreNewer:false}; emit(); if(jobId) return load(); },
    load, state:()=>({...state,entries:[...state.entries]}),
    async catchUp() { if(state.busy) return; let pages=0; do { await load(state.newerCursor ? 'newer' : 'latest'); pages++; } while(state.hasMoreNewer && !state.error && pages<20); },
  };
}
export function mountJobHistory(host, {request, onCommand, getJob}) {
  const node=(tag,text,cls)=>{const n=document.createElement(tag);if(text!=null)n.textContent=text;if(cls)n.className=cls;return n;};
  const title=node('h3','Conversation & activity'); title.id='jobHistoryHeading';
  const intro=node('p','Complete retained entries, loaded in pages. Notes are saved to this job; they do not steer an external agent.','jobs-runs-intro');
  const toolbar=node('div',null,'jobs-history-toolbar');
  const button=(label,fn)=>{const b=node('button',label,'ln-btn ln-btn--ghost');b.type='button';b.addEventListener('click',fn);toolbar.append(b);return b;};
  const older=button('Load older',()=>loader.load('older'));
  button('Oldest loaded',()=>viewport.scrollTo({top:0,behavior:'instant'}));
  button('Latest',async()=>{await loader.catchUp();viewport.scrollTo({top:viewport.scrollHeight,behavior:'instant'});});
  const search=node('input');search.type='search';search.className='ln-input';search.placeholder='Search loaded history';search.setAttribute('aria-label','Search loaded history');toolbar.append(search);
  const status=node('p','','jobs-history-status');status.setAttribute('role','status');status.setAttribute('aria-live','polite');
  const viewport=node('div',null,'jobs-history-viewport');viewport.tabIndex=0;viewport.setAttribute('role','region');viewport.setAttribute('aria-labelledby',title.id);
  const list=node('ol',null,'jobs-history-entries');viewport.append(list);
  const noMatches=node('p','No matches in loaded history. Load older entries to search more.','jobs-history-status');noMatches.hidden=true;viewport.append(noMatches);
  const boundary=node('p','','jobs-history-boundary');
  const form=node('form',null,'jobs-command-form');const label=node('label','Add a note to this job');label.htmlFor='jobCommandText';const input=node('textarea');input.id='jobCommandText';input.className='ln-textarea';input.rows=3;input.maxLength=2000;input.placeholder='Context, a question, or your next instruction. Saved as a note; no agent is launched.';
  const send=node('button','Save note','ln-btn ln-btn--primary');send.type='submit';const result=node('p','','jobs-history-status');result.setAttribute('role','status');form.append(label,input,send,result);
  host.append(title,intro,toolbar,status,viewport,boundary,form);
  let first=true,rendered='',selected='',pending=null,commandBusy=false,selectionEpoch=0;
  function render(s) {
    const nearBottom=viewport.scrollHeight-viewport.clientHeight-viewport.scrollTop<80;
    const anchor=[...list.children].find(n=>n.getBoundingClientRect().bottom>viewport.getBoundingClientRect().top);
    const top=anchor?.getBoundingClientRect().top;
    const sig=JSON.stringify(s.entries);
    if(sig!==rendered){
      rendered=sig;const old=new Map([...list.children].map(n=>[n.dataset.entryId,n]));let before=list.firstChild;
      const wanted=new Set(s.entries.map(e=>e.id));for(const child of [...list.children])if(!wanted.has(child.dataset.entryId))child.remove();before=list.firstChild;
      for(const entry of s.entries){
        let item=old.get(entry.id);
        if(!item){item=node('li',null,'jobs-history-entry');item.dataset.entryId=entry.id;item.dataset.search=historyText(entry).toLowerCase();const head=node('div',null,'jobs-history-entry-head');head.append(node('span',entry.role==='user'?'You':entry.role==='assistant'?'Assistant':entry.role==='tool'?'Tool':'System'),node('time',new Date(entry.createdAt).toLocaleString()));item.append(head,node('p',`${entry.kind}${entry.status ? ' · '+entry.status : ''}`,'jobs-history-kind'),node('pre',historyText(entry),'jobs-history-text'));}
        if(item!==before)list.insertBefore(item,before);before=item.nextSibling;
      }
      filter();
      if(first||nearBottom){viewport.scrollTop=viewport.scrollHeight;first=false;}else if(anchor?.isConnected&&top!=null)viewport.scrollTop+=anchor.getBoundingClientRect().top-top;
    }
    older.disabled=s.busy||!s.olderCursor;send.disabled=commandBusy||!getJob()?.id;
    status.textContent=s.error|| (s.busy?'Loading retained history…':`${s.entries.length} entries loaded${s.olderCursor?' · Older entries available':''}${s.hasMoreNewer?' · More updates available; choose Latest to continue':''}`);
    status.dataset.error=String(Boolean(s.error));boundary.textContent=s.boundary+(s.legacy?' Earlier retained run receipts are listed below; older conversations were not captured by this timeline.':'');
  }
  function filter(){const q=search.value.trim().toLowerCase();let visible=0;for(const n of list.children){n.hidden=Boolean(q&&!n.dataset.search.includes(q));if(!n.hidden)visible++;}noMatches.hidden=!q||visible>0;}
  const loader=createHistoryLoader(request,render);search.addEventListener('input',filter);
  form.addEventListener('submit',async e=>{
    e.preventDefault(); const job=getJob(), text=input.value.trim(), epoch=selectionEpoch;
    if(!job||!text||commandBusy)return;
    if(new TextEncoder().encode(text).length>2000){result.textContent='Shorten the note to 2,000 UTF-8 bytes.';return;}
    const fingerprint=JSON.stringify([job.id,text]);
    if(pending?.fingerprint!==fingerprint)pending={fingerprint,payload:{kind:'note',text,expectedVersion:job.version,requestId:crypto.randomUUID()}};
    const payload=pending.payload; commandBusy=true; send.disabled=true; input.disabled=true; result.textContent='Saving note.';
    try {
      const data=await request(`/api/v1/jobs/${encodeURIComponent(job.id)}/commands`,{method:'POST',json:payload});
      if(epoch!==selectionEpoch)return;
      if(data?.job?.id!==job.id||data?.command?.jobId!==job.id||data.command.kind!=='note'||data.command.status!=='recorded')throw new Error('The saved note receipt was not verified. Retry unchanged text to check this request.');
      input.value='';pending=null;result.textContent='Note recorded. No external execution instructions changed.';
      onCommand?.(data);await loader.catchUp();
    } catch(error) {
      if(epoch===selectionEpoch){if(error?.status===409){pending=null;result.textContent='This job changed. Refresh Jobs, then save the note again. Your text is kept.';}else result.textContent=error?.message||'The response was not confirmed. Retry unchanged text to reuse this request.';}
    } finally { if(epoch===selectionEpoch){commandBusy=false;send.disabled=false;input.disabled=false;} }
  });
  return {select(id){if(id===selected)return;selected=id;selectionEpoch++;commandBusy=false;input.disabled=false;first=true;rendered='';list.replaceChildren();input.value='';result.textContent='';pending=null;return loader.select(id);},refresh:()=>loader.catchUp(),state:loader.state};
}
