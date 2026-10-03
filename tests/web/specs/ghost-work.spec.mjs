import {test,expect} from '@playwright/test';
import AxeBuilder from '@axe-core/playwright';

// Explicitly labeled CONTRACT FIXTURES from retained-history v1, pinned to
// ghost-cli 672c44030d50a738d514b1d9b9ad89c754d12bae. These are not real
// provider conversations and cannot prove that the Ghost deployment is ready.
// Never run this suite against a deployed origin, even with LN_BASE_URL set.
const baseURL=process.env.LN_GHOST_BASE_URL||process.env.LN_JOBS_BASE_URL||'http://127.0.0.1:8797';
const origin=new URL(baseURL).origin;
if(!['127.0.0.1','localhost','[::1]'].includes(new URL(baseURL).hostname))throw new Error('Ghost contract-fixture tests require an explicit isolated loopback origin.');
test.use({baseURL,serviceWorkers:'block'});

const NODE='OFFICEPC',OTHER_NODE='FIXTUREPC';
const SESSION='aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee';
const SECOND='11111111-2222-3333-4444-555555555555';
const event=(n,extra={})=>({id:`fixture-position-${n}`,sequence:`batch/${String(n).padStart(20,'0')}/line/0000`,kind:'assistant',text:`CONTRACT FIXTURE retained text ${n}`,part:0,more:false,...extra});
const sessionPage=(sessions=[],extra={})=>({version:1,node_id:NODE,sessions,next_cursor:'',coverage:'retained_only',...extra});
const eventsPage=(events=[],extra={})=>({version:1,node_id:NODE,session_id:SESSION,events,coverage:'retained_only',gaps:[],resume_cursor:'fixture-resume-end',...extra});
const deferred=()=>{let resolve;const promise=new Promise(r=>resolve=r);return {promise,resolve};};
const errorBody=(code,message)=>({error:{code,message}});

test.beforeEach(async({page})=>{
  await page.route('**/*',route=>new URL(route.request().url()).origin===origin?route.continue():route.abort());
});

async function fixtures(page,{sessions,events,nodes,jobs}={}){
  const calls=[];
  await page.route('**/api/v1/ghost-work/**',async route=>{
    const url=new URL(route.request().url()),endpoint=url.pathname.split('/').at(-1);
    calls.push({endpoint,node:url.searchParams.get('node_id'),session:url.searchParams.get('session_id'),cursor:url.searchParams.get('cursor')});
    expect(route.request().method()).toBe('GET');
    if(endpoint==='nodes')return nodes?nodes(route,url):route.fulfill({json:{nodes:[{node_id:NODE,state:'offline'}],source:'ghost',binding:'explicit_provider_selection',historyAvailability:'check_per_node'}});
    if(endpoint==='jobs')return jobs?jobs(route,url):route.fulfill({json:{events:[{event_id:'fixture-scheduled-job',node:NODE,repo:'CONTRACT-FIXTURE/repo',prompt:'CONTRACT FIXTURE scheduled prompt; never a session binding',last_run_status:'succeeded'}],source:'ghost',runHistoryLimit:10,providerSessionBindingAvailable:false,coverage:'authorized_nodes_only'}});
    if(endpoint==='sessions')return sessions?sessions(route,url):route.fulfill({json:sessionPage([{session_id:SESSION,date:'2026-10-03'}])});
    if(endpoint==='events')return events?events(route,url):route.fulfill({json:eventsPage([event(1)])});
    throw new Error('Unexpected Ghost fixture endpoint '+endpoint);
  });
  return calls;
}
async function open(page){await page.goto('/jobs');await page.getByRole('button',{name:'Ghost work',exact:true}).click();await expect(page.locator('#ghostWorkWorkspace')).toBeVisible();}
async function select(page,session=SESSION){await page.getByLabel('Node',{exact:true}).selectOption(NODE);await expect(page.locator(`#ghostSession option[value="${session}"]`)).toHaveCount(1);await page.getByLabel('Provider session',{exact:true}).selectOption(session);}
const rows=page=>page.locator('#ghostWorkWorkspace .jobs-history-entry');

