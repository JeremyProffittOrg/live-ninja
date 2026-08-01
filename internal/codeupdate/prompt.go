package codeupdate

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Prompt assembly.
//
// The prompt handed to the coding agent has three parts, and the ORDER OF
// ASSEMBLY is a security property, not a style choice:
//
//	1. the owner's instructions — the only part Opus is ever shown, and the only
//	   part it may rewrite;
//	2. the operating rules (deploy gate, progress reporting, the run token);
//	3. the mandatory output directive, byte-identical to ghost-cli's own.
//
// Parts 2 and 3 are appended AFTER the rewrite returns. Sending the token
// through a model would put a live credential in a Bedrock request and let a
// rewrite mangle, summarise or drop it; sending the output directive through
// would risk the same for the one sentence that arms the whole capture-and-email
// path. Neither is worth the tidiness of one round trip.

// MaxPromptChars is ghost-cli's own LAUNCH/schedule bound (maxPrompt = 16384).
// A prompt that exceeds it is rejected upstream, so it is enforced here where
// there is still something sensible to do about it.
const MaxPromptChars = 16384

// MaxInstructionChars bounds what the owner may dictate in one go. It is well
// under MaxPromptChars so the operating rules always fit after a rewrite.
const MaxInstructionChars = 8000

// OutputDirective is byte-identical to ghost-cli's outputDirective(outputFile)
// (lambda/command/schedule_prompt.go). It is a CROSS-REPO CONTRACT: the node
// agent watches for this file, uploads it on completion, and lambda/summary
// turns it into the summary email. Drift here silently costs the owner their
// completion email while everything else still appears to work.
func OutputDirective(outputFile string) string {
	return fmt.Sprintf("Output your detailed actions and findings to %s.", outputFile)
}

// PromptInput is everything BuildPrompt needs.
type PromptInput struct {
	Repo         string
	Instructions string
	OutputFile   string
	// Deploy opens the push gate. See the deployRules comment.
	Deploy bool
	// Token is the plaintext run token. Empty omits the progress-reporting
	// block entirely rather than emitting a curl with a blank credential the
	// agent would dutifully call and get 401s from.
	Token string
	// ProgressURL is the absolute endpoint the agent posts progress to.
	ProgressURL string
}

// BuildInstructionPrompt returns just the part that is safe to hand to Opus:
// the repository line and the owner's own words. No token, no directives.
func BuildInstructionPrompt(repo, instructions string) string {
	return "Repository: " + repo + " (already cloned; you are in the working copy)\n\n" +
		strings.TrimSpace(instructions)
}

// BuildPrompt assembles the final prompt. body is either the owner's
// instructions or the Opus rewrite of them — this function does not care which,
// which is exactly why the rewrite cannot reach anything below.
//
// The body is stripped of any directive it already carries first. That is not
// defensive dressing: ghost-cli's preprocessor ALWAYS ends its rewrite with
// outputDirective(outputFile) (it instructs the model to, then canonicalizes the
// response to guarantee it). Appending blindly would emit the directive twice
// and, worse, strand the deploy gate and the progress block AFTER the sentence
// that is supposed to be last.
func BuildPrompt(in PromptInput, body string) string {
	body = strings.TrimSpace(strings.ReplaceAll(body, OutputDirective(in.OutputFile), ""))

	// The operating rules are FIXED-SIZE and non-negotiable, so they are
	// reserved out of the budget BEFORE the body is measured, and the body is
	// what gets cut.
	//
	// Doing this the other way round — assemble, then truncate the result — is
	// what a naive implementation does, and it is exactly backwards: the body
	// comes first, so truncating the assembled string eats the TAIL, which is
	// the deploy gate, the progress block and the directive. That is not a
	// cosmetic loss. The body handed to us on the preprocess path is the Opus
	// rewrite, which ghost-cli itself bounds at 16384 runes, so a rewrite that
	// ran long arrives already at the ceiling and deterministically pushes the
	// gate off the end. What survives is "Follow this repository's own
	// CLAUDE.md" — whose first line, in these repos, is "push to main IS the
	// deploy trigger". A voice command with deploy=false would have instructed
	// the push it exists to forbid, and the confirmation email would still have
	// said "Deploy: NO".
	suffix := "\n\n" + deployRules(in.Deploy)
	if block := progressRules(in.Token, in.ProgressURL); block != "" {
		suffix += "\n\n" + block
	}
	suffix += "\n\n" + OutputDirective(in.OutputFile)

	budget := MaxPromptChars - utf8.RuneCountInString(suffix)
	if budget < 0 {
		// Unreachable with the real strings (the suffix is ~1.2k runes against a
		// 16384 ceiling), but a future rule must never be allowed to produce a
		// negative slice bound.
		budget = 0
	}
	return fitBody(strings.TrimSpace(body), budget) + suffix
}

