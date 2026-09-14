// Package agentmemory is the Live Ninja seam onto Amazon Bedrock AgentCore
// Memory (plan.md workstream agentcore-memory, promoted 2026-09-14).
//
// One memory resource (template.yaml AgentCoreMemory) holds every account.
// Each user is an actor; each voice session is a session. The transcript
// sink writes one event per exchange (a user turn and the assistant turn that
// answered it — locked decision 3); AWS's built-in Semantic and
// UserPreference strategies extract long-term records asynchronously into
// the actor's namespaces /users/<actor>/facts/ and /users/<actor>/preferences/.
// The broker preloads the top records at mint, the memory tools search them,
// and account-purge deletes everything the actor owns.
//
// Everything here is best-effort from the caller's point of view: a nil
// *Service is a valid "not configured" value, every method on it is a no-op,
// and Admits() is the single rollout gate (AGENTCORE_MEMORY_MODE, locked
// decision 6). Nothing in this package can fail a transcript flush or a mint.
package agentmemory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockagentcore"
	"github.com/aws/aws-sdk-go-v2/service/bedrockagentcore/types"

	"github.com/JeremyProffittOrg/live-ninja/internal/observ"
)

// MetricsNamespace carries the per-call EMF counters. No user dimension on
// purpose: a custom metric is billed per dimension combination.
const MetricsNamespace = "LiveNinja/Memory"

// Mode is the rollout gate (stack parameter AgentCoreMemoryMode).
type Mode string

const (
	// ModeOff writes nothing and reads nothing — the rollback position.
	ModeOff Mode = "off"
	// ModeOwner admits only callers whose verified role is "owner".
	ModeOwner Mode = "owner"
	// ModeAll admits every account.
	ModeAll Mode = "all"
)

// Config is what a function needs to reach the memory.
type Config struct {
	MemoryID string
	Mode     Mode
}

// ConfigFromEnv reads AGENTCORE_MEMORY_ID and AGENTCORE_MEMORY_MODE. Any
// unknown mode, or a missing id, is ModeOff — misconfiguration fails closed.
func ConfigFromEnv() Config {
	cfg := Config{
		MemoryID: strings.TrimSpace(os.Getenv("AGENTCORE_MEMORY_ID")),
		Mode:     Mode(strings.ToLower(strings.TrimSpace(os.Getenv("AGENTCORE_MEMORY_MODE")))),
	}
	switch cfg.Mode {
	case ModeOwner, ModeAll:
	default:
		cfg.Mode = ModeOff
	}
	if cfg.MemoryID == "" {
		cfg.Mode = ModeOff
	}
	return cfg
}

// Enabled reports whether the memory is configured at all.
func (c Config) Enabled() bool { return c.MemoryID != "" && c.Mode != ModeOff }

// Admits reports whether a caller with the given verified role may read or
// write memory under this configuration.
func (c Config) Admits(role string) bool {
	if !c.Enabled() {
		return false
	}
	switch c.Mode {
	case ModeAll:
		return true
	case ModeOwner:
		return role == "owner"
	}
	return false
}

