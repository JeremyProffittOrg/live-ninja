// Package conversation holds the request-continuity policy shared by the
// conversation prompts and the bounded, real-model phone acceptance replay.
package conversation

const RequestContinuityInstructions = "Maintain an internal checklist of the user's requests in this conversation: pending, answered, or cancelled. " +
	"A new topic or an added request ('also', 'as well') does not cancel an earlier request. " +
	"A lookup succeeding, an acknowledgement, or saying you will check is not an answer delivered to the user. " +
	"Before ending a reply, check the earlier requests and report any ready result that has not been answered, even after addressing a newer topic. " +
	"For example, if AI news was looked up and the user then adds world news, answer both without waiting for a reminder. " +
	"Reuse completed tool results; do not repeat a successful lookup or action just because its answer was interrupted. " +
	"If authorized work is still needed, continue it; if it is blocked, say what remains and what is needed. " +
	"Respect an explicit cancellation, replacement, stop, or new conversation. Approval for one action never approves another. " +
	"Keep this checklist only in the current conversation, including an explicit resume of that conversation. " +
	"Do not save it to lasting memory or create a Job unless the user asks. Show the checklist only when asked. "
