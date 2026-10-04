import test from 'node:test';
import assert from 'node:assert/strict';
import {
  createGhostHistory, mergeGhostEvents, GHOST_LATEST_PAGE_LIMIT, ghostSourceIdentity, groupGhostMessages,
  ghostObservedSpan, ghostLogSummary, formatDuration, formatGhostTime, formatGhostRange, ghostRoleLabel,
  describeGhostSession, scheduledForSession, scheduledRunLabel,
} from '../../../web/static/js/ghost-work.mjs';

// Contract fixtures, not captured conversations. Canonical retained-history v1:
// ghost-cli 672c44030d50a738d514b1d9b9ad89c754d12bae. Source IDs use the pinned
// provider format key:%09d(line):%05d(block):%05d(part) with id === sequence.
// Expected strings below are written out literally; they are not recomputed with
// the helpers under test.
const NODE='OFFICEPC';
const SESSION='aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee';
const OTHER_SESSION='11111111-2222-3333-4444-555555555555';
const TZ='America/New_York';
const EVENTS_PATH='/api/v1/ghost-work/events';
const KEY=`history/${NODE}/2026-10-03/${SESSION}.jsonl`;
const pad=(n,width)=>String(n).padStart(width,'0');
const sourceID=(line,block=0,part=0,key=KEY)=>`${key}:${pad(line,9)}:${pad(block,5)}:${pad(part,5)}`;
const event=(line,{key=KEY,block=0,part=0,...extra}={})=>{const id=sourceID(line,block,part,key);return {id,sequence:id,kind:'assistant',text:`CONTRACT FIXTURE visible text ${line}`,part,more:false,...extra};};
const page=(events=[],extra={})=>({version:1,node_id:NODE,session_id:SESSION,events,coverage:'retained_only',gaps:[],resume_cursor:'resume-end',...extra});
const failure=(status,code='')=>Object.assign(new Error(`CONTRACT FIXTURE HTTP ${status}`),{status,code});
const deferred=()=>{let resolve,reject;const promise=new Promise((a,b)=>{resolve=a;reject=b;});return {promise,resolve,reject};};
// Records every archive request exactly as the loader issued it (path, method, JSON body).
function archive(answer){
  const calls=[];
  const request=async(path,options={})=>{
    const body=options.json&&typeof options.json==='object'?{...options.json}:undefined;
    calls.push({path,method:options.method,body});
    const result=await answer(body||{},calls.length-1);
    if(result instanceof Error)throw result;
    return result;
  };
  return {calls,request};
}
const cursor=call=>call.body?.cursor;

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

test('archive page reads are POST JSON with the opaque cursor only in the body and a short request path',async()=>{
  const BIG='C'.repeat(2048);
  const {calls,request}=archive(body=>body.cursor===BIG?page([event(2)]):page([event(1)],{next_cursor:BIG}));
  const loader=createGhostHistory(request);await loader.select(NODE,SESSION);await loader.more();
  assert.deepEqual(calls.map(c=>c.path),[EVENTS_PATH,EVENTS_PATH]);
  for(const call of calls){assert.equal(call.method,'POST');assert.equal(call.path.includes('?'),false);assert.ok(call.path.length<64);assert.equal(call.path.includes(BIG),false);}
  assert.deepEqual(calls[0].body,{node_id:NODE,session_id:SESSION});
  assert.deepEqual(calls[1].body,{node_id:NODE,session_id:SESSION,cursor:BIG});
  assert.deepEqual(loader.state().events.map(e=>e.id),[event(1).id,event(2).id]);
});

