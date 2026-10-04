import {test,expect} from '@playwright/test';
import AxeBuilder from '@axe-core/playwright';

// Explicitly labeled CONTRACT FIXTURES from retained-history v1, pinned to
// ghost-cli 672c44030d50a738d514b1d9b9ad89c754d12bae. These are not real
// provider conversations and cannot prove that the Ghost deployment is ready.
// Never run this suite against a deployed origin, even with LN_BASE_URL set.
//
// Auth is a deterministic fixture: /api/v1/auth/refresh returns a fixed opaque
// fake token that is not a credential, and /api/v1/jobs returns an empty list.
// Any other unrouted /api/ call gets a 404 fixture response. No header values
// are logged.
//
// EXPECTED-FAILURE NOTES (recorded, not hidden):
// 1. The touch tooltip relies on the tapped role button taking focus: the
//    pointerout that follows a touch pointerup hides the tip when focus does not
//    move (WebKit behaviour). Chromium is expected to pass; a failure here means
//    the pointerout handler needs a pointerType check.
// 2. The large-text check enlarges the root font size. If jobs.css resolves type
//    in px this only approximates Android font scaling.
// 3. Exact hours read "1 hour 0 minutes"; assertions pin current wording.
// 4. A 401/403 clears archive text, metadata, options, tooltip and jobs, but the
//    generic discovery status line ("N sessions loaded") is not cleared.
// 5. If the app bar overflows at 320px, the document-width assertion fails for
//    reasons outside Ghost work code.
const baseURL=process.env.LN_GHOST_BASE_URL||process.env.LN_JOBS_BASE_URL||'http://127.0.0.1:8797';
const origin=new URL(baseURL).origin;
if(!['127.0.0.1','localhost','[::1]'].includes(new URL(baseURL).hostname))throw new Error('Ghost contract-fixture tests require an explicit isolated loopback origin.');
test.use({baseURL,serviceWorkers:'block',timezoneId:'America/New_York',locale:'en-US'});

const NODE='OFFICEPC',OTHER_NODE='FIXTUREPC';
const SESSION='aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee';
const SECOND='11111111-2222-3333-4444-555555555555';
const THIRD='agent-0123456789abcdef0123';
const FIXTURE_TOKEN='contract-fixture-opaque-access-token-not-a-credential';
const KEY=`history/${NODE}/2026-10-03/${SESSION}.jsonl`,KEY2=`history/${NODE}/2026-10-02/${SECOND}.jsonl`;
const pad=(n,width)=>String(n).padStart(width,'0');
const sourceID=(line,block=0,part=0,key=KEY)=>`${key}:${pad(line,9)}:${pad(block,5)}:${pad(part,5)}`;
const event=(line,{key=KEY,block=0,part=0,...extra}={})=>{const id=sourceID(line,block,part,key);return {id,sequence:id,kind:'assistant',text:`CONTRACT FIXTURE retained text ${line}`,part,more:false,...extra};};
const sessionPage=(sessions=[],extra={})=>({version:1,node_id:NODE,sessions,next_cursor:'',coverage:'retained_only',...extra});
const eventsPage=(events=[],extra={})=>({version:1,node_id:NODE,session_id:SESSION,events,coverage:'retained_only',gaps:[],resume_cursor:'fixture-resume-end',...extra});
const deferred=()=>{let resolve;const promise=new Promise(r=>resolve=r);return {promise,resolve};};
const errorBody=(code,message)=>({error:{code,message}});

const TOOL_HEAD='total 8\n';
const TOOL_TAIL='drwxr-xr-x  2 fixture fixture 4096 .\n-rw-r--r--  1 fixture fixture   12 CONTRACT-FIXTURE.txt\n';
const RICH_DESCRIPTION='Please summarize the CONTRACT FIXTURE build log.';
const RICH=[
  event(1,{kind:'user',text:RICH_DESCRIPTION,timestamp:'2026-10-03T13:05:42Z'}),
  event(2,{kind:'assistant',text:'CONTRACT FIXTURE: I will list the fixture directory first.',timestamp:'2026-10-03T13:06:10Z'}),
  event(3,{kind:'tool_call',name:'bash',call_id:'toolu_01CONTRACTFIXTURE',text:'{"command":"ls -la fixture/"}',timestamp:'2026-10-03T13:06:15Z'}),
  event(4,{kind:'tool_result',name:'bash',call_id:'toolu_01CONTRACTFIXTURE',part:0,more:true,text:TOOL_HEAD,timestamp:'2026-10-03T13:06:16Z'}),
  event(4,{kind:'tool_result',name:'bash',call_id:'toolu_01CONTRACTFIXTURE',part:1,text:TOOL_TAIL,timestamp:'2026-10-03T13:06:16Z'}),
  event(5,{kind:'event',text:'CONTRACT FIXTURE session note without a source time'}),
  event(6,{kind:'assistant',text:'CONTRACT FIXTURE summary: the build log lists one file.',timestamp:'2026-10-03T15:47:03Z'}),
];
const RICH_RANGE='Oct 3, 2026, 9:05 AM \u2013 11:47 AM EDT';
const RICH_META=`observed 2 hours 41 minutes \u00b7 ${RICH_RANGE} \u00b7 6 messages \u00b7 1 without a source time`;
const SECOND_DESCRIPTION='CONTRACT FIXTURE: draft the release notes for the fixture app.';
const SECOND_EVENTS=[
  event(1,{key:KEY2,kind:'user',text:SECOND_DESCRIPTION,timestamp:'2026-10-03T03:10:00Z'}),
  event(2,{key:KEY2,kind:'assistant',text:'CONTRACT FIXTURE release notes drafted.',timestamp:'2026-10-03T05:20:00Z'}),
];
const SECOND_RANGE='Oct 2, 2026, 11:10 PM \u2013 Oct 3, 2026, 1:20 AM EDT';
const JOBS=[
  {event_id:'fixture-ok',node:NODE,repo:'CONTRACT-FIXTURE/ok',prompt:'CONTRACT FIXTURE scheduled prompt; never a session binding',last_run_status:'succeeded',last_run_ts:'2026-10-03T13:05:42Z',provider_session_id:SESSION,runs:[]},
  {event_id:'fixture-failed',node:NODE,repo:'CONTRACT-FIXTURE/failed',last_run_status:'failed',runs:[]},
  {event_id:'fixture-new',node:NODE,repo:'CONTRACT-FIXTURE/new',runs:[]},
];

let violations=[];
test.beforeEach(async({page})=>{
  violations=[];
  await page.route('**/*',route=>new URL(route.request().url()).origin===origin?route.continue():route.abort());
});
test.afterEach(()=>{expect(violations,'Ghost fixture transport contract').toEqual([]);});

