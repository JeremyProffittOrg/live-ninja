package webapp

import (
	"context"
	"encoding/json"
	"mime"
	"regexp"
	"sort"
	"strings"

	"github.com/JeremyProffittOrg/live-ninja/internal/tools"
	"github.com/gofiber/fiber/v2"
)

type mcpToolInvoker interface {
	Manifest() []map[string]any
	Invoke(context.Context, tools.Invocation) *tools.Result
}
type mcpPolicy struct {
	scope   string
	mutates bool
}

var mcpPolicies = map[string]mcpPolicy{
	"memory_search": {"memory:read", false}, "entity_get": {"memory:read", false}, "recall_note": {"memory:read", false},
	"memory_write": {"memory:write", true}, "plan_upsert": {"memory:write", true}, "forget": {"memory:write", true}, "remember_note": {"memory:write", true},
	"job_list": {"jobs:read", false}, "job_status": {"jobs:read", false},
	"job_create": {"jobs:manage", false}, "job_start": {"jobs:manage", false}, "job_pause": {"jobs:manage", false},
	"job_resume": {"jobs:manage", false}, "job_cancel": {"jobs:manage", false}, "job_retry": {"jobs:manage", false}, "job_command": {"jobs:manage", false},
	"call_message": {"phone:send", true}, "phone_message_status": {"phone:read", false},
}
var mcpRequestKey = regexp.MustCompile("^[A-Za-z0-9_-]{8,128}$")
var mcpNumericID = regexp.MustCompile("^-?[0-9]+$")

func mcpVersion(v string) bool { return v == "2025-03-26" || v == "2025-06-18" || v == "2025-11-25" }
func mcpHasScope(c *fiber.Ctx, scope string) bool {
	scopes, _ := c.Locals("mcpScopes").([]string)
	for _, s := range scopes {
		if s == scope {
			return true
		}
	}
	return false
}
func mcpAccepts(header, want string) bool {
	for _, part := range strings.Split(header, ",") {
		media, params, err := mime.ParseMediaType(strings.TrimSpace(part))
		if err == nil && media == want && params["q"] != "0" {
			return true
		}
	}
	return false
}
func mcpError(c *fiber.Ctx, id json.RawMessage, code int, message string) error {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	return c.JSON(fiber.Map{"jsonrpc": "2.0", "id": id, "error": fiber.Map{"code": code, "message": message}})
}
func mcpResult(c *fiber.Ctx, id json.RawMessage, result any) error {
	return c.JSON(fiber.Map{"jsonrpc": "2.0", "id": id, "result": result})
}
func mcpToolList(c *fiber.Ctx, registry mcpToolInvoker) []map[string]any {
	result := []map[string]any{}
	for _, tool := range registry.Manifest() {
		name, _ := tool["name"].(string)
		policy, ok := mcpPolicies[name]
		if !ok || !mcpHasScope(c, policy.scope) {
			continue
		}
		schema, _ := tool["parameters"].(map[string]any)
		// Manifest returns fresh schema maps. Preserve the shared tool's types/bounds.
		props, _ := schema["properties"].(map[string]any)
		if props == nil {
			props = map[string]any{}
			schema["properties"] = props
		}
		schema["additionalProperties"] = false
		if policy.mutates {
			props["requestId"] = map[string]any{"type": "string", "pattern": mcpRequestKey.String(), "description": "A stable unique ID for this requested action. Reuse it after a timeout; never change it to retry an uncertain action."}
			required, _ := schema["required"].([]string)
			schema["required"] = append(required, "requestId")
		}
		readonly := !policy.mutates && !strings.HasPrefix(policy.scope, "jobs:manage")
		result = append(result, map[string]any{"name": name, "description": tool["description"], "inputSchema": schema,
			"annotations":     map[string]any{"readOnlyHint": readonly, "destructiveHint": name == "forget", "idempotentHint": readonly, "openWorldHint": name == "call_message"},
			"securitySchemes": []map[string]any{{"type": "oauth2", "scopes": []string{policy.scope}}}})
	}
	sort.Slice(result, func(i, j int) bool { return result[i]["name"].(string) < result[j]["name"].(string) })
	return result
}

