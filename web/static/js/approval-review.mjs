import { apiJSON, reviewedProposalRequest } from './toolclient.mjs';

const JOB_OPERATIONS=['job_create','job_start','job_pause','job_resume','job_cancel','job_retry','job_command'];
let active = null;
const el = (tag, text, cls) => { const node = document.createElement(tag); if (text != null) node.textContent = text; if (cls) node.className = cls; return node; };
const key = () => crypto.randomUUID();
export function isReviewProposal(error) {
  return error?.code === 'confirmation_required' && ['save', 'delete', 'propose_code_update', ...JOB_OPERATIONS].includes(error?.details?.operation);
}
export function reviewedRuleRequest(details, current) {
  const proposed = details.proposed;
  if (!['save', 'delete'].includes(details.operation) || !proposed?.name) throw new Error('This proposal is incomplete. Ask for a new proposal.');
  if (details.operation === 'delete' && !current) throw new Error('This rule no longer exists. Nothing was deleted.');
  return {
    operation: details.operation, requestId: key(), reviewedAt: new Date().toISOString(),
    expectedVersion: current?.version ?? 0,
    expectedUpdatedAt: current?.updatedAt ?? '',
    ...(current ? { expectedRule: { description: current.description, body: current.body, enabled: current.enabled } } : {}),
    proposed: details.operation === 'delete' ? { name: proposed.name } : {
      name: proposed.name.trim(), description: proposed.description.trim().replace(/\s+/g, ' '), body: proposed.body.trim(), enabled: current?.enabled ?? true,
    },
  };
}
function dialog(title) {
  active?.close(); active?.remove();
  const d = el('dialog', null, 'approval-dialog'); active = d;
  const heading = el('h2', title); heading.id = 'approvalReviewTitle'; d.setAttribute('aria-labelledby', heading.id);
  const body = el('div', null, 'approval-body');
  const status = el('p', '', 'approval-status'); status.setAttribute('role', 'status'); status.setAttribute('aria-live', 'polite');
  const actions = el('div', null, 'approval-actions'); const close = el('button', 'Close', 'ln-btn ln-btn--ghost'); close.type = 'button'; close.addEventListener('click', () => d.close());
  actions.append(close); d.append(heading, body, status, actions); document.body.append(d);
  d.addEventListener('close', () => { d.remove(); if (active === d) active = null; });
  d.showModal(); close.focus();
  return { d, heading, body, status, actions, close };
}
function field(body, title, text) { body.append(el('h3', title), el('pre', String(text ?? ''), 'approval-value')); }
function button(view, text, action) {
  const b = el('button', text, 'ln-btn ln-btn--primary'); b.type = 'button';
  b.addEventListener('click', async () => {
    if (b.disabled) return; b.disabled = true; view.status.textContent = 'Saving your decision…';
    try { await action(b); } catch (error) {
      view.status.textContent = error?.message || 'The response could not be confirmed. Refresh and inspect the saved state before trying again.';
      view.status.setAttribute('role', 'alert');
      // The outcome may be ambiguous. Never automatically repeat a mutation.
    }
  }); view.actions.append(b); return b;
}
export async function openProposalReview(details, { request = apiJSON, onChanged = () => {} } = {}) {
  // Work with an isolated snapshot. Later assistant turns cannot change this review.
  let bindingError=null;
  if(JOB_OPERATIONS.includes(details?.operation)&&request===apiJSON){try{request=reviewedProposalRequest(details);}catch(e){bindingError=e;}}
  details = structuredClone(details);
  const coding = details?.operation === 'propose_code_update';
  const jobs = JOB_OPERATIONS.includes(details?.operation);
  const v = dialog(jobs ? 'Review job action' : coding ? 'Review coding proposal' : details?.operation === 'delete' ? 'Review rule deletion' : 'Review rule change');
  v.status.textContent = 'Loading the current state…';
  try {
    if(bindingError)throw bindingError;
    if(jobs) {
      const action=reviewedJobRequest(details);
      if(details.job){
        const current=await request(`/api/v1/jobs/${encodeURIComponent(details.job.id)}`);
        if(current.job?.version!==details.proposed.expectedVersion)throw new Error('This job changed. Ask for a fresh proposal after reviewing its latest state.');
        field(v.body,'Current job',`${details.job.title}\n\n${details.job.instructions}\nStatus: ${details.job.status}\nVersion: ${details.job.version}`);
      }
      field(v.body,'Exact proposed action',JSON.stringify(details.proposed,null,2));
      if(details.run)field(v.body,'Run to retry',JSON.stringify(details.run,null,2));
      v.body.append(el('p','This changes only the local reminder or review job shown here. Notes do not steer external agents. Close this review to make no change.'));
      v.status.textContent='Review the complete action before approving.';
      button(v,'Approve job action',async b=>{
        const result=await request(action.path,{method:'POST',json:action.payload});
        if(!result?.job?.id)throw new Error('The server did not confirm the change. Check Jobs before retrying.');
        b.remove();v.status.textContent=details.operation==='job_command'?'Note saved. No external instructions changed.':'Job action recorded. Open Jobs to inspect its current state and receipt.';
        onChanged(result);
      });
    } else if (coding) {
      field(v.body, 'Proposed work', codingSummary(details.proposed));
      v.body.append(el('p', 'Preparing verifies the selected repository and machine and saves a review snapshot. It does not start work. Coding execution is currently unavailable.'));
      v.status.textContent = '';
      const requestId = key();
      button(v, 'Prepare verified review', async b => {
        const prepared = await request('/api/v1/code-approvals/prepare', { method: 'POST', json: { ...details.proposed, deploy: false, requestId } });
        b.remove(); showCodeIntent(v, prepared.intent, request, onChanged);
      });
    } else {
      const result = await request('/api/v1/rules');
      if (!v.d.isConnected) return;
      const current = (result.rules || []).find(rule => rule.name === details.proposed?.name);
      const payload = reviewedRuleRequest(details, current);
      field(v.body, 'Rule name', payload.proposed.name);
      if (current) field(v.body, 'Current rule', `${current.description}\n\n${current.body}\n\n${current.enabled ? 'Enabled' : 'Disabled'} · Version ${current.version}`);
      else v.body.append(el('p', 'This creates a new enabled rule.'));
      if (payload.operation === 'save') field(v.body, 'Rule after approval', `${payload.proposed.description}\n\n${payload.proposed.body}\n\n${payload.proposed.enabled ? 'Enabled' : 'Disabled'}`);
      else v.body.append(el('p', 'Approval deletes exactly the rule shown above. If it changes before you approve, this request will be rejected.'));
      v.body.append(el('p', 'The assistant cannot approve this for you. Close this review to make no change.'));
      v.status.textContent = '';
      button(v, payload.operation === 'save' ? 'Approve and save rule' : 'Approve and delete rule', async b => {
        const result = await request('/api/v1/rule-reviews', { method: 'POST', json: payload });
        if (!['saved', 'deleted'].includes(result.status)) throw new Error('The server did not confirm the change. Check Memory before retrying.');
        b.remove(); v.status.textContent = result.status === 'saved' ? 'Rule saved. The change is recorded in Memory.' : 'Rule deleted.';
        onChanged(result);
      });
    }
  } catch (error) { v.status.textContent = error.message || 'The review could not be loaded. Nothing was approved.'; v.status.setAttribute('role', 'alert'); }
  return v.d;
}
export function codingSummary(a = {}) {
  return `Repository: ${a.repo || 'Not selected'}\nMachine: ${a.node || 'Not selected'}\nAgent: ${a.agent || 'default'}\nModel: ${a.model || 'default'}\nEffort: ${a.effort || 'default'}\nPrompt preprocessing: ${a.preprocess === false ? 'off' : 'on'}\nDeploy: disabled\n\n${a.instructions || ''}`;
}
function showCodeIntent(v, intent, request, onChanged) {
  v.body.replaceChildren(); v.heading.textContent = 'Review saved coding intent';
  field(v.body, 'Exact saved action', codingSummary(intent.action));
  field(v.body, 'Verification', `${intent.verification?.source || 'unavailable'}\nRepository verified: ${intent.verification?.repoVerified === true}\nMachine verified: ${intent.verification?.nodeVerified === true}`);
  field(v.body, 'Approval reference', `${intent.id}\nAction hash: ${intent.actionHash}\nExpires: ${intent.expiresAt}`);
  v.body.append(el('p', 'Recording approval will not start coding. Execution is blocked until safe dispatch recovery is connected. A fresh approval will be required before any future launch.'));
  v.status.textContent = intent.status === 'prepared' ? 'Review every field before recording your decision.' : `State: ${intent.status}. Execution has not started.`;
  if (intent.status !== 'prepared') return;
  const requestId = key();
  button(v, 'Record approval — do not launch', async b => {
    const response = await request(`/api/v1/code-approvals/${encodeURIComponent(intent.id)}/approve`, { method: 'POST', json: { expectedVersion: intent.version, expectedActionHash: intent.actionHash, requestId } });
    b.remove(); const saved = response.intent;
    if (saved?.status !== 'approved_blocked' || saved?.receipt?.executionState !== 'not_started') throw new Error('Unexpected approval state. Refresh the approval history; do not submit again.');
    v.status.textContent = 'Approval recorded. Coding has not started. Safe dispatch recovery and a fresh approval are required before launch.';
    field(v.body, 'Saved receipt', `${saved.receipt.id}\nApproved: ${saved.receipt.approvedAt}\nExecution: not started`);
    onChanged(saved);
  });
}
export function openCodeIntent(intent, options = {}) {
  const v = dialog('Review saved coding intent'); showCodeIntent(v, structuredClone(intent), options.request || apiJSON, options.onChanged || (() => {})); return v.d;
}