async function fixtures(page,{sessions,events,nodes,jobs}={}){
  const calls=[];
  await page.route(url=>url.origin===origin&&url.pathname.startsWith('/api/'),route=>route.fulfill({status:404,json:errorBody('fixture_not_routed','CONTRACT FIXTURE: endpoint not routed in this spec.')}));
  await page.route(url=>url.pathname==='/api/v1/auth/refresh',route=>route.fulfill({json:{accessToken:FIXTURE_TOKEN,expiresAt:Math.floor(Date.now()/1000)+3600}}));
  await page.route(url=>url.pathname==='/api/v1/jobs',route=>route.fulfill({json:{jobs:[],nextCursor:'',capabilities:{reminder:true,review:true,scheduling:false}}}));
  await page.route(url=>url.pathname.startsWith('/api/v1/ghost-work/'),async route=>{
    const request=route.request(),url=new URL(request.url()),endpoint=url.pathname.split('/').at(-1),headers=request.headers();
    const read=['sessions','events'].includes(endpoint);
    let body=null;if(read){try{body=request.postDataJSON();}catch{body=null;}}
    const call={endpoint,method:request.method(),url:request.url(),search:url.search,body,node:body?.node_id??null,session:body?.session_id??null,cursor:body?.cursor??null,bearer:headers.authorization===`Bearer ${FIXTURE_TOKEN}`};
    calls.push(call);
    const problems=[];
    if(request.method()!==(read?'POST':'GET'))problems.push(`${endpoint} used ${request.method()}`);
    if(!call.bearer)problems.push(`${endpoint} missing fixture bearer`);
    if(read){
      if(url.search)problems.push(`${endpoint} put parameters in the request line`);
      if(!(headers['content-type']||'').includes('application/json'))problems.push(`${endpoint} body is not JSON`);
      const allowed=endpoint==='sessions'?['node_id','cursor']:['node_id','session_id','cursor'];
      if(!body||typeof body!=='object'||Array.isArray(body)||Object.keys(body).some(k=>!allowed.includes(k))||Object.values(body).some(v=>typeof v!=='string'))problems.push(`${endpoint} body has unexpected shape`);
    }
    if(problems.length){violations.push(...problems);return route.fulfill({status:400,json:errorBody('fixture_contract_violation',problems.join('; '))});}
    if(endpoint==='nodes')return nodes?nodes(route,call):route.fulfill({json:{nodes:[{node_id:NODE,state:'offline'}],source:'ghost',binding:'explicit_provider_selection',historyAvailability:'check_per_node'}});
    if(endpoint==='jobs')return jobs?jobs(route,call):route.fulfill({json:{events:JOBS,source:'ghost',runHistoryLimit:10,providerSessionBindingAvailable:false,coverage:'authorized_nodes_only'}});
    if(endpoint==='sessions')return sessions?sessions(route,call):route.fulfill({json:sessionPage([{session_id:SESSION,date:'2026-10-03'}],{node_id:call.node})});
    if(endpoint==='events')return events?events(route,call):route.fulfill({json:eventsPage([event(1)],{node_id:call.node,session_id:call.session})});
    violations.push('unexpected Ghost endpoint '+endpoint);
    return route.fulfill({status:404,json:errorBody('fixture_not_routed','Unexpected Ghost fixture endpoint')});
  });
  return calls;
}
const ws=page=>page.locator('#ghostWorkWorkspace');
const rows=page=>page.locator('#ghostWorkWorkspace .ghost-log-line');
const option=(page,id)=>page.locator(`#ghostSessionListbox [role="option"][data-value="${id}"]`);
const combo=page=>page.getByRole('combobox',{name:'Session',exact:true});
const historyStatus=page=>page.locator('.ghost-conversation > p[role="status"]');
const tool=(page,name)=>page.getByRole('group',{name:'Retained conversation controls'}).getByRole('button',{name,exact:true});
const eventsCalls=calls=>calls.filter(c=>c.endpoint==='events');
const atScrollEnd=viewport=>viewport.evaluate(v=>v.scrollHeight>v.clientHeight&&Math.abs(v.scrollHeight-v.clientHeight-v.scrollTop)<2);
async function open(page){await page.goto('/jobs');await page.getByRole('button',{name:'Ghost work',exact:true}).click();await expect(ws(page)).toBeVisible();}
async function chooseSession(page,id){
  const c=combo(page);await expect(c).toHaveAttribute('aria-disabled','false');
  if(await c.getAttribute('aria-expanded')!=='true')await c.click();
  await expect(page.locator('#ghostSessionListbox')).toBeVisible();
  await option(page,id).click();await expect(c).toHaveAttribute('aria-expanded','false');
}
async function select(page,session=SESSION,node=NODE){await page.getByLabel('Node',{exact:true}).selectOption(node);await expect(option(page,session)).toHaveCount(1);await chooseSession(page,session);}
async function axe(page,label){
  const result=await new AxeBuilder({page}).include('#ghostWorkWorkspace').withTags(['wcag2a','wcag2aa','wcag21aa']).analyze();
  expect(result.violations,label).toEqual([]);
}
const noHorizontalOverflow=page=>page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth);

test('CONTRACT FIXTURES: discovery traverses empty pages, deduplicates sessions, and never guesses a job binding',async({page})=>{
  const calls=await fixtures(page,{sessions:(route,call)=>route.fulfill({json:call.cursor==='last'?sessionPage([{session_id:SESSION,date:'2026-10-03'},{session_id:SECOND,date:'2026-10-02'}]):call.cursor==='empty'?sessionPage([{session_id:SESSION,date:'2026-10-03'}],{next_cursor:'last'}):sessionPage([],{next_cursor:'empty'})})});
  await open(page);
  await page.getByLabel('Node',{exact:true}).selectOption(NODE);
  const more=page.getByRole('button',{name:'Load more sessions',exact:true});
  await expect(more).toBeVisible();await more.click();
  await expect(option(page,SESSION)).toHaveCount(1);
  await more.click();await expect(more).toBeHidden();
  await expect(option(page,SESSION)).toHaveCount(1);await expect(option(page,SECOND)).toHaveCount(1);
  expect(calls.filter(c=>c.endpoint==='sessions').map(c=>c.cursor)).toEqual([null,'empty','last']);
  expect(eventsCalls(calls)).toHaveLength(0);
  await chooseSession(page,SESSION);
  await expect(rows(page)).toHaveCount(1);
  expect(eventsCalls(calls)[0]).toMatchObject({node:NODE,session:SESSION,cursor:null});
  await expect(page.locator('#ghostNode')).toContainText('offline');
  await expect(combo(page)).toContainText('uploaded 2026-10-03');
  const only=page.getByRole('checkbox',{name:'Show Authorized Scheduled Jobs Only',exact:true});
  await only.check();
  await expect(page.locator('#ghostScheduledNotice')).toContainText('reliable provider-session binding');
  await expect(ws(page).locator('.ghost-job')).toHaveCount(0);
  await only.uncheck();
  for(const theme of ['light','dark']){
    await page.evaluate(value=>document.documentElement.dataset.theme=value,theme);
    await axe(page,`${theme} Ghost work accessibility`);
    expect(await noHorizontalOverflow(page)).toBe(true);
  }
});

test('CONTRACT FIXTURES: empty event pages, full UTF-8 fragments joined across pages, source order, missing time and literal markup remain faithful',async({page})=>{
  const long='CONTRACT FIXTURE large tool result\n'+'\u{1F642}'.repeat(12000);
  const literal='<img src=x onerror="window.fixtureExecuted=true">';
  const head=event(1,{kind:'tool_result',name:'bash',call_id:'toolu_01CONTRACTFIXTURE',text:long,part:0,more:true,timestamp:'2030-01-01T00:00:00Z'});
  const tail=event(1,{kind:'tool_result',name:'bash',call_id:'toolu_01CONTRACTFIXTURE',text:'CONTRACT FIXTURE continuation END',part:1,timestamp:'2020-01-01T00:00:00Z'});
  const user=event(2,{kind:'user',text:literal});
  const calls=await fixtures(page,{events:async(route,call)=>{
    if(!call.cursor)return route.fulfill({json:eventsPage([],{next_cursor:'first-content'})});
    if(call.cursor==='first-content')return route.fulfill({json:eventsPage([head],{next_cursor:'empty-between'})});
    if(call.cursor==='empty-between')return route.fulfill({json:eventsPage([],{next_cursor:'tail'})});
    return route.fulfill({json:eventsPage([tail,user],{gaps:['unsupported records: 1']})});
  }});
  await open(page);await select(page);
  await expect(historyStatus(page)).toContainText('More pages remain.');await expect(rows(page)).toHaveCount(0);
  await tool(page,'Latest retained').click();await expect(rows(page)).toHaveCount(2);
  expect(await rows(page).first().locator('pre').textContent()).toBe(long+'CONTRACT FIXTURE continuation END');
  expect(await rows(page).evaluateAll(nodes=>nodes.map(n=>n.dataset.messageId))).toEqual([head.id,user.id]);
  await expect(rows(page).first().getByRole('button',{name:'Tool result: bash',exact:true})).toHaveAttribute('data-tip','Tool result: bash \u00b7 Dec 31, 2029, 7:00 PM EST');
  await expect(rows(page).nth(1).getByRole('button',{name:'User message',exact:true})).toHaveAttribute('data-tip','User message \u00b7 Source time unknown');
  await expect(ws(page)).not.toContainText(/Fragment \d/);await expect(ws(page)).not.toContainText('toolu_01CONTRACTFIXTURE');
  await expect(ws(page).locator('.ghost-log-notice')).toHaveCount(0);
  await expect(rows(page).nth(1)).toContainText(literal);
  await expect(rows(page).locator('img')).toHaveCount(0);expect(await page.evaluate(()=>window.fixtureExecuted)).toBeUndefined();
  await expect(page.locator('#ghostHistoryTitle')).toContainText('duration unknown');await expect(page.locator('#ghostHistoryTitle')).toContainText('2 messages');
  await expect(ws(page)).toContainText('unsupported records: 1');await expect(ws(page)).toContainText('90 days');
  await expect(ws(page)).toContainText('capture completeness is unknown');
  expect(eventsCalls(calls).map(c=>c.cursor)).toEqual([null,'first-content','empty-between','tail']);
  expect(await noHorizontalOverflow(page)).toBe(true);
  expect(await rows(page).first().locator('pre').evaluate(e=>({wrap:getComputedStyle(e).whiteSpace,max:getComputedStyle(e).maxHeight}))).toEqual({wrap:'pre-wrap',max:'none'});
});

