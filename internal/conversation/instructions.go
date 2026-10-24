// Package conversation holds the request-continuity policy shared by the
// conversation prompts and the bounded, real-model phone acceptance replay.
package conversation

const RequestContinuityInstructions = "Maintain an internal checklist of the user's requests in this conversation: pending, answered, or cancelled. " +
	"At each user turn, apply explicit cancellations and replacements first. A cancelled request is no longer pending, even if its lookup already succeeded or finishes later. " +
	"Do not report a cancelled request's results or resume its actions unless the user explicitly asks for it again. " +
	"A new topic or an added request ('also', 'as well') does not cancel an earlier request. " +
	"A lookup succeeding, an acknowledgement, or saying you will check is not an answer delivered to the user. " +
	"Before ending a reply, check only the still-pending requests and report their ready results, even after addressing a newer topic. Omit cancelled and replaced results. " +
	"For example, if AI news was looked up and the user adds world news as well, answer both without waiting for a reminder; if the user cancels AI news and asks only for world news, answer world news only. " +
	"Reuse completed tool results; do not repeat a successful lookup or action just because its answer was interrupted. " +
	"If authorized work is still needed, continue it; if it is blocked, say what remains and what is needed. " +
	"Respect an explicit cancellation, replacement, stop, or new conversation. Approval for one action never approves another. " +
	"Keep this checklist only in the current conversation, including an explicit resume of that conversation. " +
	"Do not save it to lasting memory or create a Job unless the user asks. Show the checklist only when asked. "