// fitBody bounds the owner's (or Opus's) prose to budget runes, preferring the
// last complete sentence or line near the limit so the model is not handed a
// half-finished instruction.
func fitBody(body string, budget int) string {
	if utf8.RuneCountInString(body) <= budget {
		return body
	}
	const ellipsis = "..."
	if budget <= len(ellipsis) {
		return ""
	}
	runes := []rune(body)
	candidate := strings.TrimSpace(string(runes[:budget-len(ellipsis)]))
	if boundary := strings.LastIndexAny(candidate, ".!?\n"); boundary >= 0 &&
		utf8.RuneCountInString(candidate[:boundary+1]) >= budget*3/4 {
		return strings.TrimSpace(candidate[:boundary+1])
	}
	return candidate + ellipsis
}

// deployRules is the push gate. It is OPEN by default (owner decision
// 2026-08-01) — see the long note at the deploy parse in
// internal/tools/codeupdate.go for why the closed default was reversed. Closing
// it takes an explicit "don't push" that clears Deploy.
//
// Both branches state their requirement as a CONDITION OF COMPLETION rather
// than as a step in a list, because the failure they exist to prevent is the
// same one in both directions: a run that does the work, writes a good report,
// and leaves the result somewhere the owner has to go and finish by hand. An
// agent reading "commit and push it" as one item among several will happily
// report success with the push not done; "you have not finished until" does not
// leave that reading open.
func deployRules(deploy bool) string {
	if deploy {
		return "Follow this repository's own CLAUDE.md / agents.md conventions for verification, " +
			"committing and deploying. The owner authorized a DEPLOY for this change, and " +
			"shipping it is part of the job, not an optional last step: YOU HAVE NOT FINISHED " +
			"until the work is committed AND PUSHED through the repository's normal delivery " +
			"path and you have watched the resulting pipeline to a terminal result. Do not end " +
			"the run with work committed but unpushed, and do not leave the push for the owner " +
			"to do — they asked for the change, not for a branch to review. If the pipeline " +
			"fails, fixing it and re-pushing is part of this run, not a finding to report and " +
			"stop on. State the commit, the push, and the final pipeline result in your report."
	}
	return "Follow this repository's own CLAUDE.md / agents.md conventions for verification and " +
		"commit hygiene, with ONE override that takes precedence over anything they say: " +
		"DO NOT PUSH, and do not open a pull request. The owner explicitly asked for this " +
		"change to be held, and in these repositories a push to main is a production deploy. " +
		"You have not finished until the work is COMMITTED locally — do not end the run with " +
		"a dirty worktree or changes left only in your report. Commit, stop there, and say " +
		"clearly in your report that the work is committed locally and awaiting a push."
}

// progressRules tells the agent how to email the owner mid-run. The node has no
// mail path of its own, so this endpoint is it.
//
// The token is scoped to one run, expires with the record, and authorizes
// exactly one thing: sending the owner an email about this run. It is worth
// nothing to anyone else, which is what makes putting it in a prompt acceptable
// — but the agent is still told not to write it anywhere.
func progressRules(token, url string) string {
	if strings.TrimSpace(token) == "" || strings.TrimSpace(url) == "" {
		return ""
	}
	return "Report progress by email at three points: when you have read enough of the code to " +
		"have a plan, at roughly the halfway mark, and when you are finished or blocked. Each " +
		"report is one HTTP call:\n\n" +
		"  curl -sS -X POST " + url + " \\\n" +
		"    -H \"Authorization: Bearer " + token + "\" \\\n" +
		"    -H \"Content-Type: application/json\" \\\n" +
		"    -d '{\"status\":\"working\",\"summary\":\"<one short paragraph, plain text>\"}'\n\n" +
		"status is one of: planning | working | blocked | done. Send at most " +
		fmt.Sprintf("%d", MaxProgressPosts) + " of these in total. The bearer token above is a " +
		"credential scoped to this run: never write it into a file, a commit, a log line, or " +
		"your report."
}