test('CONTRACT FIXTURES: 503 integration unavailability is explicit and does not become an empty transcript',async({page})=>{
  const unavailable=route=>route.fulfill({status:503,json:errorBody('provider_transport_unavailable','CONTRACT FIXTURE: the upstream GET allowlist is not deployed.')});
  const calls=await fixtures(page,{nodes:unavailable,jobs:unavailable});await open(page);
  await expect(page.locator('#ghostStatus')).toContainText('Ghost work is unavailable');
  await expect(page.locator('#ghostStatus')).toContainText('integration is not ready');
  await expect(rows(page)).toHaveCount(0);expect(calls.some(c=>['sessions','events'].includes(c.endpoint))).toBe(false);
  await expect(ws(page)).not.toContainText('Current end of retained archive reached');
});

test('CONTRACT FIXTURES: 403 clears archived text, node/session options and scheduled-job content and stops automatic rescan',async({page})=>{
  let revoked=false;
  const calls=await fixtures(page,{events:(route,call)=>revoked?route.fulfill({status:403,json:errorBody('provider_access_denied','CONTRACT FIXTURE owner access revoked')}):route.fulfill({json:eventsPage([event(1,{kind:'user',text:'PRIVATE CONTRACT FIXTURE MARKER'})],{node_id:call.node,session_id:call.session})})});
  await open(page);await select(page);await expect(rows(page)).toContainText('PRIVATE CONTRACT FIXTURE MARKER');
  await expect(option(page,SESSION)).toContainText('PRIVATE CONTRACT FIXTURE MARKER');
  revoked=true;await tool(page,'Rescan history').click();
  await expect(historyStatus(page)).toContainText('Content cleared');await expect(rows(page)).toHaveCount(0);
  await expect(page.locator('#ghostNode option')).toHaveCount(0);await expect(page.locator('#ghostSessionListbox [role="option"]')).toHaveCount(0);
  await expect(ws(page).locator('.ghost-job')).toHaveCount(0);await expect(page.getByRole('tooltip')).toBeHidden();
  await expect(ws(page)).not.toContainText('PRIVATE CONTRACT FIXTURE MARKER');
  const count=calls.length;await page.evaluate(()=>{window.dispatchEvent(new Event('online'));document.dispatchEvent(new Event('visibilitychange'));});
  await page.waitForTimeout(50);expect(calls).toHaveLength(count);
});

test('CONTRACT FIXTURES: 401 clears text, Session metadata, dropdown, tooltip and jobs while the log is open',async({page})=>{
  let revoked=false;
  const calls=await fixtures(page,{events:(route,call)=>revoked?route.fulfill({status:401,json:errorBody('provider_access_denied','CONTRACT FIXTURE token no longer accepted')}):route.fulfill({json:eventsPage(RICH,{node_id:call.node,session_id:call.session})})});
  await open(page);await select(page);await expect(rows(page)).toHaveCount(6);
  await expect(option(page,SESSION)).toContainText(RICH_DESCRIPTION);
  await expect(ws(page).locator('.ghost-job')).toHaveCount(3);
  const role=rows(page).first().getByRole('button',{name:'User message',exact:true});
  await role.focus();await expect(page.getByRole('tooltip')).toBeVisible();
  revoked=true;await page.evaluate(()=>window.dispatchEvent(new Event('online')));
  await expect(historyStatus(page)).toContainText('Content cleared');
  await expect(rows(page)).toHaveCount(0);
  await expect(page.locator('#ghostTooltip')).toBeHidden();expect(await page.locator('#ghostTooltip').textContent()).toBe('');
  await expect(page.locator('[aria-describedby="ghostTooltip"]')).toHaveCount(0);
  await expect(page.locator('#ghostSessionListbox [role="option"]')).toHaveCount(0);await expect(page.locator('#ghostSessionListbox')).toBeHidden();
  await expect(combo(page)).toHaveText('Choose a node first');await expect(page.locator('#ghostNode option')).toHaveCount(0);
  await expect(ws(page).locator('.ghost-job')).toHaveCount(0);await expect(page.locator('.ghost-scheduled')).toBeHidden();
  await expect(page.locator('.ghost-log-meta')).toHaveText('');
  for(const privateText of [RICH_DESCRIPTION,SESSION,'CONTRACT-FIXTURE/ok',TOOL_TAIL.trim()])await expect(ws(page)).not.toContainText(privateText);
  const count=calls.length;await page.evaluate(()=>window.dispatchEvent(new Event('online')));await page.waitForTimeout(50);expect(calls).toHaveLength(count);
});

test('CONTRACT FIXTURES: 409 prompts a beginning rescan that replaces same-ID DOM text and removes expired entries',async({page})=>{
  let phase='initial';const calls=await fixtures(page,{events:(route,call)=>{
    if(phase==='initial')return route.fulfill({json:eventsPage([event(2,{text:'OLD CONTRACT FIXTURE BYTES'}),event(3,{text:'EXPIRED CONTRACT FIXTURE BYTES'})])});
    if(call.cursor)return route.fulfill({status:409,json:errorBody('history_rescan_required','CONTRACT FIXTURE source digest changed')});
    return route.fulfill({json:eventsPage([event(1,{text:'LATE CONTRACT FIXTURE UPLOAD'}),event(2,{text:'REPLACEMENT CONTRACT FIXTURE BYTES'})],{gaps:['malformed records: 1']})});
  }});
  await open(page);await select(page);await expect(rows(page)).toHaveCount(2);
  phase='changed';await tool(page,'Latest retained').click();await expect(historyStatus(page)).toContainText('Refresh to rescan from the beginning');
  await expect(historyStatus(page)).toHaveAttribute('data-error','true');
  await tool(page,'Rescan history').click();
  await expect(rows(page)).toHaveCount(2);await expect(rows(page).first()).toContainText('LATE CONTRACT FIXTURE UPLOAD');
  await expect(rows(page).nth(1)).toContainText('REPLACEMENT CONTRACT FIXTURE BYTES');await expect(ws(page)).not.toContainText('EXPIRED CONTRACT FIXTURE BYTES');
  await expect(ws(page)).not.toContainText('OLD CONTRACT FIXTURE BYTES');
  expect(await rows(page).evaluateAll(nodes=>nodes.map(n=>n.dataset.messageId))).toEqual([event(1).id,event(2).id]);
  expect(eventsCalls(calls).at(-1).cursor).toBe(null);
});

test('CONTRACT FIXTURES: switching nodes during discovery ignores the old response without blocking the new node',async({page})=>{
  const entered=deferred(),release=deferred();
  await fixtures(page,{nodes:route=>route.fulfill({json:{nodes:[{node_id:NODE,state:'offline'},{node_id:OTHER_NODE,state:'online'}]}}),sessions:async(route,call)=>{
    if(call.node===NODE){entered.resolve();await release.promise;return route.fulfill({json:sessionPage([{session_id:SESSION,date:'2026-10-03'}])});}
    return route.fulfill({json:sessionPage([{session_id:SECOND,date:'2026-10-03'}],{node_id:OTHER_NODE})});
  }});
  await open(page);await page.getByLabel('Node',{exact:true}).selectOption(NODE);await entered.promise;
  await page.getByLabel('Node',{exact:true}).selectOption(OTHER_NODE);
  await expect(option(page,SECOND)).toHaveCount(1);
  release.resolve();await page.waitForTimeout(50);
  await expect(option(page,SESSION)).toHaveCount(0);await expect(page.locator('#ghostNode')).toHaveValue(OTHER_NODE);
});

test('CONTRACT FIXTURES: Refresh supersedes pending discovery and preserves the actual selected provider session',async({page})=>{
  const entered=deferred(),release=deferred();let discovery=0;
  const calls=await fixtures(page,{sessions:async route=>{
    discovery++;if(discovery===1){entered.resolve();await release.promise;}
    return route.fulfill({json:sessionPage([{session_id:SESSION,date:'2026-10-03'}])});
  }});
  await open(page);await page.getByLabel('Node',{exact:true}).selectOption(NODE);await entered.promise;
  await page.getByRole('button',{name:'Refresh Ghost work',exact:true}).click();
  await expect(option(page,SESSION)).toHaveCount(1);
  release.resolve();await chooseSession(page,SESSION);await expect(rows(page)).toHaveCount(1);
  const beforeRefresh=eventsCalls(calls).length;
  await page.getByRole('button',{name:'Refresh Ghost work',exact:true}).click();
  await expect(combo(page)).toContainText(SESSION);await expect(option(page,SESSION)).toHaveAttribute('aria-selected','true');await expect(rows(page)).toHaveCount(1);
  await expect.poll(()=>eventsCalls(calls).length).toBeGreaterThan(beforeRefresh);
  expect(eventsCalls(calls).at(-1)).toMatchObject({node:NODE,session:SESSION,cursor:null});
});

