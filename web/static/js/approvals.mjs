import { apiJSON } from './toolclient.mjs';
import { openProposalReview, openCodeIntent } from './approval-review.mjs';
const $ = id => document.getElementById(id);
const node = (tag, text) => { const e = document.createElement(tag); e.textContent = text; return e; };
let sequence = 0;
let savedIntents = [], nextCursor = '', loading = false;
async function load({append = false} = {}) {
  if (append && loading) return;
  const seq = ++sequence;
  loading = true; $('approvalMore').disabled = true;
  $('approvalNotice').textContent = 'Loading approval history…';
  try {
    let cursor = append ? nextCursor : '', result, pages = 0;
    do {
      result = await apiJSON(`/api/v1/code-approvals${cursor ? `?cursor=${encodeURIComponent(cursor)}` : ''}`);
      if (result.nextCursor && result.nextCursor === cursor) throw new Error('History could not advance. Refresh before trying again.');
      cursor = result.nextCursor || ''; pages++;
    } while (!(result.intents || []).length && cursor && pages < 5);
    if (seq !== sequence) return;
    nextCursor = cursor;
    savedIntents = [...new Map([...(append ? savedIntents : []), ...(result.intents || [])].map(intent => [intent.id, intent])).values()];
    const rows = savedIntents;
    $('approvalList').replaceChildren();
    for (const intent of rows) {
      const card = node('article', ''); card.className = 'approval-card';
      card.append(node('h3', intent.action?.repo || 'Coding review'), node('p', `State: ${intent.status} · Execution not started`), node('p', intent.action?.instructions || ''));
      const button = node('button', intent.status === 'prepared' ? 'Review saved intent' : 'View receipt'); button.type = 'button'; button.className = 'ln-btn ln-btn--ghost';
      button.addEventListener('click', async () => {
        try { const fresh = await apiJSON(`/api/v1/code-approvals/${encodeURIComponent(intent.id)}`); openCodeIntent(fresh.intent, { onChanged: load }); }
        catch (error) { $('approvalNotice').textContent = error.message; }
      }); card.append(button); $('approvalList').append(card);
    }
    $('approvalMore').hidden = !nextCursor;
    $('approvalNotice').textContent = rows.length ? `${rows.length} saved reviews shown.${nextCursor ? ' More reviews are available.' : ''} Approval never starts execution.` : nextCursor ? 'No visible reviews in these pages. Load more to continue.' : 'No saved coding reviews yet.';
  } catch (error) { if (seq === sequence) $('approvalNotice').textContent = error.message || 'Approvals could not be loaded.'; }
  finally { if (seq === sequence) { loading = false; $('approvalMore').disabled = false; } }
}
async function catalog() {
  try {
    const result = await apiJSON('/api/v1/code-approvals/options');
    for (const repo of result.repositories || []) { const option = node('option', repo.repo); option.value = repo.repo; $('approvalRepo').append(option); }
    for (const item of result.nodes || []) { const option = node('option', `${item.node_id} · ${item.status}`); option.value = item.node_id; $('approvalNode').append(option); }
    const available = $('approvalRepo').options.length > 0 && $('approvalNode').options.length > 0;
    $('approvalRepo').disabled = !available; $('approvalNode').disabled = !available; $('approvalPrepare').disabled = !available;
    $('approvalCatalog').textContent = result.capabilities?.fleetVerified === true
      ? 'The repository and machine catalog is connected. Review does not enable execution.'
      : 'LOCAL TEST CATALOG: these are example entries, not a verified machine or repository. No execution is connected.';
  } catch (error) { $('approvalCatalog').textContent = error.message || 'No verified catalog is available. Preparing an approval is disabled.'; }
}
$('approvalRefresh').addEventListener('click', load);
$('approvalMore').addEventListener('click', () => load({append:true}));
$('approvalForm').addEventListener('submit', event => {
  event.preventDefault();
  void openProposalReview({ operation: 'propose_code_update', proposed: { repo: $('approvalRepo').value, node: $('approvalNode').value, instructions: $('approvalInstructions').value.trim(), preprocess: true, deploy: false } }, { onChanged: load });
});
void load(); void catalog();
