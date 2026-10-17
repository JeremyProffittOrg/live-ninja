import { pageSessionJSON, onAuthLost } from './toolclient.mjs';
import { acquireAudioAccess } from './audio-access.mjs';

export function recordingLabel(state) {
  return ({ready:'Ready',pending:'Processing',requested:'Requested',recording:'Recording',processing:'Processing',copying:'Copying to private storage',absent:'No recording',failed:'Recording failed',deleted:'Deleted',expired:'Expired',unavailable:'No longer stored'})[state] || 'Recording unavailable';
}
export function recordingPath(source, sid, disposition = 'inline') {
  if (!/^[a-z0-9][a-z0-9-]{0,99}$/.test(source) || !/^CA[0-9a-fA-F]{32}$/.test(sid) || !['inline','attachment'].includes(disposition)) throw new Error('Invalid recording');
  return '/api/v1/phone/recordings/' + sid + '/media?' + new URLSearchParams({source,disposition});
}
export function validRecordingURL(raw) {
  try { const u=new URL(raw);return u.protocol==='https:' && !u.username && !u.password && !u.port && ['live-ninja-deliverables-759775734231.s3.amazonaws.com','live-ninja-deliverables-759775734231.s3.us-east-1.amazonaws.com'].includes(u.host) && !!u.search; } catch { return false; }
}
export function mountRecordingHistory(doc = document, request = pageSessionJSON, subscribeAuthLost = onAuthLost, acquirePlayback = () => acquireAudioAccess('exclusive')) {
 const panel=doc.getElementById('phoneRecordingHistory');if(!panel)return;
 const list=doc.getElementById('phoneRecordingList'), status=doc.getElementById('phoneRecordingStatus'), source=doc.getElementById('phoneRecordingSource'), more=doc.getElementById('phoneRecordingMore');
 let cursor='', generation=0, mediaGeneration=0, invalidated=false, releasePlayback=null;
 let linkedSource='', linkedCall='';
 const query=new URLSearchParams(doc.defaultView?.location?.search||'');
 try{const selected=query.get('phoneSource'),sid=query.get('phoneCall');if(selected&&sid){recordingPath(selected,sid);linkedSource=selected;linkedCall=sid;}}catch{/* Invalid links do not change the normal private list. */}
 function stopAudio(){for(const audio of list.querySelectorAll('audio'))audio.pause();}
 function closeAudio(){mediaGeneration++;stopAudio();for(const audio of list.querySelectorAll('audio')){audio.removeAttribute('src');audio.load();audio.hidden=true;}releasePlayback?.();releasePlayback=null;}
 function clear(){closeAudio();list.replaceChildren();}
 const unsubscribe=subscribeAuthLost(()=>{invalidated=true;generation++;clear();source.replaceChildren();more.hidden=true;status.textContent='Your session changed. Reload History and sign in again.';});
 function button(text, action){const b=doc.createElement('button');b.type='button';b.className='ln-btn ln-btn--ghost';b.textContent=text;b.addEventListener('click',action);return b;}
 function render(item, selectedSource){
  const row=doc.createElement('article');row.className='ln-card';
  const title=doc.createElement('h3');title.textContent=(linkedCall===item.callSid?'Selected recording · ':'')+(item.target || 'Phone call');row.append(title);
  const detail=doc.createElement('p');detail.textContent=[recordingLabel(item.recordingStatus),item.providerStatus,item.reason].filter(Boolean).join(' · ');row.append(detail);
  if(item.updatedAt){const when=doc.createElement('p');when.textContent=new Date(Number(item.updatedAt)*1000).toLocaleString();row.append(when);}
  if(item.recordingStatus==='ready' && /^CA[0-9a-fA-F]{32}$/.test(item.callSid||'')){
   const audio=doc.createElement('audio');audio.controls=true;audio.preload='none';audio.hidden=true;
   const note=doc.createElement('p');note.setAttribute('role','status');
   const load=button('Load recording',async()=>{
    if(invalidated)return;const seq=generation,mediaSeq=mediaGeneration;load.disabled=true;stopAudio();const position=audio.currentTime||0;
    try{if(!releasePlayback){const release=await acquirePlayback();if(invalidated||seq!==generation||mediaSeq!==mediaGeneration){release();return;}if(releasePlayback)release();else releasePlayback=release;}
     const data=await request(recordingPath(selectedSource,item.callSid));if(invalidated||seq!==generation||mediaSeq!==mediaGeneration)return;if(!validRecordingURL(data.url))throw new Error('Invalid media');
     audio.src=data.url;audio.hidden=false;audio.addEventListener('loadedmetadata',()=>{if(position<audio.duration)audio.currentTime=position;},{once:true});
     note.textContent='Use the audio controls to play, pause or seek. Close recordings before starting the microphone in any tab. Reload if the five-minute link expires.';load.textContent='Refresh recording link';
    }catch{if(!invalidated&&seq===generation&&mediaSeq===mediaGeneration){closeAudio();note.textContent='Recording unavailable. End conversations and turn off Always listening in other tabs, then retry. Your browser must support private playback coordination; you can also download the recording.';}}finally{load.disabled=false;}
   });
   audio.addEventListener('play',()=>{for(const other of list.querySelectorAll('audio'))if(other!==audio)other.pause();});
   audio.addEventListener('error',()=>{note.textContent='Playback stopped. Refresh the recording link to retry.';});
   const download=button('Download',async()=>{
    if(invalidated)return;const seq=generation;download.disabled=true;
    try{const data=await request(recordingPath(selectedSource,item.callSid,'attachment'));if(invalidated||seq!==generation)return;if(!validRecordingURL(data.url))throw new Error('Invalid media');
     const link=doc.createElement('a');link.href=data.url;link.rel='noreferrer';link.referrerPolicy='no-referrer';link.download=item.callSid+'.mp3';doc.body.append(link);link.click();link.remove();
    }catch{note.textContent='Download unavailable. Refresh the list or try again later.';}finally{download.disabled=false;}
   });const close=button('Close recordings',()=>{closeAudio();note.textContent='Recordings closed. You can use the microphone again.';});row.append(load,download,close,audio,note);
  }
  list.append(row);
 }
 async function load(append=false){
  if(invalidated)return;
  const seq=++generation,selected=source.value||linkedSource||'messages';if(!append){cursor='';clear();}more.disabled=true;status.textContent='Loading phone recordings…';
  try{let pages=0;const visited=new Set();do{
   if(visited.has(cursor))throw new Error('Repeated recording cursor');visited.add(cursor);
   const data=await request('/api/v1/phone/recordings?'+new URLSearchParams({source:selected,cursor,limit:'25'}));if(seq!==generation)return;
   if(!Array.isArray(data.items)||!Array.isArray(data.sources))throw new Error('Invalid response');
   panel.hidden=false;
   source.replaceChildren(...data.sources.map(value=>{const option=doc.createElement('option');option.value=value;option.textContent=value==='messages'?'Owner phone messages':value;return option;}));source.value=selected;
   for(const item of data.items)if(!linkedCall||item.callSid===linkedCall)render(item,selected);cursor=data.nextCursor||'';pages++;
  }while(linkedCall&&!list.children.length&&cursor&&pages<10);
   more.hidden=!cursor||Boolean(linkedCall&&list.children.length);
   status.textContent=list.children.length?'Recordings stay private. Each playback or download link lasts five minutes.':linkedCall?(cursor?'The selected call is not on these pages. Load more to continue looking.':'The selected call is not available in this source. Check its current result in Jobs.'):'No phone recordings in this source.';
  }catch{if(seq===generation){panel.hidden=false;status.textContent='Phone recordings are unavailable. Owner sign-in is required.';more.hidden=true;}}
  finally{if(seq===generation)more.disabled=false;}
 }
 source.addEventListener('change',()=>{linkedCall='';linkedSource='';load();});doc.getElementById('phoneRecordingRefresh').addEventListener('click',()=>load());more.addEventListener('click',()=>load(true));
 doc.defaultView?.addEventListener('pagehide',()=>{invalidated=true;generation++;clear();unsubscribe();});
 doc.defaultView?.addEventListener('focus',()=>load());load();
}
if(typeof document!=='undefined')mountRecordingHistory();