// Client is the SDK subset this package uses; tests inject a fake.
type Client interface {
	CreateEvent(ctx context.Context, in *bedrockagentcore.CreateEventInput, optFns ...func(*bedrockagentcore.Options)) (*bedrockagentcore.CreateEventOutput, error)
	RetrieveMemoryRecords(ctx context.Context, in *bedrockagentcore.RetrieveMemoryRecordsInput, optFns ...func(*bedrockagentcore.Options)) (*bedrockagentcore.RetrieveMemoryRecordsOutput, error)
	ListMemoryRecords(ctx context.Context, in *bedrockagentcore.ListMemoryRecordsInput, optFns ...func(*bedrockagentcore.Options)) (*bedrockagentcore.ListMemoryRecordsOutput, error)
	GetMemoryRecord(ctx context.Context, in *bedrockagentcore.GetMemoryRecordInput, optFns ...func(*bedrockagentcore.Options)) (*bedrockagentcore.GetMemoryRecordOutput, error)
	DeleteMemoryRecord(ctx context.Context, in *bedrockagentcore.DeleteMemoryRecordInput, optFns ...func(*bedrockagentcore.Options)) (*bedrockagentcore.DeleteMemoryRecordOutput, error)
	BatchDeleteMemoryRecords(ctx context.Context, in *bedrockagentcore.BatchDeleteMemoryRecordsInput, optFns ...func(*bedrockagentcore.Options)) (*bedrockagentcore.BatchDeleteMemoryRecordsOutput, error)
	ListSessions(ctx context.Context, in *bedrockagentcore.ListSessionsInput, optFns ...func(*bedrockagentcore.Options)) (*bedrockagentcore.ListSessionsOutput, error)
	ListEvents(ctx context.Context, in *bedrockagentcore.ListEventsInput, optFns ...func(*bedrockagentcore.Options)) (*bedrockagentcore.ListEventsOutput, error)
	DeleteEvent(ctx context.Context, in *bedrockagentcore.DeleteEventInput, optFns ...func(*bedrockagentcore.Options)) (*bedrockagentcore.DeleteEventOutput, error)
}

// Counter receives the per-user event/retrieval counts (the web function
// wires store.AddDayMemoryUsage so usage-rollup can sum them; the broker
// leaves it nil).
type Counter func(ctx context.Context, userID string, events, retrievals int64)

// Service is the memory seam. A nil *Service is "not configured".
type Service struct {
	client  Client
	cfg     Config
	log     *slog.Logger
	now     func() time.Time
	counter Counter
}

// New builds a Service over any Client.
func New(client Client, cfg Config, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{client: client, cfg: cfg, log: log, now: func() time.Time { return time.Now().UTC() }}
}

// NewFromAWSConfig builds the production Service, or nil when the
// configuration is off — callers treat nil as "memory not configured".
func NewFromAWSConfig(awsCfg aws.Config, cfg Config, log *slog.Logger) *Service {
	if !cfg.Enabled() {
		return nil
	}
	return New(bedrockagentcore.NewFromConfig(awsCfg), cfg, log)
}

// SetCounter installs the usage hook.
func (s *Service) SetCounter(c Counter) {
	if s != nil {
		s.counter = c
	}
}

// Config returns the effective configuration (zero value when nil).
func (s *Service) Config() Config {
	if s == nil {
		return Config{}
	}
	return s.cfg
}

// Admits is the rollout gate for one caller.
func (s *Service) Admits(role string) bool { return s != nil && s.cfg.Admits(role) }

// ---- identifiers -----------------------------------------------------------

// ActorID maps a Live Ninja user id onto an AgentCore actor id. The
// namespace template only admits [a-zA-Z0-9-_/], so anything else (an LWA
// id's dots, for example) becomes "_". Deterministic, so purge and preload
// address the same actor the writer did.
func ActorID(userID string) string { return sanitizeID(userID) }

// SessionID maps a session id the same way; an empty session id becomes a
// per-day bucket so explicit "remember this" writes still have a session.
func SessionID(sessionID string, now time.Time) string {
	if strings.TrimSpace(sessionID) == "" {
		return "explicit-" + now.UTC().Format("20060102")
	}
	return sanitizeID(sessionID)
}

// Namespace is the actor's namespace path; both strategies live under it
// (/users/<actor>/facts/ and /users/<actor>/preferences/).
//
// Verified 2026-09-14 against the deployed memory (scripts/agentmemory-smoke
// -actor): the API's `namespace` parameter matches a record's namespace
// EXACTLY despite the reference calling it a prefix — `/users/<actor>/`
// returned nothing while `/users/<actor>/facts/` returned the records.
// `namespacePath` is the hierarchical form and returned every record under
// the actor for both RetrieveMemoryRecords and ListMemoryRecords, so that
// is what every read here uses.
func Namespace(userID string) string { return "/users/" + ActorID(userID) + "/" }

func sanitizeID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return "unknown"
	}
	var b strings.Builder
	b.Grow(len(id))
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if len(out) > 200 {
		out = out[:200]
	}
	return out
}

