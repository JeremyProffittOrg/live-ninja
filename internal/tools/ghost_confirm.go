package tools

// Server-issued, user-bound confirmation for the two side-effecting ghost
// tools (ghost_run_start, ghost_session_command).
//
// A model-supplied confirm=true on its own is not authority: nothing would
// bind the confirmed call to the proposal the owner actually heard, so a
// first call with confirm=true, an injected transcript (ghost_session_summary
// hands the same model verbatim session text) or a misheard turn could launch
// a run or type into a live agent with arguments nobody read back. The repo's
// other mutating tools therefore take their confirmation from a signed-in
// review (jobs.go, rules.go, internal/codeapproval). The ghost tools have no
// review page, and the owner's decision (ghost-decision-scope, 2026-10-19:
// "each start or command still needs confirmation") is a conversational one,
// so the confirmation is made server-issued instead:
//
//  1. The unconfirmed call validates everything, writes a GHOSTPROP# row keyed
//     by the caller's user id and a random id, holding the hash of the exact
//     binding arguments, and returns confirmation_required with that
//     proposalId beside the proposal. The row lives ghostProposalTTL.
//  2. The confirmed call must carry confirm=true AND that proposalId. The
//     handler recomputes the hash from the arguments it was given, refuses a
//     missing, foreign, expired, already-used or non-matching proposal, and
//     consumes the row with a conditional delete before anything is sent. A
//     proposal is therefore usable once, by the user it was issued to, within
//     15 minutes, for exactly the arguments that were read back.
//
// confirm=true without a proposalId is refused outright: there is no path on
// any surface — web, Android or MCP — from a lone confirmed call to a launch.
// The registry's IDEMP# claim still makes the confirmed call itself
// at-most-once; this layer makes it bound to a read-back.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// ghostProposalTTL is how long a read-back proposal stays confirmable. It
// matches codeapproval.IntentTTL: long enough for a spoken yes, short enough
// that a stale proposal cannot be confirmed into a different conversation.
const ghostProposalTTL = 15 * time.Minute

const ghostProposalSK = "GHOSTPROP"

var ghostProposalIDPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

func ghostProposalPK(userID, id string) string { return "GHOSTPROP#" + userID + "#" + id }

// ghostProposalParam is the proposalId argument both manage tools declare.
func ghostProposalParam() ParamSpec {
	return ParamSpec{Name: "proposalId", Type: "string", MaxLen: 64,
		Description: "Required with confirm=true: the proposalId from the confirmation_required reply whose proposal you read back. It is valid for 15 minutes, once, for exactly those arguments."}
}

// ghostProposalHash canonicalizes the binding arguments. json.Marshal sorts
// map keys, so two calls with the same values hash the same regardless of the
// order the model sent them.
func ghostProposalHash(operation string, binding map[string]any) string {
	raw, _ := json.Marshal(binding)
	sum := sha256.Sum256([]byte("live-ninja/ghost-proposal\x00" + operation + "\x00" + string(raw)))
	return hex.EncodeToString(sum[:])
}

func ghostInvocationProposalHash(inv Invocation, operation string, binding map[string]any) string {
	hash := ghostProposalHash(operation, binding)
	if inv.Surface != "alexa" {
		return hash
	}
	sum := sha256.Sum256([]byte("live-ninja/alexa-proposal\x00" + hash + "\x00" + inv.ConfirmationContext))
	return hex.EncodeToString(sum[:])
}

func ghostNewProposalID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// ghostProposalStore is the DeleteItem the atomic consume needs. Deps carries
// it as IdempotencyRelease (NewRegistry defaults it from the DynamoDB client).
type ghostProposalStore interface {
	DeleteItem(ctx context.Context, params *dynamodb.DeleteItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error)
}

func ghostProposalDeleter(d *Deps) ghostProposalStore {
	if d.IdempotencyRelease != nil {
		return d.IdempotencyRelease
	}
	if del, ok := d.DDB.(ghostProposalStore); ok {
		return del
	}
	return nil
}

