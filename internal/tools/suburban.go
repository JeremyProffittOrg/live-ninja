package tools

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/JeremyProffittOrg/live-ninja/internal/mcpauth"
	"github.com/JeremyProffittOrg/live-ninja/internal/suburban"
)

type SuburbanClient interface {
	Invoke(context.Context, string, string, map[string]any, string) (map[string]any, error)
}

func suburbanDefinition() *Definition {
	return &Definition{Name: "suburban_remote", OwnerOnly: true, SideEffecting: true,
		Description: "Use the owner's connected Suburban Remote. First use tool=list to discover currently available vehicle functions and their schemas. Use vehicle_status for connection status, a listed vehicle read for telemetry, and latest_command_status for an existing command result. Vehicle controls are real physical actions: use them only for the owner's explicit request. argumentsJSON is the selected tool's JSON arguments. Do not supply request_id for control calls: it is generated from this invocation's stable identity. To inspect an unknown command use its returned requestId as request_id in latest_command_status; never submit another control merely to retry. If not connected, the owner uses Connect Suburban Remote in Help.",
		Params:      []ParamSpec{{Name: "tool", Type: "string", Required: true, MaxLen: 128, Description: "list, vehicle_status, latest_command_status, or a tool name returned by list."}, {Name: "argumentsJSON", Type: "string", MaxLen: 4096, Description: "JSON object of arguments for the selected remote tool; omit for no arguments."}}, Handler: handleSuburban}
}
func handleSuburban(ctx context.Context, d *Deps, inv Invocation, args map[string]any) (map[string]any, *ToolError) {
	if d.Suburban == nil {
		return nil, toolErrf(CodeNotConfigured, "Connect Suburban Remote from Help.")
	}
	name, _ := args["tool"].(string)
	raw, _ := args["argumentsJSON"].(string)
	arguments := map[string]any{}
	if raw != "" {
		decoder := json.NewDecoder(strings.NewReader(raw))
		if decoder.Decode(&arguments) != nil {
			return nil, toolErrf(CodeInvalidArgs, "argumentsJSON must be a JSON object.")
		}
		var extra any
		if decoder.Decode(&extra) != io.EOF {
			return nil, toolErrf(CodeInvalidArgs, "argumentsJSON must contain one JSON object.")
		}
	}
	requestID := mcpauth.Hash("suburban|" + inv.UserID + "|" + inv.IdempotencyKey)
	out, e := d.Suburban.Invoke(ctx, inv.UserID, name, arguments, requestID)
	if e != nil {
		code, message := CodeUpstreamError, "Suburban did not confirm the request. Check command status before another control."
		if errors.Is(e, suburban.ErrConnect) {
			code, message = CodeNotConfigured, "Connect Suburban Remote from Help, then try again."
		}
		if errors.Is(e, suburban.ErrInvalid) {
			code, message = CodeInvalidArgs, "Use a currently available Suburban tool and its listed arguments. Control request IDs are assigned automatically."
		}
		if errors.Is(e, suburban.ErrBusy) {
			code, message = CodeNotConfigured, "The Suburban connection is refreshing. Try again shortly."
		}
		return nil, &ToolError{Code: code, Message: message, Details: out}
	}
	if failed, _ := out["isError"].(bool); failed {
		return nil, &ToolError{Code: CodeUpstreamError, Message: "Suburban reported a rejected or unconfirmed result. Check its status before another control.", Details: out}
	}
	return out, nil
}