// ---- events ------------------------------------------------------------------

// Turn is one transcript turn as the sink receives it.
type Turn struct {
	Seq  int
	Role string
	Text string
}

// maxMessageRunes bounds one message inside an event (the API cap is 100 KB;
// a voice turn is never near it, but a pasted document could be).
const maxMessageRunes = 20000

// PairExchanges groups turns into the events the sink writes: a user turn
// immediately followed by an assistant turn is one exchange; any other
// user or assistant turn stands alone. Tool and system turns, and empty
// turns, are dropped — they are audit rows, not conversation.
func PairExchanges(turns []Turn) [][]Turn {
	sorted := make([]Turn, 0, len(turns))
	for _, t := range turns {
		if roleOf(t.Role) == "" || strings.TrimSpace(t.Text) == "" {
			continue
		}
		sorted = append(sorted, t)
	}
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Seq < sorted[j].Seq })

	var out [][]Turn
	for i := 0; i < len(sorted); {
		t := sorted[i]
		if roleOf(t.Role) == types.RoleUser && i+1 < len(sorted) && roleOf(sorted[i+1].Role) == types.RoleAssistant {
			out = append(out, []Turn{t, sorted[i+1]})
			i += 2
			continue
		}
		out = append(out, []Turn{t})
		i++
	}
	return out
}

func roleOf(role string) types.Role {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "user":
		return types.RoleUser
	case "assistant":
		return types.RoleAssistant
	}
	return ""
}

// RecordExchanges writes one event per exchange for the session. It keeps
// going past a failed event and returns how many landed plus the joined
// errors; the client token is derived from (session, first seq) so a
// client-side retry of the same transcript batch cannot bill twice.
func (s *Service) RecordExchanges(ctx context.Context, userID, sessionID string, turns []Turn) (int, error) {
	if s == nil {
		return 0, nil
	}
	now := s.now()
	actor, session := ActorID(userID), SessionID(sessionID, now)
	written := 0
	var errs []error
	for _, ex := range PairExchanges(turns) {
		payload := make([]types.PayloadType, 0, len(ex))
		for _, t := range ex {
			payload = append(payload, &types.PayloadTypeMemberConversational{Value: types.Conversational{
				Content: &types.ContentMemberText{Value: clip(t.Text, maxMessageRunes)},
				Role:    roleOf(t.Role),
			}})
		}
		_, err := s.client.CreateEvent(ctx, &bedrockagentcore.CreateEventInput{
			MemoryId:       aws.String(s.cfg.MemoryID),
			ActorId:        aws.String(actor),
			SessionId:      aws.String(session),
			EventTimestamp: aws.Time(now),
			ClientToken:    aws.String(clientToken(sessionID, ex[0].Seq)),
			Payload:        payload,
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("event seq %d: %w", ex[0].Seq, err))
			continue
		}
		written++
	}
	if written > 0 {
		observ.EmitMetric(MetricsNamespace, "AgentCoreEvents", float64(written), "Count", nil)
		if s.counter != nil {
			s.counter(ctx, userID, int64(written), 0)
		}
	}
	if len(errs) > 0 {
		observ.EmitMetric(MetricsNamespace, "AgentCoreErrors", float64(len(errs)), "Count", map[string]string{"Op": "CreateEvent"})
	}
	return written, errors.Join(errs...)
}

