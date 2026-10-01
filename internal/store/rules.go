package store

// Rule item shape (assistant rules — "skills" the model loads on demand):
//
//	RULE: pk=USER#<uid> sk=RULE#<name>
//	      name, description, body, enabled(bool),
//	      source ("assistant"|"user"|"seed"), createdAt, updatedAt
//	      (RFC3339 UTC), version (ADD 1 on every upsert/toggle)
//
// Only name + description reach every session (the broker's mint-time
// index, internal/realtime/rules.go); the body is read on demand through
// the rule_load tool. Single-partition Query/GetItem only — never a Scan.

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// Rule limits (single source of truth for the store, the rule_* tools, and
// the mint-time index).
const (
	MaxRules                = 50
	RuleNameMinLen          = 3
	RuleNameMaxLen          = 48
	RuleDescriptionMinRunes = 10
	RuleDescriptionMaxRunes = 200
	RuleBodyMaxRunes        = 4000
)

// Rule sources.
const (
	RuleSourceAssistant = "assistant"
	RuleSourceUser      = "user"
	RuleSourceSeed      = "seed"
)

// SeedRuleName is the fixed name of the default rule seeded on a user's
// first empty rule list.
const SeedRuleName = "location-and-time"

// ErrRuleLimit is returned by UpsertRule when creating a NEW rule would take
// the user past MaxRules. Updating an existing rule is never limited.
var ErrRuleLimit = errors.New("store: rule limit reached")

// ErrInvalidRule wraps every UpsertRule validation failure, so the HTTP and
// tool layers can map it to a 400 / invalid_args without string matching.
var ErrInvalidRule = errors.New("store: invalid rule")

// ruleNameRe is the kebab-case slug shape; the length bound is checked
// separately so the error can say which part failed.
var ruleNameRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// Rule is the USER#<uid>/RULE#<name> item.
type Rule struct {
	UserID      string `dynamodbav:"-" json:"-"`
	Name        string `dynamodbav:"name" json:"name"`
	Description string `dynamodbav:"description" json:"description"`
	Body        string `dynamodbav:"body" json:"body"`
	Enabled     bool   `dynamodbav:"enabled" json:"enabled"`
	Source      string `dynamodbav:"source" json:"source"`
	CreatedAt   string `dynamodbav:"createdAt" json:"createdAt"` // RFC3339 UTC
	UpdatedAt   string `dynamodbav:"updatedAt" json:"updatedAt"` // RFC3339 UTC
	Version     int    `dynamodbav:"version" json:"version"`
}

// ValidRuleName reports whether s is a kebab-case slug of 3..48 characters.
func ValidRuleName(s string) bool {
	return len(s) >= RuleNameMinLen && len(s) <= RuleNameMaxLen && ruleNameRe.MatchString(s)
}

