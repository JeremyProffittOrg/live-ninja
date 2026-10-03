// Package codeapproval records immutable, owner-reviewed coding intentions.
// It deliberately has no execution or SQS interface: the legacy dispatcher's
// token-first retry handling cannot safely recover ambiguous launch outcomes.
package codeapproval

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/JeremyProffittOrg/live-ninja/internal/codeupdate"
	"github.com/JeremyProffittOrg/live-ninja/internal/ghost"
)

var (
	ErrValidation  = errors.New("codeapproval: invalid request")
	ErrNotFound    = errors.New("codeapproval: not found")
	ErrConflict    = errors.New("codeapproval: intent changed or was already consumed")
	ErrExpired     = errors.New("codeapproval: review expired; prepare a new intent")
	ErrUnavailable = errors.New("codeapproval: fleet verification unavailable")
	ErrForbidden   = errors.New("codeapproval: an active owner is required")
	ErrCorrupt     = errors.New("codeapproval: stored intent needs recovery")
)

const (
	IntentTTL             = 15 * time.Minute
	ReceiptRetention      = 30 * 24 * time.Hour
	BlockedReason         = "legacy_dispatch_recovery_required"
	StatusPrepared        = "prepared"
	StatusApprovedBlocked = "approved_blocked"
)

var requestPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{8,128}$`)
var idPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

type Input struct {
	Repo         string `json:"repo"`
	Node         string `json:"node"`
	Instructions string `json:"instructions"`
	Agent        string `json:"agent,omitempty"`
	Model        string `json:"model,omitempty"`
	Effort       string `json:"effort,omitempty"`
	Preprocess   *bool  `json:"preprocess,omitempty"`
	Deploy       bool   `json:"deploy"`
}
type Action struct {
	Repo         string `json:"repo"`
	Node         string `json:"node"`
	Instructions string `json:"instructions"`
	Agent        string `json:"agent"`
	Model        string `json:"model,omitempty"`
	Effort       string `json:"effort,omitempty"`
	Preprocess   bool   `json:"preprocess"`
	Deploy       bool   `json:"deploy"`
}
type Verification struct {
	Source                   string `json:"source"`
	RepoVerified             bool   `json:"repoVerified"`
	NodeVerified             bool   `json:"nodeVerified"`
	LaunchPermissionVerified bool   `json:"launchPermissionVerified"`
	NodeStatus               string `json:"nodeStatus"`
	CheckedAt                string `json:"checkedAt"`
}
type Receipt struct {
	ID                    string `json:"id"`
	RequestID             string `json:"requestId"`
	IntentVersion         int64  `json:"intentVersion"`
	ApprovedAt            string `json:"approvedAt"`
	ApprovedBy            string `json:"approvedBy"`
	ActionHash            string `json:"actionHash"`
	State                 string `json:"state"`
	ExecutionState        string `json:"executionState"`
	Reason                string `json:"reason"`
	RequiresFreshApproval bool   `json:"requiresFreshApproval"`
	Message               string `json:"message"`
}
type Intent struct {
	Generation   string       `json:"generation"`
	ID           string       `json:"id"`
	UserID       string       `json:"userId"`
	Version      int64        `json:"version"`
	Status       string       `json:"status"`
	ActionHash   string       `json:"actionHash"`
	Action       Action       `json:"action"`
	CreatedAt    string       `json:"createdAt"`
	ExpiresAt    string       `json:"expiresAt"`
	RetainUntil  int64        `json:"retainUntil"`
	Verification Verification `json:"verification"`
	Receipt      *Receipt     `json:"receipt,omitempty"`
}
type Capabilities struct {
	Prepare       bool   `json:"prepare"`
	Approve       bool   `json:"approve"`
	Execute       bool   `json:"execute"`
	Reason        string `json:"reason"`
	CatalogSource string `json:"catalogSource"`
	FleetVerified bool   `json:"fleetVerified"`
	Message       string `json:"message"`
}
type Options struct {
	Repositories []ghost.Repo `json:"repositories"`
	Nodes        []ghost.Node `json:"nodes"`
	Capabilities Capabilities `json:"capabilities"`
}
type Page struct {
	Intents    []Intent `json:"intents"`
	NextCursor string   `json:"nextCursor,omitempty"`
}
type Store interface {
	Get(context.Context, string, string) (*Intent, error)
	List(context.Context, string, int, string) ([]Intent, string, error)
	CompareAndSwap(context.Context, *Intent, int64) error
}
type Catalog interface {
	ListRepos(context.Context) ([]ghost.Repo, error)
	Nodes(context.Context, string) ([]ghost.Node, error)
}
type Service struct {
	store   Store
	catalog Catalog
	source  string
	Now     func() time.Time
}

func NewService(st Store, catalog Catalog) *Service {
	return &Service{store: st, catalog: catalog, source: "ghost", Now: time.Now}
}

// NewPreviewService is only for the authenticated loopback preview. Fixture
// membership is NEVER represented as verified Ghost fleet access.
func NewPreviewService(st Store) *Service {
	return &Service{store: st, catalog: previewCatalog{}, source: "preview_fixture", Now: time.Now}
}

type previewCatalog struct{}

func (previewCatalog) ListRepos(context.Context) ([]ghost.Repo, error) {
	return []ghost.Repo{{Repo: "preview/example", Owner: "preview", Name: "example"}}, nil
}
func (previewCatalog) Nodes(context.Context, string) ([]ghost.Node, error) {
	return []ghost.Node{{NodeID: "PREVIEW_ONLY", Status: "PREVIEW_FIXTURE"}}, nil
}
func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// catalogReady checks configured availability, including a typed-nil Ghost
// client passed through Catalog. It does not claim a remote health check.
func (s *Service) catalogReady() bool {
	if s.catalog == nil {
		return false
	}
	if ready, ok := s.catalog.(interface{ Ready() bool }); ok {
		return ready.Ready()
	}
	return true
}
func (s *Service) Capabilities() Capabilities {
	c := Capabilities{Prepare: s.store != nil && s.catalogReady(), Approve: s.store != nil, Execute: false, Reason: BlockedReason, CatalogSource: s.source,
		Message: "Approval records your review only. External execution is unavailable until dispatch recovery is implemented; a fresh approval will then be required."}
	if !c.Prepare {
		c.Message = "Fleet verification is unavailable, so new reviews cannot be prepared. Existing reviews and receipts remain accessible when their store is available. External execution is unavailable."
	}
	// Only a successful Options response can attest to fetched fleet inventory.
	// A configured provider alone is not verified fleet access.
	return c
}
func (s *Service) Options(ctx context.Context) (*Options, error) {
	if s.store == nil || !s.catalogReady() {
		return nil, ErrUnavailable
	}
	repos, e := s.catalog.ListRepos(ctx)
	if e != nil {
		return nil, ErrUnavailable
	}
	nodes, e := s.catalog.Nodes(ctx, "")
	if e != nil {
		return nil, ErrUnavailable
	}
	capabilities := s.Capabilities()
	capabilities.FleetVerified = s.source == "ghost"
	return &Options{Repositories: repos, Nodes: nodes, Capabilities: capabilities}, nil
}
func normalize(in Input) (Action, error) {
	a := Action{Repo: strings.TrimSpace(in.Repo), Node: strings.TrimSpace(in.Node), Instructions: strings.TrimSpace(in.Instructions), Agent: strings.TrimSpace(in.Agent), Model: strings.TrimSpace(in.Model), Effort: strings.TrimSpace(in.Effort), Preprocess: true}
	if in.Deploy {
		return a, fmt.Errorf("%w: deployment is unavailable", ErrValidation)
	}
	if a.Agent == "" {
		a.Agent = codeupdate.DefaultCLI
	}
	if in.Preprocess != nil {
		a.Preprocess = *in.Preprocess
	}
	if a.Repo == "" || len(a.Repo) > 140 || a.Node == "" || len(a.Node) > 128 || !codeupdate.ValidCLI(a.Agent) || len(a.Model) > 128 || len(a.Effort) > 32 {
		return a, fmt.Errorf("%w: select an exact repository, machine and supported agent", ErrValidation)
	}
	if n := utf8.RuneCountInString(a.Instructions); n < 10 || n > codeupdate.MaxInstructionChars {
		return a, fmt.Errorf("%w: instructions must contain 10 to %d characters", ErrValidation, codeupdate.MaxInstructionChars)
	}
	return a, nil
}
func digest(parts ...string) string {
	v := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(v[:])
}
func actionHash(uid, source, generation string, a Action) string {
	b, _ := json.Marshal(a)
	return digest("codeapproval-v1-noexecution", uid, source, generation, string(b))
}
func validID(id string) bool { return idPattern.MatchString(id) }
func validRequest(uid, request string) bool {
	return uid != "" && !strings.ContainsRune(uid, 0) && requestPattern.MatchString(request)
}
func pageLimit(n int) int {
	if n < 1 {
		return 30
	}
	if n > 50 {
		return 50
	}
	return n
}
func validateIntent(i *Intent) error {
	if i == nil || !validID(i.ID) || i.UserID == "" || i.Version < 1 || i.Action.Deploy || !validID(i.Generation) || i.ActionHash != actionHash(i.UserID, i.Verification.Source, i.Generation, i.Action) {
		return ErrCorrupt
	}
	if i.Verification.Source != "ghost" && i.Verification.Source != "preview_fixture" {
		return ErrCorrupt
	}
	if _, e := time.Parse(time.RFC3339Nano, i.ExpiresAt); e != nil {
		return ErrCorrupt
	}
	if i.Status != StatusPrepared && i.Status != StatusApprovedBlocked {
		return ErrCorrupt
	}
	if i.Status == StatusPrepared && (i.Version != 1 || i.Receipt != nil) {
		return ErrCorrupt
	}
	if i.Status == StatusApprovedBlocked && (i.Version != 2 || i.Receipt == nil || i.Receipt.ActionHash != i.ActionHash || i.Receipt.ApprovedBy != i.UserID || i.Receipt.ExecutionState != "not_started" || !i.Receipt.RequiresFreshApproval) {
		return ErrCorrupt
	}
	return nil
}
func (s *Service) load(ctx context.Context, uid, id string) (*Intent, error) {
	if s.store == nil {
		return nil, ErrUnavailable
	}
	if uid == "" || !validID(id) {
		return nil, ErrNotFound
	}
	i, e := s.store.Get(ctx, uid, id)
	if e != nil {
		return nil, e
	}
	if i.UserID != uid || i.ID != id {
		return nil, ErrCorrupt
	}
	if e = validateIntent(i); e != nil {
		return nil, e
	}
	if i.RetainUntil <= s.now().Unix() {
		return nil, ErrNotFound
	}
	return i, nil
}
func (s *Service) view(i *Intent) *Intent {
	out := cloneIntent(*i)
	if out.Status == StatusPrepared {
		expiry, _ := time.Parse(time.RFC3339Nano, out.ExpiresAt)
		if !s.now().Before(expiry) {
			out.Status = "expired"
		}
	}
	return &out
}
func (s *Service) Get(ctx context.Context, uid, id string) (*Intent, error) {
	i, e := s.load(ctx, uid, id)
	if e != nil {
		return nil, e
	}
	return s.view(i), nil
}
func (s *Service) List(ctx context.Context, uid string, limit int, cursor string) (*Page, error) {
	if s.store == nil {
		return nil, ErrUnavailable
	}
	if uid == "" || cursor != "" && !validID(cursor) {
		return nil, ErrValidation
	}
	rows, next, e := s.store.List(ctx, uid, pageLimit(limit), cursor)
	if e != nil {
		return nil, e
	}
	out := make([]Intent, 0, len(rows))
	for n := range rows {
		i := &rows[n]
		if i.UserID != uid {
			return nil, ErrCorrupt
		}
		if e := validateIntent(i); e != nil {
			return nil, e
		}
		if i.RetainUntil > s.now().Unix() {
			out = append(out, *s.view(i))
		}
	}
	return &Page{Intents: out, NextCursor: next}, nil
}
func (s *Service) Prepare(ctx context.Context, uid string, in Input, requestID string) (*Intent, error) {
	if !validRequest(uid, requestID) {
		return nil, fmt.Errorf("%w: requestId must be 8 to 128 letters, digits, hyphens or underscores", ErrValidation)
	}
	a, e := normalize(in)
	if e != nil {
		return nil, e
	}
	id := digest("prepare", uid, requestID)[:32]
	if old, e := s.load(ctx, uid, id); e == nil {
		if old.Action != a || old.Verification.Source != s.source {
			return nil, ErrConflict
		}
		return s.view(old), nil
	} else if !errors.Is(e, ErrNotFound) {
		return nil, e
	}
	options, e := s.Options(ctx)
	if e != nil {
		return nil, e
	}
	repoFound := false
	for _, r := range options.Repositories {
		if r.Repo == a.Repo {
			repoFound = true
			break
		}
	}
	nodeStatus := ""
	nodeFound := false
	for _, n := range options.Nodes {
		if n.NodeID == a.Node {
			nodeFound = true
			nodeStatus = n.Status
			break
		}
	}
	if !repoFound || !nodeFound {
		return nil, fmt.Errorf("%w: repository or machine is not in the current fleet catalog", ErrValidation)
	}
	// A random generation prevents an old approval being replayed after TTL
	// removal and a new prepare request reusing its idempotency key (ABA).
	var random [16]byte
	if _, e := rand.Read(random[:]); e != nil {
		return nil, e
	}
	generation := hex.EncodeToString(random[:])
	hash := actionHash(uid, s.source, generation, a)
	now := s.now()
	stamp := now.Format(time.RFC3339Nano)
	i := &Intent{Generation: generation, ID: id, UserID: uid, Version: 1, Status: StatusPrepared, ActionHash: hash, Action: a, CreatedAt: stamp, ExpiresAt: now.Add(IntentTTL).Format(time.RFC3339Nano), RetainUntil: now.Add(ReceiptRetention).Unix(), Verification: Verification{Source: s.source, RepoVerified: s.source == "ghost", NodeVerified: s.source == "ghost", NodeStatus: nodeStatus, CheckedAt: stamp}}
	if e = s.store.CompareAndSwap(ctx, i, 0); errors.Is(e, ErrConflict) {
		old, re := s.load(ctx, uid, id)
		if re != nil {
			return nil, re
		}
		if old.Action != a || old.Verification.Source != s.source {
			return nil, ErrConflict
		}
		return s.view(old), nil
	} else if e != nil {
		return nil, e
	}
	return s.view(i), nil
}
func (s *Service) Approve(ctx context.Context, uid, id string, version int64, expectedActionHash, requestID string) (*Intent, error) {
	if !validRequest(uid, requestID) || version < 1 || len(expectedActionHash) != 64 {
		return nil, ErrValidation
	}
	i, e := s.load(ctx, uid, id)
	if e != nil {
		return nil, e
	}
	if i.ActionHash != expectedActionHash {
		return nil, ErrConflict
	}
	// Exact network retry returns its durable receipt, even after review expiry.
	if i.Receipt != nil {
		if i.Receipt.RequestID == requestID && i.Receipt.IntentVersion == version {
			return s.view(i), nil
		}
		return nil, ErrConflict
	}
	if i.Version != version || i.Status != StatusPrepared || i.Verification.Source != s.source {
		return nil, ErrConflict
	}
	expires, _ := time.Parse(time.RFC3339Nano, i.ExpiresAt)
	if !s.now().Before(expires) {
		return nil, ErrExpired
	}
	i.Version++
	i.Status = StatusApprovedBlocked
	i.Receipt = &Receipt{ID: digest("approval", uid, id, requestID)[:32], RequestID: requestID, IntentVersion: version, ApprovedAt: s.now().Format(time.RFC3339Nano), ApprovedBy: uid, ActionHash: i.ActionHash, State: StatusApprovedBlocked, ExecutionState: "not_started", Reason: BlockedReason, RequiresFreshApproval: true, Message: "Your review was recorded. No coding job was queued or started. External execution is unavailable; this approval cannot launch later without a fresh review."}
	if e = s.store.CompareAndSwap(ctx, i, version); errors.Is(e, ErrConflict) {
		old, re := s.load(ctx, uid, id)
		if re != nil {
			return nil, re
		}
		if old.ActionHash == expectedActionHash && old.Receipt != nil && old.Receipt.RequestID == requestID && old.Receipt.IntentVersion == version {
			return s.view(old), nil
		}
		return nil, ErrConflict
	} else if e != nil {
		return nil, e
	}
	return s.view(i), nil
}
func cloneIntent(i Intent) Intent {
	b, _ := json.Marshal(i)
	var out Intent
	_ = json.Unmarshal(b, &out)
	return out
}