// RememberFact records an explicit "remember this" as its own exchange so
// the extraction strategies see it as a stated fact rather than passing
// chatter. Used by memory_write and plan_upsert alongside their DynamoDB
// write (locked decision 7: dual running).
func (s *Service) RememberFact(ctx context.Context, userID, sessionID, text string) error {
	if s == nil {
		return nil
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	_, err := s.RecordExchanges(ctx, userID, sessionID, []Turn{
		{Seq: int(s.now().UnixNano() % 1_000_000), Role: "user", Text: "Please remember this about me: " + text},
		{Seq: int(s.now().UnixNano()%1_000_000) + 1, Role: "assistant", Text: "Understood, I will remember that."},
	})
	return err
}

func clientToken(sessionID string, seq int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s#%06d", sessionID, seq)))
	return hex.EncodeToString(sum[:])
}

func clip(s string, maxRunes int) string {
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	r := []rune(s)
	return string(r[:maxRunes])
}

// ---- records -------------------------------------------------------------------

// Record is one long-term memory record as the rest of the app sees it.
type Record struct {
	ID        string
	Namespace string
	Text      string
	Score     float64
	CreatedAt time.Time
}

// PreloadQuery is the semantic query the broker runs at mint. There is no
// user utterance yet, so it asks for the durable core of what the memory
// knows; the model calls memory_search for anything narrower.
const PreloadQuery = "Who the user is: their name, the people, places, projects and plans that matter to them, and their standing preferences."

// Preload returns the records the broker renders into the REMEMBERED block.
func (s *Service) Preload(ctx context.Context, userID string, topK int) ([]Record, error) {
	return s.Retrieve(ctx, userID, PreloadQuery, topK)
}

// Retrieve is semantic search over the actor's namespaces.
func (s *Service) Retrieve(ctx context.Context, userID, query string, topK int) ([]Record, error) {
	if s == nil {
		return nil, nil
	}
	if topK <= 0 {
		topK = 10
	}
	if topK > 100 {
		topK = 100
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, errors.New("agentmemory: empty query")
	}
	out, err := s.client.RetrieveMemoryRecords(ctx, &bedrockagentcore.RetrieveMemoryRecordsInput{
		MemoryId:      aws.String(s.cfg.MemoryID),
		NamespacePath: aws.String(Namespace(userID)),
		SearchCriteria: &types.SearchCriteria{
			SearchQuery: aws.String(clip(query, 10000)),
			TopK:        aws.Int32(int32(topK)),
		},
		MaxResults: aws.Int32(int32(topK)),
	})
	if err != nil {
		observ.EmitMetric(MetricsNamespace, "AgentCoreErrors", 1, "Count", map[string]string{"Op": "RetrieveMemoryRecords"})
		return nil, err
	}
	observ.EmitMetric(MetricsNamespace, "AgentCoreRetrievals", 1, "Count", nil)
	if s.counter != nil {
		s.counter(ctx, userID, 0, 1)
	}
	recs := make([]Record, 0, len(out.MemoryRecordSummaries))
	for _, sum := range out.MemoryRecordSummaries {
		if r, ok := recordFromSummary(sum); ok {
			recs = append(recs, r)
		}
	}
	return recs, nil
}

// ListRecords pages the actor's records (newest the API returns first) up to
// limit; it is the Memory page's "Learned from conversations" list and the
// account export.
func (s *Service) ListRecords(ctx context.Context, userID string, limit int) ([]Record, error) {
	if s == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 100
	}
	var (
		recs  []Record
		token *string
	)
	for {
		page := int32(100)
		if remaining := limit - len(recs); remaining < 100 {
			page = int32(remaining)
		}
		out, err := s.client.ListMemoryRecords(ctx, &bedrockagentcore.ListMemoryRecordsInput{
			MemoryId:      aws.String(s.cfg.MemoryID),
			NamespacePath: aws.String(Namespace(userID)),
			MaxResults:    aws.Int32(page),
			NextToken:     token,
		})
		if err != nil {
			observ.EmitMetric(MetricsNamespace, "AgentCoreErrors", 1, "Count", map[string]string{"Op": "ListMemoryRecords"})
			return recs, err
		}
		for _, rec := range out.MemoryRecordSummaries {
			if r, ok := recordFromSummary(rec); ok {
				recs = append(recs, r)
			}
		}
		if out.NextToken == nil || len(recs) >= limit {
			return recs, nil
		}
		token = out.NextToken
	}
}

