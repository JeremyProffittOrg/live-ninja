package webapp

import (
	"encoding/json"
	"io"
	"strings"

	"github.com/JeremyProffittOrg/live-ninja/internal/jobs"
	"github.com/gofiber/fiber/v2"
)

func registerJobsHistoryRoutes(api fiber.Router, svc *jobs.Service) {
	api.Get("/:id/conversation", func(c *fiber.Ctx) error {
		p, e := svc.Conversation(c.UserContext(), UserID(c), c.Params("id"), c.Query("cursor"), queryLimit(c, 30, 100))
		if e != nil {
			return jobsError(c, e)
		}
		return c.JSON(p)
	})
	for _, route := range []string{"history", "commands"} {
		name := route
		api.Get("/:id/"+name, func(c *fiber.Ctx) error {
			q := jobs.HistoryQuery{Limit: queryLimit(c, 30, 100), Cursor: c.Query("cursor"), After: c.Query("after")}
			if name == "commands" {
				q.Kind = "note"
			}
			p, e := svc.ListHistory(c.UserContext(), UserID(c), c.Params("id"), q)
			if e != nil {
				return jobsError(c, e)
			}
			return c.JSON(p)
		})
	}
	api.Post("/:id/commands", func(c *fiber.Ctx) error {
		if len(c.Body()) > 16384 {
			return errorJSON(c, 413, "request_too_large", "Job commands must be under 16 KiB.")
		}
		var body struct {
			jobs.NoteInput
			ExpectedVersion int64  `json:"expectedVersion"`
			RequestID       string `json:"requestId"`
		}
		decoder := json.NewDecoder(strings.NewReader(string(c.Body())))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&body) != nil {
			return apiBadRequest(c, "Invalid job command.")
		}
		var extra any
		if e := decoder.Decode(&extra); e != io.EOF {
			return apiBadRequest(c, "Send one JSON command.")
		}
		entry, j, e := svc.AddNote(c.UserContext(), UserID(c), c.Params("id"), body.NoteInput, body.ExpectedVersion, body.RequestID)
		if e != nil {
			return jobsError(c, e)
		}
		return c.JSON(fiber.Map{"command": entry, "job": j, "executionChanged": false})
	})
}
