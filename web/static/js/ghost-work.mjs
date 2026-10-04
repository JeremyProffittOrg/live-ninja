// Ghost retained archive v1. String source positions are never treated as timestamps.
// Fragments become one logical message only when they are provably adjacent parts
// of the same entry. Absent source times stay unknown and are never inferred.
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

export const GHOST_LATEST_PAGE_LIMIT=20;
// Offscreen rendering is skipped (content-visibility:auto) only for logs longer
// than this. Short logs always render fully so their real height is known when
// jumping to the end and every rendered surface (including captures) shows text.
export const GHOST_RENDER_SKIP_MIN=200;
const validTime=value=>{if(typeof value!=='string'||!value)return null;const time=Date.parse(value);return Number.isFinite(time)?time:null;};
const tidy=text=>text.replace(/[  ]/g,' ');
const ROLES={user:['User message','U'],assistant:['Assistant message','A'],tool_call:['Tool call','⚙'],tool_result:['Tool result','↳'],command:['Command','$'],event:['Event','i']};

export function ghostRoleLabel(kind,name=''){
  const base=ROLES[kind]?.[0]||`Entry (${String(kind||'unknown kind')})`;
  return (kind==='tool_call'||kind==='tool_result')&&typeof name==='string'&&name?`${base}: ${name}`:base;
}
export function formatGhostTime(value,{timeZone}={}){
  const time=validTime(value);if(time==null)return 'Source time unknown';
  return tidy(new Intl.DateTimeFormat('en-US',{timeZone,month:'short',day:'numeric',year:'numeric',hour:'numeric',minute:'2-digit',hour12:true,timeZoneName:'short'}).format(time));
}
export function formatDuration(ms){
  if(!Number.isFinite(ms)||ms<0)return '';
  const total=Math.floor(ms/60000),minutes=n=>`${n} minute${n===1?'':'s'}`;
  if(total<1)return 'under 1 minute';
  if(total<60)return minutes(total);
  const hours=Math.floor(total/60);
  return `${hours} hour${hours===1?'':'s'} ${minutes(total%60)}`;
}
export function formatGhostRange(first,last,{timeZone}={}){
  const make=options=>new Intl.DateTimeFormat('en-US',{timeZone,...options});
  const date=make({month:'short',day:'numeric',year:'numeric'}),clock=make({hour:'numeric',minute:'2-digit',hour12:true}),zoneParts=make({timeZoneName:'short'});
  const zone=time=>zoneParts.formatToParts(time).find(part=>part.type==='timeZoneName')?.value||'';
  const firstDate=date.format(first),lastDate=date.format(last),firstZone=zone(first),lastZone=zone(last);
  if(first===last)return tidy(`${firstDate}, ${clock.format(first)} ${firstZone}`.trim());
  const start=`${firstDate}, ${clock.format(first)}${firstZone!==lastZone?` ${firstZone}`:''}`;
  const end=firstDate===lastDate?clock.format(last):`${lastDate}, ${clock.format(last)}`;
  return tidy(`${start} – ${end} ${lastZone}`.trim());
}
// Pinned provider format (ghost-cli 672c440 lambda/command/history.go):
// id === sequence === `${key}:${line:%09d}:${block:%05d}:${part:%05d}`.
// Only the trailing part suffix is read as a number; the sequence stays opaque.
const SOURCE_POSITION=/^(.+):(\d{9,}):(\d{5,}):(\d{5,})$/;
export function ghostSourceIdentity(event){
  if(!event||typeof event.id!=='string'||event.id!==event.sequence)return null;
  const match=SOURCE_POSITION.exec(event.sequence);if(!match)return null;
  const part=Number(match[4]);
  if(!Number.isSafeInteger(part)||part!==event.part)return null;
  return {entry:`${match[1]}:${match[2]}:${match[3]}`,part};
}
// Join only provably adjacent fragments of the same source entry; never join by heuristics.
export function groupGhostMessages(events,{morePagesRemain=false}={}){
  const messages=[];let open=null;
  for(const event of events||[]){
    const identity=ghostSourceIdentity(event);
    const part=Number.isInteger(event.part)&&event.part>=0?event.part:0;
    const name=typeof event.name==='string'?event.name:'',callID=typeof event.call_id==='string'?event.call_id:'';
    if(open&&open.more&&identity&&open.entry&&identity.entry===open.entry&&identity.part===open.lastPart+1&&event.kind===open.kind&&callID===open.callID&&name===open.name){
      open.text+=event.text;open.ids.push(event.id);open.lastPart=part;open.more=event.more===true;continue;
    }
    if(open?.more)open.continuation='missing';
    open={id:event.id,ids:[event.id],kind:event.kind,name,callID,entry:identity?.entry||'',text:event.text,timestamp:validTime(event.timestamp)==null?'':event.timestamp,lastPart:part,more:event.more===true,leadingMissing:part>0,continuation:'',unattributed:!identity&&(part>0||event.more===true)};
    messages.push(open);
  }
  if(open?.more)open.continuation=morePagesRemain?'pending':'missing';
  return messages.map(({lastPart,callID,more,entry,...message})=>message);
}
export function ghostObservedSpan(messages){
  let first=null,last=null,timed=0;
  for(const message of messages||[]){const time=validTime(message.timestamp);if(time==null)continue;timed++;if(first==null||time<first)first=time;if(last==null||time>last)last=time;}
  return {first,last,timed,untimed:(messages||[]).length-timed};
}
// An observed span between retained times; never a claim that the run completed.
export function ghostLogSummary(messages,{morePagesRemain=false,reachedEnd=false,timeZone}={}){
  const span=ghostObservedSpan(messages),count=messages.length,parts=[];
  const uncertain=messages.filter(m=>m.unattributed).length;
  if(span.timed>=2)parts.push(`observed ${formatDuration(span.last-span.first)}`);
  else if(span.timed===1)parts.push('duration unknown');
  if(span.timed)parts.push(formatGhostRange(span.first,span.last,{timeZone}));
  else if(count)parts.push('start and stop times unknown');
  parts.push(`${count} message${count===1?'':'s'}${morePagesRemain?' loaded so far':reachedEnd?'':' loaded'}`);
  if(uncertain)parts.push(`message count uncertain (${uncertain} unattributed fragment${uncertain===1?'':'s'})`);
  if(span.timed&&span.untimed)parts.push(`${span.untimed} without a source time`);
  return {text:parts.join(' · '),span,uncertain};
}
export function describeGhostSession(messages,limit=80){
  const list=messages||[];
  const pick=list.find(m=>m.kind==='user'&&m.text.trim())||list.find(m=>m.kind==='assistant'&&m.text.trim());
  if(!pick)return '';
  const chars=[...pick.text.replace(/\s+/g,' ').trim()];
  return chars.length>limit?chars.slice(0,limit-1).join('')+'…':chars.join('');
}
// Only an explicit authoritative binding may associate a scheduled job with a session.
export function scheduledForSession(events,session,bindingAvailable){
  if(bindingAvailable!==true||typeof session!=='string'||!session)return {authoritative:false,events:[]};
  return {authoritative:true,events:(events||[]).filter(event=>typeof event?.provider_session_id==='string'&&event.provider_session_id===session)};
}
export function scheduledRunLabel(event,{timeZone}={}){
  const status=typeof event?.last_run_status==='string'?event.last_run_status.trim():'';
  if(!status)return 'No recorded run';
  const ts=event.last_run_ts;
  const when=validTime(ts)!=null?formatGhostTime(ts,{timeZone}):typeof ts==='string'&&ts?ts:'time unknown';
  return `Last run ${status} · ${when}`;
}
function readFailure(error){
  switch(error?.status){
    case 409:return 'The archived source changed. Refresh to rescan from the beginning.';
    case 410:return 'Some archived content expired or is missing. Refresh to rescan retained content.';
    case 413:return error?.code==='request_too_large'?'This archive page request was larger than the server accepts. Loaded content is kept; reload the page, then retry.':'A retained object is larger than Ghost supports, so this history is incomplete.';
    case 422:return 'A retained object could not be decoded, so this history is incomplete.';
    case 429:return 'Ghost is rate limited. Wait, then refresh this page.';
    case 431:return 'The server rejected this request as too large (HTTP 431). Loaded content is kept and this is not a complete history. Reload the page, then refresh.';
    case 503:return 'Ghost history is temporarily unavailable. Retry after the integration is ready.';
    default:return error?.message||'History could not be read. This is not a complete empty conversation.';
  }
}
// Archive page reads send the opaque cursor in a bounded JSON body (POST), never
// in the request line. They are reads only; the server treats them as such.
export function createGhostHistory(request, changed=()=>{}, denied=()=>{}) {
  let epoch=0,scanSeen=new Set();
  const empty=()=>({node:'',session:'',events:[],next:'',resume:'',gaps:[],busy:false,error:'',scanning:false,reachedEnd:false});
  let state=empty();const emit=()=>changed({...state,events:[...state.events]});
  async function page(cursor,scan=false) {
    if(!state.node||!state.session||state.busy)return;
    const generation=epoch,node=state.node,session=state.session;state.busy=true;state.error='';emit();
    try {
      const body={node_id:node,session_id:session};if(cursor)body.cursor=cursor;
      const result=await request('/api/v1/ghost-work/events',{method:'POST',json:body});
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
      else {state.error=readFailure(error);state.reachedEnd=false;}
    } finally {if(generation===epoch){state.busy=false;emit();}else emit();}
  }
  return {
    state:()=>({...state,events:[...state.events]}),
    select(node,session){epoch++;scanSeen=new Set();state={...empty(),node,session,scanning:true};emit();return page('',true);},
    clear(){epoch++;state=empty();emit();},
    async more(){return page(state.next||state.resume,state.scanning);},
    async latest(){for(let count=0;count<GHOST_LATEST_PAGE_LIMIT;count++){if(state.busy||state.error||(!state.next&&count>0))break;await page(state.next||state.resume,state.scanning);}},
    async rescan(){if(state.busy)return;scanSeen=new Set();state.next='';state.resume='';state.gaps=[];state.scanning=true;state.reachedEnd=false;await page('',true);},
    // Incremental refresh: continue the next page, else read after the current end.
    async refresh(){if(state.busy||!state.node||!state.session)return;if(state.next)return page(state.next,state.scanning);if(state.resume)return page(state.resume,false);return page('',true);},
    async poll(){if(!state.busy&&!state.error&&!state.next&&state.resume)await page(state.resume);},
  };
}