test('CONTRACT FIXTURES: a preserved session selection becomes one real option when rediscovered on a later page',async({page})=>{
  let refreshed=false;
  await fixtures(page,{sessions:(route,call)=>route.fulfill({json:!refreshed?sessionPage([{session_id:SESSION,date:'2026-10-01'}]):call.cursor?sessionPage([{session_id:SESSION,date:'2026-10-03'}]):sessionPage([{session_id:SECOND,date:'2026-10-02'}],{next_cursor:'selected-later'})})});
  await open(page);await select(page);await expect(rows(page)).toHaveCount(1);
  refreshed=true;await page.getByRole('button',{name:'Refresh Ghost work',exact:true}).click();
  await expect(option(page,SESSION)).toContainText('previously selected');
  await expect(combo(page)).toContainText(SESSION);await expect(option(page,SESSION)).toHaveAttribute('aria-selected','true');
  await page.getByRole('button',{name:'Load more sessions',exact:true}).click();
  await expect(page.getByRole('button',{name:'Load more sessions',exact:true})).toBeHidden();
  await expect(option(page,SESSION)).toHaveCount(1);
  await expect(option(page,SESSION)).toContainText('uploaded 2026-10-03');await expect(option(page,SESSION)).not.toContainText('previously selected');
  await expect(combo(page)).toContainText(SESSION);
  await expect(page.locator('#ghostStatus')).toContainText('2 sessions loaded.');
});

test('CONTRACT FIXTURES: the retained conversation is one continuous log with role icons, observed AM/PM bounds and a logical count',async({page})=>{
  await fixtures(page,{events:(route,call)=>route.fulfill({json:eventsPage(RICH,{node_id:call.node,session_id:call.session})})});
  await open(page);await select(page);
  await expect(rows(page)).toHaveCount(6);
  const title=page.locator('#ghostHistoryTitle');
  await expect(title).toHaveText(`Retained conversation, ${RICH_META}`);
  await expect(page.getByRole('heading',{name:`Retained conversation, ${RICH_META}`,exact:true})).toHaveCount(1);
  // The explicit heading name must mirror the visible title text exactly (name and summary).
  expect(await title.getAttribute('aria-label')).toBe(await title.textContent());
  expect(await title.textContent()).not.toMatch(/\d:\d{2}:\d{2}/);
  expect(await rows(page).evaluateAll(n=>n.map(r=>r.querySelector('.ghost-role').getAttribute('aria-label')))).toEqual(['User message','Assistant message','Tool call: bash','Tool result: bash','Event','Assistant message']);
  expect(await rows(page).evaluateAll(n=>n.map(r=>r.querySelector('.ghost-role').dataset.tip))).toEqual([
    'User message \u00b7 Oct 3, 2026, 9:05 AM EDT','Assistant message \u00b7 Oct 3, 2026, 9:06 AM EDT','Tool call: bash \u00b7 Oct 3, 2026, 9:06 AM EDT',
    'Tool result: bash \u00b7 Oct 3, 2026, 9:06 AM EDT','Event \u00b7 Source time unknown','Assistant message \u00b7 Oct 3, 2026, 11:47 AM EDT']);
  expect(await page.locator('.ghost-role .ghost-glyph').evaluateAll(n=>n.every(g=>g.getAttribute('aria-hidden')==='true'))).toBe(true);
  expect(await rows(page).nth(2).locator('pre').textContent()).toBe('{"command":"ls -la fixture/"}');
  expect(await rows(page).nth(3).locator('pre').textContent()).toBe(TOOL_HEAD+TOOL_TAIL);
  await expect(ws(page)).not.toContainText(/Fragment \d/);await expect(ws(page)).not.toContainText('toolu_');await expect(ws(page)).not.toContainText('bash \u00b7');
  await expect(ws(page).locator('.ghost-log-notice')).toHaveCount(0);
  await expect(page.locator('#ghostWorkWorkspace ol.ghost-log')).toHaveCount(1);
  await expect(page.getByRole('list',{name:'Retained messages'}).getByRole('listitem')).toHaveCount(6);
  await expect(page.locator('#ghostWorkWorkspace .jobs-history-entry')).toHaveCount(0);
  const layout=await rows(page).evaluateAll(n=>n.map(r=>{const s=getComputedStyle(r),b=r.getBoundingClientRect();return {border:s.borderTopStyle,radius:s.borderTopLeftRadius,bg:s.backgroundColor,top:b.top,bottom:b.bottom};}));
  for(const row of layout)expect({border:row.border,radius:row.radius,bg:row.bg}).toEqual({border:'none',radius:'0px',bg:'rgba(0, 0, 0, 0)'});
  for(let i=1;i<layout.length;i++)expect(Math.abs(layout[i].top-layout[i-1].bottom)).toBeLessThan(1.5);
  expect(await option(page,SESSION).locator('.ghost-opt-line').allTextContents()).toEqual([RICH_DESCRIPTION,SESSION,`Observed ${RICH_RANGE} \u00b7 uploaded 2026-10-03`]);
  expect(await combo(page).locator('.ghost-opt-line').allTextContents()).toEqual([RICH_DESCRIPTION,SESSION,`Observed ${RICH_RANGE} \u00b7 uploaded 2026-10-03`]);
  await expect(historyStatus(page)).toHaveText('6 messages loaded. Current end of retained archive reached; capture completeness is unknown.');
});

test('CONTRACT FIXTURES: unattributed fragments stay separate with notices and an uncertain count',async({page})=>{
  const opaque=[{id:'opaque-a',sequence:'opaque-a',kind:'assistant',text:'CONTRACT FIXTURE opaque head ',part:0,more:true},{id:'opaque-b',sequence:'opaque-b',kind:'assistant',text:'CONTRACT FIXTURE opaque tail',part:1,more:false}];
  await fixtures(page,{events:(route,call)=>route.fulfill({json:eventsPage(opaque,{node_id:call.node,session_id:call.session})})});
  await open(page);await select(page);
  await expect(rows(page)).toHaveCount(2);
  expect(await rows(page).locator('pre').allTextContents()).toEqual(['CONTRACT FIXTURE opaque head ','CONTRACT FIXTURE opaque tail']);
  for(const index of [0,1])await expect(rows(page).nth(index)).toContainText('could not be attributed to a source entry');
  await expect(historyStatus(page)).toContainText('2 messages loaded (count uncertain).');
  await expect(page.locator('#ghostHistoryTitle')).toContainText('message count uncertain (2 unattributed fragments)');
});

test('CONTRACT FIXTURES: role icon tooltips open on hover and focus, describe their target and close with Escape',async({page})=>{
  await fixtures(page,{events:(route,call)=>route.fulfill({json:eventsPage(RICH,{node_id:call.node,session_id:call.session})})});
  await open(page);await select(page);await expect(rows(page)).toHaveCount(6);
  const tip=page.getByRole('tooltip');
  const user=rows(page).first().getByRole('button',{name:'User message',exact:true});
  await user.hover();await expect(tip).toBeVisible();await expect(tip).toHaveText('User message \u00b7 Oct 3, 2026, 9:05 AM EDT');
  await expect(user).toHaveAttribute('aria-describedby','ghostTooltip');
  await page.mouse.move(0,0);await expect(tip).toBeHidden();expect(await user.getAttribute('aria-describedby')).toBeNull();
  const event=rows(page).nth(4).getByRole('button',{name:'Event',exact:true});
  await event.focus();await expect(tip).toBeVisible();await expect(tip).toHaveText('Event \u00b7 Source time unknown');
  const box=await tip.boundingBox(),viewport=page.viewportSize();
  expect(box.x).toBeGreaterThanOrEqual(0);expect(box.x+box.width).toBeLessThanOrEqual(viewport.width);
  await page.keyboard.press('Escape');await expect(tip).toBeHidden();expect(await event.getAttribute('aria-describedby')).toBeNull();
  await tool(page,'Latest retained').focus();await expect(tip).toContainText('read up to 20 more pages');
  await page.keyboard.press('Escape');await expect(tip).toBeHidden();
});