test('CONTRACT FIXTURES: discovery traverses empty pages, deduplicates sessions, and never guesses a job binding',async({page})=>{
  const calls=await fixtures(page,{sessions:async(route,url)=>{
    const cursor=url.searchParams.get('cursor');
    return route.fulfill({json:cursor==='last'?sessionPage([{session_id:SESSION,date:'2026-10-03'},{session_id:SECOND,date:'2026-10-02'}]):cursor==='empty'?sessionPage([{session_id:SESSION,date:'2026-10-03'}],{next_cursor:'last'}):sessionPage([],{next_cursor:'empty'})});
  }});
  await open(page);await expect(page.locator('#ghostWorkWorkspace')).toContainText('reliable provider-session binding');
  await page.getByLabel('Node',{exact:true}).selectOption(NODE);
  await expect(page.getByRole('button',{name:'Load more sessions',exact:true})).toBeVisible();
  await page.getByRole('button',{name:'Load more sessions',exact:true}).click();
  await expect(page.locator(`#ghostSession option[value="${SESSION}"]`)).toHaveCount(1);
  await page.getByRole('button',{name:'Load more sessions',exact:true}).click();
  await expect(page.getByRole('button',{name:'Load more sessions',exact:true})).toBeHidden();
  await expect(page.locator(`#ghostSession option[value="${SESSION}"]`)).toHaveCount(1);
  await expect(page.locator(`#ghostSession option[value="${SECOND}"]`)).toHaveCount(1);
  expect(calls.filter(c=>c.endpoint==='events')).toHaveLength(0);
  await page.getByLabel('Provider session',{exact:true}).selectOption(SESSION);
  await expect(rows(page)).toHaveCount(1);
  expect(calls.filter(c=>c.endpoint==='events')[0]).toMatchObject({node:NODE,session:SESSION});
  await expect(page.locator('#ghostNode')).toContainText('offline');
  await expect(page.locator('#ghostSession')).toContainText('uploaded 2026-10-03');
  for(const theme of ['light','dark']){
    await page.evaluate(value=>document.documentElement.dataset.theme=value,theme);
    const accessibility=await new AxeBuilder({page}).include('#ghostWorkWorkspace').withTags(['wcag2a','wcag2aa','wcag21aa']).analyze();
    expect(accessibility.violations,`${theme} Ghost work accessibility`).toEqual([]);
    expect(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth)).toBe(true);
  }
});

test('CONTRACT FIXTURES: empty event pages, full UTF-8 fragments, source order, missing time and literal markup remain faithful',async({page})=>{
  const long='CONTRACT FIXTURE large tool result\n'+'🙂'.repeat(12000);
  const literal='<img src=x onerror="window.fixtureExecuted=true">';
  const calls=await fixtures(page,{events:async(route,url)=>{
    const cursor=url.searchParams.get('cursor');
    if(!cursor)return route.fulfill({json:eventsPage([],{next_cursor:'first-content'})});
    if(cursor==='first-content')return route.fulfill({json:eventsPage([event(1,{kind:'tool_result',text:long,call_id:'fixture-call',part:0,more:true,timestamp:'2030-01-01T00:00:00Z'})],{next_cursor:'empty-between'})});
    if(cursor==='empty-between')return route.fulfill({json:eventsPage([],{next_cursor:'tail'})});
    return route.fulfill({json:eventsPage([event(2,{kind:'tool_result',text:'CONTRACT FIXTURE continuation END',call_id:'fixture-call',part:1,timestamp:'2020-01-01T00:00:00Z'}),event(3,{kind:'user',text:literal})],{gaps:['unsupported records: 1']})});
  }});
  await open(page);await select(page);
  await expect(page.locator('#ghostWorkWorkspace')).toContainText('More pages remain.');await expect(rows(page)).toHaveCount(0);
  await page.getByRole('button',{name:'Latest retained',exact:true}).click();await expect(rows(page)).toHaveCount(3);
  expect(await rows(page).first().locator('pre').textContent()).toBe(long);
  expect(await rows(page).evaluateAll(nodes=>nodes.map(n=>n.dataset.eventId))).toEqual(['fixture-position-1','fixture-position-2','fixture-position-3']);
  await expect(rows(page).nth(0)).toContainText('Fragment 1 (continues)');await expect(rows(page).nth(1)).toContainText('Fragment 2');
  await expect(rows(page).nth(2)).toContainText('Source time unavailable');await expect(rows(page).nth(2)).toContainText(literal);
  await expect(rows(page).locator('img')).toHaveCount(0);expect(await page.evaluate(()=>window.fixtureExecuted)).toBeUndefined();
  await expect(page.locator('#ghostWorkWorkspace')).toContainText('unsupported records: 1');await expect(page.locator('#ghostWorkWorkspace')).toContainText('30 days');
  await expect(page.locator('#ghostWorkWorkspace')).toContainText('capture completeness is unknown');
  expect(calls.filter(c=>c.endpoint==='events').map(c=>c.cursor)).toEqual([null,'first-content','empty-between','tail']);
  expect(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth)).toBe(true);
  expect(await rows(page).first().locator('pre').evaluate(e=>({wrap:getComputedStyle(e).whiteSpace,max:getComputedStyle(e).maxHeight}))).toEqual({wrap:'pre-wrap',max:'none'});
});

