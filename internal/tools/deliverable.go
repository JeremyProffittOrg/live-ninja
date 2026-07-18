package tools

// deliverable_create / deliverable_zip / deliverable_deliver — the M9
// Deliverables Store tool surface (FR-DLV-01..03). Thin adapters over
// internal/deliv.Service (which owns the S3 key discipline, ownership
// checks, async zipper dispatch, and presigning); this file only maps the
// tool-call schema onto the service and the service's sentinel errors
// back onto client-safe ToolError codes.

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/JeremyProffittOrg/live-ninja/internal/deliv"
	"github.com/JeremyProffittOrg/live-ninja/internal/store"
)

// DeliverableService is the subset of *deliv.Service the tool handlers
// use (interface seam so tool tests inject a fake without S3/Lambda).
type DeliverableService interface {
	Create(ctx context.Context, userID, filename, contentType string, content []byte) (*store.Deliverable, error)
	Zip(ctx context.Context, userID string, deliverableIDs []string, zipName string) (*store.Deliverable, error)
	Deliver(ctx context.Context, userID, deliverableID, emailTo string) (*deliv.DeliverResult, error)
}

// deliverableFormats maps the tool's closed `format` enum onto the MIME
// type stored on the object and the filename extension enforced on the
// deliverable (locked M9 decision: text/markdown/html/csv are the direct
// content formats; zips come from deliverable_zip).
var deliverableFormats = map[string]struct {
	contentType string
	ext         string
}{
	"text":     {"text/plain; charset=utf-8", ".txt"},
	"markdown": {"text/markdown; charset=utf-8", ".md"},
	"html":     {"text/html; charset=utf-8", ".html"},
	"csv":      {"text/csv; charset=utf-8", ".csv"},
}

func deliverableCreateDefinition() *Definition {
	return &Definition{
		Name: "deliverable_create",
		Description: "Create a downloadable file (a 'deliverable') from content you produce — a " +
			"document, report, list, or table. The file is stored in the user's Download Center; " +
			"use deliverable_deliver to hand the user a download link or email it.",
		SideEffecting: true,
		Params: []ParamSpec{
			{Name: "name", Type: "string", Required: true, MinLen: 1, MaxLen: 100,
				Description: "Filename for the deliverable, e.g. \"trip-plan\" or \"expenses.csv\". " +
					"The correct extension for the format is appended automatically if missing."},
			{Name: "format", Type: "string", Required: true,
				Enum:        []string{"text", "markdown", "html", "csv"},
				Description: "Content format: plain text, Markdown, a standalone HTML page, or CSV data."},
			{Name: "content", Type: "string", Required: true, MinLen: 1, MaxLen: 100_000,
				Description: "The full file content."},
		},
		Handler: handleDeliverableCreate,
	}
}

func deliverableZipDefinition() *Definition {
	return &Definition{
		Name: "deliverable_zip",
		Description: "Bundle several of the user's existing deliverables into one ZIP archive " +
			"(itself a new deliverable). The archive is built in the background: it appears as " +
			"'pending' and becomes 'ready' shortly — tell the user it will be ready in a moment.",
		SideEffecting: true,
		Params: []ParamSpec{
			{Name: "deliverableIds", Type: "string_array", Required: true,
				Description: "IDs of the deliverables to bundle (from deliverable_create results or the user's list)."},
			{Name: "name", Type: "string", MaxLen: 100,
				Description: "Optional archive name, e.g. \"vacation-docs\". \".zip\" is appended automatically."},
		},
		Handler: handleDeliverableZip,
	}
}

func deliverableDeliverDefinition() *Definition {
	return &Definition{
		// Deliberately NOT SideEffecting: presigning is a pure read, and
		// the only side effect (re-emailing the user their own download
		// link) is harmless to repeat — while the duplicate-suppressed
		// response of an idempotency-guarded retry would LOSE the URL the
		// caller needs. At-least-once beats at-most-once here.
		Name: "deliverable_deliver",
		Description: "Deliver an existing deliverable to the user: mint a download link that " +
			"expires in 15 minutes, and optionally email that link to the user's own inbox.",
		Params: []ParamSpec{
			{Name: "deliverableId", Type: "string", Required: true, MinLen: 1, MaxLen: 64,
				Description: "The deliverable's ID."},
			{Name: "method", Type: "string", Enum: []string{"link", "email"},
				Description: "\"link\" (default) returns the download URL; \"email\" also sends it to the user's own email address."},
		},
		Handler: handleDeliverableDeliver,
	}
}

