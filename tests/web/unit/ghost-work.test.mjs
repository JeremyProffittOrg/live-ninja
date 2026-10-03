import test from 'node:test';
import assert from 'node:assert/strict';
import {createGhostHistory, mergeGhostEvents} from '../../../web/static/js/ghost-work.mjs';

// Contract fixtures, not captured conversations. Canonical retained-history v1:
// ghost-cli 672c44030d50a738d514b1d9b9ad89c754d12bae.
const NODE='OFFICEPC';
const SESSION='aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee';
const OTHER_SESSION='11111111-2222-3333-4444-555555555555';
const event=(n,extra={})=>({id:`fixture-source-${n}`,sequence:`batch/${String(n).padStart(20,'0')}/line/0000`,kind:'assistant',text:`CONTRACT FIXTURE visible text ${n}`,part:0,more:false,...extra});
const page=(events=[],extra={})=>({version:1,node_id:NODE,session_id:SESSION,events,coverage:'retained_only',gaps:[],resume_cursor:'resume-end',...extra});
const failure=status=>Object.assign(new Error(`CONTRACT FIXTURE HTTP ${status}`),{status});
const deferred=()=>{let resolve,reject;const promise=new Promise((a,b)=>{resolve=a;reject=b;});return {promise,resolve,reject};};
const query=path=>new URL(path,'http://localhost').searchParams;

test('string source positions sort lexically without numeric conversion or timestamp ordering',()=>{
  const early=event(1,{sequence:'archive/90071992547409930001/line/0001',timestamp:'2030-01-02T00:00:00Z'});
  const late=event(2,{sequence:'archive/90071992547409930002/line/0001',timestamp:'2020-01-01T00:00:00Z'});
  const merged=mergeGhostEvents([],[late,early]);
  assert.deepEqual(merged.map(e=>e.id),[early.id,late.id]);
  assert.equal(merged[0].sequence,early.sequence);
  assert.equal(typeof merged[0].sequence,'string');
});

test('stable IDs deduplicate retries but mirrored text in distinct source records survives',()=>{
  const first=event(1),mirrored=event(2,{text:first.text});
  assert.equal(mergeGhostEvents([first],[{...first},mirrored]).length,2);
  assert.throws(()=>mergeGhostEvents([first],[{...first,text:'Changed source bytes'}]),/changed|rescan/i);
  assert.throws(()=>mergeGhostEvents([],[{...first,sequence:1}]),/invalid/i);
});

test('all pages retain over 600 fragments and long UTF-8 output without a render retention cap',async()=>{
  const fragments=Array.from({length:620},(_,i)=>event(i+1));
  fragments[5]=event(6,{kind:'tool_result',call_id:'fixture-call',part:0,more:true,text:'🙂'.repeat(12000)});
  fragments[6]=event(7,{kind:'tool_result',call_id:'fixture-call',part:1,more:false,text:'END_OF_LONG_CONTRACT_FIXTURE'});
  const paths=[];
  const loader=createGhostHistory(async path=>{
    paths.push(path);const offset=Number(query(path).get('cursor')||0),end=Math.min(offset+80,fragments.length);
    return page(fragments.slice(offset,end),{next_cursor:end<fragments.length?String(end):undefined,resume_cursor:'620-end'});
  });
  await loader.select(NODE,SESSION);await loader.latest();
  const state=loader.state();
  assert.equal(state.events.length,620);assert.equal(state.reachedEnd,true);assert.equal(state.next,'');
  assert.equal(state.events[5].text,fragments[5].text);assert.equal(Buffer.byteLength(state.events[5].text),48000);
  assert.equal(state.events[5].more,true);assert.equal(state.events[6].part,1);assert.equal(state.events[6].text,'END_OF_LONG_CONTRACT_FIXTURE');
  assert.equal(paths.length,8);
});