// DeleteRecord removes one record after proving it lives under the caller's
// namespace — the id comes from the client, so ownership is not assumed.
// Returns false when the record does not exist or belongs to another actor.
func (s *Service) DeleteRecord(ctx context.Context, userID, recordID string) (bool, error) {
	if s == nil {
		return false, nil
	}
	recordID = strings.TrimSpace(recordID)
	if recordID == "" {
		return false, nil
	}
	got, err := s.client.GetMemoryRecord(ctx, &bedrockagentcore.GetMemoryRecordInput{
		MemoryId:       aws.String(s.cfg.MemoryID),
		MemoryRecordId: aws.String(recordID),
	})
	if err != nil {
		var nf *types.ResourceNotFoundException
		if errors.As(err, &nf) {
			return false, nil
		}
		return false, err
	}
	if got.MemoryRecord == nil || !ownedBy(got.MemoryRecord.Namespaces, userID) {
		return false, nil
	}
	_, err = s.client.DeleteMemoryRecord(ctx, &bedrockagentcore.DeleteMemoryRecordInput{
		MemoryId:       aws.String(s.cfg.MemoryID),
		MemoryRecordId: aws.String(recordID),
		Namespace:      firstNamespace(got.MemoryRecord.Namespaces),
	})
	if err != nil {
		observ.EmitMetric(MetricsNamespace, "AgentCoreErrors", 1, "Count", map[string]string{"Op": "DeleteMemoryRecord"})
		return false, err
	}
	return true, nil
}

// ForgetMatching deletes the actor's records whose text names the entity
// being forgotten (case-insensitive substring over the top retrieval hits).
// Precision over recall: a record that merely resembles the name is kept.
func (s *Service) ForgetMatching(ctx context.Context, userID, name string) (int, error) {
	if s == nil {
		return 0, nil
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return 0, nil
	}
	hits, err := s.Retrieve(ctx, userID, name, 5)
	if err != nil {
		return 0, err
	}
	needle := strings.ToLower(name)
	var victims []types.MemoryRecordDeleteInput
	for _, h := range hits {
		if strings.Contains(strings.ToLower(h.Text), needle) {
			victims = append(victims, types.MemoryRecordDeleteInput{
				MemoryRecordId: aws.String(h.ID),
				Namespace:      aws.String(h.Namespace),
			})
		}
	}
	if len(victims) == 0 {
		return 0, nil
	}
	if _, err := s.client.BatchDeleteMemoryRecords(ctx, &bedrockagentcore.BatchDeleteMemoryRecordsInput{
		MemoryId: aws.String(s.cfg.MemoryID),
		Records:  victims,
	}); err != nil {
		observ.EmitMetric(MetricsNamespace, "AgentCoreErrors", 1, "Count", map[string]string{"Op": "BatchDeleteMemoryRecords"})
		return 0, err
	}
	return len(victims), nil
}

// purgeBatch bounds one BatchDeleteMemoryRecords call.
const purgeBatch = 25

// PurgeActor deletes every event in every session of the actor and every
// long-term record under the actor's namespace. Returns the counts; any
// error is returned so the caller (account-purge) fails and retries — a
// silent partial purge would leave user data behind.
func (s *Service) PurgeActor(ctx context.Context, userID string) (events, records int, err error) {
	if s == nil {
		return 0, 0, nil
	}
	actor := ActorID(userID)

	var sessToken *string
	for {
		sessions, lerr := s.client.ListSessions(ctx, &bedrockagentcore.ListSessionsInput{
			MemoryId:   aws.String(s.cfg.MemoryID),
			ActorId:    aws.String(actor),
			MaxResults: aws.Int32(100),
			NextToken:  sessToken,
		})
		if lerr != nil {
			return events, records, fmt.Errorf("agentmemory: list sessions: %w", lerr)
		}
		for _, sess := range sessions.SessionSummaries {
			n, derr := s.purgeSession(ctx, actor, aws.ToString(sess.SessionId))
			events += n
			if derr != nil {
				return events, records, derr
			}
		}
		if sessions.NextToken == nil {
			break
		}
		sessToken = sessions.NextToken
	}

	for {
		out, lerr := s.client.ListMemoryRecords(ctx, &bedrockagentcore.ListMemoryRecordsInput{
			MemoryId:      aws.String(s.cfg.MemoryID),
			NamespacePath: aws.String(Namespace(userID)),
			MaxResults:    aws.Int32(purgeBatch),
		})
		if lerr != nil {
			return events, records, fmt.Errorf("agentmemory: list records: %w", lerr)
		}
		if len(out.MemoryRecordSummaries) == 0 {
			break
		}
		batch := make([]types.MemoryRecordDeleteInput, 0, len(out.MemoryRecordSummaries))
		for _, rec := range out.MemoryRecordSummaries {
			batch = append(batch, types.MemoryRecordDeleteInput{
				MemoryRecordId: rec.MemoryRecordId,
				Namespace:      firstNamespace(rec.Namespaces),
			})
		}
		if _, derr := s.client.BatchDeleteMemoryRecords(ctx, &bedrockagentcore.BatchDeleteMemoryRecordsInput{
			MemoryId: aws.String(s.cfg.MemoryID),
			Records:  batch,
		}); derr != nil {
			return events, records, fmt.Errorf("agentmemory: delete records: %w", derr)
		}
		records += len(batch)
		if out.NextToken == nil && len(out.MemoryRecordSummaries) < purgeBatch {
			break
		}
	}
	return events, records, nil
}