test('CONTRACT FIXTURES: 503 integration unavailability is explicit and does not become an empty transcript',async({page})=>{
  const unavailable=route=>route.fulfill({status:503,json:errorBody('provider_transport_unavailable','CONTRACT FIXTURE: the upstream GET allowlist is not deployed.')});
  const calls=await fixtures(page,{nodes:unavailable,jobs:unavailable});await open(page);
  await expect(page.locator('#ghostWorkWorkspace')).toContainText('Ghost work is unavailable');
  await expect(page.locator('#ghostWorkWorkspace')).toContainText('integration is not ready');
  await expect(rows(page)).toHaveCount(0);expect(calls.some(c=>['sessions','events'].includes(c.endpoint))).toBe(false);
  await expect(page.locator('#ghostWorkWorkspace')).not.toContainText('Current end of retained archive reached');
});

test('CONTRACT FIXTURES: 403 clears archived text, node/session options and scheduled-job content and stops automatic rescan',async({page})=>{
  let revoked=false;
  const calls=await fixtures(page,{events:route=>revoked?route.fulfill({status:403,json:errorBody('provider_access_denied','CONTRACT FIXTURE owner access revoked')}):route.fulfill({json:eventsPage([event(1,{text:'PRIVATE CONTRACT FIXTURE MARKER'})])})});
  await open(page);await select(page);await expect(rows(page)).toContainText('PRIVATE CONTRACT FIXTURE MARKER');
  revoked=true;await page.getByRole('button',{name:'Rescan history',exact:true}).click();
  await expect(page.locator('#ghostWorkWorkspace')).toContainText('Content cleared');await expect(rows(page)).toHaveCount(0);
  await expect(page.locator('#ghostNode option')).toHaveCount(0);await expect(page.locator('#ghostSession option')).toHaveCount(0);await expect(page.locator('#ghostWorkWorkspace .ghost-jobs')).toBeEmpty();
  await expect(page.locator('#ghostWorkWorkspace')).not.toContainText('PRIVATE CONTRACT FIXTURE MARKER');
  const count=calls.length;await page.evaluate(()=>{window.dispatchEvent(new Event('online'));document.dispatchEvent(new Event('visibilitychange'));});
  await page.waitForTimeout(50);expect(calls).toHaveLength(count);
});

test('CONTRACT FIXTURES: 409 prompts a beginning rescan that replaces same-ID DOM text and removes expired entries',async({page})=>{
  let phase='initial';const calls=await fixtures(page,{events:(route,url)=>{
    if(phase==='initial')return route.fulfill({json:eventsPage([event(2,{text:'OLD CONTRACT FIXTURE BYTES'}),event(3,{text:'EXPIRED CONTRACT FIXTURE BYTES'})])});
    if(url.searchParams.has('cursor'))return route.fulfill({status:409,json:errorBody('history_rescan_required','CONTRACT FIXTURE source digest changed')});
    return route.fulfill({json:eventsPage([event(1,{text:'LATE CONTRACT FIXTURE UPLOAD'}),event(2,{text:'REPLACEMENT CONTRACT FIXTURE BYTES'})],{gaps:['malformed records: 1']})});
  }});
  await open(page);await select(page);await expect(rows(page)).toHaveCount(2);
  phase='changed';await page.getByRole('button',{name:'Latest retained',exact:true}).click();await expect(page.locator('#ghostWorkWorkspace')).toContainText('Refresh to rescan from the beginning');
  await page.getByRole('button',{name:'Rescan history',exact:true}).click();
  await expect(rows(page)).toHaveCount(2);await expect(rows(page).first()).toContainText('LATE CONTRACT FIXTURE UPLOAD');
  await expect(rows(page).nth(1)).toContainText('REPLACEMENT CONTRACT FIXTURE BYTES');await expect(page.locator('#ghostWorkWorkspace')).not.toContainText('EXPIRED CONTRACT FIXTURE BYTES');
  await expect(page.locator('#ghostWorkWorkspace')).not.toContainText('OLD CONTRACT FIXTURE BYTES');
  expect(calls.filter(c=>c.endpoint==='events').at(-1).cursor).toBe(null);
});

