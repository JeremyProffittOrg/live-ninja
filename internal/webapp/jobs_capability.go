package webapp

import (
	"strings"

	"github.com/JeremyProffittOrg/live-ninja/internal/realtime"
	"github.com/JeremyProffittOrg/live-ninja/internal/tools"
	"github.com/gofiber/fiber/v2"
)

func clientCanReviewJobs(c *fiber.Ctx) bool {
	raw := c.Get("X-LN-Capabilities")
	return Scope(c) == "" && len(raw) <= 2048 && realtime.ClientSupportsJobsReview(Surface(c), parseClientCapabilities(raw))
}
func jobsCompatibilityDenial(c *fiber.Ctx, tool, callID string) *tools.Result {
	if !strings.HasPrefix(strings.TrimSpace(tool), "job_") || clientCanReviewJobs(c) {
		return nil
	}
	return &tools.Result{Tool: tool, CallID: callID, TxID: TxID(c), Error: &tools.ToolError{Code: "client_upgrade_required", Message: "Update the Android app or reload the web page before using Jobs tools. No Jobs data was read or action proposed.", TxID: TxID(c)}}
}
