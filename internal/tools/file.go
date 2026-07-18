package tools

// file_list / file_read / file_create — the assistant's document surface
// over the M9 deliverables corpus (same store as deliverable_create/zip/
// deliver: per-user prefix in the deliverables S3 bucket, DELIV# index in
// DynamoDB, Query-only).
//
// Owner rule (locked): the model may LIST and READ the user's documents
// and CREATE new ones — it must NEVER overwrite or delete. There is
// deliberately no file_delete/file_update/file_overwrite tool, file_create
// fails atomically on an existing name (a conditional DynamoDB name-claim
// write inside deliv.Create — not check-then-put), and every name argument
// is schema-gated to a safe slug (SafeName) so traversal ('..', '/',
// control characters) never reaches a handler.

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/JeremyProffittOrg/live-ninja/internal/deliv"
)

const (
	// maxFileListLimit caps one file_list page (single-partition Query,
	// never a Scan).
	maxFileListLimit     = 100
	defaultFileListLimit = 50

	// maxFileCursorLen bounds the opaque pagination cursor argument.
	maxFileCursorLen = 512

	// defaultFileContentType is applied when file_create omits contentType.
	defaultFileContentType = "text/markdown"
)

// fileCreateTypes is the closed contentType set for file_create: every
// member is text-like, so anything file_create writes is always readable
// back through file_read. Maps the advertised MIME type to the stored
// content type (with charset) and the extension appended when the name
// carries none.
var fileCreateTypes = map[string]struct {
	stored string
	ext    string
}{
	"text/markdown":    {"text/markdown; charset=utf-8", ".md"},
	"text/plain":       {"text/plain; charset=utf-8", ".txt"},
	"text/html":        {"text/html; charset=utf-8", ".html"},
	"text/csv":         {"text/csv; charset=utf-8", ".csv"},
	"application/json": {"application/json", ".json"},
}

// fileCreateTypeEnum is fileCreateTypes' keys in advertised order.
var fileCreateTypeEnum = []string{
	"text/markdown", "text/plain", "text/html", "text/csv", "application/json",
}

func fileListDefinition() *Definition {
	return &Definition{
		Name: "file_list",
		Description: "List the user's stored documents newest first: name, size in bytes, content type, " +
			"and creation time, plus the fileId used by file_read and deliverable_deliver. Paginated — " +
			"when the result carries a nextCursor, pass it as cursor to fetch the next page.",
		Params: []ParamSpec{
			{Name: "limit", Type: "integer", Min: floatPtr(1), Max: floatPtr(maxFileListLimit),
				Description: "Maximum files to return in one page (default 50, max 100)."},
			{Name: "cursor", Type: "string", MaxLen: maxFileCursorLen,
				Description: "Opaque pagination cursor from a previous file_list result; omit for the first page."},
		},
		Handler: handleFileList,
	}
}

func fileReadDefinition() *Definition {
	return &Definition{
		Name: "file_read",
		Description: "Read the text content of one of the user's stored documents, by fileId (from file_list " +
			"or a create result) or by exact filename. Only text-like files can be read (text, markdown, " +
			"CSV, HTML, JSON); for binary files (e.g. zip archives) this fails — offer the user a download " +
			"link via deliverable_deliver instead. Content longer than 64 KB is returned truncated.",
		Params: []ParamSpec{
			{Name: "fileId", Type: "string", MaxLen: 64,
				Description: "The document's id. Provide fileId or name (fileId wins if both are given)."},
			{Name: "name", Type: "string", MaxLen: 100, SafeName: true,
				Description: "The document's exact filename, e.g. 'trip-plan.md'."},
		},
		Handler: handleFileRead,
	}
}

func fileCreateDefinition() *Definition {
	return &Definition{
		Name: "file_create",
		Description: "Create a NEW document in the user's file store. Fails with already_exists if a file " +
			"with that name already exists — documents are never overwritten or replaced; to revise one, " +
			"create a new file under a different name.",
		SideEffecting: true,
		Params: []ParamSpec{
			{Name: "name", Type: "string", Required: true, MinLen: 1, MaxLen: 100, SafeName: true,
				Description: "Filename, e.g. 'meeting-notes.md'. Letters, digits, dot, dash, and underscore " +
					"only. If no extension is given, one matching the content type is appended."},
			{Name: "content", Type: "string", Required: true, MinLen: 1, MaxLen: 100_000,
				Description: "The full document content."},
			{Name: "contentType", Type: "string", Enum: fileCreateTypeEnum,
				Description: "MIME type of the content (default text/markdown)."},
		},
		Handler: handleFileCreate,
	}
}