// ghostIssueProposal records the proposal and returns the
// confirmation_required error the handler hands back. operation names the
// tool; proposed is what the model reads back (it may carry informational
// fields such as nodeOnline); binding is the subset the confirmation is bound
// to. The registry releases the IDEMP# claim for confirmation_required, so the
// confirmed retry uses a fresh key.
func ghostIssueProposal(ctx context.Context, d *Deps, inv Invocation, operation, message string, proposed, binding map[string]any) *ToolError {
	if inv.Surface == "alexa" && len(inv.ConfirmationContext) != 64 {
		return toolErrf(CodeForbidden, "Alexa conversation authority is missing; nothing was proposed")
	}
	if d.Store == nil || ghostProposalDeleter(d) == nil {
		return toolErrf(CodeNotConfigured, "the confirmation store is not configured, so %s cannot be proposed", operation)
	}
	id, err := ghostNewProposalID()
	if err != nil {
		d.Log.Error("tools: ghost proposal id", "error", err.Error())
		return toolErrf(CodeUpstreamError, "could not record the proposal; nothing was sent")
	}
	now := d.Now()
	expires := now.Add(ghostProposalTTL)
	err = d.Store.ConditionalPut(ctx, ghostProposalPK(inv.UserID, id), ghostProposalSK, map[string]any{
		"operation": operation,
		"hash":      ghostInvocationProposalHash(inv, operation, binding),
		"userId":    inv.UserID,
		"tool":      inv.Tool,
		"createdAt": now.UTC().Format(time.RFC3339),
		"expiresAt": expires.Unix(),
	}, expires.Unix())
	if err != nil {
		d.Log.Error("tools: ghost proposal write failed", "operation", operation, "error", err.Error())
		return toolErrf(CodeUpstreamError, "could not record the proposal; nothing was sent")
	}
	return &ToolError{
		Code:    CodeConfirmationRequired,
		Message: message,
		Details: map[string]any{
			"operation":       operation,
			"proposed":        proposed,
			"confirmRequired": true,
			"proposalId":      id,
			"proposalExpires": expires.UTC().Format(time.RFC3339),
			"confirmWith":     "call " + operation + " again with the same arguments, confirm=true, this proposalId and a new idempotencyKey",
		},
	}
}

// ghostConsumeProposal checks the confirmed call against the proposal it
// names and consumes it. nil means the caller may proceed to the side effect;
// every error is returned BEFORE anything external is attempted, with a code
// idempotencyReleasable allows, except a store failure.
func ghostConsumeProposal(ctx context.Context, d *Deps, inv Invocation, operation string, args map[string]any, binding map[string]any) *ToolError {
	id, _ := args["proposalId"].(string)
	id = strings.TrimSpace(id)
	if !ghostProposalIDPattern.MatchString(id) {
		return toolErrf(CodeInvalidArgs, "confirm=true needs the proposalId from the confirmation_required reply; call %s without confirm first, read the proposal back, and confirm that proposalId", operation)
	}
	deleter := ghostProposalDeleter(d)
	if d.Store == nil || deleter == nil {
		return toolErrf(CodeNotConfigured, "the confirmation store is not configured, so %s cannot be confirmed", operation)
	}
	pk := ghostProposalPK(inv.UserID, id)
	item, err := d.Store.GetItem(ctx, pk, ghostProposalSK)
	if err != nil {
		d.Log.Error("tools: ghost proposal read failed", "operation", operation, "error", err.Error())
		return toolErrf(CodeUpstreamError, "could not read the proposal; nothing was sent")
	}
	storedOp, _ := item["operation"].(string)
	storedUser, _ := item["userId"].(string)
	if item == nil || storedOp != operation || storedUser != inv.UserID {
		return toolErrf(CodeNotFound, "that proposal is unknown or was already used; call %s without confirm to get a fresh one and read it back", operation)
	}
	if exp, ok := ghostItemInt64(item["expiresAt"]); !ok || !d.Now().Before(time.Unix(exp, 0)) {
		return toolErrf(CodeNotFound, "that proposal expired (they last %d minutes); call %s without confirm to get a fresh one and read it back", int(ghostProposalTTL.Minutes()), operation)
	}
	hash := ghostInvocationProposalHash(inv, operation, binding)
	if stored, _ := item["hash"].(string); stored != hash {
		return toolErrf(CodeInvalidArgs, "the confirmed arguments differ from the proposal that was read back; nothing was sent. Call %s without confirm so the owner hears the exact change, then confirm that proposalId", operation)
	}
	_, err = deleter.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(d.TableName),
		Key: map[string]ddbtypes.AttributeValue{
			"pk": &ddbtypes.AttributeValueMemberS{Value: pk},
			"sk": &ddbtypes.AttributeValueMemberS{Value: ghostProposalSK},
		},
		ConditionExpression:      aws.String("attribute_exists(pk) AND #h = :h"),
		ExpressionAttributeNames: map[string]string{"#h": "hash"},
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":h": &ddbtypes.AttributeValueMemberS{Value: hash},
		},
	})
	if err != nil {
		var cond *ddbtypes.ConditionalCheckFailedException
		if errors.As(err, &cond) {
			return toolErrf(CodeAlreadyExists, "that proposal was already used; nothing more was sent")
		}
		d.Log.Error("tools: ghost proposal consume failed", "operation", operation, "error", err.Error())
		return toolErrf(CodeUpstreamError, "could not consume the proposal; nothing was sent")
	}
	return nil
}

// ghostItemInt64 reads a DynamoDB number as the store unmarshals it into a
// map[string]any (float64, or an integer type from a fake).
func ghostItemInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int64:
		return n, true
	case int:
		return int64(n), true
	default:
		return 0, false
	}
}
