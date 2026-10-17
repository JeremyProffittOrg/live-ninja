import { test } from 'node:test';
import assert from 'node:assert/strict';
import { recordingLabel, recordingPath, validRecordingURL, mountRecordingHistory } from '../../../web/static/js/recording-history.mjs';
import { acquireAudioAccess, acquireGuardedMicrophone } from '../../../web/static/js/audio-access.mjs';

// Two pages use the same origin-scoped lock manager.
function audioLocks() {
 const held = new Set();
 return { async request(name, options, callback) {
  if ([...held].some(lock => lock.mode === 'exclusive' || options.mode === 'exclusive')) return callback(null);
  const lock={mode:options.mode};held.add(lock);
  try{return await callback({name});}finally{held.delete(lock);}
 }};
}
const unlockedPlayback=async()=>()=>{};

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
test('hotel recording link finds the exact call across pages without fetching audio',async()=>{
 const doc=documentFixture(),requests=[];const source='hotel-trial-2026-10-15';
 doc.defaultView.location={search:'?'+new URLSearchParams({phoneSource:source,phoneCall:sid})};
 mountRecordingHistory(doc,async path=>{requests.push(path);const query=new URL(path,'https://live.jeremy.ninja').searchParams;
  assert.equal(query.get('source'),source);assert.ok(!path.includes('/media?'));
  return query.get('cursor')?{items:[{callSid:sid,target:'Selected hotel',recordingStatus:'ready'}],sources:['messages',source]}:{items:[{callSid:'CA'+'2'.repeat(32),target:'Another hotel',recordingStatus:'ready'}],sources:['messages',source],nextCursor:'next-page'};
 },()=>()=>{},unlockedPlayback);await settle();
 assert.equal(requests.length,2);assert.equal(doc.ids.phoneRecordingList.children.length,1);
 const row=doc.ids.phoneRecordingList.children[0];assert.match(row.querySelectorAll('h3')[0].textContent,/Selected recording.*Selected hotel/);
 assert.equal(row.querySelectorAll('audio')[0].src,undefined);assert.equal(doc.ids.phoneRecordingSource.value,source);
});

test('recording lookup bounds automatic pages and keeps missing audio unverified',async()=>{
 const doc=documentFixture();doc.defaultView.location={search:'?'+new URLSearchParams({phoneSource:'messages',phoneCall:sid})};let pages=0;
 mountRecordingHistory(doc,async()=>({items:[],sources:['messages'],nextCursor:'page-'+(++pages)}),()=>()=>{},unlockedPlayback);await settle();
 assert.equal(pages,10);assert.equal(doc.ids.phoneRecordingList.children.length,0);assert.equal(doc.ids.phoneRecordingMore.hidden,false);
 assert.match(doc.ids.phoneRecordingStatus.textContent,/Load more/);
});

test('invalid recording deep link cannot select an arbitrary source or fetch media',async()=>{
 const doc=documentFixture();doc.defaultView.location={search:'?'+new URLSearchParams({phoneSource:'../foreign',phoneCall:sid})};const requests=[];
 mountRecordingHistory(doc,async path=>{requests.push(path);return {items:[],sources:['messages']};},()=>()=>{},unlockedPlayback);await settle();
 assert.equal(requests.length,1);assert.equal(new URL(requests[0],'https://live.jeremy.ninja').searchParams.get('source'),'messages');
 assert.equal(doc.ids.phoneRecordingList.children.length,0);
});

test('history fetches no audio until explicit request and never autoplays',async()=>{
 const doc=documentFixture(),requests=[];const request=async path=>{requests.push(path);return path.includes('/media?')?{url:'https://live-ninja-deliverables-759775734231.s3.amazonaws.com/audio?signature=test'}:{items:[{callSid:sid,target:'primary',recordingStatus:'ready'},{target:'failed call',recordingStatus:'failed'}],sources:['messages']};};
 mountRecordingHistory(doc,request,()=>()=>{},unlockedPlayback);await settle();assert.equal(requests.length,1);
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
 mountRecordingHistory(doc,request,fn=>{loseAuth=fn;return ()=>{};},unlockedPlayback);await settle();
 const row=doc.ids.phoneRecordingList.children[0],audio=row.querySelectorAll('audio')[0];
 const pending=row.querySelectorAll('button')[0].events.click();await settle();loseAuth();
 resolveMedia({url:'https://live-ninja-deliverables-759775734231.s3.amazonaws.com/audio?signature=test'});await pending;
 assert.equal(audio.src,undefined);assert.equal(doc.ids.phoneRecordingList.children.length,0);assert.match(doc.ids.phoneRecordingStatus.textContent,/session changed/);
});

