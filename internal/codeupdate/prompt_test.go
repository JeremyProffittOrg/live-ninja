package codeupdate

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

const (
	testToken   = "cu_req-1_" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	testProgURL = "https://live.jeremy.ninja/v1/code-update/progress"
)

func testInput(deploy bool) PromptInput {
	return PromptInput{
		Repo:        "JeremyProffittOrg/live-ninja",
		OutputFile:  DefaultOutputFile,
		Deploy:      deploy,
		Token:       testToken,
		ProgressURL: testProgURL,
	}
}

// The output directive is a CROSS-REPO CONTRACT with ghost-cli: the node agent
// watches for this exact file and lambda/summary turns it into the completion
// email. Drift silently costs the owner that email while everything else looks
// fine, so it is pinned literally rather than derived in the test.
func TestOutputDirectiveMatchesGhostCLI(t *testing.T) {
	const want = "Output your detailed actions and findings to update-report.md."
	if got := OutputDirective("update-report.md"); got != want {
		t.Fatalf("OutputDirective = %q, want %q (ghost-cli lambda/command/schedule_prompt.go)", got, want)
	}
}

// The directive must be the FINAL paragraph, exactly once, whatever the body
// contained — including a body that already tried to include one.
func TestDirectiveIsLastAndSingle(t *testing.T) {
	for _, body := range []string{
		"do the thing",
		"do the thing\n\nOutput your detailed actions and findings to update-report.md.",
	} {
		got := BuildPrompt(testInput(false), body)
		directive := OutputDirective(DefaultOutputFile)
		if !strings.HasSuffix(got, directive) {
			t.Errorf("prompt does not end with the directive:\n%s", got)
		}
		if n := strings.Count(got, directive); n != 1 {
			t.Errorf("directive appears %d times, want exactly 1", n)
		}
	}
}

// The default MUST be closed. A push to main is a production deploy in these
// repositories, so a misheard sentence must not be able to ship.
func TestDeployGateDefaultsClosed(t *testing.T) {
	got := BuildPrompt(testInput(false), "tighten the retry logic")
	lower := strings.ToLower(got)
	if !strings.Contains(lower, "do not push") {
		t.Errorf("non-deploy prompt does not forbid pushing:\n%s", got)
	}
	if strings.Contains(lower, "commit and push it through") {
		t.Errorf("non-deploy prompt still instructs a push:\n%s", got)
	}
}

func TestDeployGateOpensOnExplicitOptIn(t *testing.T) {
	got := BuildPrompt(testInput(true), "tighten the retry logic")
	lower := strings.ToLower(got)
	if !strings.Contains(lower, "explicitly authorized a deploy") {
		t.Errorf("deploy prompt does not state the authorization:\n%s", got)
	}
	if strings.Contains(lower, "do not push") {
		t.Errorf("deploy prompt still forbids pushing:\n%s", got)
	}
}

// The prompt is the only place the token exists in plaintext, so the block has
// to actually be usable — and must tell the agent not to spread it further.
func TestProgressBlockIsUsable(t *testing.T) {
	got := BuildPrompt(testInput(false), "do the thing")
	for _, want := range []string{testToken, testProgURL, "Authorization: Bearer", "curl"} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt is missing %q:\n%s", want, got)
		}
	}
	if !strings.Contains(strings.ToLower(got), "never write it into a file") {
		t.Error("prompt does not tell the agent to keep the token out of files and commits")
	}
}

// Without a token (or a URL) the block must be omitted entirely rather than
// emitting a curl with a blank credential the agent would dutifully 401 against.
func TestProgressBlockOmittedWithoutCredentials(t *testing.T) {
	for _, in := range []PromptInput{
		{Repo: "o/r", OutputFile: DefaultOutputFile, ProgressURL: testProgURL},
		{Repo: "o/r", OutputFile: DefaultOutputFile, Token: testToken},
		{Repo: "o/r", OutputFile: DefaultOutputFile},
	} {
		got := BuildPrompt(in, "do the thing")
		if strings.Contains(got, "curl") {
			t.Errorf("prompt emitted a progress curl without full credentials:\n%s", got)
		}
		if !strings.HasSuffix(got, OutputDirective(DefaultOutputFile)) {
			t.Error("the output directive was lost when the progress block was omitted")
		}
	}
}

