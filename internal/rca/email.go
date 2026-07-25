package rca

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	sestypes "github.com/aws/aws-sdk-go-v2/service/sesv2/types"

	"github.com/JeremyProffittOrg/live-ninja/internal/store"
)

// Mailer is the SES v2 seam. A *sesv2.Client satisfies it; tests inject a fake.
type Mailer interface {
	SendEmail(ctx context.Context, params *sesv2.SendEmailInput, optFns ...func(*sesv2.Options)) (*sesv2.SendEmailOutput, error)
}

// SES sender identity, fixed by house policy (CLAUDE.md): sends MUST originate
// from the DKIM-verified @jeremy.ninja identity with Reply-To pointed at the
// human inbox. NEVER send *from* proffitt.jeremy@gmail.com — that identity has
// DKIM disabled, so SES accepts the send and returns a MessageId while Gmail
// silently drops it on DMARC failure. The failure mode is a report that looks
// delivered and never arrives, which is worse than no report at all.
//
// These duplicate the identical unexported constants in cmd/email-dispatch,
// which is package main and therefore not importable. Accepted duplication for
// M17: hoisting them into internal/config would mean editing the one code path
// that provably delivers owner mail today, which is not a risk worth taking in
// this milestone. TestSESSenderIsJeremyNinja guards this copy.
const (
	DefaultFromAddress = "Jeremy Proffitt <jeremy@jeremy.ninja>"
	DefaultReplyTo     = "proffitt.jeremy@gmail.com"
	DefaultRecipient   = "proffitt.jeremy@gmail.com"
)

// Report delivery is a direct sesv2.SendEmail from the analyzer rather than an
// enqueue onto EmailQueue for cmd/email-dispatch, because:
//
//	(a) that path would add an SQS hop and a second Lambda to a <=10/day flow;
//	(b) it is not a least-privilege win — sqs:SendMessage on the email queue is
//	    no narrower than ses:SendEmail on one identity;
//	(c) email-dispatch's EmailMessage shape gives no way back to the SES
//	    MessageId, which is persisted on the RCA record so a "did it actually
//	    send?" question is answerable from the table.
//
// ConfigurationSetName is still passed so bounce/complaint events keep
// streaming to the M7 ops topic.
const (
	subjectPrefix = "Live Ninja RCA: "
	emDash        = "—"

	maxSubjectSymptomRunes = 80
	maxSubjectRunes        = 200

	// bodyWrapColumns is where the prose fields wrap. 92 keeps a 2-space
	// indented line inside a typical plain-text mail viewport without
	// hyphen-splitting identifiers.
	bodyWrapColumns = 92
)

// Subject renders "Live Ninja RCA: <tool> — <symptom>". Newlines and tabs in
// the symptom collapse to spaces (a subject header cannot contain them), the
// symptom is rune-capped with a trailing ellipsis, and the whole subject is
// rune-capped. An empty symptom falls back to the error code so the subject is
// never just a tool name.
func Subject(tool, symptom, errorCode string) string {
	tail := oneLine(symptom)
	if tail == "" {
		tail = oneLine(errorCode)
	}
	if r := []rune(tail); len(r) > maxSubjectSymptomRunes {
		tail = strings.TrimSpace(string(r[:maxSubjectSymptomRunes-1])) + "…"
	}
	subject := subjectPrefix + dash(tool) + " " + emDash + " " + tail
	return truncateRunes(subject, maxSubjectRunes)
}