test('capture blocks recording fetch across tabs until every capture stream ends',async()=>{
 const locks=audioLocks(),tracks=[];
 const nav={locks,mediaDevices:{async getUserMedia(){const track={stop(){},addEventListener(type,fn){this.end=fn;}};tracks.push(track);return {getTracks:()=>[track]};}}};
 await acquireGuardedMicrophone({audio:true},nav);await acquireGuardedMicrophone({audio:true},nav);
 await assert.rejects(acquireAudioAccess('exclusive',locks),/Close loaded recordings/);
 tracks[0].stop();await settle();
 await assert.rejects(acquireAudioAccess('exclusive',locks));
 tracks[1].end();await settle();
 const release=await acquireAudioAccess('exclusive',locks);
 await assert.rejects(acquireGuardedMicrophone({audio:true},nav));assert.equal(tracks.length,2);
 release();await settle();const stream=await acquireGuardedMicrophone({audio:true},nav);stream.getTracks()[0].stop();
});

test('failed microphone acquisition releases access and unsupported playback fails closed',async()=>{
 const locks=audioLocks();
 await assert.rejects(acquireGuardedMicrophone({audio:true},{locks,mediaDevices:{getUserMedia:async()=>{throw Error('denied');}}}),/denied/);
 const release=await acquireAudioAccess('exclusive',locks);release();
 await assert.rejects(acquireAudioAccess('exclusive',null),/Download/);
});

test('history cannot fetch media while microphone is held and close releases playback',async()=>{
 const locks=audioLocks(),doc=documentFixture(),requests=[];
 const request=async path=>{requests.push(path);return path.includes('/media?')?{url:'https://live-ninja-deliverables-759775734231.s3.amazonaws.com/audio?signature=test'}:{items:[{callSid:sid,recordingStatus:'ready'}],sources:['messages']};};
 const releaseMic=await acquireAudioAccess('shared',locks);
 mountRecordingHistory(doc,request,()=>()=>{},()=>acquireAudioAccess('exclusive',locks));await settle();
 const row=doc.ids.phoneRecordingList.children[0],buttons=row.querySelectorAll('button');
 await buttons[0].events.click();assert.equal(requests.length,1);
 releaseMic();await settle();await buttons[0].events.click();assert.equal(requests.length,2);
 await assert.rejects(acquireAudioAccess('shared',locks));
 buttons[2].events.click();await settle();const release=await acquireAudioAccess('shared',locks);release();
 assert.equal(row.querySelectorAll('audio')[0].src,undefined);
});

test('closing recordings invalidates a pending URL before releasing audio access',async()=>{
 const locks=audioLocks(),doc=documentFixture();let finish;
 const request=async path=>path.includes('/media?')?new Promise(resolve=>{finish=resolve;}):{items:[{callSid:sid,recordingStatus:'ready'}],sources:['messages']};
 mountRecordingHistory(doc,request,()=>()=>{},()=>acquireAudioAccess('exclusive',locks));await settle();
 const row=doc.ids.phoneRecordingList.children[0],buttons=row.querySelectorAll('button');
 const pending=buttons[0].events.click();await settle();buttons[2].events.click();await settle();
 const release=await acquireAudioAccess('shared',locks);
 finish({url:'https://live-ninja-deliverables-759775734231.s3.amazonaws.com/audio?signature=test'});await pending;
 assert.equal(row.querySelectorAll('audio')[0].src,undefined);release();
});

test('failed parallel load invalidates another pending recording before capture resumes',async()=>{
 const locks=audioLocks(),doc=documentFixture(),pendingMedia=[];
 const request=async path=>path.includes('/media?')?new Promise((resolve,reject)=>pendingMedia.push({resolve,reject})):{items:[sid,'CA'+'2'.repeat(32)].map(callSid=>({callSid,recordingStatus:'ready'})),sources:['messages']};
 mountRecordingHistory(doc,request,()=>()=>{},()=>acquireAudioAccess('exclusive',locks));await settle();
 const rows=doc.ids.phoneRecordingList.children;
 const first=rows[0].querySelectorAll('button')[0].events.click();await settle();
 const second=rows[1].querySelectorAll('button')[0].events.click();await settle();
 assert.equal(pendingMedia.length,2);
 await assert.rejects(acquireAudioAccess('shared',locks));
 pendingMedia[0].reject(new Error('media unavailable'));await first;await settle();
 const release=await acquireAudioAccess('shared',locks);
 pendingMedia[1].resolve({url:'https://live-ninja-deliverables-759775734231.s3.amazonaws.com/audio?signature=test'});await second;
 for(const row of rows){const audio=row.querySelectorAll('audio')[0];assert.equal(audio.src,undefined);assert.equal(audio.hidden,true);}
 release();
});

test('closing while playback access is pending releases its late lease without fetching media',async()=>{
 const doc=documentFixture();let finishLock,releases=0,requests=0;
 const request=async()=>{requests++;return {items:[{callSid:sid,recordingStatus:'ready'}],sources:['messages']};};
 mountRecordingHistory(doc,request,()=>()=>{},()=>new Promise(resolve=>{finishLock=resolve;}));await settle();
 const row=doc.ids.phoneRecordingList.children[0],buttons=row.querySelectorAll('button');
 const pending=buttons[0].events.click();buttons[2].events.click();
 finishLock(()=>{releases++;});await pending;
 assert.equal(releases,1);assert.equal(requests,1);
 assert.equal(row.querySelectorAll('audio')[0].src,undefined);
 buttons[2].events.click();assert.equal(releases,1);
});