test.describe('touch',()=>{
  test.use({hasTouch:true});
  test('CONTRACT FIXTURES: touch opens the Session list, selects an option and shows a role tooltip until Escape',async({page})=>{
    const calls=await fixtures(page,{sessions:(route,call)=>route.fulfill({json:sessionPage([{session_id:SESSION,date:'2026-10-03'},{session_id:SECOND,date:'2026-10-02'}],{node_id:call.node})}),events:(route,call)=>route.fulfill({json:eventsPage(call.session===SESSION?RICH:SECOND_EVENTS,{node_id:call.node,session_id:call.session})})});
    await open(page);await page.getByLabel('Node',{exact:true}).selectOption(NODE);await expect(option(page,SECOND)).toHaveCount(1);
    await combo(page).tap();await expect(page.locator('#ghostSessionListbox')).toBeVisible();await expect(combo(page)).toHaveAttribute('aria-expanded','true');
    await option(page,SESSION).tap();await expect(combo(page)).toHaveAttribute('aria-expanded','false');
    await expect(option(page,SESSION)).toHaveAttribute('aria-selected','true');await expect(rows(page)).toHaveCount(6);
    expect(eventsCalls(calls).at(-1)).toMatchObject({node:NODE,session:SESSION,cursor:null});
    const role=rows(page).first().getByRole('button',{name:'User message',exact:true});
    await role.tap();const tip=page.getByRole('tooltip');
    await expect(tip).toBeVisible();await expect(tip).toHaveText('User message \u00b7 Oct 3, 2026, 9:05 AM EDT');
    await expect(role).toHaveAttribute('aria-describedby','ghostTooltip');
    await page.keyboard.press('Escape');await expect(tip).toBeHidden();
    await combo(page).tap();await expect(page.locator('#ghostSessionListbox')).toBeVisible();
    await page.locator('#ghostHistoryTitle').tap();await expect(page.locator('#ghostSessionListbox')).toBeHidden();
    await expect(combo(page)).toHaveAttribute('aria-expanded','false');
  });
});

test('CONTRACT FIXTURES: Latest stops after 20 pages, Oldest and Latest move the scroller, Rescan and Refresh use the right cursors',async({page})=>{
  const calls=await fixtures(page,{events:(route,call)=>{
    const n=call.cursor?Number(call.cursor.slice(1)):0;
    return route.fulfill({json:eventsPage([event(n+1,{text:`CONTRACT FIXTURE page ${n}\n`+'retained line\n'.repeat(8)})],{node_id:call.node,session_id:call.session,next_cursor:n<29?`p${n+1}`:undefined})});
  }});
  await open(page);await select(page);await expect(rows(page)).toHaveCount(1);
  const viewport=page.locator('.ghost-viewport');
  await tool(page,'Latest retained').click();
  await expect(rows(page)).toHaveCount(21);
  await expect(historyStatus(page)).toContainText('Stopped after 20 pages to keep requests bounded; select Latest retained again to continue.');
  expect(eventsCalls(calls)).toHaveLength(21);
  expect(eventsCalls(calls).slice(1).map(c=>c.cursor)).toEqual(Array.from({length:20},(_,i)=>`p${i+1}`));
  // A short log renders every row (no skipped placeholders), so the end position is real.
  await expect(page.locator('#ghostWorkWorkspace ol.ghost-log')).toHaveAttribute('data-render-skip','false');
  await expect.poll(()=>atScrollEnd(viewport)).toBe(true);
  await viewport.scrollIntoViewIfNeeded();await expect(rows(page).last()).toBeInViewport();
  await tool(page,'Oldest loaded').click();await expect.poll(()=>viewport.evaluate(v=>v.scrollTop)).toBe(0);
  await tool(page,'Latest retained').click();await expect(rows(page)).toHaveCount(30);
  await expect(historyStatus(page)).toContainText('Current end of retained archive reached');
  expect(eventsCalls(calls)).toHaveLength(30);
  await tool(page,'Rescan history').click();
  await expect.poll(()=>eventsCalls(calls).length).toBe(31);expect(eventsCalls(calls).at(-1).cursor).toBe(null);
  await expect(rows(page)).toHaveCount(30);await expect(historyStatus(page)).toContainText('More pages remain.');
  await tool(page,'Refresh retained conversation').click();
  await expect.poll(()=>eventsCalls(calls).length).toBe(32);expect(eventsCalls(calls).at(-1).cursor).toBe('p1');
});

test('CONTRACT FIXTURES: action controls expose disabled busy states and ignore activation while a read is pending',async({page})=>{
  let gate=null;const nodeGate={value:null};
  const calls=await fixtures(page,{
    nodes:async route=>{const hold=nodeGate.value;if(hold)await hold.promise;return route.fulfill({json:{nodes:[{node_id:NODE,state:'offline'}]}});},
    events:async(route,call)=>{const hold=gate;if(hold)await hold.promise;return route.fulfill({json:eventsPage([event(1)],{node_id:call.node,session_id:call.session,next_cursor:'later'})});},
  });
  await open(page);await select(page);await expect(rows(page)).toHaveCount(1);
  for(const name of ['Refresh retained conversation','Oldest loaded','Latest retained','Rescan history'])await expect(tool(page,name)).toHaveAttribute('aria-disabled','false');
  gate=deferred();const before=eventsCalls(calls).length;
  await tool(page,'Rescan history').click();
  for(const name of ['Refresh retained conversation','Latest retained','Rescan history'])await expect(tool(page,name)).toHaveAttribute('aria-disabled','true');
  await expect(tool(page,'Oldest loaded')).toHaveAttribute('aria-disabled','false');
  await expect(page.locator('.ghost-viewport')).toHaveAttribute('aria-busy','true');
  await expect(historyStatus(page)).toHaveText('Scanning retained source pages.');
  await expect.poll(()=>eventsCalls(calls).length).toBe(before+1);
  await tool(page,'Latest retained').dispatchEvent('click');await tool(page,'Refresh retained conversation').dispatchEvent('click');await tool(page,'Rescan history').dispatchEvent('click');
  await page.waitForTimeout(100);expect(eventsCalls(calls)).toHaveLength(before+1);
  const release=gate;gate=null;release.resolve();
  await expect(tool(page,'Latest retained')).toHaveAttribute('aria-disabled','false');
  await expect(page.locator('.ghost-viewport')).toHaveAttribute('aria-busy','false');
  const refresh=page.getByRole('button',{name:'Refresh Ghost work',exact:true});
  nodeGate.value=deferred();
  await refresh.click();
  await expect(refresh).toHaveAttribute('aria-disabled','true');await expect(refresh).toHaveAttribute('aria-busy','true');
  await expect(page.locator('#ghostStatus')).toHaveText('Reading authorized Ghost work.');
  const nodesBefore=calls.filter(c=>c.endpoint==='nodes').length;
  await refresh.dispatchEvent('click');await page.waitForTimeout(50);
  expect(calls.filter(c=>c.endpoint==='nodes')).toHaveLength(nodesBefore);
  const nodeRelease=nodeGate.value;nodeGate.value=null;nodeRelease.resolve();
  await expect(refresh).toHaveAttribute('aria-disabled','false');
  await expect.poll(()=>refresh.getAttribute('aria-busy')).toBeNull();
});

