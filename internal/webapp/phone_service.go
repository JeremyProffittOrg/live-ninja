package webapp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/JeremyProffittOrg/live-ninja/internal/tools"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
)

type phoneLambda interface {
	Invoke(context.Context, *lambda.InvokeInput, ...func(*lambda.Options)) (*lambda.InvokeOutput, error)
}
type phoneMessages struct {
	client   phoneLambda
	function string
}

func (p *phoneMessages) invoke(ctx context.Context, payload map[string]any) (map[string]any, *tools.ToolError) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, &tools.ToolError{Code: tools.CodeInvalidArgs, Message: "Invalid phone message request."}
	}
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	out, err := p.client.Invoke(ctx, &lambda.InvokeInput{FunctionName: aws.String(p.function), Payload: raw})
	if err != nil || out == nil || out.FunctionError != nil {
		return nil, &tools.ToolError{Code: tools.CodeUpstreamError, Message: "Phone request outcome is unknown. Check phone_message_status before retrying.", Details: map[string]any{"requestId": payload["requestId"]}}
	}
	var response struct {
		StatusCode int    `json:"statusCode"`
		Body       string `json:"body"`
	}
	var body map[string]any
	if json.Unmarshal(out.Payload, &response) != nil || json.Unmarshal([]byte(response.Body), &body) != nil {
		return nil, &tools.ToolError{Code: tools.CodeUpstreamError, Message: "Phone response could not be verified. Check its requestId before retrying.", Details: map[string]any{"requestId": payload["requestId"]}}
	}
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		if payload["operation"] == "owner_message_send" && (body["providerStatus"] == "rejected" || body["providerStatus"] == "unknown") {
			return nil, &tools.ToolError{Code: tools.CodeUpstreamError, Message: "The provider did not confirm this call. Check the existing request; do not redial automatically.", Details: body}
		}
		return body, nil
	}
	code := tools.CodeUpstreamError
	switch response.StatusCode {
	case 400:
		code = tools.CodeInvalidArgs
	case 401, 403:
		code = tools.CodeForbidden
	case 404:
		code = tools.CodeNotFound
	case 409:
		code = tools.CodeAlreadyExists
	case 402, 429:
		code = tools.CodeConfirmationRequired
	case 503:
		code = tools.CodeNotConfigured
	}
	return nil, &tools.ToolError{Code: code, Message: "Phone message was not accepted. Review the returned status before another request.", Details: body}
}
func (p *phoneMessages) Send(ctx context.Context, owner, id, destination, message string) (map[string]any, *tools.ToolError) {
	return p.invoke(ctx, map[string]any{"operation": "owner_message_send", "ownerUserId": owner, "requestId": id, "destination": destination, "message": message})
}
func (p *phoneMessages) Status(ctx context.Context, owner, id string) (map[string]any, *tools.ToolError) {
	return p.invoke(ctx, map[string]any{"operation": "owner_message_status", "ownerUserId": owner, "requestId": id})
}
func (p *phoneMessages) BudgetStatus(ctx context.Context, owner string) (map[string]any, *tools.ToolError) {
	return p.invoke(ctx, map[string]any{"operation": "owner_budget_status", "ownerUserId": owner})
}
func (p *phoneMessages) BudgetGrant(ctx context.Context, owner, id, day string) (map[string]any, *tools.ToolError) {
	return p.invoke(ctx, map[string]any{"operation": "owner_budget_grant", "ownerUserId": owner, "requestId": id, "expectedDay": day})
}
func newPhoneMessages(cfg aws.Config, function string) (tools.PhoneMessages, error) {
	if function == "" {
		return nil, nil
	}
	parts := strings.Split(function, ":")
	if len(parts) != 7 || parts[0] != "arn" || parts[2] != "lambda" || parts[3] != "us-east-1" || parts[5] != "function" || parts[6] != "live-ninja-phone-trial-recorder" {
		return nil, fmt.Errorf("invalid owner phone function ARN")
	}
	return &phoneMessages{client: lambda.NewFromConfig(cfg, func(o *lambda.Options) { o.Region = parts[3] }), function: function}, nil
}