// Body renders the plain-text report. Deterministic and pure; snapshot-tested.
//
// cfg is taken (rather than the spec's hardcoded "1 per hour, 10 per day"
// footer text) so the footer cannot lie about the dedupe policy after
// RcaCooldownMinutes or RcaDailyCap is changed in template.yaml — a report that
// misstates its own suppression rules would send the reader looking for
// failures that were never dropped.
func Body(rec store.RCARecord, rep Report, in PromptInput, notes []string, cfg Config) string {
	var b strings.Builder

	fmt.Fprintf(&b, "Live Ninja RCA %s %s / %s\n", emDash, dash(rec.Tool), dash(rec.ErrorCode))
	fmt.Fprintf(&b, "rcaId %s   txId %s   confidence %s\n",
		dash(rec.RCAID), dash(rec.TxID), dash(rep.Confidence))

	if rep.Symptom != "" {
		b.WriteString("\nSYMPTOM\n")
		b.WriteString(indentWrapped(rep.Symptom))
	}
	if rep.RootCause != "" {
		b.WriteString("\nROOT CAUSE\n")
		b.WriteString(indentWrapped(rep.RootCause))
	}
	writeNumbered(&b, "EVIDENCE", rep.Evidence)
	writeNumbered(&b, "SUGGESTED CODE FIXES", rep.CodeFixSuggestions)
	writeNumbered(&b, "REPRO STEPS", rep.ReproSteps)

	b.WriteString("\nBASE-KNOWLEDGE SUGGESTIONS (filed as pending; approve in Settings > About you)\n")
	filed, _ := AllowedSuggestions(rep)
	if len(filed) == 0 {
		b.WriteString("  (none)\n")
	}
	for i, sg := range filed {
		line := fmt.Sprintf("  - %s = %s", sg.Field, sg.ProposedValue)
		if sg.Reason != "" {
			line += " " + emDash + " " + sg.Reason
		}
		if i < len(rec.SuggestionIDs) {
			line += fmt.Sprintf("   [PROFSUGG id %s]", rec.SuggestionIDs[i])
		}
		b.WriteString(line + "\n")
	}

	b.WriteString("\nCONTEXT\n")
	fmt.Fprintf(&b, "  occurred      %s\n", dash(rec.OccurredAt))
	fmt.Fprintf(&b, "  surface       %s        engine  %s\n", dash(rec.Surface), dash(rec.Engine))
	fmt.Fprintf(&b, "  session       %s   call    %s\n", dash(rec.SessionID), dash(rec.CallID))
	fmt.Fprintf(&b, "  args          %s\n", oneLine(rec.ArgsJSON))
	fmt.Fprintf(&b, "  error         %s\n", oneLine(rec.ErrorMessage))
	fmt.Fprintf(&b, "  turns in window   %d\n", rec.TurnsInWindow)
	fmt.Fprintf(&b, "  suppressed similar since last report   %d\n", rec.SuppressedCount)
	fmt.Fprintf(&b, "  prior RCAs for this tool+code (30d)    %d\n", len(in.Prior))

	b.WriteString("\nMODEL\n")
	fmt.Fprintf(&b, "  %s   in %d tok / out %d tok   stop=%s\n",
		dash(rec.ModelID), rec.InputTokens, rec.OutputTokens, dash(rec.StopReason))
	fmt.Fprintf(&b, "  prompt sha256 %s\n", truncateRunes(rec.PromptSHA256, 12))

	if len(notes) > 0 {
		b.WriteString("\nNOTES\n")
		for _, n := range notes {
			if n = oneLine(n); n != "" {
				fmt.Fprintf(&b, "  %s\n", n)
			}
		}
	}

	// "-- " (dash dash space) is the conventional signature separator; mail
	// clients hide everything below it by default.
	b.WriteString("\n-- \n")
	fmt.Fprintf(&b, "Automated by cmd/rca-analyzer. Deduped by failure signature %s,\n", dash(rec.Signature))
	fmt.Fprintf(&b, "1 analysis per %s per signature, %d per UTC day.\n",
		humanDuration(cfg.Cooldown), cfg.DailyCap)
	return b.String()
}

func writeNumbered(b *strings.Builder, heading string, items []string) {
	if len(items) == 0 {
		return
	}
	b.WriteString("\n" + heading + "\n")
	for i, item := range items {
		fmt.Fprintf(b, "  %d. %s\n", i+1, item)
	}
}

// indentWrapped word-wraps at bodyWrapColumns with a two-space indent. A token
// longer than the wrap width (a URL, a long identifier) is left intact on its
// own line rather than broken — a split symbol name is unsearchable.
func indentWrapped(s string) string {
	words := strings.Fields(oneLine(s))
	if len(words) == 0 {
		return ""
	}
	var b strings.Builder
	line := "  "
	for _, w := range words {
		if len(line) > 2 && len(line)+1+len(w) > bodyWrapColumns {
			b.WriteString(line + "\n")
			line = "  "
		}
		if len(line) > 2 {
			line += " "
		}
		line += w
	}
	b.WriteString(line + "\n")
	return b.String()
}