test('CONTRACT FIXTURES: the Session combobox supports arrows, Home, End, Enter, Space, Escape and Tab with listbox semantics',async({page})=>{
  const calls=await fixtures(page,{sessions:(route,call)=>route.fulfill({json:sessionPage([{session_id:SESSION,date:'2026-10-03'},{session_id:SECOND,date:'2026-10-02'},{session_id:THIRD,date:'2026-10-01'}],{node_id:call.node})})});
  await open(page);await page.getByLabel('Node',{exact:true}).selectOption(NODE);await expect(option(page,THIRD)).toHaveCount(1);
  // Scope to the Session listbox by accessible name; the page also has an unrelated
  // timezone <datalist> (implicit listbox role) in the hidden job editor.
  const c=combo(page),listbox=page.getByRole('listbox',{name:'Session',exact:true,includeHidden:true});
  await expect(listbox).toHaveCount(1);await expect(listbox).toHaveAttribute('id','ghostSessionListbox');
  await expect(c).toHaveAttribute('aria-haspopup','listbox');await expect(c).toHaveAttribute('aria-controls','ghostSessionListbox');
  await expect(page.locator('#ghostSessionListbox')).toHaveAttribute('role','listbox');
  await expect(page.locator('#ghostSessionListbox [role="option"]')).toHaveCount(3);
  const active=async()=>{const id=await c.getAttribute('aria-activedescendant');expect(id).toBeTruthy();const node=page.locator(`#${id}`);await expect(node).toHaveClass(/is-active/);return node.getAttribute('data-value');};
  await c.focus();await expect(c).toHaveAttribute('aria-expanded','false');
  await page.keyboard.press('ArrowDown');await expect(c).toHaveAttribute('aria-expanded','true');await expect(listbox).toBeVisible();
  expect(await active()).toBe(SESSION);
  await page.keyboard.press('ArrowDown');expect(await active()).toBe(SECOND);
  await page.keyboard.press('End');expect(await active()).toBe(THIRD);
  await page.keyboard.press('ArrowDown');expect(await active()).toBe(THIRD);
  await page.keyboard.press('Home');expect(await active()).toBe(SESSION);
  await page.keyboard.press('ArrowUp');expect(await active()).toBe(SESSION);
  await page.keyboard.press('ArrowDown');await page.keyboard.press('Enter');
  await expect(c).toHaveAttribute('aria-expanded','false');expect(await c.getAttribute('aria-activedescendant')).toBeNull();
  await expect(option(page,SECOND)).toHaveAttribute('aria-selected','true');await expect(option(page,SESSION)).toHaveAttribute('aria-selected','false');
  await expect(c).toBeFocused();await expect(c).toContainText(SECOND);
  await expect.poll(()=>eventsCalls(calls).at(-1)?.session).toBe(SECOND);
  const afterSelect=eventsCalls(calls).length;
  await page.keyboard.press('Space');await expect(c).toHaveAttribute('aria-expanded','true');expect(await active()).toBe(SECOND);
  await page.keyboard.press('ArrowDown');await page.keyboard.press('Escape');
  await expect(c).toHaveAttribute('aria-expanded','false');await expect(option(page,SECOND)).toHaveAttribute('aria-selected','true');
  await page.keyboard.press('Enter');await expect(c).toHaveAttribute('aria-expanded','true');
  await page.keyboard.press('Tab');await expect(c).toHaveAttribute('aria-expanded','false');await expect(c).not.toBeFocused();
  await expect(page.locator('#ghostSessionListbox')).toBeHidden();
  await page.waitForTimeout(50);expect(eventsCalls(calls)).toHaveLength(afterSelect);
  await chooseSession(page,THIRD);await expect(option(page,THIRD)).toHaveAttribute('aria-selected','true');
  await expect.poll(()=>eventsCalls(calls).at(-1)?.session).toBe(THIRD);
});

test('CONTRACT FIXTURES: Node is left of Session on desktop and stacked when narrow, with the checkbox and Refresh inside the Session box',async({page})=>{
  await page.setViewportSize({width:1280,height:900});
  await fixtures(page);await open(page);await select(page);
  for(const text of ['Your personal work','A place for commitments','Ghost work & retained conversations','Owner access only'])await expect(page.locator('main')).not.toContainText(text);
  await expect(page.getByRole('heading',{level:1,name:'Jobs',exact:true})).toHaveCount(1);
  await expect(page.getByText('Provider session',{exact:true})).toHaveCount(0);
  await expect(page.locator('#ghostWorkWorkspace select')).toHaveCount(1);
  await expect(page.getByRole('combobox',{name:/scheduled/i})).toHaveCount(0);
  await expect(page.getByRole('checkbox',{name:'Show Authorized Scheduled Jobs Only',exact:true})).toHaveCount(1);
  const boxes=async()=>({
    node:await page.locator('.ghost-field--node').boundingBox(),box:await page.locator('.ghost-session-box').boundingBox(),
    combo:await page.locator('.ghost-combo').boundingBox(),check:await page.locator('.ghost-check').boundingBox(),refresh:await page.locator('#ghostRefresh').boundingBox(),
  });
  const wide=await boxes();
  expect(wide.node.x+wide.node.width).toBeLessThanOrEqual(wide.box.x+1);
  expect(Math.abs(wide.node.y-wide.box.y)).toBeLessThan(4);
  expect(wide.box.width).toBeGreaterThan(wide.node.width);
  const inside=b=>{expect(b.check.y).toBeGreaterThanOrEqual(b.combo.y+b.combo.height-1);expect(b.refresh.y).toBeGreaterThanOrEqual(b.combo.y+b.combo.height-1);
    expect(b.refresh.x+b.refresh.width).toBeLessThanOrEqual(b.box.x+b.box.width+0.5);expect(b.refresh.x+b.refresh.width).toBeGreaterThan(b.box.x+b.box.width-40);
    expect(b.refresh.y+b.refresh.height).toBeGreaterThan(b.box.y+b.box.height-40);expect(b.refresh.x).toBeGreaterThanOrEqual(b.check.x);};
  inside(wide);
  await page.setViewportSize({width:375,height:812});
  const narrow=await boxes();
  expect(narrow.node.y+narrow.node.height).toBeLessThanOrEqual(narrow.box.y+1);
  inside(narrow);
  expect(await noHorizontalOverflow(page)).toBe(true);
});

test('CONTRACT FIXTURES: scheduled jobs show last-run status and the exact checkbox never shows a fake session binding',async({page})=>{
  await fixtures(page);await open(page);
  const jobs=ws(page).locator('.ghost-job'),notice=page.locator('#ghostScheduledNotice');
  await expect(jobs).toHaveCount(3);
  expect(await ws(page).locator('.ghost-job-status').allTextContents()).toEqual(['Last run succeeded \u00b7 Oct 3, 2026, 9:05 AM EDT','Last run failed \u00b7 time unknown','No recorded run']);
  expect(await ws(page).locator('.ghost-job-name').allTextContents()).toEqual([`CONTRACT-FIXTURE/ok \u00b7 ${NODE}`,`CONTRACT-FIXTURE/failed \u00b7 ${NODE}`,`CONTRACT-FIXTURE/new \u00b7 ${NODE}`]);
  await expect(notice).toHaveText('3 authorized scheduled jobs, not filtered by session. Up to 10 recent run receipts per job; these are not conversation transcripts.');
  const only=page.getByRole('checkbox',{name:'Show Authorized Scheduled Jobs Only',exact:true});
  await only.check();await expect(jobs).toHaveCount(0);await expect(notice).toContainText('Choose a session to show the authorized scheduled jobs bound to it.');
  await select(page);await expect(only).toBeChecked();
  await expect(jobs).toHaveCount(0,{timeout:2000});
  await expect(notice).toContainText('Ghost does not expose a reliable provider-session binding');await expect(notice).toContainText('Nothing is guessed');
  await expect(notice).not.toContainText('bound to this session by Ghost');
  await only.uncheck();await expect(jobs).toHaveCount(3);
});

test('CONTRACT FIXTURES: the same session GUID on another node never inherits a description or observed times',async({page})=>{
  const calls=await fixtures(page,{nodes:route=>route.fulfill({json:{nodes:[{node_id:NODE,state:'offline'},{node_id:OTHER_NODE,state:'online'}]}}),events:(route,call)=>route.fulfill({json:eventsPage(RICH,{node_id:call.node,session_id:call.session})})});
  await open(page);await select(page);await expect(rows(page)).toHaveCount(6);
  await expect(option(page,SESSION)).toContainText(RICH_DESCRIPTION);await expect(option(page,SESSION)).toContainText(`Observed ${RICH_RANGE}`);
  await page.getByLabel('Node',{exact:true}).selectOption(OTHER_NODE);
  await expect(option(page,SESSION)).toHaveCount(1);
  await expect(option(page,SESSION)).toContainText('Description unknown');await expect(option(page,SESSION)).toContainText('Start and stop unknown');
  await expect(option(page,SESSION)).not.toContainText(RICH_DESCRIPTION);await expect(option(page,SESSION)).toHaveAttribute('aria-selected','false');
  await expect(combo(page)).toHaveText('Choose a session');await expect(rows(page)).toHaveCount(0);
  expect(eventsCalls(calls).every(c=>c.node===NODE)).toBe(true);
  await page.getByLabel('Node',{exact:true}).selectOption(NODE);await expect(option(page,SESSION)).toHaveCount(1);
  await expect(option(page,SESSION)).toContainText('Description unknown');
});