export function reviewedJobRequest(details) {
  const p=details.proposed;
  if(!JOB_OPERATIONS.includes(details.operation)||details.executionAvailable!==true||!p)throw new Error('This job proposal is incomplete. Ask for a new proposal.');
  if(details.operation==='job_create') {
    if(!p.title||(p.instructions!=null&&typeof p.instructions!=='string')||!['reminder','review'].includes(p.kind)||!p.schedule)throw new Error('This job proposal is incomplete.');
    return {path:'/api/v1/jobs',payload:{title:p.title,instructions:p.instructions || '',kind:p.kind,schedule:p.schedule,requestId:key()}};
  }
  if(!p.jobId||details.job?.id!==p.jobId||!Number.isSafeInteger(p.expectedVersion)||p.expectedVersion!==details.job.version)throw new Error('This job snapshot is incomplete.');
  const payload={expectedVersion:p.expectedVersion,requestId:key()};
  const root=`/api/v1/jobs/${encodeURIComponent(p.jobId)}`;
  if(details.operation==='job_command') {
    if(p.kind!=='note'||typeof p.text!=='string'||!p.text.trim())throw new Error('Only a saved note is supported.');
    return {path:root+'/commands',payload:{...payload,kind:'note',text:p.text,...(p.runId?{runId:p.runId}:{})}};
  }
  if(details.operation==='job_retry') {
    if(!p.runId||details.run?.id!==p.runId)throw new Error('The exact run to retry is missing.');
    return {path:root+`/runs/${encodeURIComponent(p.runId)}/retry`,payload};
  }
  const action={job_start:'run',job_pause:'pause',job_resume:'resume',job_cancel:'cancel'}[details.operation];
  return {path:root+'/'+action,payload};
}
