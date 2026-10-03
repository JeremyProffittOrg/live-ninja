package jobs

import (
	"context"
	"fmt"
)

// ConversationReader is an adapter contract, not a claimed Ghost endpoint.
// Implementations must reauthorize provider ownership and preserve ALL retained
// pages. No implementation is installed until the upstream contract is verified.
type ConversationReader interface {
	ReadConversation(context.Context, string, string, string, int) (ConversationPage, error)
}
type ConversationMessage struct {
	ID        string `json:"id"`
	Role      string `json:"role"`
	Text      string `json:"text"`
	CreatedAt string `json:"createdAt"`
}
type ConversationPage struct {
	Messages          []ConversationMessage `json:"messages"`
	NextCursor        string                `json:"nextCursor,omitempty"`
	HasMore           bool                  `json:"hasMore"`
	RetentionBoundary string                `json:"retentionBoundary"`
}

// WithConversationReader returns a service copy; configure it once before serving.
func (s *Service) WithConversationReader(reader ConversationReader) *Service {
	copy := *s
	copy.conversations = reader
	return &copy
}
func (s *Service) ConversationAvailable() bool { return s.conversations != nil }
func (s *Service) Conversation(ctx context.Context, uid, id, cursor string, limit int) (ConversationPage, error) {
	empty := ConversationPage{Messages: []ConversationMessage{}}
	if uid == "" {
		return empty, ErrForbidden
	}
	if len(cursor) > 4096 {
		return empty, ErrValidation
	}
	if _, e := s.store.Get(ctx, uid, id); e != nil {
		return empty, e
	}
	if s.conversations == nil {
		return empty, ErrUnsupported
	}
	p, e := s.conversations.ReadConversation(ctx, uid, id, cursor, pageLimit(limit))
	if e != nil {
		return empty, e
	}
	if p.HasMore && (p.NextCursor == "" || p.NextCursor == cursor) {
		return empty, fmt.Errorf("%w: provider did not supply a progressing full-history cursor", ErrCorrupt)
	}
	if p.Messages == nil {
		p.Messages = []ConversationMessage{}
	}
	return p, nil
}