test('CONTRACT FIXTURES: empty discovery and an empty archive say what is unknown instead of implying no conversation',async({page})=>{
  await fixtures(page,{nodes:route=>route.fulfill({json:{nodes:[{node_id:NODE,state:'offline'},{node_id:OTHER_NODE,state:'online'}]}}),
    sessions:(route,call)=>route.fulfill({json:sessionPage(call.node===NODE?[]:[{session_id:SESSION,date:'2026-10-03'}],{node_id:call.node})}),
    events:(route,call)=>route.fulfill({json:eventsPage([],{node_id:call.node,session_id:call.session})})});
  await open(page);await page.getByLabel('Node',{exact:true}).selectOption(NODE);
  await expect(page.locator('#ghostStatus')).toHaveText('0 sessions loaded. Retained sessions only; an empty archive does not prove there was no conversation.');
  await expect(combo(page)).toHaveText('No retained sessions loaded');await expect(combo(page)).toHaveAttribute('aria-disabled','true');
  await select(page,SESSION,OTHER_NODE);
  await expect(historyStatus(page)).toHaveText('0 messages loaded. Current end of retained archive reached; capture completeness is unknown.');
  await expect(rows(page)).toHaveCount(0);await expect(tool(page,'Oldest loaded')).toHaveAttribute('aria-disabled','true');
  await expect(page.locator('#ghostHistoryTitle')).toHaveText('Retained conversation, 0 messages');
  await expect(option(page,SESSION)).toContainText('Description unknown');await expect(option(page,SESSION)).toContainText('Start and stop unknown');
});

for(const [status,expected,reply] of [
  [410,'Some archived content expired or is missing',route=>route.fulfill({status:410,json:errorBody('history_expired','CONTRACT FIXTURE expired')})],
  [431,'The server rejected this request as too large (HTTP 431). Loaded content is kept',route=>route.fulfill({status:431,contentType:'text/plain',body:'Request Header Fields Too Large'})],
  [503,'Ghost history is temporarily unavailable',route=>route.fulfill({status:503,json:errorBody('provider_unavailable','CONTRACT FIXTURE unavailable')})],
]){
  test(`CONTRACT FIXTURES: ${status} on a later page keeps loaded content and never claims complete history`,async({page})=>{
    const calls=await fixtures(page,{events:(route,call)=>call.cursor==='second'?reply(route):route.fulfill({json:eventsPage([event(1,{kind:'user',text:'CONTRACT FIXTURE LOADED BEFORE ERROR'})],{node_id:call.node,session_id:call.session,next_cursor:'second'})})});
    await open(page);await select(page);await expect(rows(page)).toHaveCount(1);
    await tool(page,'Refresh retained conversation').click();
    await expect(historyStatus(page)).toContainText(expected);await expect(historyStatus(page)).toHaveAttribute('data-error','true');
    await expect(rows(page)).toHaveCount(1);await expect(rows(page).first()).toContainText('CONTRACT FIXTURE LOADED BEFORE ERROR');
    await expect(ws(page)).not.toContainText('Current end of retained archive reached');
    await expect(page.locator('#ghostHistoryTitle')).toContainText('1 message loaded so far');
    await expect(page.locator('#ghostNode option')).not.toHaveCount(0);await expect(option(page,SESSION)).toHaveCount(1);
    expect(eventsCalls(calls).at(-1).cursor).toBe('second');
    const count=calls.length;await page.evaluate(()=>window.dispatchEvent(new Event('online')));
    await expect.poll(()=>calls.length).toBeGreaterThan(count);
  });
}

test('CONTRACT FIXTURES: 2048-byte cursors travel in POST bodies and the request URL stays short',async({page})=>{
  const EVENTS_CURSOR='E'.repeat(2048),SESSIONS_CURSOR='S'.repeat(2048);
  const calls=await fixtures(page,{
    sessions:(route,call)=>route.fulfill({json:call.cursor===SESSIONS_CURSOR?sessionPage([{session_id:SECOND,date:'2026-10-02'}],{node_id:call.node}):sessionPage([{session_id:SESSION,date:'2026-10-03'}],{node_id:call.node,next_cursor:SESSIONS_CURSOR})}),
    events:(route,call)=>route.fulfill({json:call.cursor===EVENTS_CURSOR?eventsPage([event(2)],{node_id:call.node,session_id:call.session}):eventsPage([event(1)],{node_id:call.node,session_id:call.session,next_cursor:EVENTS_CURSOR})}),
  });
  await open(page);await select(page);
  await page.getByRole('button',{name:'Load more sessions',exact:true}).click();await expect(option(page,SECOND)).toHaveCount(1);
  await tool(page,'Latest retained').click();await expect(rows(page)).toHaveCount(2);
  const reads=calls.filter(c=>['sessions','events'].includes(c.endpoint));
  for(const call of reads){expect(call.method).toBe('POST');expect(call.search).toBe('');expect(call.url.length).toBeLessThan(origin.length+40);expect(call.url).not.toContain('cursor');expect(call.bearer).toBe(true);}
  expect(calls.filter(c=>c.endpoint==='sessions').map(c=>c.body)).toEqual([{node_id:NODE},{node_id:NODE,cursor:SESSIONS_CURSOR}]);
  expect(eventsCalls(calls).map(c=>c.body)).toEqual([{node_id:NODE,session_id:SESSION},{node_id:NODE,session_id:SESSION,cursor:EVENTS_CURSOR}]);
  for(const call of calls.filter(c=>['nodes','jobs'].includes(c.endpoint)))expect(call.method).toBe('GET');
});

test('CONTRACT FIXTURES: a long session stays in its own scroller without horizontal overflow at 320px and with large text',async({page})=>{
  const unbroken='https://contract-fixture.invalid/'+'a'.repeat(4000);
  const start=Date.parse('2026-10-03T13:00:00Z');
  const all=Array.from({length:300},(_,i)=>event(i+1,{kind:['user','assistant','tool_result','command'][i%4],text:i===150?unbroken:`CONTRACT FIXTURE line ${i+1} `+'retained word '.repeat((i%7)*10),timestamp:new Date(start+i*60000).toISOString()}));
  await fixtures(page,{events:(route,call)=>route.fulfill({json:call.cursor==='second'?eventsPage(all.slice(150),{node_id:call.node,session_id:call.session}):eventsPage(all.slice(0,150),{node_id:call.node,session_id:call.session,next_cursor:'second'})})});
  await open(page);await select(page);await tool(page,'Latest retained').click();
  await expect(rows(page)).toHaveCount(300);
  await expect(page.locator('#ghostHistoryTitle')).toContainText('observed 4 hours 59 minutes');await expect(page.locator('#ghostHistoryTitle')).toContainText('300 messages');
  expect(await rows(page).nth(150).locator('pre').textContent()).toBe(unbroken);
  // Long logs skip offscreen rendering for responsiveness; Latest must still land at the real end.
  const scroller=page.locator('.ghost-viewport');
  await expect(page.locator('#ghostWorkWorkspace ol.ghost-log')).toHaveAttribute('data-render-skip','true');
  await expect.poll(()=>atScrollEnd(scroller)).toBe(true);
  await scroller.scrollIntoViewIfNeeded();await expect(rows(page).last()).toBeInViewport();
  for(const [width,height,font] of [[320,640,'100%'],[390,844,'200%']]){
    await page.setViewportSize({width,height});
    await page.evaluate(size=>{document.documentElement.style.fontSize=size;},font);
    await rows(page).nth(150).scrollIntoViewIfNeeded();
    const metrics=await page.evaluate(()=>{
      const v=document.querySelector('.ghost-viewport'),pres=[...document.querySelectorAll('.ghost-log-text')];
      return {doc:document.documentElement.scrollWidth<=innerWidth,own:v.scrollHeight>v.clientHeight,noSideScroll:v.scrollWidth<=v.clientWidth+1,
        pre:pres.every(p=>p.scrollWidth<=p.clientWidth+1),
        right:[...document.querySelectorAll('.ghost-icon-btn,#ghostSession,#ghostRefresh,#ghostNode,.ghost-log-title')].every(e=>e.getBoundingClientRect().right<=innerWidth+0.5)};
    });
    expect(metrics,`${width}px at ${font}`).toEqual({doc:true,own:true,noSideScroll:true,pre:true,right:true});
  }
});