test('CONTRACT FIXTURES: switching nodes during discovery ignores the old response without blocking the new node',async({page})=>{
  const entered=deferred(),release=deferred();
  await fixtures(page,{nodes:route=>route.fulfill({json:{nodes:[{node_id:NODE,state:'offline'},{node_id:OTHER_NODE,state:'online'}]}}),sessions:async(route,url)=>{
    if(url.searchParams.get('node_id')===NODE){entered.resolve();await release.promise;return route.fulfill({json:sessionPage([{session_id:SESSION,date:'2026-10-03'}])});}
    return route.fulfill({json:sessionPage([{session_id:SECOND,date:'2026-10-03'}],{node_id:OTHER_NODE})});
  }});
  await open(page);await page.getByLabel('Node',{exact:true}).selectOption(NODE);await entered.promise;
  await page.getByLabel('Node',{exact:true}).selectOption(OTHER_NODE);
  await expect(page.locator(`#ghostSession option[value="${SECOND}"]`)).toHaveCount(1);
  release.resolve();await page.waitForTimeout(50);
  await expect(page.locator(`#ghostSession option[value="${SESSION}"]`)).toHaveCount(0);await expect(page.locator('#ghostNode')).toHaveValue(OTHER_NODE);
});

test('CONTRACT FIXTURES: Refresh supersedes pending discovery and preserves the actual selected provider session',async({page})=>{
  const entered=deferred(),release=deferred();let discovery=0;
  const calls=await fixtures(page,{sessions:async route=>{
    discovery++;if(discovery===1){entered.resolve();await release.promise;}
    return route.fulfill({json:sessionPage([{session_id:SESSION,date:'2026-10-03'}])});
  }});
  await open(page);await page.getByLabel('Node',{exact:true}).selectOption(NODE);await entered.promise;
  await page.getByRole('button',{name:'Refresh Ghost work',exact:true}).click();
  await expect(page.locator(`#ghostSession option[value="${SESSION}"]`)).toHaveCount(1);
  release.resolve();await page.getByLabel('Provider session',{exact:true}).selectOption(SESSION);await expect(rows(page)).toHaveCount(1);
  const beforeRefresh=calls.filter(c=>c.endpoint==='events').length;
  await page.getByRole('button',{name:'Refresh Ghost work',exact:true}).click();
  await expect(page.locator('#ghostSession')).toHaveValue(SESSION);await expect(rows(page)).toHaveCount(1);
  await expect.poll(()=>calls.filter(c=>c.endpoint==='events').length).toBeGreaterThan(beforeRefresh);
  expect(calls.filter(c=>c.endpoint==='events').at(-1)).toMatchObject({node:NODE,session:SESSION,cursor:null});
});

test('CONTRACT FIXTURES: a preserved session selection becomes one real option when rediscovered on a later page',async({page})=>{
  let refreshed=false;
  await fixtures(page,{sessions:(route,url)=>route.fulfill({json:!refreshed?sessionPage([{session_id:SESSION,date:'2026-10-01'}]):url.searchParams.has('cursor')?sessionPage([{session_id:SESSION,date:'2026-10-03'}]):sessionPage([{session_id:SECOND,date:'2026-10-02'}],{next_cursor:'selected-later'})})});
  await open(page);await select(page);await expect(rows(page)).toHaveCount(1);
  refreshed=true;await page.getByRole('button',{name:'Refresh Ghost work',exact:true}).click();
  await expect(page.locator(`#ghostSession option[value="${SESSION}"]`)).toContainText('previously selected');
  await expect(page.locator('#ghostSession')).toHaveValue(SESSION);
  await page.getByRole('button',{name:'Load more sessions',exact:true}).click();
  await expect(page.getByRole('button',{name:'Load more sessions',exact:true})).toBeHidden();
  await expect(page.locator(`#ghostSession option[value="${SESSION}"]`)).toHaveCount(1);
  await expect(page.locator(`#ghostSession option[value="${SESSION}"]`)).toContainText('uploaded 2026-10-03');
  await expect(page.locator('#ghostSession')).toHaveValue(SESSION);
  await expect(page.locator('#ghostWorkWorkspace')).toContainText('2 provider sessions loaded.');
});
