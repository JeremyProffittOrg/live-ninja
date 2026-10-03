import test from 'node:test';
import assert from 'node:assert/strict';
import {reviewedJobRequest} from '../../../web/static/js/approval-review.mjs';
const token=(sub,sid)=>'e30.'+Buffer.from(JSON.stringify({sub,sid})).toString('base64url')+'.sig';
const body={error:{code:'confirmation_required',details:{operation:'job_create',executionAvailable:true,proposed:{title:'Test',instructions:'Review',kind:'review',schedule:{kind:'once',timezone:'UTC'}}}}};
const savedFetch=globalThis.fetch,savedDocument=globalThis.document,savedLocal=globalThis.localStorage;
test('bound proposal refuses account switch and never submits a mutation',async()=>{
 globalThis.window={location:{assign:()=>{}}};globalThis.document={cookie:''};globalThis.localStorage={getItem:()=>null,setItem:()=>{}};
 const mod=await import('../../../web/static/js/toolclient.mjs?binding-a');const requests=[];let current='A';
 globalThis.fetch=async(path,options)=>{requests.push({path,options});return Response.json(path.endsWith('/auth/refresh')?{accessToken:token(current,'s'+current),expiresAt:Date.now()/1000+300}:body);};
 const result=await mod.apiJSON('/api/v1/tools/invoke',{method:'POST',json:{tool:'job_create'}});const bound=mod.reviewedProposalRequest(result.error.details);current='B';
 await assert.rejects(bound('/api/v1/jobs',{method:'POST',json:{title:'wrongaccount'}}),/session.*(changed|expired)/);
 assert.equal(requests.filter(r=>r.path==='/api/v1/jobs').length,0);
});
test('same-session refresh uses exact bearer and a 401 cannot replay',async()=>{
 const mod=await import('../../../web/static/js/toolclient.mjs?binding-b');let mutations=0,refreshes=0;
 globalThis.fetch=async(path)=>{if(path.endsWith('/auth/refresh')){refreshes++;return Response.json({accessToken:token('A','sA'),expiresAt:Date.now()/1000+300});}if(path==='/api/v1/jobs'){mutations++;return Response.json({error:'unauthorized'},{status:401});}return Response.json(body);};
 const result=await mod.apiJSON('/api/v1/tools/invoke',{method:'POST',json:{}});const bound=mod.reviewedProposalRequest(result.error.details);
 await assert.rejects(bound('/api/v1/jobs',{method:'POST',json:{}}));assert.equal(mutations,1);assert.equal(refreshes,2);
 await assert.rejects(bound('https://other.example/api/v1/jobs'),/Invalid bound/);
});
test('proposal JSON alone cannot forge client session binding',async()=>{
 const mod=await import('../../../web/static/js/toolclient.mjs?binding-c');assert.throws(()=>mod.reviewedProposalRequest(body.error.details),/verified session/);
});
test('reviewed action uses exact version and excludes unsupported supplied fields',()=>{
 const details={operation:'job_start',executionAvailable:true,job:{id:'job/a',version:7},proposed:{jobId:'job/a',expectedVersion:7,deploy:true}};
 const action=reviewedJobRequest(details);assert.equal(action.path,'/api/v1/jobs/job%2Fa/run');assert.equal(action.payload.expectedVersion,7);assert.equal(action.payload.deploy,undefined);assert.ok(action.payload.requestId);
 assert.throws(()=>reviewedJobRequest({...details,proposed:{...details.proposed,expectedVersion:8}}),/snapshot/);
});
test.after(()=>{globalThis.fetch=savedFetch;globalThis.document=savedDocument;globalThis.localStorage=savedLocal;});

test('voice reminders may have empty optional instructions',()=>{
 const action=reviewedJobRequest({operation:'job_create',executionAvailable:true,proposed:{title:'Stretch',kind:'reminder',schedule:{kind:'once',timezone:'UTC'}}});
 assert.equal(action.payload.instructions,'');assert.equal(action.path,'/api/v1/jobs');
});