// This is the security property the whole assembly order exists for: what Opus
// is shown must contain the owner's words and NOTHING ELSE.
func TestOpusNeverSeesTheTokenOrTheDirectives(t *testing.T) {
	body := BuildInstructionPrompt("JeremyProffittOrg/live-ninja", "tighten the retry logic")
	for _, forbidden := range []string{
		testToken,
		"cu_",
		testProgURL,
		OutputDirective(DefaultOutputFile),
		"do not push",
		"DO NOT PUSH",
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("the text sent to Opus contains %q:\n%s", forbidden, body)
		}
	}
	if !strings.Contains(body, "tighten the retry logic") {
		t.Error("the text sent to Opus lost the owner's instructions")
	}
	if !strings.Contains(body, "JeremyProffittOrg/live-ninja") {
		t.Error("the text sent to Opus lost the repository")
	}
}

// ghost-cli rejects a prompt over 16384 chars outright, so the bound is enforced
// here where there is still something sensible to do — and the sacrifice is the
// owner's prose, never the operating rules.
//
// The lengths below are not arbitrary. The body on the preprocess path is the
// OPUS REWRITE, which ghost-cli bounds at its own maxPrompt (16384) — so a
// rewrite that ran long arrives already at the ceiling. Every length from "just
// over the budget" to "already at ghost-cli's own limit" is exercised, because
// an earlier version of this function truncated the ASSEMBLED string and so
// silently deleted the deploy gate in exactly that band. What survived was
// "Follow this repository's own CLAUDE.md" — whose first line, in these repos,
// is that pushing to main IS the production deploy. A deploy=false command
// would have instructed the push it exists to forbid, and the confirmation
// email would still have read "Deploy: NO".
func TestOverlongPromptSacrificesTheBodyNotTheRules(t *testing.T) {
	lengths := []int{12000, 15000, 15161, 15190, 16110, 16260, 16321, 16384, 56000}
	for _, deploy := range []bool{false, true} {
		for _, n := range lengths {
			t.Run(fmt.Sprintf("deploy=%v/body=%d", deploy, n), func(t *testing.T) {
				body := strings.Repeat("do the thing. ", n/14+1)[:n]
				got := BuildPrompt(testInput(deploy), body)

				if r := utf8.RuneCountInString(got); r > MaxPromptChars {
					t.Fatalf("prompt is %d runes, over ghost-cli's %d limit", r, MaxPromptChars)
				}
				// The directive must still be the last thing, exactly once.
				directive := OutputDirective(DefaultOutputFile)
				if !strings.HasSuffix(got, directive) {
					t.Error("truncation dropped the output directive")
				}
				if c := strings.Count(got, directive); c != 1 {
					t.Errorf("directive appears %d times, want 1", c)
				}
				// The deploy gate must survive INTACT — a partial sentence is as
				// dangerous as none, because the surviving half says "follow the
				// repo's own conventions".
				lower := strings.ToLower(got)
				if deploy {
					if !strings.Contains(lower, "explicitly authorized a deploy") {
						t.Error("the deploy authorization was truncated away")
					}
				} else {
					if !strings.Contains(got, "DO NOT PUSH") {
						t.Error("the push prohibition was truncated away")
					}
					if !strings.Contains(lower, "did not authorize a deploy") {
						t.Error("the deploy gate was only partially preserved")
					}
				}
				// And the progress block, token and post cap must survive whole:
				// a token with no handling rule beside it is worse than no token.
				for _, want := range []string{testToken, testProgURL, "never write it into a file"} {
					if !strings.Contains(got, want) {
						t.Errorf("truncation dropped %q from the progress block", want)
					}
				}
			})
		}
	}
}

// A body that is entirely consumed must still yield a usable prompt rather than
// a negative slice bound or a stray ellipsis where the task should be.
func TestTinyBudgetDoesNotPanic(t *testing.T) {
	got := BuildPrompt(testInput(false), strings.Repeat("x", 100000))
	if utf8.RuneCountInString(got) > MaxPromptChars {
		t.Fatal("over budget")
	}
	if !strings.HasSuffix(got, OutputDirective(DefaultOutputFile)) {
		t.Error("directive lost")
	}
}

func TestNormalPromptIsNotTruncated(t *testing.T) {
	body := "Add a retry with exponential backoff to the Bedrock client."
	got := BuildPrompt(testInput(false), body)
	if !strings.Contains(got, body) {
		t.Errorf("a short prompt was altered:\n%s", got)
	}
	if strings.Contains(got, "...") {
		t.Error("a short prompt was truncated")
	}
}