test('all pages retain over 600 fragments and long UTF-8 output without a render retention cap',async()=>{
  const fragments=Array.from({length:620},(_,i)=>event(i+1));
  fragments[5]=event(6,{kind:'tool_result',call_id:'fixture-call',part:0,more:true,text:'\u{1F642}'.repeat(12000)});
  fragments[6]=event(6,{kind:'tool_result',call_id:'fixture-call',part:1,more:false,text:'END_OF_LONG_CONTRACT_FIXTURE'});
  const {calls,request}=archive(body=>{
    const offset=Number(body.cursor||0),end=Math.min(offset+80,fragments.length);
    return page(fragments.slice(offset,end),{next_cursor:end<fragments.length?String(end):undefined,resume_cursor:'620-end'});
  });
  const loader=createGhostHistory(request);
  await loader.select(NODE,SESSION);await loader.latest();
  const state=loader.state();
  assert.equal(state.events.length,620);assert.equal(state.reachedEnd,true);assert.equal(state.next,'');
  assert.equal(state.events[5].text,fragments[5].text);assert.equal(Buffer.byteLength(state.events[5].text),48000);
  assert.equal(state.events[5].more,true);assert.equal(state.events[6].part,1);assert.equal(state.events[6].text,'END_OF_LONG_CONTRACT_FIXTURE');
  assert.equal(calls.length,8);assert.ok(calls.every(c=>c.method==='POST'&&c.path===EVENTS_PATH));
  const messages=groupGhostMessages(state.events);
  assert.equal(messages.length,619,'the two parts of one source entry are one logical message');
  assert.equal(messages[5].text,'\u{1F642}'.repeat(12000)+'END_OF_LONG_CONTRACT_FIXTURE');
  assert.deepEqual(messages[5].ids,[fragments[5].id,fragments[6].id]);
});

test('empty pages with continuation are followed and never called a complete empty history',async()=>{
  const answers=[page([],{next_cursor:'empty-page-2'}),page([],{next_cursor:'data-page'}),page([event(1)])];
  const {calls,request}=archive(()=>answers.shift());const loader=createGhostHistory(request);
  await loader.select(NODE,SESSION);
  assert.equal(loader.state().events.length,0);assert.equal(loader.state().reachedEnd,false);
  await loader.latest();
  assert.equal(calls.length,3);assert.equal(cursor(calls[1]),'empty-page-2');assert.equal(cursor(calls[2]),'data-page');
  assert.equal(loader.state().events.length,1);assert.equal(loader.state().reachedEnd,true);
});

test('bounded catch-up exposes continuation and can finish without dropping prior pages',async()=>{
  const {request}=archive(body=>{
    const offset=Number(body.cursor||0);
    return page([event(offset+1)],{next_cursor:offset<25?String(offset+1):undefined});
  });
  const loader=createGhostHistory(request);
  await loader.select(NODE,SESSION);await loader.latest();
  assert.equal(loader.state().events.length,21);assert.equal(loader.state().reachedEnd,false);assert.equal(loader.state().next,'21');
  await loader.latest();assert.equal(loader.state().events.length,26);assert.equal(loader.state().reachedEnd,true);
});

test('Latest retained reads exactly 20 pages per activation',async()=>{
  const {calls,request}=archive(body=>{const n=Number(body.cursor||0);return page([event(n+1)],{next_cursor:String(n+1)});});
  const loader=createGhostHistory(request);await loader.select(NODE,SESSION);
  const before=calls.length;await loader.latest();
  assert.equal(GHOST_LATEST_PAGE_LIMIT,20);
  assert.equal(calls.length-before,20);
  assert.deepEqual(calls.slice(before).map(cursor),Array.from({length:20},(_,i)=>String(i+1)));
  assert.equal(loader.state().next,'21');assert.equal(loader.state().reachedEnd,false);assert.equal(loader.state().error,'');
});

test('empty current-end polling retains its resume cursor without treating it as a stuck next cursor',async()=>{
  const answers=[page([event(1)],{resume_cursor:'end-a'}),page([],{resume_cursor:'end-a'}),page([event(2)],{resume_cursor:'end-b'})];
  const {calls,request}=archive(()=>answers.shift());const loader=createGhostHistory(request);
  await loader.select(NODE,SESSION);await loader.poll();
  assert.equal(loader.state().error,'');assert.equal(loader.state().resume,'end-a');assert.equal(loader.state().events.length,1);
  await loader.poll();assert.equal(cursor(calls[1]),'end-a');assert.equal(cursor(calls[2]),'end-a');
  assert.equal(loader.state().resume,'end-b');assert.deepEqual(loader.state().events.map(e=>e.id),[event(1).id,event(2).id]);
});