test('SYNTHETIC FIXTURE SCREENSHOTS: labelled desktop/mobile Ghost work and the multiline Session dropdown',async({page,isMobile},testInfo)=>{
  await fixtures(page,{
    sessions:(route,call)=>route.fulfill({json:sessionPage([{session_id:SESSION,date:'2026-10-03'},{session_id:SECOND,date:'2026-10-02'},{session_id:THIRD,date:'2026-10-01'}],{node_id:call.node})}),
    events:(route,call)=>route.fulfill({json:call.session===SECOND?eventsPage(SECOND_EVENTS,{node_id:call.node,session_id:SECOND}):eventsPage(RICH,{node_id:call.node,session_id:call.session})}),
  });
  await open(page);await select(page,SECOND);await expect(rows(page)).toHaveCount(2);
  await chooseSession(page,SESSION);await expect(rows(page)).toHaveCount(6);
  await expect(page.locator('#ghostHistoryTitle')).toHaveText(`Retained conversation, ${RICH_META}`);
  expect(await option(page,SECOND).locator('.ghost-opt-line').allTextContents()).toEqual([SECOND_DESCRIPTION,SECOND,`Observed ${SECOND_RANGE} \u00b7 uploaded 2026-10-02`]);
  expect(await option(page,THIRD).locator('.ghost-opt-line').allTextContents()).toEqual(['Description unknown',THIRD,'Start and stop unknown \u00b7 uploaded 2026-10-01']);
  for(const theme of ['light','dark']){
    await page.evaluate(value=>document.documentElement.dataset.theme=value,theme);
    await axe(page,`${theme} closed`);
    await rows(page).first().getByRole('button',{name:'User message',exact:true}).focus();await expect(page.getByRole('tooltip')).toBeVisible();
    await axe(page,`${theme} tooltip open`);await page.keyboard.press('Escape');
    await combo(page).click();await expect(page.locator('#ghostSessionListbox')).toBeVisible();
    await axe(page,`${theme} Session listbox open`);await page.keyboard.press('Escape');await expect(page.locator('#ghostSessionListbox')).toBeHidden();
  }
  await page.evaluate(()=>{
    const label=document.createElement('div');label.dataset.testFixtureLabel='true';
    label.textContent='TEST FIXTURE \u00b7 synthetic data, not a real conversation';
    Object.assign(label.style,{position:'fixed',left:'8px',bottom:'8px',zIndex:'100',padding:'2px 6px',font:'600 11px/1.4 system-ui,sans-serif',background:'#ffd400',color:'#111',border:'1px solid #111',borderRadius:'3px',pointerEvents:'none'});
    document.body.append(label);
  });
  await expect(page.locator('[data-test-fixture-label]')).toBeVisible();
  // The screenshot must truthfully show retained text: every message must actually be
  // rendered (not skipped by content-visibility) even where it sits below the fold.
  await expect(page.locator('#ghostWorkWorkspace ol.ghost-log')).toHaveAttribute('data-render-skip','false');
  expect(await rows(page).locator('pre').evaluateAll(n=>n.map(p=>typeof p.checkVisibility!=='function'||p.checkVisibility({contentVisibilityAuto:true})))).toEqual([true,true,true,true,true,true]);
  // Real page scroll: bring the log scroller (and its last row) into view the way a
  // reader would. Above 800px the .ln-appbar is sticky and stays pinned at the viewport
  // top; at <=800px the existing jobs.css responsive rule makes it static, so it scrolls
  // away with the document. Either way it is a regression only if it hides the content
  // just revealed.
  const scroller=page.locator('.ghost-viewport'),lastText=rows(page).last().locator('pre'),appbar=page.locator('.ln-appbar');
  await scroller.scrollIntoViewIfNeeded();
  await expect(rows(page).first().locator('pre')).toHaveText(RICH_DESCRIPTION);
  await expect(lastText).toBeInViewport();
  await expect(appbar).toHaveCount(1);await expect(appbar).toBeVisible();
  const appbarState=()=>page.evaluate(()=>{const h=document.querySelector('.ln-appbar'),r=h.getBoundingClientRect();return {position:getComputedStyle(h).position,top:r.top,bottom:r.bottom,scrollY:window.scrollY,scrollable:document.documentElement.scrollHeight>window.innerHeight,narrow:window.matchMedia('(max-width: 800px)').matches,width:window.innerWidth};});
  const scrolled=await appbarState();
  const expectedPosition=scrolled.narrow?'static':'sticky';
  expect(scrolled.scrollable,'document must be taller than the viewport for a real page scroll').toBe(true);
  expect(scrolled.scrollY,'the window itself must have scrolled').toBeGreaterThan(0);
  expect(scrolled.position,`app bar position at ${scrolled.width}px (jobs.css makes it static at <=800px)`).toBe(expectedPosition);
  if(scrolled.narrow)expect(Math.abs(scrolled.top+scrolled.scrollY),'static app bar scrolls away with the document').toBeLessThanOrEqual(1);
  else expect(Math.abs(scrolled.top),'sticky app bar pinned at the viewport top while scrolled').toBeLessThanOrEqual(1);
  // Fail if the app bar genuinely occludes the last retained message the scroll revealed:
  // the part of that text visible inside both the window and the log scroller, below the
  // app bar, must be non-empty and must hit-test to the text itself.
  const occlusion=await lastText.evaluate((pre,headerBottom)=>{
    const r=pre.getBoundingClientRect(),v=pre.closest('.ghost-viewport').getBoundingClientRect();
    const top=Math.max(r.top,v.top,headerBottom,0),bottom=Math.min(r.bottom,v.bottom,window.innerHeight);
    const left=Math.max(r.left,v.left,0),right=Math.min(r.right,v.right,window.innerWidth);
    if(bottom-top<1||right-left<1)return {visibleBelowAppbar:false,hitIsText:false};
    const hit=document.elementFromPoint((left+right)/2,(top+bottom)/2);
    return {visibleBelowAppbar:true,hitIsText:!!hit&&pre.contains(hit)};
  },scrolled.bottom);
  expect(occlusion,'app bar must not cover the last retained message revealed by the scroll').toEqual({visibleBelowAppbar:true,hitIsText:true});
  const kind=isMobile?'mobile':'desktop';
  await expect(page.locator('[data-test-fixture-label]')).toBeVisible();
  const scrolledShot=testInfo.outputPath(`ghost-jobs-${kind}-scrolled-viewport.png`);
  await page.screenshot({path:scrolledShot});
  await testInfo.attach(`ghost-jobs-${kind}-scrolled-viewport`,{path:scrolledShot,contentType:'image/png'});
  // Return the window to the top before the full-page capture so a sticky bar is
  // composited at its in-flow position rather than at the previous scrollY.
  const resetY=await page.evaluate(()=>{window.scrollTo({top:0,left:0,behavior:'instant'});return window.scrollY;});
  expect(resetY,'window scroll reset to the top').toBe(0);
  const atTop=await appbarState();
  expect(atTop.narrow,'viewport width class unchanged').toBe(scrolled.narrow);
  expect(atTop.scrollY).toBe(0);expect(atTop.position).toBe(expectedPosition);
  expect(Math.abs(atTop.top),'app bar at the document top after reset').toBeLessThanOrEqual(1);
  await expect(page.locator('#ghostWorkWorkspace ol.ghost-log')).toHaveAttribute('data-render-skip','false');
  expect(await rows(page).locator('pre').evaluateAll(n=>n.map(p=>typeof p.checkVisibility!=='function'||p.checkVisibility({contentVisibilityAuto:true})))).toEqual([true,true,true,true,true,true]);
  await expect(rows(page).first().locator('pre')).toHaveText(RICH_DESCRIPTION);
  await expect(lastText).toHaveText('CONTRACT FIXTURE summary: the build log lists one file.');
  await expect(page.locator('[data-test-fixture-label]')).toBeVisible();
  const full=testInfo.outputPath(`ghost-jobs-${kind}.png`);
  await page.screenshot({path:full,fullPage:true});
  await testInfo.attach(`ghost-jobs-${kind}`,{path:full,contentType:'image/png'});
  await combo(page).scrollIntoViewIfNeeded();await combo(page).click();await expect(page.locator('#ghostSessionListbox')).toBeVisible();
  expect(await page.locator('#ghostSessionListbox [role="option"]').evaluateAll(n=>n.map(o=>o.querySelectorAll('.ghost-opt-line').length))).toEqual([3,3,3]);
  await expect(page.locator('[data-test-fixture-label]')).toBeVisible();
  const dropdown=testInfo.outputPath(`ghost-session-dropdown-${kind}.png`);
  await page.screenshot({path:dropdown});
  await testInfo.attach(`ghost-session-dropdown-${kind}`,{path:dropdown,contentType:'image/png'});
  await page.keyboard.press('Escape');
});