test('old voice session cannot read Jobs after cookie account changes',async()=>{
 const mod=await import('../../../web/static/js/toolclient.mjs?voice-binding');let account='A',invokes=0,closed=0;const events=[];
 globalThis.document={cookie:''};globalThis.window={location:{assign:()=>{}}};globalThis.localStorage={getItem:()=>null,setItem:()=>{}};
 globalThis.fetch=async path=>{if(path.endsWith('/auth/refresh'))return Response.json({accessToken:token(account,'s'+account),expiresAt:Date.now()/1000+300});if(path.endsWith('/tools/invoke')){invokes++;return Response.json({ok:true,output:{jobs:['private']}});}return Response.json({sessionId:'provider-A'});};
 const mint=await mod.authFetch('/api/v1/realtime/session');const dispatcher=mod.createToolDispatcher({sendEvent:e=>events.push(e),onSessionInvalidated:()=>closed++});dispatcher.bindSession(mod.voiceBindingForResponse(mint));account='B';
 await dispatcher.dispatch({name:'job_list',callId:'old-A',argsJson:'{}'});
 assert.equal(invokes,0);assert.equal(closed,1);assert.ok(!JSON.stringify(events).includes('private'));
 await assert.rejects(mod.apiJSON('/api/v1/transcript',{method:'POST',json:{turns:['old A text']}}),/expired/);
});
test('voice binding rejects an account change while private results are in flight',async()=>{
 const mod=await import('../../../web/static/js/toolclient.mjs?voice-late');let account='A';const events=[];
 globalThis.fetch=async path=>{if(path.endsWith('/auth/refresh'))return Response.json({accessToken:token(account,'s'+account),expiresAt:Date.now()/1000+300});if(path.endsWith('/tools/invoke')){account='B';return Response.json({ok:true,output:{jobs:['private-A-result']}});}return Response.json({sessionId:'provider-A'});};
 const mint=await mod.authFetch('/api/v1/realtime/session');const dispatcher=mod.createToolDispatcher({sendEvent:e=>events.push(e)});dispatcher.bindSession(mod.voiceBindingForResponse(mint));
 await dispatcher.dispatch({name:'job_status',callId:'late-A',argsJson:'{}'});assert.ok(!JSON.stringify(events).includes('private-A-result'));
});

test('archive page binding blocks account change before read and drops a late private response',async()=>{
 for(const late of [false,true]) {
  const mod=await import('../../../web/static/js/toolclient.mjs?archive-'+late);let account='A',reads=0;
  globalThis.document={cookie:''};globalThis.window={location:{assign:()=>{}}};globalThis.localStorage={getItem:()=>null,setItem:()=>{}};
  globalThis.fetch=async path=>{if(path.endsWith('/auth/refresh'))return Response.json({accessToken:token(account,'s'+account),expiresAt:Date.now()/1000+300});reads++;if(late)account='B';return Response.json({events:[{text:'private-A'}]});};
  await mod.ensureAccessToken();if(!late)account='B';
  await assert.rejects(mod.pageSessionJSON('/api/v1/ghost-work/events?node_id=A&session_id=S'),/expired/);
  assert.equal(reads,late?1:0);
 }
});
test('archive unauthorized response is not replayed under a refreshed account',async()=>{
 const mod=await import('../../../web/static/js/toolclient.mjs?archive-401');let reads=0,refreshes=0;
 globalThis.fetch=async path=>{if(path.endsWith('/auth/refresh')){refreshes++;return Response.json({accessToken:token('A','sA'),expiresAt:Date.now()/1000+300});}reads++;return Response.json({error:'denied'},{status:401});};
 await assert.rejects(mod.pageSessionJSON('/api/v1/ghost-work/events?node_id=A&session_id=S'),e=>e.status===401);assert.equal(reads,1);assert.equal(refreshes,1);
});
