import { test } from 'node:test';
import assert from 'node:assert/strict';
import { reviewedRuleRequest, isReviewProposal, codingSummary } from '../../../web/static/js/approval-review.mjs';
test('reviewed rule edits bind both version and exact prior content', () => {
 const old = { name:'packing',description:'When preparing luggage',body:'Old rule',enabled:false,version:3,updatedAt:'2030-01-01T00:00:00Z' };
 const request = reviewedRuleRequest({operation:'save',proposed:{name:'packing',description:'When preparing luggage',body:'New rule'}},old);
 assert.equal(request.expectedVersion,3); assert.equal(request.expectedUpdatedAt,old.updatedAt);
 assert.deepEqual(request.expectedRule,{description:old.description,body:old.body,enabled:false});
 assert.equal(request.proposed.enabled,false); assert.equal(request.proposed.body,'New rule');
 assert.equal(old.body,'Old rule');
});
test('new rules require absent version; deleted rules are not resurrected by review', () => {
 const details={operation:'save',proposed:{name:'packing',description:'When preparing luggage',body:'New rule'}};
 assert.equal(reviewedRuleRequest(details,null).expectedVersion,0);
 assert.throws(()=>reviewedRuleRequest({operation:'delete',proposed:{name:'packing'}},null),/no longer exists/);
});
test('only recognized confirmation proposals receive human review affordance', () => {
 assert.equal(isReviewProposal({code:'confirmation_required',details:{operation:'save'}}),true);
 assert.equal(isReviewProposal({code:'upstream_error',details:{operation:'save'}}),false);
 assert.equal(isReviewProposal({code:'confirmation_required',details:{operation:'send_email'}}),false);
 assert.match(codingSummary({deploy:true,instructions:'private <text>'}),/Deploy: disabled/);
});