export function mountGhostWork(host,{request,timeZone}={}) {
  const el=(tag,text,cls)=>{const node=document.createElement(tag);if(text!=null)node.textContent=text;if(cls)node.className=cls;return node;};
  const glyph=text=>{const node=el('span',text,'ghost-glyph');node.setAttribute('aria-hidden','true');return node;};
  const setDisabled=(node,value)=>node.setAttribute('aria-disabled',String(Boolean(value)));
  const isDisabled=node=>node.getAttribute('aria-disabled')==='true';
  const heading=el('h2','Ghost work','jobs-sr-only');heading.id='ghostWorkHeading';host.setAttribute('aria-labelledby',heading.id);
  const status=el('p','','jobs-history-status ghost-status');status.id='ghostStatus';status.setAttribute('role','status');
  const nodeField=el('div',null,'ghost-field ghost-field--node');
  const nodeLabel=el('label','Node','ghost-label');const nodes=el('select',null,'ln-select ghost-node');nodes.id='ghostNode';nodeLabel.htmlFor=nodes.id;nodeField.append(nodeLabel,nodes);
  const sessionBox=el('div',null,'ghost-field ghost-session-box');
  const sessionLabel=el('span','Session','ghost-label');sessionLabel.id='ghostSessionLabel';
  const comboWrap=el('div',null,'ghost-combo');
  const combo=el('div',null,'ghost-combo-button');combo.id='ghostSession';combo.tabIndex=0;
  for(const [k,v] of Object.entries({role:'combobox','aria-labelledby':sessionLabel.id,'aria-haspopup':'listbox','aria-expanded':'false','aria-controls':'ghostSessionListbox'}))combo.setAttribute(k,v);
  const listbox=el('ul',null,'ghost-listbox');listbox.id='ghostSessionListbox';listbox.setAttribute('role','listbox');listbox.setAttribute('aria-labelledby',sessionLabel.id);listbox.hidden=true;
  comboWrap.append(combo,listbox);
  const moreSessions=el('button','Load more sessions','ln-btn ln-btn--ghost ghost-more');moreSessions.type='button';moreSessions.hidden=true;
  const foot=el('div',null,'ghost-session-foot');
  const check=el('label',null,'ghost-check');const scheduledOnly=el('input');scheduledOnly.type='checkbox';scheduledOnly.id='ghostScheduledOnly';check.append(scheduledOnly,el('span','Show Authorized Scheduled Jobs Only'));
  const refresh=el('button',null,'ln-btn ln-btn--ghost ghost-refresh');refresh.type='button';refresh.id='ghostRefresh';refresh.append(glyph('↻'),el('span','Refresh Ghost work'));
  foot.append(check,refresh);sessionBox.append(sessionLabel,comboWrap,moreSessions,foot);
  const selectors=el('div',null,'ghost-selectors');selectors.append(nodeField,sessionBox);
  const scheduledSection=el('section',null,'ghost-scheduled');const scheduledTitle=el('h3','Scheduled jobs','ghost-scheduled-title');scheduledTitle.id='ghostScheduledTitle';scheduledSection.setAttribute('aria-labelledby',scheduledTitle.id);
  const scheduledNotice=el('p','','jobs-history-status');scheduledNotice.id='ghostScheduledNotice';scheduledNotice.setAttribute('aria-live','polite');
  const jobs=el('ul',null,'ghost-jobs');jobs.setAttribute('aria-labelledby',scheduledTitle.id);
  scheduledSection.append(scheduledTitle,scheduledNotice,jobs);scheduledSection.hidden=true;
  const historyHost=el('section',null,'jobs-conversation ghost-conversation');historyHost.hidden=true;
  const logHead=el('div',null,'ghost-log-head');
  const historyTitle=el('h3',null,'ghost-log-title');historyTitle.id='ghostHistoryTitle';
  // The visible title is two block lines (name, then summary). Block boundaries add
  // whitespace in accessible-name computation, so the screen-reader separator alone
  // would yield "Retained conversation , ...". The heading therefore carries an
  // explicit name that mirrors its text exactly; the separator keeps linear reading.
  const HISTORY_NAME='Retained conversation';
  const titleSep=el('span','','jobs-sr-only');const titleMeta=el('span','','ghost-log-meta');
  historyTitle.append(el('span',HISTORY_NAME,'ghost-log-name'),titleSep,titleMeta);historyTitle.setAttribute('aria-label',HISTORY_NAME);historyHost.setAttribute('aria-labelledby',historyTitle.id);
  const tools=el('div',null,'ghost-log-tools');tools.setAttribute('role','group');tools.setAttribute('aria-label','Retained conversation controls');
  const icon=(label,symbol,tip,action)=>{const b=el('button',null,'ghost-icon-btn');b.type='button';b.setAttribute('aria-label',label);b.dataset.tip=tip;b.append(glyph(symbol));b.addEventListener('click',()=>{if(!isDisabled(b))void action();});tools.append(b);return b;};
  let capped=false;
  const refreshLog=icon('Refresh retained conversation','↻','Refresh: read entries retained after the last loaded page',()=>{capped=false;return loader.refresh();});
  const oldest=icon('Oldest loaded','⇡','Oldest loaded: jump to the first entry loaded in this view',()=>{unpin();viewport.scrollTo({top:0,behavior:'instant'});});
  const latest=icon('Latest retained','⇣','Latest retained: read up to 20 more pages, then jump to the newest entry',async()=>{capped=false;await loader.latest();const s=loader.state();capped=Boolean(s.next)&&!s.error;render(s);pinToEnd();});
  const rescan=icon('Rescan history','⟲','Rescan history: read the retained archive again from the beginning',()=>{capped=false;return loader.rescan();});
  logHead.append(historyTitle,tools);
  const historyStatus=el('p','','jobs-history-status');historyStatus.setAttribute('role','status');
  const viewport=el('div',null,'jobs-history-viewport ghost-viewport');viewport.tabIndex=0;viewport.setAttribute('role','region');viewport.setAttribute('aria-labelledby',historyTitle.id);
  const list=el('ol',null,'ghost-log');list.setAttribute('aria-label','Retained messages');list.dataset.renderSkip='false';viewport.append(list);
  const gaps=el('p','','jobs-history-boundary');
  historyHost.append(logHead,historyStatus,viewport,gaps);
  const tooltip=el('div','','ghost-tooltip');tooltip.id='ghostTooltip';tooltip.setAttribute('role','tooltip');tooltip.hidden=true;
  host.append(heading,selectors,status,scheduledSection,historyHost,tooltip);

  // Latest pins the scroller to the end. Rows may change height after layout
  // (fonts, wrapping, or skipped offscreen rows rendering when they come into view),
  // so a ResizeObserver re-applies the end position until the reader scrolls up.
  let pinned=false,stuckTop=0;
  function stick(){viewport.scrollTop=viewport.scrollHeight;stuckTop=viewport.scrollTop;}
  function pinToEnd(){pinned=true;stick();}
  function unpin(){pinned=false;}
  if(typeof ResizeObserver==='function')new ResizeObserver(()=>{if(pinned&&!historyHost.hidden)stick();}).observe(list);
  // Any scroll that moves up away from the pinned end (wheel, touch, keys,
  // scrollbar, scrollIntoView) releases the pin; growth below never does.
  viewport.addEventListener('scroll',()=>{if(pinned&&viewport.scrollTop<stuckTop-1&&viewport.scrollHeight-viewport.clientHeight-viewport.scrollTop>2)unpin();},{passive:true});

  let active=false,loaded=false,generation=0,sessionCursor='',sessionBusy=false,rendered='',activeIndex=-1,selectedSession='',scheduled=null,tipTarget=null,tipTimer=0,sessionList=[];
  // Derived descriptions/time bounds are keyed by exact node+session so the same
  // provider GUID on another authorized node can never inherit them.
  const sessionIDs=new Set(),sessionMeta=new Map(),metaKey=(node,session)=>`${node}\n${session}`;
  const loader=createGhostHistory(request,render,clearPrivate);
  const option=(select,value,label)=>{const o=el('option',label);o.value=value;select.append(o);};
  function emptySelect(select,label){select.replaceChildren();option(select,'',label);}

  function showTip(target){
    clearTimeout(tipTimer);if(tipTarget&&tipTarget!==target)tipTarget.removeAttribute('aria-describedby');
    tipTarget=target;tooltip.textContent=target.dataset.tip||'';tooltip.hidden=false;target.setAttribute('aria-describedby',tooltip.id);
    const r=target.getBoundingClientRect(),w=tooltip.offsetWidth,h=tooltip.offsetHeight,vw=document.documentElement.clientWidth,vh=document.documentElement.clientHeight;
    const left=Math.min(Math.max(8,r.left+r.width/2-w/2),Math.max(8,vw-w-8));let top=r.bottom+6;if(top+h>vh-8)top=Math.max(8,r.top-h-6);
    tooltip.style.left=left+'px';tooltip.style.top=top+'px';
  }
  function hideTip(target){if(target&&target!==tipTarget)return;clearTimeout(tipTimer);tooltip.hidden=true;tooltip.textContent='';tipTarget?.removeAttribute('aria-describedby');tipTarget=null;}
  const tipFor=event=>{const t=event.target?.closest?.('[data-tip]');return t&&host.contains(t)?t:null;};
  host.addEventListener('pointerover',event=>{const t=tipFor(event);if(t&&event.pointerType!=='touch')showTip(t);});
  host.addEventListener('pointerout',event=>{const t=tipFor(event);if(t&&!t.contains(event.relatedTarget)&&document.activeElement!==t)hideTip(t);});
  host.addEventListener('focusin',event=>{const t=tipFor(event);if(t)showTip(t);});
  host.addEventListener('focusout',event=>{const t=tipFor(event);if(t)hideTip(t);});
  host.addEventListener('pointerdown',event=>{if(event.pointerType==='mouse')return;const t=tipFor(event);if(t){showTip(t);tipTimer=setTimeout(()=>hideTip(t),4000);}});
  document.addEventListener('keydown',event=>{if(event.key==='Escape'&&!tooltip.hidden&&listbox.hidden)hideTip();});
  viewport.addEventListener('scroll',()=>{if(tipTarget&&viewport.contains(tipTarget))hideTip();},{passive:true});

  function sessionLines(entry){
    const meta=sessionMeta.get(metaKey(nodes.value,entry.id)),when=meta?.range?`Observed ${meta.range}`:'Start and stop unknown';
    return [el('span',meta?.description||'Description unknown','ghost-opt-line ghost-opt-desc'),el('span',entry.id,'ghost-opt-line ghost-opt-id'),el('span',`${when} · uploaded ${entry.date||'date unavailable'}${entry.previous?' · previously selected':''}`,'ghost-opt-line ghost-opt-time')];
  }
  function renderCombo(){
    const entry=sessionList.find(s=>s.id===selectedSession);
    combo.replaceChildren(...(entry?sessionLines(entry):[el('span',!nodes.value?'Choose a node first':sessionBusy&&!sessionList.length?'Loading sessions…':sessionList.length?'Choose a session':'No retained sessions loaded','ghost-combo-placeholder')]));
    setDisabled(combo,!sessionList.length);
  }
  function setActive(index){
    const options=[...listbox.children];if(!options.length)return;
    activeIndex=Math.min(Math.max(index,0),options.length-1);
    options.forEach((o,i)=>o.classList.toggle('is-active',i===activeIndex));
    combo.setAttribute('aria-activedescendant',options[activeIndex].id);options[activeIndex].scrollIntoView?.({block:'nearest'});
  }
  function openList(){if(isDisabled(combo)||!sessionList.length)return;hideTip();listbox.hidden=false;combo.setAttribute('aria-expanded','true');setActive(Math.max(0,sessionList.findIndex(s=>s.id===selectedSession)));}
  function closeList(){listbox.hidden=true;combo.setAttribute('aria-expanded','false');combo.removeAttribute('aria-activedescendant');activeIndex=-1;}
  function renderSessionOptions(){
    const wasOpen=!listbox.hidden,activeID=sessionList[activeIndex]?.id;listbox.replaceChildren();
    sessionList.forEach((entry,index)=>{const li=el('li',null,'ghost-option');li.id=`ghostSessionOption-${index}`;li.dataset.value=entry.id;li.setAttribute('role','option');li.setAttribute('aria-selected',String(entry.id===selectedSession));li.append(...sessionLines(entry));li.addEventListener('click',()=>choose(index));listbox.append(li);});
    renderCombo();
    if(wasOpen){if(!sessionList.length)closeList();else setActive(Math.max(0,sessionList.findIndex(s=>s.id===activeID)));}
  }
  function choose(index){
    const entry=sessionList[index];closeList();combo.focus({preventScroll:true});
    if(!entry||entry.id===selectedSession)return;
    selectedSession=entry.id;rendered='';capped=false;unpin();renderSessionOptions();renderScheduled();void loader.select(nodes.value,entry.id);
  }
  combo.addEventListener('click',()=>{if(listbox.hidden)openList();else closeList();});
  combo.addEventListener('blur',()=>{if(!listbox.hidden)closeList();});
  combo.addEventListener('keydown',event=>{
    if(isDisabled(combo))return;const open=!listbox.hidden;
    switch(event.key){
      case 'ArrowDown':event.preventDefault();if(open)setActive(activeIndex+1);else openList();break;
      case 'ArrowUp':event.preventDefault();if(open)setActive(activeIndex-1);else openList();break;
      case 'Home':if(open){event.preventDefault();setActive(0);}break;
      case 'End':if(open){event.preventDefault();setActive(sessionList.length-1);}break;
      case 'Enter':case ' ':event.preventDefault();if(open&&activeIndex>=0)choose(activeIndex);else if(open)closeList();else openList();break;
      case 'Escape':if(open){event.preventDefault();event.stopPropagation();closeList();}break;
      case 'Tab':if(open)closeList();break;
    }
  });
  listbox.addEventListener('mousedown',event=>event.preventDefault());
  document.addEventListener('pointerdown',event=>{if(!listbox.hidden&&!comboWrap.contains(event.target))closeList();});

  function renderScheduled(){
    jobs.replaceChildren();
    if(!scheduled){scheduledNotice.textContent='';scheduledSection.hidden=true;return;}
    scheduledSection.hidden=false;let visible=scheduled.events;
    if(scheduledOnly.checked){
      if(!selectedSession){visible=[];scheduledNotice.textContent='Choose a session to show the authorized scheduled jobs bound to it.';}
      else{const result=scheduledForSession(scheduled.events,selectedSession,scheduled.bindingAvailable);visible=result.events;
        scheduledNotice.textContent=result.authoritative?`${visible.length} authorized scheduled job${visible.length===1?'':'s'} bound to this session by Ghost.`:'Ghost does not expose a reliable provider-session binding for scheduled jobs, so none can be shown as belonging to this session. Nothing is guessed from node, time or prompt text. Clear the checkbox to see every authorized scheduled job.';}
    }else scheduledNotice.textContent=`${visible.length} authorized scheduled job${visible.length===1?'':'s'}, not filtered by session. Up to ${scheduled.limit} recent run receipts per job; these are not conversation transcripts.`;
    for(const event of visible){
      const item=el('li',null,'ghost-job'),details=el('details'),summary=el('summary',null,'ghost-job-summary');
      summary.append(el('span',`${event.repo||event.event_id||'Unnamed scheduled job'} · ${event.node||'node unknown'}`,'ghost-job-name'),el('span',scheduledRunLabel(event,{timeZone}),'ghost-job-status'));
      details.append(summary,el('pre',JSON.stringify(event,null,2),'jobs-history-text ghost-job-detail'));item.append(details);jobs.append(item);
    }
  }
  scheduledOnly.addEventListener('change',renderScheduled);

  function fillMessage(row,message){
    const hadFocus=row.contains(document.activeElement);
    const kind=Object.hasOwn(ROLES,message.kind)?message.kind:'unknown';row.dataset.kind=kind;
    const label=ghostRoleLabel(message.kind,message.name);
    const role=el('button',null,`ghost-role ghost-role--${kind}`);role.type='button';role.setAttribute('aria-label',label);
    role.dataset.tip=`${label} · ${formatGhostTime(message.timestamp,{timeZone})}`;role.append(glyph(ROLES[message.kind]?.[1]||'?'));
    const body=el('div',null,'ghost-log-body');
    if(message.unattributed)body.append(el('p','This fragment could not be attributed to a source entry, so it is shown on its own and the message count is uncertain.','ghost-log-notice'));
    if(message.leadingMissing)body.append(el('p','Earlier parts of this entry are not adjacent in the retained source and may be missing.','ghost-log-notice'));
    body.append(el('pre',message.text,'jobs-history-text ghost-log-text'));
    if(message.continuation==='pending')body.append(el('p','Continues on a later page that is not loaded yet.','ghost-log-notice'));
    else if(message.continuation==='missing')body.append(el('p','The rest of this entry is not adjacent in the retained source and may be missing.','ghost-log-notice'));
    row.replaceChildren(role,body);if(hadFocus)role.focus({preventScroll:true});
  }
  function render(state){
    historyHost.hidden=!state.session&&!state.error;
    const more=Boolean(state.next),messages=groupGhostMessages(state.events,{morePagesRemain:more});
    const signature=JSON.stringify([state.events,more]);
    if(signature!==rendered){
      rendered=signature;
      const atBottom=viewport.scrollHeight-viewport.clientHeight-viewport.scrollTop<70,viewTop=viewport.getBoundingClientRect().top;
      const anchor=[...list.children].find(node=>node.getBoundingClientRect().bottom>viewTop),anchorTop=anchor?.getBoundingClientRect().top;
      const old=new Map([...list.children].map(node=>[node.dataset.messageId,node])),wanted=new Set(messages.map(m=>m.id));
      for(const node of [...list.children])if(!wanted.has(node.dataset.messageId))node.remove();
      list.dataset.renderSkip=String(messages.length>GHOST_RENDER_SKIP_MIN);
      let before=list.firstChild;
      for(const message of messages){
        let row=old.get(message.id);if(!row){row=el('li',null,'ghost-log-line');row.dataset.messageId=message.id;}
        const rowSignature=JSON.stringify(message);if(row._signature!==rowSignature){row._signature=rowSignature;fillMessage(row,message);}
        if(row!==before)list.insertBefore(row,before);before=row.nextSibling;
      }
      if(pinned||atBottom)stick();else if(anchor?.isConnected&&anchorTop!=null)viewport.scrollTop+=anchor.getBoundingClientRect().top-anchorTop;
    }
    const summary=ghostLogSummary(messages,{morePagesRemain:more,reachedEnd:state.reachedEnd,timeZone});
    const meta=state.session?summary.text:'';titleMeta.textContent=meta;titleSep.textContent=meta?', ':'';
    historyTitle.setAttribute('aria-label',meta?`${HISTORY_NAME}, ${meta}`:HISTORY_NAME);
    if(state.node&&state.session&&messages.length){
      const key=metaKey(state.node,state.session);
      const description=describeGhostSession(messages),range=summary.span.timed?formatGhostRange(summary.span.first,summary.span.last,{timeZone}):'';
      const prior=sessionMeta.get(key);if(!prior||prior.description!==description||prior.range!==range){sessionMeta.set(key,{description,range});renderSessionOptions();}
    }
    const has=Boolean(state.session);
    setDisabled(refreshLog,state.busy||!has);setDisabled(latest,state.busy||!has);setDisabled(rescan,state.busy||!has);setDisabled(oldest,!messages.length);
    viewport.setAttribute('aria-busy',String(state.busy));
    const count=`${messages.length} message${messages.length===1?'':'s'} loaded${summary.uncertain?' (count uncertain)':''}.`;
    historyStatus.textContent=state.error||(state.busy?(state.scanning?'Scanning retained source pages.':'Reading retained source pages.'):!has?'Choose a session.':`${count} ${more?(capped?'Stopped after 20 pages to keep requests bounded; select Latest retained again to continue.':'More pages remain.'):state.reachedEnd?'Current end of retained archive reached; capture completeness is unknown.':'Reading may be incomplete; refresh to continue.'}`);
    historyStatus.dataset.error=String(Boolean(state.error));
    gaps.textContent=(state.gaps.length?'Archive gaps: '+state.gaps.join('; ')+'. ':'')+'Retained text only. Images, attachments and hidden reasoning are not represented. The archive keeps up to 90 days; older content may have expired and capture completeness is unknown.';
  }
  function clearPrivate(){
    generation++;closeList();hideTip();nodes.replaceChildren();sessionList=[];sessionIDs.clear();selectedSession='';sessionCursor='';sessionBusy=false;sessionMeta.clear();scheduled=null;moreSessions.hidden=true;loaded=false;capped=false;rendered='';unpin();
    renderSessionOptions();renderScheduled();
  }
  function authError(error){if([401,403].includes(error?.status)||error?.name==='AuthLostError'){clearPrivate();loader.clear();return true;}return false;}
  async function loadSessions(append=false){
    if(sessionBusy||!nodes.value)return;
    sessionBusy=true;const seq=generation,node=nodes.value;moreSessions.disabled=true;renderCombo();
    try{
      const body={node_id:node};if(append&&sessionCursor)body.cursor=sessionCursor;
      const data=await request('/api/v1/ghost-work/sessions',{method:'POST',json:body});
      if(seq!==generation||node!==nodes.value)return;
      if(data?.version!==1||data.node_id!==node||data.coverage!=='retained_only'||!Array.isArray(data.sessions))throw new Error('Unexpected session archive response.');
      const keep=selectedSession;if(!append){sessionList=[];sessionIDs.clear();}
      for(const session of data.sessions){
        if(typeof session?.session_id!=='string'||!session.session_id)continue;
        const entry={id:session.session_id,date:typeof session.date==='string'?session.date:'',previous:false};
        const index=sessionList.findIndex(s=>s.id===entry.id);if(index>=0)sessionList[index]=entry;else sessionList.push(entry);sessionIDs.add(entry.id);
      }
      if(!append&&keep&&!sessionIDs.has(keep))sessionList.push({id:keep,date:'',previous:true});
      sessionCursor=typeof data.next_cursor==='string'?data.next_cursor:'';moreSessions.hidden=!sessionCursor;
      status.textContent=`${sessionIDs.size} session${sessionIDs.size===1?'':'s'} loaded.${sessionCursor?' More sessions remain.':' Retained sessions only; an empty archive does not prove there was no conversation.'}`;
      renderSessionOptions();
    }catch(error){if(seq===generation){authError(error);status.textContent=error?.message||'Retained sessions are unavailable.';}}
    finally{if(seq===generation){sessionBusy=false;moreSessions.disabled=false;renderCombo();}}
  }
  async function load(){
    const seq=++generation;sessionBusy=false;setDisabled(refresh,true);refresh.setAttribute('aria-busy','true');status.textContent='Reading authorized Ghost work.';
    try{
      const [nodeData,jobData]=await Promise.all([request('/api/v1/ghost-work/nodes'),request('/api/v1/ghost-work/jobs')]);
      if(seq!==generation)return;
      const oldNode=nodes.value;emptySelect(nodes,'Choose a node');
      for(const node of nodeData?.nodes||[])option(nodes,node.node_id,`${node.node_id} · ${node.state||node.status||'status unavailable'}`);
      if([...nodes.options].some(o=>o.value===oldNode))nodes.value=oldNode;
      else if(oldNode){loader.clear();sessionList=[];sessionIDs.clear();sessionMeta.clear();selectedSession='';sessionCursor='';moreSessions.hidden=true;unpin();}
      scheduled={events:Array.isArray(jobData?.events)?jobData.events:[],bindingAvailable:jobData?.providerSessionBindingAvailable===true,limit:Number(jobData?.runHistoryLimit)||10};
      renderSessionOptions();renderScheduled();
      loaded=true;status.textContent='Choose a node, then select the actual session.';
      if(nodes.value)await loadSessions(false);
    }catch(error){if(seq===generation){authError(error);status.textContent='Ghost work is unavailable for this account or the integration is not ready. '+(error?.message||'');}}
    finally{setDisabled(refresh,false);refresh.removeAttribute('aria-busy');}
  }
  nodes.addEventListener('change',()=>{generation++;sessionBusy=false;loader.clear();closeList();sessionList=[];sessionIDs.clear();sessionMeta.clear();selectedSession='';sessionCursor='';moreSessions.hidden=true;capped=false;unpin();renderSessionOptions();renderScheduled();void loadSessions();});
  moreSessions.addEventListener('click',()=>loadSessions(true));
  refresh.addEventListener('click',async()=>{if(isDisabled(refresh))return;await load();if(loader.state().session)await loader.rescan();});
  document.addEventListener('visibilitychange',()=>{if(active&&!document.hidden&&loader.state().session)void loader.rescan();});
  window.addEventListener('online',()=>{if(active&&loader.state().session)void loader.rescan();});
  setInterval(()=>{if(active&&!document.hidden)void loader.poll();},20000);
  renderCombo();
  return {show(){active=true;if(!loaded)void load();else if(loader.state().session)void loader.rescan();},hide(){active=false;hideTip();closeList();}};
}