test('empty pages with continuation are followed and never called a complete empty history',async()=>{
  const answers=[page([],{next_cursor:'empty-page-2'}),page([],{next_cursor:'data-page'}),page([event(1)])];
  const paths=[];const loader=createGhostHistory(async path=>{paths.push(path);return answers.shift();});
  await loader.select(NODE,SESSION);
  assert.equal(loader.state().events.length,0);assert.equal(loader.state().reachedEnd,false);
  await loader.latest();
  assert.equal(paths.length,3);assert.equal(query(paths[1]).get('cursor'),'empty-page-2');assert.equal(query(paths[2]).get('cursor'),'data-page');
  assert.equal(loader.state().events.length,1);assert.equal(loader.state().reachedEnd,true);
});

test('bounded catch-up exposes continuation and can finish without dropping prior pages',async()=>{
  const loader=createGhostHistory(async path=>{
    const offset=Number(query(path).get('cursor')||0);
    return page([event(offset+1)],{next_cursor:offset<25?String(offset+1):undefined});
  });
  await loader.select(NODE,SESSION);await loader.latest();
  assert.equal(loader.state().events.length,21);assert.equal(loader.state().reachedEnd,false);assert.equal(loader.state().next,'21');
  await loader.latest();assert.equal(loader.state().events.length,26);assert.equal(loader.state().reachedEnd,true);
});

test('empty current-end polling retains its resume cursor without treating it as a stuck next cursor',async()=>{
  const paths=[];const answers=[page([event(1)],{resume_cursor:'end-a'}),page([],{resume_cursor:'end-a'}),page([event(2)],{resume_cursor:'end-b'})];
  const loader=createGhostHistory(async path=>{paths.push(path);return answers.shift();});
  await loader.select(NODE,SESSION);await loader.poll();
  assert.equal(loader.state().error,'');assert.equal(loader.state().resume,'end-a');assert.equal(loader.state().events.length,1);
  await loader.poll();assert.equal(query(paths[1]).get('cursor'),'end-a');assert.equal(query(paths[2]).get('cursor'),'end-a');
  assert.equal(loader.state().resume,'end-b');assert.deepEqual(loader.state().events.map(e=>e.id),[event(1).id,event(2).id]);
});

test('refresh rescans from the beginning and includes late uploads before the prior source cursor',async()=>{
  const paths=[];const answers=[page([event(20)]),page([event(1)],{next_cursor:'scan-last'}),page([event(20)])];
  const loader=createGhostHistory(async path=>{paths.push(path);return answers.shift();});
  await loader.select(NODE,SESSION);await loader.rescan();
  assert.equal(query(paths[1]).has('cursor'),false);assert.equal(loader.state().reachedEnd,false);
  await loader.latest();assert.deepEqual(loader.state().events.map(e=>e.id),[event(1).id,event(20).id]);
});

test('409 recovery replaces changed same-ID bytes and prunes expired cached records only when rescan completes',async()=>{
  const previous=event(1),expired=event(2),replacement={...previous,text:'CONTRACT FIXTURE replacement after source digest changed'};
  const paths=[];const answers=[page([previous,expired]),failure(409),page([replacement],{next_cursor:'scan-end'}),page([event(3)])];
  const loader=createGhostHistory(async path=>{paths.push(path);const answer=answers.shift();if(answer instanceof Error)throw answer;return answer;});
  await loader.select(NODE,SESSION);await loader.poll();
  assert.match(loader.state().error,/changed.*rescan/i);assert.equal(loader.state().reachedEnd,false);assert.equal(loader.state().events.length,2);
  await loader.rescan();assert.equal(query(paths[2]).has('cursor'),false);
  assert.ok(loader.state().events.some(e=>e.id===expired.id),'incomplete rescan must not silently discard cached content');
  await loader.latest();
  assert.deepEqual(loader.state().events.map(e=>e.id),[previous.id,event(3).id]);assert.equal(loader.state().events[0].text,replacement.text);
  assert.equal(loader.state().error,'');assert.equal(loader.state().reachedEnd,true);
});

test('changed duplicates within one scan fail instead of silently overwriting an already observed source entry',async()=>{
  const first=event(1);const answers=[page([first],{next_cursor:'second'}),page([{...first,text:'Inconsistent retry of this same scan'}])];
  const loader=createGhostHistory(async()=>answers.shift());await loader.select(NODE,SESSION);await loader.more();
  assert.match(loader.state().error,/changed|rescan/i);assert.equal(loader.state().reachedEnd,false);
  assert.equal(loader.state().events[0].text,first.text);
});

