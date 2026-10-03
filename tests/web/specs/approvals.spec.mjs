import { test, expect } from '@playwright/test';
import AxeBuilder from '@axe-core/playwright';
const baseURL = process.env.LN_APPROVALS_BASE_URL || 'http://127.0.0.1:8795';
if (!['localhost','127.0.0.1','[::1]'].includes(new URL(baseURL).hostname)) throw new Error('Approval tests require an isolated loopback runner.');
test.use({baseURL,serviceWorkers:'block'});
const current = {name:'packing',description:'When preparing luggage',body:'Existing private rule',enabled:false,version:7,updatedAt:'2030-01-01T00:00:00Z'};
async function openRule(page, operation='save') {
 await page.goto('/approvals');
 await page.route('**/api/v1/rules', route=>route.fulfill({json:{rules:[current]}}));
 await page.evaluate(async ({operation})=>{
  const {openProposalReview}=await import('/static/js/approval-review.mjs');
  await openProposalReview({operation,proposed:{name:'packing',description:'When preparing luggage',body:'New <img src=x onerror=alert(1)> private rule'}});
 },{operation});
}
test('real local coding intent: verify snapshot, record one receipt, never launch', async({page})=>{
 await page.goto('/approvals');
 await expect(page.locator('#approvalPrepare')).toBeEnabled();
 await expect(page.locator('#approvalCatalog')).toContainText('LOCAL TEST CATALOG');
 const instructions=`Browser review ${Date.now()}-${Math.random()}`;
 await page.getByLabel('Instructions',{exact:true}).fill(instructions);
 await page.getByRole('button',{name:'Review proposal',exact:true}).click();
 await page.getByRole('button',{name:'Prepare verified review',exact:true}).click();
 const dialog=page.getByRole('dialog');
 await expect(dialog).toContainText(instructions);
 await expect(dialog).toContainText('Repository verified: false');
 const approve=page.getByRole('button',{name:'Record approval — do not launch',exact:true});
 await approve.dblclick();
 await expect(dialog).toContainText('Approval recorded. Coding has not started.');
 await expect(dialog).toContainText('Execution: not started');
 await page.getByRole('button',{name:'Close',exact:true}).click();
 await page.reload();
 const card=page.locator('.approval-card').filter({hasText:instructions});
 await expect(card).toHaveCount(1);
 await expect(card).toContainText('approved_blocked');
 await card.getByRole('button',{name:'View receipt'}).click();
 await expect(page.getByRole('dialog')).toContainText('Execution has not started');
 await expect(page.getByRole('button',{name:'Record approval — do not launch'})).toHaveCount(0);
});
test('rule review saves only after deliberate click with exact snapshot (API fixture)', async({page})=>{
 let requests=[];
 await page.route('**/api/v1/rule-reviews',async route=>{requests.push(route.request().postDataJSON());await new Promise(resolve=>setTimeout(resolve,200));await route.fulfill({json:{status:'saved'}});});
 await openRule(page);
 const dialog=page.getByRole('dialog');
 await expect(dialog).toContainText(current.body); await expect(dialog).toContainText('New <img');
 expect(await dialog.locator('img').count()).toBe(0); expect(requests).toHaveLength(0);
 await page.getByRole('button',{name:'Approve and save rule',exact:true}).dblclick();
 await expect(dialog).toContainText('Rule saved.');
 expect(requests).toHaveLength(1);
 expect(requests[0]).toMatchObject({operation:'save',expectedVersion:7,expectedUpdatedAt:current.updatedAt,expectedRule:{description:current.description,body:current.body,enabled:false},proposed:{enabled:false}});
});
test('rule review declining and stale state never mutate again (API fixture)', async({page})=>{
 let calls=0;
 await page.route('**/api/v1/rule-reviews',route=>{calls++;return route.fulfill({status:409,json:{error:{code:'version_conflict',message:'This rule changed. Review its latest version.'}}});});
 await openRule(page,'delete');
 await page.getByRole('button',{name:'Close',exact:true}).click(); expect(calls).toBe(0);
 await openRule(page,'delete');
 await page.getByRole('button',{name:'Approve and delete rule'}).click();
 await expect(page.getByRole('dialog')).toContainText('This rule changed');
 await expect(page.getByRole('button',{name:'Approve and delete rule'})).toBeDisabled(); expect(calls).toBe(1);
});
test('review dialog keyboard and narrow layout (API fixture)',async({page})=>{
 await openRule(page);
 await page.setViewportSize({width:320,height:720});
 await expect(page.getByRole('dialog')).toBeVisible();
 expect(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth)).toBe(true);
 const results=await new AxeBuilder({page}).include('.approval-dialog').analyze();
 expect(results.violations).toEqual([]);
 await page.keyboard.press('Escape'); await expect(page.getByRole('dialog')).toHaveCount(0);
});
test('approval history follows cursors including filtered empty pages (API fixture)',async({page})=>{
 const row=id=>({id,status:'prepared',action:{repo:`example/${id}`,instructions:`Saved ${id}`}});
 await page.route('**/api/v1/code-approvals?*',route=>{
   const cursor=new URL(route.request().url()).searchParams.get('cursor');
   return route.fulfill({json:cursor==='empty' ? {intents:[],nextCursor:'last'} : {intents:[row('second')],nextCursor:''}});
 });
 await page.route('**/api/v1/code-approvals',route=>route.fulfill({json:{intents:[row('first')],nextCursor:'empty'}}));
 await page.goto('/approvals'); await expect(page.locator('#approvalList')).toContainText('Saved first');
 await page.getByRole('button',{name:'Load more reviews'}).click();
 await expect(page.locator('#approvalList')).toContainText('Saved second');
 await expect(page.locator('#approvalList')).toContainText('Saved first');
 await expect(page.getByRole('button',{name:'Load more reviews'})).toBeHidden();
});