func (s *Service) purgeSession(ctx context.Context, actor, sessionID string) (int, error) {
	deleted := 0
	var token *string
	for {
		out, err := s.client.ListEvents(ctx, &bedrockagentcore.ListEventsInput{
			MemoryId:        aws.String(s.cfg.MemoryID),
			ActorId:         aws.String(actor),
			SessionId:       aws.String(sessionID),
			IncludePayloads: aws.Bool(false),
			MaxResults:      aws.Int32(100),
			NextToken:       token,
		})
		if err != nil {
			return deleted, fmt.Errorf("agentmemory: list events: %w", err)
		}
		for _, ev := range out.Events {
			if _, err := s.client.DeleteEvent(ctx, &bedrockagentcore.DeleteEventInput{
				MemoryId:  aws.String(s.cfg.MemoryID),
				ActorId:   aws.String(actor),
				SessionId: aws.String(sessionID),
				EventId:   ev.EventId,
			}); err != nil {
				return deleted, fmt.Errorf("agentmemory: delete event: %w", err)
			}
			deleted++
		}
		if out.NextToken == nil {
			return deleted, nil
		}
		token = out.NextToken
	}
}

func recordFromSummary(sum types.MemoryRecordSummary) (Record, bool) {
	text, ok := sum.Content.(*types.MemoryContentMemberText)
	if !ok || strings.TrimSpace(text.Value) == "" {
		return Record{}, false
	}
	r := Record{
		ID:        aws.ToString(sum.MemoryRecordId),
		Namespace: aws.ToString(firstNamespace(sum.Namespaces)),
		Text:      DisplayText(text.Value),
		Score:     aws.ToFloat64(sum.Score),
	}
	if sum.CreatedAt != nil {
		r.CreatedAt = sum.CreatedAt.UTC()
	}
	return r, true
}

// DisplayText renders a record's content for a prompt or a page. The
// UserPreference strategy stores JSON — observed 2026-09-14:
// {"context":"...","preference":"Prefers temperatures displayed in Celsius",
// "categories":[...]} — so its `preference` sentence is what a reader (or the
// model) should see; a Semantic record is already plain text.
func DisplayText(raw string) string {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, "{") {
		return raw
	}
	var pref struct {
		Preference string `json:"preference"`
		Context    string `json:"context"`
	}
	if err := json.Unmarshal([]byte(raw), &pref); err != nil {
		return raw
	}
	if p := strings.TrimSpace(pref.Preference); p != "" {
		return p
	}
	if c := strings.TrimSpace(pref.Context); c != "" {
		return c
	}
	return raw
}

func firstNamespace(ns []string) *string {
	if len(ns) == 0 {
		return nil
	}
	return aws.String(ns[0])
}

func ownedBy(namespaces []string, userID string) bool {
	prefix := Namespace(userID)
	for _, ns := range namespaces {
		if strings.HasPrefix(ns, prefix) {
			return true
		}
	}
	return false
}