// Stateless Streamable HTTP: JSON responses; no optional server-initiated SSE.
func handleMCP(registry mcpToolInvoker, origin string) fiber.Handler {
	return func(c *fiber.Ctx) error {
		c.Set("Cache-Control", "no-store")
		if got := c.Get("Origin"); got != "" && got != origin {
			return c.SendStatus(fiber.StatusForbidden)
		}
		if UserID(c) == "" || Role(c) != "owner" {
			return c.SendStatus(fiber.StatusForbidden)
		}
		if c.Method() != fiber.MethodPost {
			c.Set("Allow", "POST")
			return c.SendStatus(fiber.StatusMethodNotAllowed)
		}
		if !mcpAccepts(c.Get("Accept"), "application/json") || !mcpAccepts(c.Get("Accept"), "text/event-stream") {
			return c.SendStatus(fiber.StatusNotAcceptable)
		}
		contentType, _, err := mime.ParseMediaType(c.Get("Content-Type"))
		if err != nil || contentType != "application/json" {
			return c.SendStatus(fiber.StatusUnsupportedMediaType)
		}
		if len(c.Body()) > 65536 {
			return c.SendStatus(fiber.StatusRequestEntityTooLarge)
		}
		version := c.Get("MCP-Protocol-Version")
		if version != "" && !mcpVersion(version) {
			return c.Status(400).JSON(fiber.Map{"error": "unsupported_protocol_version"})
		}
		var req struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
		}
		if !json.Valid(c.Body()) {
			return mcpError(c, nil, -32700, "Invalid JSON")
		}
		if json.Unmarshal(c.Body(), &req) != nil {
			return mcpError(c, nil, -32600, "Invalid request")
		}
		if req.JSONRPC != "2.0" || req.Method == "" {
			return mcpError(c, nil, -32600, "Invalid request")
		}
		if len(req.ID) == 0 {
			if req.Method == "notifications/initialized" || req.Method == "notifications/cancelled" {
				return c.SendStatus(fiber.StatusAccepted)
			}
			return c.Status(400).SendString("Unsupported notification")
		}
		var idText string
		if len(req.ID) > 256 || (!mcpNumericID.Match(req.ID) && json.Unmarshal(req.ID, &idText) != nil) || string(req.ID) == "null" {
			return mcpError(c, nil, -32600, "Invalid request ID")
		}
		if registry == nil {
			return mcpError(c, req.ID, -32603, "Tool service unavailable")
		}
		switch req.Method {
		case "initialize":
			var p struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			if json.Unmarshal(req.Params, &p) != nil || p.ProtocolVersion == "" {
				return mcpError(c, req.ID, -32602, "protocolVersion is required")
			}
			selected := p.ProtocolVersion
			if !mcpVersion(selected) {
				selected = "2025-11-25"
			}
			return mcpResult(c, req.ID, fiber.Map{"protocolVersion": selected, "capabilities": fiber.Map{"tools": fiber.Map{}}, "serverInfo": fiber.Map{"name": "live-ninja", "version": "1.0.0"}, "instructions": "Owner-scoped phone messages, jobs and memory. Job mutation tools return review proposals; they do not launch general AI workers. Queued calls do not prove a message was heard."})
		case "ping":
			return mcpResult(c, req.ID, fiber.Map{})
		case "tools/list":
			var p struct {
				Cursor string `json:"cursor"`
			}
			if len(req.Params) > 0 && (json.Unmarshal(req.Params, &p) != nil || p.Cursor != "") {
				return mcpError(c, req.ID, -32602, "Invalid cursor")
			}
			return mcpResult(c, req.ID, fiber.Map{"tools": mcpToolList(c, registry)})
		case "tools/call":
			var p struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			if json.Unmarshal(req.Params, &p) != nil {
				return mcpError(c, req.ID, -32602, "Invalid tool parameters")
			}
			policy, ok := mcpPolicies[p.Name]
			if !ok {
				return mcpError(c, req.ID, -32602, "Unknown tool")
			}
			if !mcpHasScope(c, policy.scope) {
				c.Set("WWW-Authenticate", `Bearer error="insufficient_scope", scope="`+policy.scope+`", resource_metadata="https://live.jeremy.ninja/.well-known/oauth-protected-resource/mcp"`)
				return c.Status(403).JSON(fiber.Map{"error": "insufficient_scope", "scope": policy.scope})
			}
			requestID := ""
			if policy.mutates {
				requestID, _ = p.Arguments["requestId"].(string)
				if !mcpRequestKey.MatchString(requestID) {
					return mcpError(c, req.ID, -32602, "A stable requestId of 8 through 128 letters, digits, underscores or hyphens is required")
				}
				delete(p.Arguments, "requestId")
			}
			idem := ""
			if requestID != "" {
				idem = "mcp:" + p.Name + ":" + requestID
			}
			result := registry.Invoke(c.UserContext(), tools.Invocation{Tool: p.Name, Args: p.Arguments, IdempotencyKey: idem, CallID: string(req.ID), TxID: TxID(c), UserID: UserID(c), SessionID: SessionID(c), Surface: "mcp", Role: Role(c)})
			raw, err := json.Marshal(result)
			if err != nil {
				return mcpError(c, req.ID, -32603, "Tool result unavailable")
			}
			return mcpResult(c, req.ID, fiber.Map{"content": []fiber.Map{{"type": "text", "text": string(raw)}}, "structuredContent": result, "isError": !result.OK})
		default:
			return mcpError(c, req.ID, -32601, "Method not found")
		}
	}
}
