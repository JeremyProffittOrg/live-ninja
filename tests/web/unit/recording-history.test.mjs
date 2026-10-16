import { test } from 'node:test';
import assert from 'node:assert/strict';
import { recordingLabel, recordingPath, validRecordingURL, mountRecordingHistory } from '../../../web/static/js/recording-history.mjs';

const sid='CA'+'1'.repeat(32);
test('recording states and paths never imply absent audio is ready',()=>{
 assert.equal(recordingLabel('failed'),'Recording failed');assert.equal(recordingLabel('absent'),'No recording');assert.equal(recordingLabel('invented'),'Recording unavailable');
 assert.match(recordingPath('messages',sid),/disposition=inline/);assert.match(recordingPath('messages',sid,'attachment'),/disposition=attachment/);
 assert.throws(()=>recordingPath('../other',sid));assert.throws(()=>recordingPath('messages','wrong'));assert.throws(()=>recordingPath('messages',sid,'other'));
});
test('only HTTPS links for the private recording bucket are accepted',()=>{
 assert.equal(validRecordingURL('https://live-ninja-deliverables-759775734231.s3.us-east-1.amazonaws.com/a?signature=test'),true);
 for(const raw of ['https://other.example/a?signature=test','http://live-ninja-deliverables-759775734231.s3.amazonaws.com/a?signature=test','https://user@live-ninja-deliverables-759775734231.s3.amazonaws.com/a?signature=test','https://live-ninja-deliverables-759775734231.s3.amazonaws.com/a'])assert.equal(validRecordingURL(raw),false);
});
class Element {
 constructor(tag='div'){this.tag=tag;this.children=[];this.events={};this.value='';this.hidden=false;this.disabled=false;this.pauses=0;}
 append(...items){this.children.push(...items);}replaceChildren(...items){this.children=items;}setAttribute(){}removeAttribute(name){delete this[name];}load(){}
 addEventListener(name,fn){this.events[name]=fn;}pause(){this.pauses++;}querySelectorAll(tag){return this.children.flatMap(e=>[...(e.tag===tag?[e]:[]),...e.querySelectorAll(tag)]);}
}
function documentFixture(){const ids=Object.fromEntries(['phoneRecordingHistory','phoneRecordingList','phoneRecordingStatus','phoneRecordingSource','phoneRecordingMore','phoneRecordingRefresh'].map(id=>[id,new Element()]));return {ids,body:new Element(),getElementById:id=>ids[id],createElement:tag=>new Element(tag),defaultView:new Element()};}
const settle=()=>new Promise(resolve=>setImmediate(resolve));
test('history fetches no audio until explicit request and never autoplays',async()=>{
 const doc=documentFixture(),requests=[];const request=async path=>{requests.push(path);return path.includes('/media?')?{url:'https://live-ninja-deliverables-759775734231.s3.amazonaws.com/audio?signature=test'}:{items:[{callSid:sid,target:'primary',recordingStatus:'ready'},{target:'failed call',recordingStatus:'failed'}],sources:['messages']};};
 mountRecordingHistory(doc,request,()=>()=>{});await settle();assert.equal(requests.length,1);
 const rows=doc.ids.phoneRecordingList.children;assert.equal(rows.length,2);assert.equal(rows[1].querySelectorAll('button').length,0);
 const audio=rows[0].querySelectorAll('audio')[0];assert.equal(audio.src,undefined);assert.equal(audio.hidden,true);assert.notEqual(audio.autoplay,true);
 await rows[0].querySelectorAll('button')[0].events.click();assert.equal(requests.length,2);assert.equal(audio.hidden,false);assert.match(audio.src,/signature=test/);assert.notEqual(audio.autoplay,true);
 doc.defaultView.events.pagehide();assert.ok(audio.pauses>0);assert.equal(doc.ids.phoneRecordingList.children.length,0);
});
test('access failure remains visible and never fabricates an empty success',async()=>{
 const doc=documentFixture();mountRecordingHistory(doc,async()=>{throw new Error('403');},()=>()=>{});await settle();assert.equal(doc.ids.phoneRecordingHistory.hidden,false);assert.match(doc.ids.phoneRecordingStatus.textContent,/unavailable/);assert.equal(doc.ids.phoneRecordingList.children.length,0);
});

test('auth loss clears media and suppresses a late private media response',async()=>{
 const doc=documentFixture();let loseAuth,resolveMedia;
 const request=async path=>path.includes('/media?')?await new Promise(resolve=>{resolveMedia=resolve;}):{items:[{callSid:sid,target:'primary',recordingStatus:'ready'}],sources:['messages']};
 mountRecordingHistory(doc,request,fn=>{loseAuth=fn;return ()=>{};});await settle();
 const row=doc.ids.phoneRecordingList.children[0],audio=row.querySelectorAll('audio')[0];
 const pending=row.querySelectorAll('button')[0].events.click();loseAuth();
 resolveMedia({url:'https://live-ninja-deliverables-759775734231.s3.amazonaws.com/audio?signature=test'});await pending;
 assert.equal(audio.src,undefined);assert.equal(doc.ids.phoneRecordingList.children.length,0);assert.match(doc.ids.phoneRecordingStatus.textContent,/session changed/);
});