test('refresh continues the next page, then reads after the current end, and does nothing without a selection',async()=>{
  const answers=[page([event(1)],{next_cursor:'next-a'}),page([event(2)],{resume_cursor:'end-b'}),page([event(3)],{resume_cursor:'end-c'})];
  const {calls,request}=archive(()=>answers.shift());const loader=createGhostHistory(request);
  await loader.refresh();assert.equal(calls.length,0);
  await loader.select(NODE,SESSION);await loader.refresh();
  assert.equal(cursor(calls[1]),'next-a');
  await loader.refresh();assert.equal(cursor(calls[2]),'end-b');
  assert.deepEqual(loader.state().events.map(e=>e.id),[event(1).id,event(2).id,event(3).id]);
});

test('rescan reads from the beginning and includes late uploads before the prior source cursor',async()=>{
  const answers=[page([event(20)]),page([event(1)],{next_cursor:'scan-last'}),page([event(20)])];
  const {calls,request}=archive(()=>answers.shift());const loader=createGhostHistory(request);
  await loader.select(NODE,SESSION);await loader.rescan();
  assert.equal('cursor' in calls[1].body,false);assert.equal(loader.state().reachedEnd,false);
  await loader.latest();assert.deepEqual(loader.state().events.map(e=>e.id),[event(1).id,event(20).id]);
});

test('409 recovery replaces changed same-ID bytes and prunes expired cached records only when rescan completes',async()=>{
  const previous=event(1),expired=event(2),replacement={...previous,text:'CONTRACT FIXTURE replacement after source digest changed'};
  const answers=[page([previous,expired]),failure(409),page([replacement],{next_cursor:'scan-end'}),page([event(3)])];
  const {calls,request}=archive(()=>answers.shift());const loader=createGhostHistory(request);
  await loader.select(NODE,SESSION);await loader.poll();
  assert.match(loader.state().error,/changed.*rescan/i);assert.equal(loader.state().reachedEnd,false);assert.equal(loader.state().events.length,2);
  await loader.rescan();assert.equal('cursor' in calls[2].body,false);
  assert.ok(loader.state().events.some(e=>e.id===expired.id),'incomplete rescan must not silently discard cached content');
  await loader.latest();
  assert.deepEqual(loader.state().events.map(e=>e.id),[previous.id,event(3).id]);assert.equal(loader.state().events[0].text,replacement.text);
  assert.equal(loader.state().error,'');assert.equal(loader.state().reachedEnd,true);
});

test('changed duplicates within one scan fail instead of silently overwriting an already observed source entry',async()=>{
  const first=event(1);const answers=[page([first],{next_cursor:'second'}),page([{...first,text:'Inconsistent retry of this same scan'}])];
  const {request}=archive(()=>answers.shift());const loader=createGhostHistory(request);await loader.select(NODE,SESSION);await loader.more();
  assert.match(loader.state().error,/changed|rescan/i);assert.equal(loader.state().reachedEnd,false);
  assert.equal(loader.state().events[0].text,first.text);
});

test('non-authorization read failures preserve loaded bytes, explain the failure and never imply complete history',async t=>{
  const cases=[
    [410,'',/expired or is missing/],
    [413,'history_object_too_large',/larger than Ghost supports/],
    [413,'request_too_large',/page request was larger than the server accepts/],
    [422,'',/could not be decoded/],
    [429,'',/rate limited/],
    [431,'',/HTTP 431\)\. Loaded content is kept/],
    [503,'',/temporarily unavailable/],
  ];
  for(const [status,code,message] of cases)await t.test(`${status}${code?' '+code:''}`,async()=>{
    let calls=0;const loader=createGhostHistory(async()=>{if(calls++)throw failure(status,code);return page([event(1)],{gaps:['CONTRACT FIXTURE unsupported record: 1']});});
    await loader.select(NODE,SESSION);await loader.poll();
    assert.equal(loader.state().events[0].text,event(1).text);assert.equal(loader.state().reachedEnd,false);assert.match(loader.state().error,message);
    assert.equal(loader.state().resume,'resume-end');assert.equal(loader.state().gaps[0],'CONTRACT FIXTURE unsupported record: 1');
    assert.equal(loader.state().node,NODE);assert.equal(loader.state().session,SESSION);
    const afterError=calls;await loader.poll();await loader.poll();assert.equal(calls,afterError,'automatic polling must stop after an actionable read failure');
  });
});

