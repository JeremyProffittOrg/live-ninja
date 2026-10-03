// Ghost retained archive v1. String source positions are never treated as timestamps.
export function mergeGhostEvents(current, incoming) {
  const map=new Map(current.map(e=>[e.id,e]));
  for(const event of incoming || []) {
    if(!event || typeof event.id!=='string'||!event.id||typeof event.sequence!=='string'||!event.sequence||typeof event.text!=='string')throw new Error('Invalid retained event. Refresh to rescan.');
    const prior=map.get(event.id);
    if(prior && JSON.stringify(prior)!==JSON.stringify(event))throw new Error('A retained source entry changed. Refresh to rescan.');
    map.set(event.id,event);
  }
  return [...map.values()].sort((a,b)=>a.sequence<b.sequence?-1:a.sequence>b.sequence?1:a.id<b.id?-1:a.id>b.id?1:0);
}
export function createGhostHistory(request, changed=()=>{}, denied=()=>{}) {
  let epoch=0,scanSeen=new Set();
  const empty=()=>({node:'',session:'',events:[],next:'',resume:'',gaps:[],busy:false,error:'',scanning:false,reachedEnd:false});
  let state=empty();const emit=()=>changed({...state,events:[...state.events]});
  async function page(cursor,scan=false) {
    if(!state.node||!state.session||state.busy)return;
    const generation=epoch,node=state.node,session=state.session;state.busy=true;state.error='';emit();
    try {
      const query=new URLSearchParams({node_id:node,session_id:session});if(cursor)query.set('cursor',cursor);
      const result=await request('/api/v1/ghost-work/events?'+query);
      if(generation!==epoch)return;
      if(result.version!==1||result.node_id!==node||result.session_id!==session||result.coverage!=='retained_only'||!Array.isArray(result.events))throw new Error('The archive response does not match this node and provider session.');
      if(result.next_cursor && result.next_cursor===cursor)throw new Error('The archive cursor did not advance. Refresh to rescan.');
      if(scan){
        const validated=mergeGhostEvents([],result.events);
        for(const event of validated){if(scanSeen.has(event.id))mergeGhostEvents(state.events,[event]);scanSeen.add(event.id);}
        const merged=new Map(state.events.map(e=>[e.id,e]));for(const event of validated)merged.set(event.id,event);
        state.events=mergeGhostEvents([],[...merged.values()]);
      }else state.events=mergeGhostEvents(state.events,result.events);
      state.next=result.next_cursor||'';state.resume=result.resume_cursor||state.resume;state.reachedEnd=!state.next;
      state.scanning=scan&&Boolean(state.next);
      if(scan&&!state.next)state.events=state.events.filter(e=>scanSeen.has(e.id));
      state.gaps=[...new Set([...state.gaps,...(result.gaps||[]).map(g=>typeof g==='string'?g:JSON.stringify(g))])];
    } catch(error) {
      if(generation!==epoch)return;
      if([401,403].includes(error?.status)||error?.name==='AuthLostError'){epoch++;state=empty();state.error='Archive access is no longer available. Content cleared.';denied();}
      else {state.error=error?.status===409?'The archived source changed. Refresh to rescan from the beginning.':error?.status===410?'Some archived content expired or is missing. Refresh to rescan retained content.':error?.status===503?'Ghost history is temporarily unavailable. Retry after the integration is ready.':error?.message||'History could not be read. This is not a complete empty conversation.';state.reachedEnd=false;}
    } finally {if(generation===epoch){state.busy=false;emit();}else emit();}
  }
  return {
    state:()=>({...state,events:[...state.events]}),
    select(node,session){epoch++;scanSeen=new Set();state={...empty(),node,session,scanning:true};emit();return page('',true);},
    clear(){epoch++;state=empty();emit();},
    async more(){return page(state.next||state.resume,state.scanning);},
    async latest(){for(let count=0;count<20;count++){if(state.busy||state.error||(!state.next&&count>0))break;await page(state.next||state.resume,state.scanning);}},
    async rescan(){if(state.busy)return;scanSeen=new Set();state.next='';state.resume='';state.gaps=[];state.scanning=true;state.reachedEnd=false;await page('',true);},
    async poll(){if(!state.busy&&!state.error&&!state.next&&state.resume)await page(state.resume);},
  };
}
export function mountGhostWork(host,{request}) {
  const el=(tag,text,cls)=>{const n=document.createElement(tag);if(text!=null)n.textContent=text;if(cls)n.className=cls;return n;};
  const heading=el('h2','Ghost work & retained conversations');
  const boundary=el('p','Owner access only. Select an actual node and provider session. The archive retains up to 90 days; capture may be incomplete, and missing history cannot be reconstructed. Scheduled jobs do not currently expose a reliable provider-session binding.','jobs-runs-intro');
  const status=el('p','','jobs-history-status');status.setAttribute('role','status');
  const bar=el('div',null,'jobs-history-toolbar');const refresh=el('button','Refresh Ghost work','ln-btn ln-btn--ghost');refresh.type='button';bar.append(refresh);
  const nodeLabel=el('label','Node');const nodes=el('select',null,'ln-select');nodes.id='ghostNode';nodeLabel.htmlFor=nodes.id;
  const sessionLabel=el('label','Provider session');const sessions=el('select',null,'ln-select');sessions.id='ghostSession';sessionLabel.htmlFor=sessions.id;
  const moreSessions=el('button','Load more sessions','ln-btn ln-btn--ghost');moreSessions.type='button';moreSessions.hidden=true;
  const controls=el('div',null,'jobs-history-toolbar');controls.append(nodeLabel,nodes,sessionLabel,sessions,moreSessions);
  const jobs=el('div',null,'ghost-jobs');const details=el('details');details.append(el('summary','Authorized scheduled jobs'),jobs);
  const historyHost=el('section',null,'jobs-conversation');const historyTitle=el('h3','Retained conversation');historyTitle.id='ghostHistoryTitle';
  const historyStatus=el('p','','jobs-history-status');historyStatus.setAttribute('role','status');
  const toolbar=el('div',null,'jobs-history-toolbar');
  const button=(label,fn)=>{const b=el('button',label,'ln-btn ln-btn--ghost');b.type='button';b.onclick=fn;toolbar.append(b);return b;};
  const more=button('Load next page',()=>loader.more());
  button('Oldest loaded',()=>viewport.scrollTo({top:0,behavior:'instant'}));
  button('Latest retained',async()=>{await loader.latest();viewport.scrollTo({top:viewport.scrollHeight,behavior:'instant'});});
  button('Rescan history',()=>loader.rescan());
  const viewport=el('div',null,'jobs-history-viewport');viewport.tabIndex=0;viewport.setAttribute('role','region');viewport.setAttribute('aria-labelledby',historyTitle.id);
  const list=el('ol',null,'jobs-history-entries');viewport.append(list);const gaps=el('p','','jobs-history-boundary');
  historyHost.append(historyTitle,toolbar,historyStatus,viewport,gaps);historyHost.hidden=true;
  host.append(heading,boundary,bar,status,details,controls,historyHost);
  let active=false,loaded=false,generation=0,sessionCursor='',sessionBusy=false,sessionIDs=new Set(),rendered='';
  const option=(select,value,label)=>{const o=el('option',label);o.value=value;select.append(o);};
  function emptySelect(select,label){select.replaceChildren();option(select,'',label);}
  function clearPrivate(){generation++;nodes.replaceChildren();sessions.replaceChildren();jobs.replaceChildren();sessionIDs.clear();sessionCursor='';loaded=false;}
  const loader=createGhostHistory(request,state=>{
    historyHost.hidden=!state.session&&!state.error;
    const atBottom=viewport.scrollHeight-viewport.clientHeight-viewport.scrollTop<70;
    const anchor=[...list.children].find(n=>n.getBoundingClientRect().bottom>viewport.getBoundingClientRect().top),top=anchor?.getBoundingClientRect().top;
    const signature=JSON.stringify(state.events);
    if(signature!==rendered){rendered=signature;const old=new Map([...list.children].map(n=>[n.dataset.eventId,n]));let before=list.firstChild;const wanted=new Set(state.events.map(e=>e.id));for(const n of [...list.children])if(!wanted.has(n.dataset.eventId))n.remove();before=list.firstChild;
      for(const event of state.events){let row=old.get(event.id);if(!row){row=el('li',null,'jobs-history-entry');row.dataset.eventId=event.id;}if(JSON.stringify(row._event)!==JSON.stringify(event)){row.replaceChildren();row._event=event;const head=el('div',null,'jobs-history-entry-head');head.append(el('strong',event.kind),el('span',event.timestamp?new Date(event.timestamp).toLocaleString():'Source time unavailable'));row.append(head,el('p',[event.name,event.call_id,Number.isInteger(event.part)?`Fragment ${event.part+1}${event.more?' (continues)':''}`:''].filter(Boolean).join(' · '),'jobs-history-kind'),el('pre',event.text,'jobs-history-text'));}if(row!==before)list.insertBefore(row,before);before=row.nextSibling;}
      if(atBottom)viewport.scrollTop=viewport.scrollHeight;else if(anchor?.isConnected&&top!=null)viewport.scrollTop+=anchor.getBoundingClientRect().top-top;
    }
    more.disabled=state.busy||!state.next;more.textContent=state.scanning?'Continue rescan':'Load next page';
    historyStatus.textContent=state.error||(state.busy?'Reading retained source pages.':`${state.events.length} fragments loaded. ${state.next?'More pages remain.':state.reachedEnd?'Current end of retained archive reached; capture completeness is unknown.':'Choose a provider session.'}`);
    gaps.textContent=state.gaps.length?'Archive gaps: '+state.gaps.join('; '):'Retained text only. Images, attachments and hidden reasoning are not represented. Older than 90 days may have expired.';
  },clearPrivate);
  function authError(error){if([401,403].includes(error?.status)||error?.name==='AuthLostError'){clearPrivate();loader.clear();return true;}return false;}
  async function loadSessions(append=false){
    if(sessionBusy||!nodes.value)return;sessionBusy=true;const seq=generation,node=nodes.value;moreSessions.disabled=true;
    try{const q=new URLSearchParams({node_id:node});if(append&&sessionCursor)q.set('cursor',sessionCursor);const data=await request('/api/v1/ghost-work/sessions?'+q);if(seq!==generation||node!==nodes.value)return;if(data.version!==1||data.node_id!==node||data.coverage!=='retained_only'||!Array.isArray(data.sessions))throw new Error('Unexpected session archive response.');
      const oldSession=sessions.value;
      if(!append){emptySelect(sessions,'Choose an actual provider session');sessionIDs.clear();}
      for(const s of data.sessions)if(!sessionIDs.has(s.session_id)){sessionIDs.add(s.session_id);const label=`${s.session_id} · uploaded ${s.date||'date unavailable'}`;const existing=[...sessions.options].find(o=>o.value===s.session_id);if(existing)existing.textContent=label;else option(sessions,s.session_id,label);}
      if(!append&&oldSession){if(!sessionIDs.has(oldSession))option(sessions,oldSession,oldSession+' · previously selected');sessions.value=oldSession;}
      sessionCursor=data.next_cursor||'';moreSessions.hidden=!sessionCursor;status.textContent=`${sessionIDs.size} provider sessions loaded.${sessionCursor?' More sessions remain.':' Retained sessions only; an empty archive does not prove there was no conversation.'}`;
    }catch(error){if(seq===generation){authError(error);status.textContent=error?.message||'Retained sessions are unavailable.';}}finally{if(seq===generation){sessionBusy=false;moreSessions.disabled=false;}}
  }
  async function load(){
    const seq=++generation;sessionBusy=false;refresh.disabled=true;status.textContent='Reading authorized Ghost work.';
    try{const results=await Promise.all([request('/api/v1/ghost-work/nodes'),request('/api/v1/ghost-work/jobs')]);if(seq!==generation)return;
      const oldNode=nodes.value;emptySelect(nodes,'Choose a node');for(const n of results[0].nodes||[])option(nodes,n.node_id,`${n.node_id} · ${n.state||n.status||'status unavailable'}`);if([...nodes.options].some(o=>o.value===oldNode))nodes.value=oldNode;else if(oldNode){loader.clear();emptySelect(sessions,'Choose a provider session');sessionIDs.clear();sessionCursor='';}
      jobs.replaceChildren();const events=results[1].events||[];jobs.append(el('p',`${events.length} authorized scheduled jobs. Up to ${results[1].runHistoryLimit||10} recent run receipts per job; these are not conversation transcripts.`,'jobs-runs-intro'));
      for(const event of events){const row=el('details',null,'jobs-run');row.append(el('summary',`${event.repo||event.event_id} · ${event.node} · ${event.last_run_status||'No recorded run'}`),el('pre',JSON.stringify(event,null,2),'jobs-history-text'));jobs.append(row);}
      loaded=true;status.textContent='Choose a node, then select the actual provider session.';if(nodes.value)await loadSessions(false);
    }catch(error){if(seq===generation){authError(error);status.textContent='Ghost work is unavailable for this account or the integration is not ready. '+(error?.message||'');}}finally{refresh.disabled=false;}
  }
  nodes.onchange=()=>{generation++;sessionBusy=false;loader.clear();emptySelect(sessions,'Choose a provider session');sessionCursor='';sessionIDs.clear();void loadSessions();};
  sessions.onchange=()=>{rendered='';loader.select(nodes.value,sessions.value);};moreSessions.onclick=()=>loadSessions(true);
  refresh.onclick=async()=>{await load();if(loader.state().session)await loader.rescan();};
  document.addEventListener('visibilitychange',()=>{if(active&&!document.hidden&&loader.state().session)void loader.rescan();});
  window.addEventListener('online',()=>{if(active&&loader.state().session)void loader.rescan();});
  setInterval(()=>{if(active&&!document.hidden)void loader.poll();},20000);
  return {show(){active=true;if(!loaded)void load();else if(loader.state().session)void loader.rescan();},hide(){active=false;}};
}
