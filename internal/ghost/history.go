package ghost

// Canonical source: ghost-cli 672c44030d50a738d514b1d9b9ad89c754d12bae,
// lambda/command/history.go, nodes.go and schedule.go. History requires the
// additional GET-only internal-invoke allowlist entries on the Ghost deployment.

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

var historySessionID = regexp.MustCompile(`^(?:[a-fA-F0-9]{8}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{12}|agent-[a-fA-F0-9]{16,32})$`)
var historyNodeID = regexp.MustCompile(`^[A-Za-z0-9:_-]{1,128}$`)

type HistorySession struct {
	SessionID string `json:"session_id"`
	Date      string `json:"date"`
}
type HistorySessionsPage struct {
	Version    int              `json:"version"`
	NodeID     string           `json:"node_id"`
	Sessions   []HistorySession `json:"sessions"`
	NextCursor string           `json:"next_cursor"`
	Coverage   string           `json:"coverage"`
}
type HistoryEvent struct {
	ID        string `json:"id"`
	Sequence  string `json:"sequence"`
	Timestamp string `json:"timestamp,omitempty"`
	Kind      string `json:"kind"`
	Text      string `json:"text"`
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Part      int    `json:"part"`
	More      bool   `json:"more"`
}

// Zero is meaningful for part and false for more. Missing/null fields must not
// silently become those values, or a malformed provider page could look like a
// complete fragment and permanently lose content after client deduplication.
func (event *HistoryEvent) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for _, name := range []string{"id", "sequence", "kind", "text", "part", "more"} {
		value, present := fields[name]
		if !present || strings.TrimSpace(string(value)) == "null" {
			return fmt.Errorf("ghost: missing required history fragment field %s", name)
		}
	}
	type plainEvent HistoryEvent
	var decoded plainEvent
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*event = HistoryEvent(decoded)
	return nil
}

type HistoryEventsPage struct {
	Version      int            `json:"version"`
	NodeID       string         `json:"node_id"`
	SessionID    string         `json:"session_id"`
	Events       []HistoryEvent `json:"events"`
	NextCursor   string         `json:"next_cursor,omitempty"`
	ResumeCursor string         `json:"resume_cursor"`
	Coverage     string         `json:"coverage"`
	Gaps         []string       `json:"gaps"`
}

func historyQuery(node, session, cursor string) (map[string]string, error) {
	if !historyNodeID.MatchString(node) || len(cursor) > 2048 || strings.ContainsAny(cursor, "\x00\r\n") {
		return nil, ErrInvalidRequest
	}
	q := map[string]string{"node_id": node}
	if session != "" {
		if !historySessionID.MatchString(session) {
			return nil, ErrInvalidRequest
		}
		q["session_id"] = session
	}
	if cursor != "" {
		q["cursor"] = cursor
	}
	return q, nil
}

func (c *Client) HistorySessions(ctx context.Context, node, cursor, correlation string) (HistorySessionsPage, error) {
	var page HistorySessionsPage
	q, e := historyQuery(node, "", cursor)
	if e != nil {
		return page, e
	}
	body, e := c.call(ctx, "GET", "/history/sessions", "", q, correlation)
	if e != nil {
		return page, e
	}
	if len(body) > 2<<20 || json.Unmarshal([]byte(body), &page) != nil || page.Version != 1 || page.NodeID != node || page.Coverage != "retained_only" || page.Sessions == nil || len(page.NextCursor) > 2048 || (page.NextCursor != "" && page.NextCursor == cursor) {
		return HistorySessionsPage{}, fmt.Errorf("%w: invalid history discovery response", ErrUpstream)
	}
	for _, session := range page.Sessions {
		if !historySessionID.MatchString(session.SessionID) {
			return HistorySessionsPage{}, ErrUpstream
		}
	}
	return page, nil
}