test('401, 403, and lost authentication clear private content, cursors, and automatic requests',async t=>{
  for(const error of [failure(401),failure(403),Object.assign(new Error('lost'),{name:'AuthLostError'})])await t.test(error.name==='AuthLostError'?'lost auth':String(error.status),async()=>{
    let calls=0,denied=0;const loader=createGhostHistory(async()=>{if(calls++)throw error;return page([event(1)],{gaps:['private fixture gap']});},()=>{},()=>denied++);
    await loader.select(NODE,SESSION);await loader.poll();const count=calls,state=loader.state();
    assert.deepEqual(state.events,[]);assert.deepEqual(state.gaps,[]);assert.equal(state.node,'');assert.equal(state.session,'');assert.equal(state.next,'');assert.equal(state.resume,'');assert.equal(state.busy,false);assert.equal(denied,1);
    assert.match(state.error,/Content cleared/);
    await loader.poll();await loader.more();await loader.latest();await loader.refresh();assert.equal(calls,count);
  });
});

test('late responses and denials from the previous selection cannot replace or clear the current scope',async t=>{
  for(const rejectOld of [false,true])await t.test(rejectOld?'late forbidden':'late success',async()=>{
    const pending=deferred();let denied=0;
    const {request}=archive(body=>body.session_id===SESSION?pending.promise:page([event(8)],{session_id:OTHER_SESSION}));
    const loader=createGhostHistory(request,()=>{},()=>denied++);
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

test('pinned source identity is read only from key:9-digit line:5-digit block:5-digit part with id equal to sequence',()=>{
  const id=sourceID(42,3,7);
  assert.equal(id,`${KEY}:000000042:00003:00007`);
  assert.deepEqual(ghostSourceIdentity({id,sequence:id,part:7}),{entry:`${KEY}:000000042:00003`,part:7});
  const colonKey=`s3:fixture-bucket/${SESSION}:jsonl`,colonID=sourceID(1,0,0,colonKey);
  assert.deepEqual(ghostSourceIdentity({id:colonID,sequence:colonID,part:0}),{entry:`${colonKey}:000000001:00000`,part:0});
  assert.equal(ghostSourceIdentity({id:'different',sequence:id,part:7}),null,'id must equal sequence');
  assert.equal(ghostSourceIdentity({id,sequence:id,part:6}),null,'suffix must match the numeric part');
  assert.equal(ghostSourceIdentity({id,sequence:id}),null,'missing part is not inferred');
  assert.equal(ghostSourceIdentity({id:`${KEY}:42:3:7`,sequence:`${KEY}:42:3:7`,part:7}),null,'unpadded positions are not the pinned format');
  assert.equal(ghostSourceIdentity({id:'batch/0001/0000',sequence:'batch/0001/0000',part:0}),null);
  assert.equal(ghostSourceIdentity(null),null);
});

test('fragments of one source entry join into one logical message across pages with bytes untouched',async()=>{
  const a=event(4,{kind:'tool_result',name:'bash',call_id:'toolu_01FIXTURE',part:0,more:true,text:'total 8\n'});
  const b=event(4,{kind:'tool_result',name:'bash',call_id:'toolu_01FIXTURE',part:1,more:true,text:'drwxr-xr-x  2 fixture fixture 4096 .\n'});
  const c=event(4,{kind:'tool_result',name:'bash',call_id:'toolu_01FIXTURE',part:2,more:false,text:'-rw-r--r--  1 fixture fixture   12 notes.txt\n'});
  const answers=[page([a],{next_cursor:'p2'}),page([b],{next_cursor:'p3'}),page([c])];
  const loader=createGhostHistory(async()=>answers.shift());
  await loader.select(NODE,SESSION);
  let messages=groupGhostMessages(loader.state().events,{morePagesRemain:Boolean(loader.state().next)});
  assert.equal(messages.length,1);assert.equal(messages[0].continuation,'pending');
  await loader.more();
  messages=groupGhostMessages(loader.state().events,{morePagesRemain:Boolean(loader.state().next)});
  assert.equal(messages.length,1);assert.equal(messages[0].continuation,'pending');
  await loader.more();
  messages=groupGhostMessages(loader.state().events,{morePagesRemain:Boolean(loader.state().next)});
  assert.equal(messages.length,1);
  assert.equal(messages[0].text,'total 8\ndrwxr-xr-x  2 fixture fixture 4096 .\n-rw-r--r--  1 fixture fixture   12 notes.txt\n');
  assert.deepEqual(messages[0].ids,[a.id,b.id,c.id]);
  assert.equal(messages[0].kind,'tool_result');assert.equal(messages[0].name,'bash');
  assert.equal(messages[0].continuation,'');assert.equal(messages[0].leadingMissing,false);assert.equal(messages[0].unattributed,false);
  assert.doesNotMatch(messages[0].text,/Fragment \d|toolu_/);
  const truncated=groupGhostMessages([a]);
  assert.equal(truncated[0].continuation,'missing','an open entry with no further pages is marked possibly missing');
});

test('different entries sharing kind, call, name and consecutive part numbers never join',()=>{
  const first=event(10,{kind:'tool_result',name:'bash',call_id:'toolu_01SAME',part:0,more:true,text:'first entry head'});
  const second=event(11,{kind:'tool_result',name:'bash',call_id:'toolu_01SAME',part:1,text:'second entry tail'});
  const messages=groupGhostMessages([first,second]);
  assert.equal(messages.length,2);
  assert.deepEqual(messages.map(m=>m.text),['first entry head','second entry tail']);
  assert.equal(messages[0].continuation,'missing');assert.equal(messages[1].leadingMissing,true);
  const otherBlock=groupGhostMessages([event(12,{block:0,kind:'assistant',part:0,more:true,text:'block zero'}),event(12,{block:1,kind:'assistant',part:1,text:'block one'})]);
  assert.deepEqual(otherBlock.map(m=>m.text),['block zero','block one']);
  const gap=groupGhostMessages([event(13,{part:0,more:true,text:'part zero'}),event(13,{part:2,text:'part two'})]);
  assert.equal(gap.length,2,'a skipped part is not joined');
  const kindChange=groupGhostMessages([event(14,{kind:'tool_call',call_id:'toolu_01K',part:0,more:true,text:'call'}),event(14,{kind:'tool_result',call_id:'toolu_01K',part:1,text:'result'})]);
  assert.equal(kindChange.length,2);
});

test('unrecognized identity stays separate, is marked uncertain, and loses no bytes',()=>{
  const head={id:'opaque-1',sequence:'opaque-1',kind:'assistant',text:'opaque head ',part:0,more:true};
  const tail={id:'opaque-2',sequence:'opaque-2',kind:'assistant',text:'opaque tail',part:1,more:false};
  const merged=mergeGhostEvents([],[tail,head]);
  const messages=groupGhostMessages(merged);
  assert.equal(messages.length,2);
  assert.deepEqual(messages.map(m=>m.text),['opaque head ','opaque tail']);
  assert.deepEqual(messages.map(m=>m.unattributed),[true,true]);
  const summary=ghostLogSummary(messages,{reachedEnd:true,timeZone:TZ});
  assert.equal(summary.uncertain,2);
  assert.equal(summary.text,'start and stop times unknown \u00b7 2 messages \u00b7 message count uncertain (2 unattributed fragments)');
  const whole=groupGhostMessages([{id:'opaque-3',sequence:'opaque-3',kind:'user',text:'whole message',part:0,more:false}]);
  assert.equal(whole[0].unattributed,false,'a complete single-part entry is countable without an identity');
  const wrongPart=event(15,{part:1,text:'pinned shape but mismatched part'});wrongPart.part=2;
  assert.equal(groupGhostMessages([wrongPart])[0].unattributed,true);
});

test('distinct source entries with identical text all survive grouping',()=>{
  const same='Same retained sentence.';
  const messages=groupGhostMessages(mergeGhostEvents([],[event(20,{text:same}),event(21,{text:same}),event(22,{kind:'user',text:same})]));
  assert.equal(messages.length,3);
  assert.deepEqual(messages.map(m=>m.text),[same,same,same]);
  assert.deepEqual(messages.map(m=>m.kind),['assistant','assistant','user']);
});

test('durations read in minutes under an hour and hours plus minutes above it',()=>{
  assert.equal(formatDuration(0),'under 1 minute');
  assert.equal(formatDuration(59999),'under 1 minute');
  assert.equal(formatDuration(60000),'1 minute');
  assert.equal(formatDuration(45*60000+59000),'45 minutes');
  assert.equal(formatDuration(60*60000),'1 hour 0 minutes');
  assert.equal(formatDuration(61*60000),'1 hour 1 minute');
  assert.equal(formatDuration((2*60+41)*60000+21000),'2 hours 41 minutes');
  assert.equal(formatDuration(-1),'');
  assert.equal(formatDuration(Number.NaN),'');
});

test('source times use AM/PM without seconds and unknown times stay unknown',()=>{
  assert.equal(formatGhostTime('2026-10-03T13:05:42Z',{timeZone:TZ}),'Oct 3, 2026, 9:05 AM EDT');
  assert.equal(formatGhostTime('2026-10-03T13:05:59Z',{timeZone:TZ}),'Oct 3, 2026, 9:05 AM EDT');
  assert.equal(formatGhostTime('2026-10-04T03:10:00Z',{timeZone:TZ}),'Oct 3, 2026, 11:10 PM EDT');
  assert.doesNotMatch(formatGhostTime('2026-10-03T13:05:42Z',{timeZone:TZ}),/\d:\d{2}:\d{2}/);
  for(const unknown of [undefined,'','yesterday',12345,null])assert.equal(formatGhostTime(unknown,{timeZone:TZ}),'Source time unknown');
});

test('observed ranges show date, AM/PM bounds and timezone, including overnight and clock changes',()=>{
  const at=value=>Date.parse(value);
  assert.equal(formatGhostRange(at('2026-10-03T13:05:42Z'),at('2026-10-03T15:47:03Z'),{timeZone:TZ}),'Oct 3, 2026, 9:05 AM \u2013 11:47 AM EDT');
  assert.equal(formatGhostRange(at('2026-10-04T03:10:00Z'),at('2026-10-04T05:20:00Z'),{timeZone:TZ}),'Oct 3, 2026, 11:10 PM \u2013 Oct 4, 2026, 1:20 AM EDT');
  assert.equal(formatGhostRange(at('2026-11-01T05:30:00Z'),at('2026-11-01T06:45:00Z'),{timeZone:TZ}),'Nov 1, 2026, 1:30 AM EDT \u2013 1:45 AM EST');
  assert.equal(formatGhostRange(at('2026-10-03T13:05:42Z'),at('2026-10-03T13:05:42Z'),{timeZone:TZ}),'Oct 3, 2026, 9:05 AM EDT');
});

test('log summary reports the observed span, bounds, logical count and partial data honestly',()=>{
  const messages=groupGhostMessages([
    event(1,{kind:'user',text:'question',timestamp:'2026-10-03T13:05:42Z'}),
    event(2,{kind:'tool_result',call_id:'toolu_01X',name:'bash',part:0,more:true,text:'a',timestamp:'2026-10-03T13:06:00Z'}),
    event(2,{kind:'tool_result',call_id:'toolu_01X',name:'bash',part:1,text:'b',timestamp:'2026-10-03T18:00:00Z'}),
    event(3,{kind:'event',text:'no source time'}),
    event(4,{kind:'assistant',text:'done',timestamp:'2026-10-03T15:47:03Z'}),
  ]);
  assert.equal(messages.length,4,'two fragments of one entry are one message');
  const done=ghostLogSummary(messages,{reachedEnd:true,timeZone:TZ});
  assert.equal(done.text,'observed 2 hours 41 minutes \u00b7 Oct 3, 2026, 9:05 AM \u2013 11:47 AM EDT \u00b7 4 messages \u00b7 1 without a source time');
  assert.doesNotMatch(done.text,/\d:\d{2}:\d{2}/);
  assert.equal(done.uncertain,0);
  assert.match(ghostLogSummary(messages,{morePagesRemain:true,timeZone:TZ}).text,/ 4 messages loaded so far /);
  assert.match(ghostLogSummary(messages,{timeZone:TZ}).text,/ 4 messages loaded /);
  const one=groupGhostMessages([event(1,{kind:'user',text:'q',timestamp:'2026-10-03T13:05:42Z'})]);
  assert.equal(ghostLogSummary(one,{reachedEnd:true,timeZone:TZ}).text,'duration unknown \u00b7 Oct 3, 2026, 9:05 AM EDT \u00b7 1 message');
  const untimed=groupGhostMessages([event(1,{timestamp:'not-a-time'}),event(2)]);
  assert.equal(untimed[0].timestamp,'','an unparseable source time is not kept as a time');
  assert.deepEqual(ghostObservedSpan(untimed),{first:null,last:null,timed:0,untimed:2});
  assert.equal(ghostLogSummary(untimed,{reachedEnd:true,timeZone:TZ}).text,'start and stop times unknown \u00b7 2 messages');
  assert.equal(ghostLogSummary([],{reachedEnd:true}).text,'0 messages');
});

test('role labels name every supported kind and keep unknown kinds visible',()=>{
  assert.equal(ghostRoleLabel('user'),'User message');
  assert.equal(ghostRoleLabel('assistant','ignored'),'Assistant message');
  assert.equal(ghostRoleLabel('tool_call','bash'),'Tool call: bash');
  assert.equal(ghostRoleLabel('tool_result','bash'),'Tool result: bash');
  assert.equal(ghostRoleLabel('tool_result'),'Tool result');
  assert.equal(ghostRoleLabel('command'),'Command');
  assert.equal(ghostRoleLabel('event'),'Event');
  assert.equal(ghostRoleLabel('mystery'),'Entry (mystery)');
});

test('session descriptions come from the first user text, then assistant text, collapsed and bounded by code points',()=>{
  const user=groupGhostMessages([event(1,{kind:'assistant',text:'assistant first'}),event(2,{kind:'user',text:'  Please   summarize\n the build log.  '})]);
  assert.equal(describeGhostSession(user),'Please summarize the build log.');
  assert.equal(describeGhostSession(groupGhostMessages([event(1,{kind:'tool_call',text:'{}'}),event(2,{kind:'assistant',text:'Only assistant text'})])),'Only assistant text');
  assert.equal(describeGhostSession(groupGhostMessages([event(1,{kind:'tool_result',text:'output'})])),'');
  assert.equal(describeGhostSession([]),'');
  const long=describeGhostSession(groupGhostMessages([event(1,{kind:'user',text:'\u{1F642}'.repeat(100)})]));
  assert.equal([...long].length,80);assert.equal(long,'\u{1F642}'.repeat(79)+'\u2026');
});

test('scheduled jobs are bound to a session only by an authoritative binding, and last-run status is explicit',()=>{
  const jobs=[{event_id:'bound',provider_session_id:SESSION},{event_id:'other',provider_session_id:OTHER_SESSION},{event_id:'none'}];
  assert.deepEqual(scheduledForSession(jobs,SESSION,false),{authoritative:false,events:[]},'a field without an available binding is never trusted');
  assert.deepEqual(scheduledForSession(jobs,SESSION,undefined),{authoritative:false,events:[]});
  assert.deepEqual(scheduledForSession(jobs,'',true),{authoritative:false,events:[]});
  assert.deepEqual(scheduledForSession(jobs,SESSION,true),{authoritative:true,events:[jobs[0]]});
  assert.equal(scheduledRunLabel({last_run_status:'succeeded',last_run_ts:'2026-10-03T13:05:42Z'},{timeZone:TZ}),'Last run succeeded \u00b7 Oct 3, 2026, 9:05 AM EDT');
  assert.equal(scheduledRunLabel({last_run_status:'failed'},{timeZone:TZ}),'Last run failed \u00b7 time unknown');
  assert.equal(scheduledRunLabel({last_run_status:'running',last_run_ts:'provider-local 09:05'},{timeZone:TZ}),'Last run running \u00b7 provider-local 09:05');
  assert.equal(scheduledRunLabel({last_run_status:'  '}),'No recorded run');
  assert.equal(scheduledRunLabel({}),'No recorded run');
});
