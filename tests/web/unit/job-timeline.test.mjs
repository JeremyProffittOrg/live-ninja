import test from 'node:test';
import assert from 'node:assert/strict';
import {createHistoryLoader,mergeHistory,historyText} from '../../../web/static/js/job-timeline.mjs';
const entry=(n,id='job-a')=>({id:`e${n}`,jobId:id,sequence:n,role:'system',text:`entry ${n}`});
test('retains more than 100 events, full large output, and merges duplicate pages',async()=>{
 const entries=Array.from({length:135},(_,i)=>entry(i+1));entries[0].text='x'.repeat(180000)+'FINAL';
 const request=async path=>{const u=new URL(path,'http://localhost');const before=Number(u.searchParams.get('cursor')||136);const data=entries.filter(e=>e.sequence<before).slice(-30);return {entries:data,olderCursor:data[0].sequence>1?String(data[0].sequence):'',newerCursor:String(data.at(-1).sequence),hasMore:data[0].sequence>1};};
 const loader=createHistoryLoader(request);await loader.select('job-a');while(loader.state().olderCursor)await loader.load('older');
 assert.equal(loader.state().entries.length,135);assert.equal(historyText(loader.state().entries[0]).length,180005);
 assert.equal(mergeHistory(loader.state().entries,[entry(135)],'job-a').length,135);
});
test('late responses from another selection cannot replace visible history',async()=>{
 let resolve;const old=new Promise(r=>resolve=r);const loader=createHistoryLoader(path=>path.includes('job-a')?old:Promise.resolve({entries:[entry(1,'job-b')]}));
 const a=loader.select('job-a');await loader.select('job-b');resolve({entries:[entry(1)]});await a;
 assert.equal(loader.state().jobId,'job-b');assert.equal(loader.state().entries[0].jobId,'job-b');
});
test('catch-up fills multiple pages without dropping previously loaded history',async()=>{
 const responses=[{entries:[entry(1)],newerCursor:'1'},{entries:[entry(2)],newerCursor:'2',hasMore:true},{entries:[entry(3)],newerCursor:'3',hasMore:false}];
 const paths=[];const loader=createHistoryLoader(async path=>{paths.push(path);return responses.shift();});await loader.select('job-a');await loader.catchUp();
 assert.deepEqual(loader.state().entries.map(e=>e.sequence),[1,2,3]);assert.match(paths[1],/after=1/);assert.match(paths[2],/after=2/);
});
test('rejects cross-job and changed immutable records',()=>{
 assert.throws(()=>mergeHistory([],[entry(1,'other')],'job-a'),/selected job/);
 assert.throws(()=>mergeHistory([entry(1)],[{...entry(1),text:'changed'}],'job-a'),/immutable/);
});
test('refresh failure preserves full loaded records and cursor for retry',async()=>{
 let fail=false;const loader=createHistoryLoader(async()=>{if(fail)throw new Error('offline');return{entries:[entry(1)],newerCursor:'1'};});await loader.select('job-a');fail=true;await loader.catchUp();
 assert.equal(loader.state().entries.length,1);assert.equal(loader.state().newerCursor,'1');assert.equal(loader.state().error,'offline');
});

test('a progress text never hides a retained full run output',()=>{
 const text=historyText({text:'Completed',run:{title:'Work',instructions:'Full instructions',progress:'Completed',result:'x'.repeat(100000)+'END',error:'Error detail'}});
 assert.ok(text.includes('Full instructions'));assert.ok(text.includes('x'.repeat(100000)+'END'));assert.ok(text.includes('Error detail'));assert.equal(text.split('Completed').length,2);
});