// NormalizeRuleDescription collapses every whitespace run (newlines
// included) to one space, so a description is always a single index line.
func NormalizeRuleDescription(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// DefaultRule returns the seed rule. internal/realtime uses the same value
// for a brand-new user's mint-time index (the broker never writes), so the
// two can never drift.
func DefaultRule() Rule {
	return Rule{
		Name: SeedRuleName,
		Description: "Load before answering anything about the current time, date, timezone, " +
			"weather, temperature, or where the user is.",
		Body: "Use the user's CURRENT location when one is set; otherwise use their HOME location. If neither " +
			"gives a timezone, assume America/New_York (US Eastern). The BASE KNOWLEDGE block already shows " +
			"which location and clock are in effect. When the user says where they are now (\"I'm in Denver\", " +
			"\"we landed in London\") or gives GPS coordinates, call set_current_location before answering, then " +
			"answer using the local time that tool returns. When they say they are back home, call " +
			"set_current_location with home=true. Call get_weather with no location argument for \"here\" — it " +
			"uses the current location, then home. Never ask which timezone they are in when one of these applies.",
		Enabled: true,
		Source:  RuleSourceSeed,
		Version: 1,
	}
}

func ruleSK(name string) string { return "RULE#" + name }

// validateRule normalizes r in place (trimmed name/body, one-line
// description) and checks every field bound.
func validateRule(r *Rule) error {
	r.Name = strings.TrimSpace(r.Name)
	r.Description = NormalizeRuleDescription(r.Description)
	r.Body = strings.TrimSpace(r.Body)
	switch {
	case r.UserID == "":
		return fmt.Errorf("%w: userID is required", ErrInvalidRule)
	case !ValidRuleName(r.Name):
		return fmt.Errorf("%w: name must be a lowercase kebab-case slug of %d to %d characters",
			ErrInvalidRule, RuleNameMinLen, RuleNameMaxLen)
	}
	if n := utf8.RuneCountInString(r.Description); n < RuleDescriptionMinRunes || n > RuleDescriptionMaxRunes {
		return fmt.Errorf("%w: description must be %d to %d characters",
			ErrInvalidRule, RuleDescriptionMinRunes, RuleDescriptionMaxRunes)
	}
	if n := utf8.RuneCountInString(r.Body); n < 1 || n > RuleBodyMaxRunes {
		return fmt.Errorf("%w: body must be 1 to %d characters", ErrInvalidRule, RuleBodyMaxRunes)
	}
	switch r.Source {
	case RuleSourceAssistant, RuleSourceUser, RuleSourceSeed:
	default:
		return fmt.Errorf("%w: source must be assistant, user, or seed", ErrInvalidRule)
	}
	return nil
}

// ---- RULE CRUD ----

// ListRules returns the user's rules sorted by name. On a user's very first
// list — no RULE# items at all — it seeds the default rule (conditional put,
// so a racing seed is a no-op) and returns it. Like the default guide,
// deleting the seed sticks only until the next empty list; disabling it is
// the persistent opt-out.
func (s *Store) ListRules(ctx context.Context, userID string) ([]Rule, error) {
	if userID == "" {
		return nil, errors.New("store: userID is required")
	}
	rules, err := s.listRulesRaw(ctx, userID)
	if err != nil {
		return nil, err
	}
	if len(rules) == 0 {
		if err := s.seedDefaultRule(ctx, userID); err != nil {
			return nil, err
		}
		if rules, err = s.listRulesRaw(ctx, userID); err != nil {
			return nil, err
		}
	}
	sort.SliceStable(rules, func(i, j int) bool { return rules[i].Name < rules[j].Name })
	return rules, nil
}

// GetRule fetches one rule; ErrNotFound when absent. A miss on the seed
// rule's name for a user with no rules at all seeds it first: the broker's
// mint-time index advertises the seed to a brand-new user without writing
// it, so the rule_load that index invites must find it.
func (s *Store) GetRule(ctx context.Context, userID, name string) (*Rule, error) {
	if userID == "" || name == "" {
		return nil, errors.New("store: userID and name are required")
	}
	r, err := s.getRuleRaw(ctx, userID, name)
	if err != nil {
		return nil, err
	}
	if r == nil && name == SeedRuleName {
		n, err := s.countRules(ctx, userID)
		if err != nil {
			return nil, err
		}
		if n == 0 {
			if err := s.seedDefaultRule(ctx, userID); err != nil {
				return nil, err
			}
			if r, err = s.getRuleRaw(ctx, userID, name); err != nil {
				return nil, err
			}
		}
	}
	if r == nil {
		return nil, ErrNotFound
	}
	return r, nil
}

// UpsertRule validates and writes a rule, bumping version by 1 (UpdateItem
// SET+ADD). A new rule gets createdAt=now and version 1; an existing rule
// keeps its createdAt. Creating a NEW name when the user already holds
// MaxRules rules fails with ErrRuleLimit. Returns the stored rule.
//
// The create/update branch is decided by a read and enforced by a
// condition (attribute_not_exists / attribute_exists on pk), so a
// concurrent create or delete between the two is retried once against the
// fresh state instead of silently resetting createdAt or resurrecting a
// deleted rule with a stale version.
func (s *Store) UpsertRule(ctx context.Context, r Rule) (*Rule, error) {
	if err := validateRule(&r); err != nil {
		return nil, err
	}
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		existing, err := s.getRuleRaw(ctx, r.UserID, r.Name)
		if err != nil {
			return nil, err
		}
		if existing == nil {
			n, err := s.countRules(ctx, r.UserID)
			if err != nil {
				return nil, err
			}
			if n >= MaxRules {
				return nil, ErrRuleLimit
			}
		}
		err = s.writeRule(ctx, r, existing == nil)
		if err == nil {
			return s.GetRule(ctx, r.UserID, r.Name)
		}
		var condErr *types.ConditionalCheckFailedException
		if !errors.As(err, &condErr) {
			return nil, fmt.Errorf("store: upsert rule: %w", err)
		}
		lastErr = err
	}
	return nil, fmt.Errorf("store: upsert rule: concurrent change, retry: %w", lastErr)
}

