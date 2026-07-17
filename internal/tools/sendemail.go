package tools

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

// emailQueueMessage mirrors cmd/email-dispatch's EmailMessage SQS body
// shape ({template,to,subject,text}); the dispatcher sends via SES from
// jeremy@jeremy.ninja with Reply-To proffitt.jeremy@gmail.com and its
// own per-MessageId idempotency.
type emailQueueMessage struct {
	Template string `json:"template"`
	To       string `json:"to"`
	Subject  string `json:"subject"`
	Text     string `json:"text"`
}

const emailTemplateTool = "tool-send-email"

// sendEmailDefinition is the `send_email` tool: enqueue an email onto
// the EmailQueue. Confirm-before-send policy (plan.md M2 / Voice §7):
// mail to the owner's own inbox sends directly; any external recipient
// requires the model to have obtained explicit user confirmation and to
// pass confirmExternal=true — otherwise the call is rejected with
// confirmation_required so the assistant asks first.
func sendEmailDefinition() *Definition {
	return &Definition{
		Name: "send_email",
		Description: "Send an email. Without a 'to' address it goes to the account owner's inbox. " +
			"Sending to any other address requires the user's explicit spoken confirmation first; " +
			"after the user confirms, call again with confirmExternal=true.",
		SideEffecting: true,
		Params: []ParamSpec{
			{Name: "subject", Type: "string", Required: true, MinLen: 1, MaxLen: 200,
				Description: "Email subject line."},
			{Name: "body", Type: "string", Required: true, MinLen: 1, MaxLen: 10000,
				Description: "Plain-text email body."},
			{Name: "to", Type: "string", MaxLen: 254,
				Description: "Recipient email address. Omit to send to the account owner."},
			{Name: "confirmExternal", Type: "boolean",
				Description: "Set true only after the user explicitly confirmed sending to a non-owner address."},
		},
		Handler: handleSendEmail,
	}
}

func handleSendEmail(ctx context.Context, deps *Deps, inv Invocation, args map[string]any) (map[string]any, *ToolError) {
	if deps.SQS == nil || deps.EmailQueueURL == "" {
		return nil, toolErrf(CodeNotConfigured, "email queue is not configured")
	}
	if deps.OwnerEmail == "" {
		return nil, toolErrf(CodeNotConfigured, "owner email is not configured")
	}

	to := deps.OwnerEmail
	if v, ok := args["to"].(string); ok && v != "" {
		to = v
	}
	if !looksLikeEmail(to) {
		return nil, toolErrf(CodeInvalidArgs, "argument %q is not a valid email address", "to")
	}

	external := !strings.EqualFold(strings.TrimSpace(to), strings.TrimSpace(deps.OwnerEmail))
	confirmed, _ := args["confirmExternal"].(bool)
	if external && !confirmed {
		return nil, toolErrf(CodeConfirmationRequired,
			"sending to an external address (%s) requires the user's explicit confirmation; "+
				"ask the user, then retry with confirmExternal=true", to)
	}

	body, _ := json.Marshal(emailQueueMessage{
		Template: emailTemplateTool,
		To:       to,
		Subject:  args["subject"].(string),
		Text:     args["body"].(string),
	})
	if _, err := deps.SQS.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:    aws.String(deps.EmailQueueURL),
		MessageBody: aws.String(string(body)),
	}); err != nil {
		deps.Log.Error("tools: send_email enqueue failed", "error", err.Error())
		return nil, toolErrf(CodeUpstreamError, "failed to enqueue email")
	}

	return map[string]any{
		"status":   "queued",
		"to":       to,
		"external": external,
	}, nil
}

// looksLikeEmail is a light structural check — the SES layer is the real
// gatekeeper; this just catches obviously-not-an-address model output.
func looksLikeEmail(s string) bool {
	at := strings.Index(s, "@")
	if at < 1 || at != strings.LastIndex(s, "@") {
		return false
	}
	domain := s[at+1:]
	return strings.Contains(domain, ".") && !strings.ContainsAny(s, " \t\r\n<>,;")
}