func handleDeliverableCreate(ctx context.Context, deps *Deps, inv Invocation, args map[string]any) (map[string]any, *ToolError) {
	if deps.Deliverables == nil {
		return nil, toolErrf(CodeNotConfigured, "the deliverables store is not configured")
	}

	format := args["format"].(string)
	spec := deliverableFormats[format] // enum-validated upstream
	name := args["name"].(string)
	if !strings.HasSuffix(strings.ToLower(name), spec.ext) {
		name += spec.ext
	}

	d, err := deps.Deliverables.Create(ctx, inv.UserID, name, spec.contentType, []byte(args["content"].(string)))
	if err != nil {
		return nil, deliverableToolError(deps, "deliverable_create", err)
	}
	return map[string]any{
		"status":        "created",
		"deliverableId": d.DeliverableID,
		"name":          d.Name,
		"sizeBytes":     d.SizeBytes,
		"contentType":   d.ContentType,
	}, nil
}

func handleDeliverableZip(ctx context.Context, deps *Deps, inv Invocation, args map[string]any) (map[string]any, *ToolError) {
	if deps.Deliverables == nil {
		return nil, toolErrf(CodeNotConfigured, "the deliverables store is not configured")
	}

	ids, _ := args["deliverableIds"].([]string)
	if len(ids) == 0 {
		return nil, toolErrf(CodeInvalidArgs, "deliverableIds must contain at least one id")
	}
	if len(ids) > deliv.MaxZipSources {
		return nil, toolErrf(CodeInvalidArgs, "deliverableIds must contain at most %d ids", deliv.MaxZipSources)
	}
	zipName, _ := args["name"].(string)

	d, err := deps.Deliverables.Zip(ctx, inv.UserID, ids, zipName)
	if err != nil {
		return nil, deliverableToolError(deps, "deliverable_zip", err)
	}
	return map[string]any{
		"status":        d.Status, // "pending" — the zipper flips it to ready
		"deliverableId": d.DeliverableID,
		"name":          d.Name,
		"sourceCount":   len(d.Sources),
		"note":          "the archive is being built and will be ready shortly",
	}, nil
}

func handleDeliverableDeliver(ctx context.Context, deps *Deps, inv Invocation, args map[string]any) (map[string]any, *ToolError) {
	if deps.Deliverables == nil {
		return nil, toolErrf(CodeNotConfigured, "the deliverables store is not configured")
	}

	method, _ := args["method"].(string)
	emailTo := ""
	if method == "email" {
		// Email delivery goes to the calling user's OWN inbox only — an
		// external recipient must go through send_email's explicit
		// confirm-before-send policy instead, so this tool never needs a
		// confirmation flow of its own.
		u, err := deps.Store.GetUser(ctx, inv.UserID)
		if err != nil {
			deps.Log.Error("tools: deliverable_deliver user lookup failed", "error", err.Error())
			return nil, toolErrf(CodeUpstreamError, "failed to resolve your email address")
		}
		if u != nil && u.Email != "" {
			emailTo = u.Email
		} else if deps.OwnerEmail != "" {
			emailTo = deps.OwnerEmail
		} else {
			return nil, toolErrf(CodeNotConfigured, "no email address is available for delivery")
		}
	}

	res, err := deps.Deliverables.Deliver(ctx, inv.UserID, args["deliverableId"].(string), emailTo)
	if err != nil {
		return nil, deliverableToolError(deps, "deliverable_deliver", err)
	}

	out := map[string]any{
		"status":        "delivered",
		"deliverableId": res.Deliverable.DeliverableID,
		"name":          res.Deliverable.Name,
		"url":           res.URL,
		"expiresAt":     res.ExpiresAt.Format(time.RFC3339),
		"note":          "the download link expires in 15 minutes",
	}
	if res.EmailedTo != "" {
		out["emailedTo"] = res.EmailedTo
	}
	return out, nil
}

// deliverableToolError maps internal/deliv sentinel errors onto
// client-safe ToolError codes, logging the raw error server-side.
func deliverableToolError(deps *Deps, tool string, err error) *ToolError {
	switch {
	case errors.Is(err, deliv.ErrNotFound):
		return toolErrf(CodeNotFound, "no such deliverable (or it belongs to another user)")
	case errors.Is(err, deliv.ErrNotReady):
		return toolErrf(CodeInvalidArgs, "that deliverable is not ready yet (still building or failed)")
	case errors.Is(err, deliv.ErrBadInput):
		return toolErrf(CodeInvalidArgs, "%s", err.Error())
	}
	deps.Log.Error("tools: "+tool+" failed", "error", err.Error())
	return toolErrf(CodeUpstreamError, "the deliverables store request failed")
}