func handleFileList(ctx context.Context, deps *Deps, inv Invocation, args map[string]any) (map[string]any, *ToolError) {
	if deps.Deliverables == nil {
		return nil, toolErrf(CodeNotConfigured, "the file store is not configured")
	}

	limit := defaultFileListLimit
	if v, ok := args["limit"].(int); ok {
		limit = v // 1..100 enforced by the schema gate
	}
	cursor, _ := args["cursor"].(string)

	items, next, err := deps.Deliverables.List(ctx, inv.UserID, int32(limit), cursor)
	if err != nil {
		deps.Log.Error("tools: file_list failed", "error", err.Error())
		return nil, toolErrf(CodeUpstreamError, "listing the user's files failed")
	}

	files := make([]map[string]any, 0, len(items))
	for _, d := range items {
		files = append(files, map[string]any{
			"fileId":      d.DeliverableID,
			"name":        d.Name,
			"sizeBytes":   d.SizeBytes,
			"createdAt":   d.CreatedAt,
			"contentType": d.ContentType,
			"status":      d.Status,
			"kind":        d.Kind,
		})
	}
	out := map[string]any{"files": files, "count": len(files)}
	if next != "" {
		out["nextCursor"] = next
	}
	return out, nil
}

func handleFileRead(ctx context.Context, deps *Deps, inv Invocation, args map[string]any) (map[string]any, *ToolError) {
	if deps.Deliverables == nil {
		return nil, toolErrf(CodeNotConfigured, "the file store is not configured")
	}

	fileID, _ := args["fileId"].(string)
	name, _ := args["name"].(string)
	if fileID == "" && name == "" {
		return nil, toolErrf(CodeInvalidArgs, "provide fileId or name")
	}
	if fileID == "" {
		d, err := deps.Deliverables.FindByName(ctx, inv.UserID, name)
		if err != nil {
			return nil, deliverableToolError(deps, "file_read", err)
		}
		if d == nil {
			return nil, toolErrf(CodeNotFound, "no file named %q — use file_list to see the user's files", name)
		}
		fileID = d.DeliverableID
	}

	d, content, truncated, err := deps.Deliverables.ReadContent(ctx, inv.UserID, fileID)
	if err != nil {
		if errors.Is(err, deliv.ErrNotText) {
			ct := "unknown"
			if d != nil {
				ct = d.ContentType
			}
			return nil, toolErrf(CodeInvalidArgs,
				"this file is binary (%s) and cannot be read as text — offer the user a download "+
					"link with deliverable_deliver instead", ct)
		}
		return nil, deliverableToolError(deps, "file_read", err)
	}

	out := map[string]any{
		"fileId":      d.DeliverableID,
		"name":        d.Name,
		"contentType": d.ContentType,
		"sizeBytes":   d.SizeBytes,
		"content":     string(content),
		"truncated":   truncated,
	}
	if truncated {
		out["note"] = "content truncated at 64 KB — the full file is larger; offer the user a " +
			"download link with deliverable_deliver for the complete document"
	}
	return out, nil
}

func handleFileCreate(ctx context.Context, deps *Deps, inv Invocation, args map[string]any) (map[string]any, *ToolError) {
	if deps.Deliverables == nil {
		return nil, toolErrf(CodeNotConfigured, "the file store is not configured")
	}

	ct, _ := args["contentType"].(string)
	if ct == "" {
		ct = defaultFileContentType
	}
	spec := fileCreateTypes[ct] // enum-validated upstream

	name := args["name"].(string) // SafeName-validated upstream
	if filepath.Ext(name) == "" {
		name += spec.ext
	}

	d, err := deps.Deliverables.Create(ctx, inv.UserID, name, spec.stored, []byte(args["content"].(string)))
	if err != nil {
		return nil, deliverableToolError(deps, "file_create", err)
	}
	return map[string]any{
		"status":      "created",
		"fileId":      d.DeliverableID,
		"name":        d.Name,
		"sizeBytes":   d.SizeBytes,
		"contentType": d.ContentType,
	}, nil
}