test('non-authorization read failures preserve loaded bytes and never imply complete history',async t=>{
  for(const status of [410,413,422,429,503])await t.test(String(status),async()=>{
    let calls=0;const loader=createGhostHistory(async()=>{if(calls++)throw failure(status);return page([event(1)],{gaps:['CONTRACT FIXTURE unsupported record: 1']});});
    await loader.select(NODE,SESSION);await loader.poll();
    assert.equal(loader.state().events[0].text,event(1).text);assert.equal(loader.state().reachedEnd,false);assert.ok(loader.state().error);
    assert.equal(loader.state().resume,'resume-end');assert.equal(loader.state().gaps[0],'CONTRACT FIXTURE unsupported record: 1');
    const afterError=calls;await loader.poll();await loader.poll();assert.equal(calls,afterError,'automatic polling must stop after an actionable read failure');
  });
});

test('401, 403, and lost authentication clear private content, cursors, and automatic requests',async t=>{
  for(const error of [failure(401),failure(403),Object.assign(new Error('lost'),{name:'AuthLostError'})])await t.test(error.name==='AuthLostError'?'lost auth':String(error.status),async()=>{
    let calls=0,denied=0;const loader=createGhostHistory(async()=>{if(calls++)throw error;return page([event(1)],{gaps:['private fixture gap']});},()=>{},()=>denied++);
    await loader.select(NODE,SESSION);await loader.poll();const count=calls,state=loader.state();
    assert.deepEqual(state.events,[]);assert.deepEqual(state.gaps,[]);assert.equal(state.node,'');assert.equal(state.session,'');assert.equal(state.next,'');assert.equal(state.resume,'');assert.equal(state.busy,false);assert.equal(denied,1);
    await loader.poll();await loader.more();await loader.latest();assert.equal(calls,count);
  });
});

test('late responses and denials from the previous selection cannot replace or clear the current scope',async t=>{
  for(const rejectOld of [false,true])await t.test(rejectOld?'late forbidden':'late success',async()=>{
    const pending=deferred();let denied=0;
    const loader=createGhostHistory(path=>query(path).get('session_id')===SESSION?pending.promise:Promise.resolve(page([event(8)],{session_id:OTHER_SESSION})),()=>{},()=>denied++);
    const old=loader.select(NODE,SESSION);await loader.select(NODE,OTHER_SESSION);
    if(rejectOld)pending.reject(failure(403));else pending.resolve(page([event(1)]));await old;
    assert.equal(loader.state().session,OTHER_SESSION);assert.deepEqual(loader.state().events.map(e=>e.id),[event(8).id]);assert.equal(denied,0);
  });
});

test('node, session, version, and coverage mismatches fail without accepting foreign fragments',async t=>{
  for(const patch of [{node_id:'OTHERPC'},{session_id:OTHER_SESSION},{version:2},{coverage:'complete'}])await t.test(JSON.stringify(patch),async()=>{
    const loader=createGhostHistory(async()=>page([event(1)],patch));await loader.select(NODE,SESSION);
    assert.equal(loader.state().events.length,0);assert.equal(loader.state().reachedEnd,false);assert.match(loader.state().error,/does not match/i);
  });
});

test('a non-advancing continuation stops with an actionable error',async()=>{
  const answers=[page([event(1)],{next_cursor:'same'}),page([],{next_cursor:'same'})];
  const loader=createGhostHistory(async()=>answers.shift());await loader.select(NODE,SESSION);await loader.more();
  assert.match(loader.state().error,/cursor.*advance.*rescan/i);assert.equal(loader.state().reachedEnd,false);assert.equal(loader.state().events.length,1);
});

test('gaps persist across pages and absent timestamps remain absent',async()=>{
  const answers=[page([event(1)],{next_cursor:'next',gaps:['unsupported records: 1']}),page([event(2)],{gaps:['malformed records: 2']})];
  const loader=createGhostHistory(async()=>answers.shift());await loader.select(NODE,SESSION);await loader.more();
  assert.deepEqual(loader.state().gaps,['unsupported records: 1','malformed records: 2']);
  assert.equal(Object.hasOwn(loader.state().events[0],'timestamp'),false);
});