// writeRule is UpsertRule's single conditional UpdateItem.
func (s *Store) writeRule(ctx context.Context, r Rule, create bool) error {
	now := time.Now().UTC().Format(time.RFC3339)
	set := "SET #n = :n, #d = :d, #b = :b, #en = :en, #src = :src, #upd = :upd"
	cond := "attribute_exists(pk)"
	names := map[string]string{
		"#n":   "name",
		"#d":   "description",
		"#b":   "body",
		"#en":  "enabled",
		"#src": "source",
		"#upd": "updatedAt",
		"#ver": "version",
	}
	values := map[string]types.AttributeValue{
		":n":   &types.AttributeValueMemberS{Value: r.Name},
		":d":   &types.AttributeValueMemberS{Value: r.Description},
		":b":   &types.AttributeValueMemberS{Value: r.Body},
		":en":  &types.AttributeValueMemberBOOL{Value: r.Enabled},
		":src": &types.AttributeValueMemberS{Value: r.Source},
		":upd": &types.AttributeValueMemberS{Value: now},
		":one": &types.AttributeValueMemberN{Value: "1"},
	}
	if create {
		set += ", #crt = :crt"
		cond = "attribute_not_exists(pk)"
		names["#crt"] = "createdAt"
		values[":crt"] = &types.AttributeValueMemberS{Value: now}
	}
	_, err := s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.table),
		Key: map[string]types.AttributeValue{
			"pk": &types.AttributeValueMemberS{Value: userPK(r.UserID)},
			"sk": &types.AttributeValueMemberS{Value: ruleSK(r.Name)},
		},
		UpdateExpression:          aws.String(set + " ADD #ver :one"),
		ConditionExpression:       aws.String(cond),
		ExpressionAttributeNames:  names,
		ExpressionAttributeValues: values,
	})
	return err
}

// SetRuleEnabled flips one rule's enabled switch (bumping version);
// ErrNotFound when the rule does not exist — a toggle never creates a rule.
func (s *Store) SetRuleEnabled(ctx context.Context, userID, name string, enabled bool) (*Rule, error) {
	if userID == "" || name == "" {
		return nil, errors.New("store: userID and name are required")
	}
	_, err := s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.table),
		Key: map[string]types.AttributeValue{
			"pk": &types.AttributeValueMemberS{Value: userPK(userID)},
			"sk": &types.AttributeValueMemberS{Value: ruleSK(name)},
		},
		UpdateExpression:    aws.String("SET #en = :en, #upd = :upd ADD #ver :one"),
		ConditionExpression: aws.String("attribute_exists(pk)"),
		ExpressionAttributeNames: map[string]string{
			"#en":  "enabled",
			"#upd": "updatedAt",
			"#ver": "version",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":en":  &types.AttributeValueMemberBOOL{Value: enabled},
			":upd": &types.AttributeValueMemberS{Value: time.Now().UTC().Format(time.RFC3339)},
			":one": &types.AttributeValueMemberN{Value: "1"},
		},
	})
	if err != nil {
		var condErr *types.ConditionalCheckFailedException
		if errors.As(err, &condErr) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("store: set rule enabled: %w", err)
	}
	return s.GetRule(ctx, userID, name)
}

