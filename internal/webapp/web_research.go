package webapp

import (
	"context"
	"encoding/json"

	"github.com/JeremyProffittOrg/live-ninja/internal/store"
	"github.com/JeremyProffittOrg/live-ninja/internal/tools"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
)

func webResearchThroughBroker(deps *Deps) func(context.Context, tools.Invocation, string, int) (*tools.WebResearchResult, *tools.ToolError) {
	return func(ctx context.Context, inv tools.Invocation, query string, days int) (*tools.WebResearchResult, *tools.ToolError) {
		if deps.Lambda == nil || deps.BrokerFn == "" {
			return nil, &tools.ToolError{Code: tools.CodeNotConfigured, Message: "Public web search is not configured."}
		}
		payload, _ := json.Marshal(map[string]any{"query": query, "days": days})
		request := brokerRequest{Mode: "web-search", UserID: inv.UserID, Surface: inv.Surface, Role: inv.Role, TxID: inv.TxID, Payload: payload}
		if inv.Surface == store.SurfaceAlexa {
			request.AlexaTurn = inv.TxID
		}
		response, err := invokeRealtimeBroker(ctx, deps, request, func(o *lambda.Options) { o.Retryer = aws.NopRetryer{} })
		if err != nil {
			return nil, &tools.ToolError{Code: tools.CodeUpstreamError, Message: "Public web search is unavailable."}
		}
		if response.Error != "" {
			code := tools.CodeUpstreamError
			if response.Code >= 400 && response.Code < 500 {
				code = tools.CodeForbidden
			}
			return nil, &tools.ToolError{Code: code, Message: response.Message}
		}
		return response.Search, nil
	}
}