func (c *Client) HistoryEvents(ctx context.Context, node, session, cursor, correlation string) (HistoryEventsPage, error) {
	var page HistoryEventsPage
	if session == "" {
		return page, ErrInvalidRequest
	}
	q, e := historyQuery(node, session, cursor)
	if e != nil {
		return page, e
	}
	body, e := c.call(ctx, "GET", "/history/events", "", q, correlation)
	if e != nil {
		return page, e
	}
	if len(body) > 2<<20 || json.Unmarshal([]byte(body), &page) != nil || page.Version != 1 || page.NodeID != node || page.SessionID != session || page.Coverage != "retained_only" || page.Events == nil || page.Gaps == nil || page.ResumeCursor == "" || len(page.ResumeCursor) > 2048 || len(page.NextCursor) > 2048 || (page.NextCursor != "" && page.NextCursor == cursor) {
		return HistoryEventsPage{}, fmt.Errorf("%w: invalid retained history response", ErrUpstream)
	}
	// Preserve order, fragments, missing timestamps, gaps and full visible text.
	// Sequence is a provider source-position STRING, never parsed as an integer.
	bytes := 0
	seenIDs := make(map[string]bool, len(page.Events))
	for i, event := range page.Events {
		bytes += len(event.Text)
		if event.ID == "" || seenIDs[event.ID] || event.Sequence == "" || event.Part < 0 || len(event.Text) > 48<<10 || (i > 0 && event.Sequence <= page.Events[i-1].Sequence) {
			return HistoryEventsPage{}, ErrUpstream
		}
		seenIDs[event.ID] = true
		switch event.Kind {
		case "user", "assistant", "tool_call", "tool_result", "command", "event":
		default:
			return HistoryEventsPage{}, ErrUpstream
		}
	}
	if len(page.Events) > 256 || bytes > 192<<10 {
		return HistoryEventsPage{}, ErrUpstream
	}
	return page, nil
}

// ScheduledEvent has no authoritative provider-session identifier. It must not
// be joined to history by latest session, timestamps, prompt text or node alone.
type ScheduledEvent struct {
	EventID       string         `json:"event_id"`
	Node          string         `json:"node"`
	Repo          string         `json:"repo"`
	CLI           string         `json:"cli"`
	Model         string         `json:"model"`
	Effort        string         `json:"effort"`
	Prompt        string         `json:"prompt"`
	BackupNode    string         `json:"backup_node"`
	OutputFile    string         `json:"output_file"`
	Cron          string         `json:"cron"`
	RunAt         string         `json:"run_at"`
	Timezone      string         `json:"timezone"`
	Enabled       bool           `json:"enabled"`
	Archived      bool           `json:"archived"`
	Deploy        bool           `json:"deploy"`
	CreatedAt     string         `json:"created_at"`
	NextRun       string         `json:"next_run"`
	LastRunID     string         `json:"last_run_id"`
	LastRunStatus string         `json:"last_run_status"`
	LastRunTs     string         `json:"last_run_ts"`
	LastRunNode   string         `json:"last_run_node"`
	Runs          []ScheduledRun `json:"runs"`
}
type ScheduledRun struct {
	RunID     string `json:"run_id"`
	Status    string `json:"status"`
	Ts        string `json:"ts"`
	Node      string `json:"node"`
	Trigger   string `json:"trigger"`
	OutputKey string `json:"output_key"`
	Summary   string `json:"summary"`
}

// ScheduledWork filters the upstream fleet-wide list using a fresh authorized
// node inventory. Fail closed for an event referencing an unauthorized node,
// including backups/historical execution, rather than leaking their metadata.
func (c *Client) ScheduledWork(ctx context.Context, correlation string) ([]ScheduledEvent, error) {
	nodes, e := c.Nodes(ctx, correlation)
	if e != nil {
		return nil, e
	}
	allowed := map[string]bool{}
	for _, node := range nodes {
		if historyNodeID.MatchString(node.NodeID) {
			allowed[node.NodeID] = true
		}
	}
	body, e := c.call(ctx, "GET", "/schedule", "", nil, correlation)
	if e != nil {
		return nil, e
	}
	var page struct {
		Events []ScheduledEvent `json:"events"`
	}
	if json.Unmarshal([]byte(body), &page) != nil || page.Events == nil {
		return nil, ErrUpstream
	}
	visible := []ScheduledEvent{}
	for _, event := range page.Events {
		ok := allowed[event.Node]
		for _, node := range []string{event.BackupNode, event.LastRunNode} {
			if node != "" && !allowed[node] {
				ok = false
			}
		}
		for _, run := range event.Runs {
			if run.Node != "" && !allowed[run.Node] {
				ok = false
			}
		}
		if ok {
			if event.Runs == nil {
				event.Runs = []ScheduledRun{}
			}
			visible = append(visible, event)
		}
	}
	return visible, nil
}