// DeleteRule removes the rule; ErrNotFound when it didn't exist.
func (s *Store) DeleteRule(ctx context.Context, userID, name string) error {
	if userID == "" || name == "" {
		return errors.New("store: userID and name are required")
	}
	out, err := s.client.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(s.table),
		Key: map[string]types.AttributeValue{
			"pk": &types.AttributeValueMemberS{Value: userPK(userID)},
			"sk": &types.AttributeValueMemberS{Value: ruleSK(name)},
		},
		ReturnValues: types.ReturnValueAllOld,
	})
	if err != nil {
		return fmt.Errorf("store: delete rule: %w", err)
	}
	if out.Attributes == nil {
		return ErrNotFound
	}
	return nil
}

// ---- internals ----

func (s *Store) getRuleRaw(ctx context.Context, userID, name string) (*Rule, error) {
	out, err := s.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:      aws.String(s.table),
		ConsistentRead: aws.Bool(true),
		Key: map[string]types.AttributeValue{
			"pk": &types.AttributeValueMemberS{Value: userPK(userID)},
			"sk": &types.AttributeValueMemberS{Value: ruleSK(name)},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store: get rule: %w", err)
	}
	if out.Item == nil {
		return nil, nil
	}
	var r Rule
	if err := attributevalue.UnmarshalMap(out.Item, &r); err != nil {
		return nil, fmt.Errorf("store: unmarshal rule: %w", err)
	}
	r.UserID = userID
	return &r, nil
}

func (s *Store) listRulesRaw(ctx context.Context, userID string) ([]Rule, error) {
	items, err := s.queryAllPages(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(s.table),
		KeyConditionExpression: aws.String("pk = :pk AND begins_with(sk, :pfx)"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk":  &types.AttributeValueMemberS{Value: userPK(userID)},
			":pfx": &types.AttributeValueMemberS{Value: "RULE#"},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store: list rules: %w", err)
	}
	out := make([]Rule, 0, len(items))
	for _, raw := range items {
		var r Rule
		if err := attributevalue.UnmarshalMap(raw, &r); err != nil {
			return nil, fmt.Errorf("store: unmarshal rule: %w", err)
		}
		if r.Name == "" {
			if sk, ok := raw["sk"].(*types.AttributeValueMemberS); ok {
				r.Name = strings.TrimPrefix(sk.Value, "RULE#")
			}
		}
		r.UserID = userID
		out = append(out, r)
	}
	return out, nil
}

// countRules counts the user's RULE# items, projecting only the key so a
// full set of 4000-rune bodies is never read just to enforce the cap.
func (s *Store) countRules(ctx context.Context, userID string) (int, error) {
	items, err := s.queryAllPages(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(s.table),
		KeyConditionExpression: aws.String("pk = :pk AND begins_with(sk, :pfx)"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk":  &types.AttributeValueMemberS{Value: userPK(userID)},
			":pfx": &types.AttributeValueMemberS{Value: "RULE#"},
		},
		ProjectionExpression: aws.String("sk"),
	})
	if err != nil {
		return 0, fmt.Errorf("store: count rules: %w", err)
	}
	return len(items), nil
}

// seedDefaultRule conditionally creates the default rule (a lost race with
// another surface's first list is an idempotent no-op).
func (s *Store) seedDefaultRule(ctx context.Context, userID string) error {
	r := DefaultRule()
	now := time.Now().UTC().Format(time.RFC3339)
	err := s.ConditionalPut(ctx, userPK(userID), ruleSK(r.Name), map[string]any{
		"name":        r.Name,
		"description": r.Description,
		"body":        r.Body,
		"enabled":     r.Enabled,
		"source":      r.Source,
		"createdAt":   now,
		"updatedAt":   now,
		"version":     r.Version,
	}, 0)
	if err != nil && !errors.Is(err, ErrAlreadyExists) {
		return fmt.Errorf("store: seed default rule: %w", err)
	}
	return nil
}