// humanDuration renders a cooldown the way the footer reads it ("hour",
// "30m"), so the sentence stays grammatical for the default and honest for a
// tuned value.
func humanDuration(d time.Duration) string {
	switch {
	case d == time.Hour:
		return "hour"
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	default:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
}

// ---- operational notices ----

// Notice kinds, used as the ClaimRCANotice key. Each is rate-limited to one
// email per Config.NoticeWindow.
const (
	NoticeModelUnavailable  = "model_unavailable"
	NoticeMalformedResponse = "malformed_response"
)

// modelUnavailableNotice is the one email sent (at most once per notice window)
// when Bedrock says the account cannot call the configured model. It names the
// exact remediation because the whole point of the degradation path is that the
// pipeline is *waiting on a specific owner action*, not broken.
func modelUnavailableNotice(cfg Config, reason string) (subject, body string) {
	subject = subjectPrefix + "disabled " + emDash + " Bedrock model access unavailable"
	body = fmt.Sprintf(`The tool-failure RCA analyzer cannot call its Bedrock model, so failures are
being recorded WITHOUT analysis.

  RCA_MODEL_ID   %s
  region         %s
  reason         %s

Remediation (owner action):
  Bedrock console -> %s -> Model access -> request access to Anthropic Claude
  Opus. This is the same flow as the Nova Sonic request. Then confirm the exact
  model id with:
    aws bedrock list-inference-profiles --region %s
    aws bedrock list-foundation-models --by-provider anthropic --region %s
  and, if it differs, update the RcaBedrockModelId stack parameter.

Nothing is lost in the meantime: every failure is still persisted with
status=model_unavailable under pk prefix "RCA#" (30-day TTL), including the
arguments, the error, the transcript window size and the prompt digest — so an
analysis can be re-run by hand once access is granted.

This notice is rate-limited to once per %s.
`, cfg.ModelID, bedrockRegion, reason, bedrockRegion, bedrockRegion, bedrockRegion,
		humanDuration(cfg.NoticeWindow))
	return subject, body
}

// malformedResponseNotice is the once-per-window heads-up that the model is
// replying with something that is not the agreed JSON object. No report is sent
// for such a run — there is nothing to report — but silence would hide a
// contract drift that makes the whole pipeline useless.
func malformedResponseNotice(cfg Config, rcaID, family, reason string) (subject, body string) {
	subject = subjectPrefix + "model reply not parseable " + emDash + " no report sent"
	body = fmt.Sprintf(`The RCA analyzer got a reply from Bedrock that did not contain the agreed JSON
report, so no analysis email was sent for that failure.

  RCA_MODEL_ID   %s
  rcaId          %s
  family (pk)    %s
  reason         %s

The reply is persisted verbatim on that RCA# item as "rawResponse" (capped, 30-day
TTL) so the extraction can be debugged without re-running the model call. If this
repeats, the likely causes are a model generation that ignores the output
contract, or an output-token limit (RCA_MAX_OUTPUT_TOKENS) too low for the
requested report.

This notice is rate-limited to once per %s.
`, cfg.ModelID, dash(rcaID), dash(family), reason, humanDuration(cfg.NoticeWindow))
	return subject, body
}

// send delivers one plain-text mail through SES v2 and returns the MessageId.
func (a *Analyzer) send(ctx context.Context, subject, body string) (string, error) {
	in := &sesv2.SendEmailInput{
		FromEmailAddress: aws.String(a.cfg.EmailFrom),
		ReplyToAddresses: []string{a.cfg.EmailReplyTo},
		Destination:      &sestypes.Destination{ToAddresses: []string{a.cfg.EmailTo}},
		Content: &sestypes.EmailContent{
			Simple: &sestypes.Message{
				Subject: &sestypes.Content{Data: aws.String(subject)},
				Body: &sestypes.Body{
					Text: &sestypes.Content{Data: aws.String(body)},
				},
			},
		},
	}
	if a.cfg.ConfigurationSet != "" {
		in.ConfigurationSetName = aws.String(a.cfg.ConfigurationSet)
	}
	out, err := a.mail.SendEmail(ctx, in)
	if err != nil {
		return "", err
	}
	return aws.ToString(out.MessageId), nil
}
