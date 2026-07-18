package store

// User-editable personas + the shared persona catalog (personas platform
// feature).
//
// Item shapes (all access is GetItem / single-partition Query — never a
// Scan, per the package rule):
//
//	OWN:    pk=USER#<uid>  sk=PERSONA#<personaId>
//	        personaId, name, description, instructions, voice,
//	        shared(bool), createdAt, updatedAt
//	MIRROR: pk=CATALOG     sk=PERSONA#<personaId>      (shared only)
//	        same fields + ownerId, ownerName, sharedAt — the write-through
//	        copy that makes "list every shared persona" a single-partition
//	        Query and lets the broker re-check shared visibility at mint
//	        with one GetItem.
//
// Write-through discipline: share -> put mirror; unshare/delete -> delete
// mirror; edit-while-shared -> refresh mirror. Persona IDs are 32-hex
// server-generated random values, so a CATALOG mirror can only ever have
// been written by its owner (no cross-user sk collisions).
//
// Built-in personas (internal/realtime's registry) never appear in
// DynamoDB; the webapp layer guards them from create/update/delete and
// the broker resolves them first, so a stored persona can never shadow a
// built-in ID.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

const (
	personaSKPrefix  = "PERSONA#"
	pkPersonaCatalog = "CATALOG"
)

func personaSK(personaID string) string { return personaSKPrefix + personaID }

// NewPersonaID returns a 32-hex-char random persona ID (crypto/rand).
// Server-generated only — IDs never come from clients, and the length/
// alphabet guarantees no collision with built-in registry IDs.
func NewPersonaID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("store: persona id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// UserPersona is one USER#<uid>/PERSONA#<personaId> item: a custom persona
// the user authored (or copied from a built-in / shared persona).
type UserPersona struct {
	PersonaID    string `dynamodbav:"personaId"`
	Name         string `dynamodbav:"name"`
	Description  string `dynamodbav:"description,omitempty"`
	Instructions string `dynamodbav:"instructions"`
	Voice        string `dynamodbav:"voice,omitempty"`
	Shared       bool   `dynamodbav:"shared"`
	CreatedAt    string `dynamodbav:"createdAt"` // RFC3339 UTC
	UpdatedAt    string `dynamodbav:"updatedAt"` // RFC3339 UTC
}

// CatalogPersona is one CATALOG/PERSONA#<personaId> mirror row: the
// shared, attributed copy every allowlisted user's picker lists.
type CatalogPersona struct {
	PersonaID    string `dynamodbav:"personaId"`
	Name         string `dynamodbav:"name"`
	Description  string `dynamodbav:"description,omitempty"`
	Instructions string `dynamodbav:"instructions"`
	Voice        string `dynamodbav:"voice,omitempty"`
	Shared       bool   `dynamodbav:"shared"` // always true in a live mirror; the broker requires it
	OwnerID      string `dynamodbav:"ownerId"`
	OwnerName    string `dynamodbav:"ownerName,omitempty"`
	SharedAt     string `dynamodbav:"sharedAt"` // RFC3339 UTC
}

func validatePersonaFields(userID, personaID string) error {
	if userID == "" || personaID == "" {
		return errors.New("store: userID and personaID are required")
	}
	if strings.Contains(personaID, "#") || strings.Contains(personaID, ":") {
		return errors.New("store: personaID must not contain '#' or ':'")
	}
	return nil
}

// CreateUserPersona writes a new custom persona. Conditional put so an ID
// collision is ErrAlreadyExists, never a silent overwrite. Fills
// CreatedAt/UpdatedAt when unset.
func (s *Store) CreateUserPersona(ctx context.Context, userID string, p *UserPersona) error {
	if p == nil {
		return errors.New("store: persona is required")
	}
	if err := validatePersonaFields(userID, p.PersonaID); err != nil {
		return err
	}
	if strings.TrimSpace(p.Name) == "" || strings.TrimSpace(p.Instructions) == "" {
		return errors.New("store: persona name and instructions are required")
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if p.CreatedAt == "" {
		p.CreatedAt = now
	}
	if p.UpdatedAt == "" {
		p.UpdatedAt = now
	}

	av, err := attributevalue.MarshalMap(p)
	if err != nil {
		return fmt.Errorf("store: marshal persona: %w", err)
	}
	av["pk"] = &types.AttributeValueMemberS{Value: userPK(userID)}
	av["sk"] = &types.AttributeValueMemberS{Value: personaSK(p.PersonaID)}

	_, err = s.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:           aws.String(s.table),
		Item:                av,
		ConditionExpression: aws.String("attribute_not_exists(pk) AND attribute_not_exists(sk)"),
	})
	if err != nil {
		var condErr *types.ConditionalCheckFailedException
		if errors.As(err, &condErr) {
			return ErrAlreadyExists
		}
		return fmt.Errorf("store: create persona: %w", err)
	}
	return nil
}

// GetUserPersona fetches one of the user's own personas. (nil, nil) when
// absent — and because pk is always the caller's own partition, another
// user's persona ID is indistinguishable from absent.
func (s *Store) GetUserPersona(ctx context.Context, userID, personaID string) (*UserPersona, error) {
	if err := validatePersonaFields(userID, personaID); err != nil {
		return nil, err
	}
	out, err := s.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.table),
		Key: map[string]types.AttributeValue{
			"pk": &types.AttributeValueMemberS{Value: userPK(userID)},
			"sk": &types.AttributeValueMemberS{Value: personaSK(personaID)},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store: get persona: %w", err)
	}
	if out.Item == nil {
		return nil, nil
	}
	var p UserPersona
	if err := attributevalue.UnmarshalMap(out.Item, &p); err != nil {
		return nil, fmt.Errorf("store: unmarshal persona: %w", err)
	}
	return &p, nil
}

// ListUserPersonas returns the user's whole custom-persona set
// (single-partition Query on begins_with PERSONA#; the webapp caps
// creation, so the set is small).
func (s *Store) ListUserPersonas(ctx context.Context, userID string) ([]UserPersona, error) {
	if userID == "" {
		return nil, errors.New("store: userID is required")
	}
	raw, err := s.queryAllPages(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(s.table),
		KeyConditionExpression: aws.String("pk = :pk AND begins_with(sk, :pfx)"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk":  &types.AttributeValueMemberS{Value: userPK(userID)},
			":pfx": &types.AttributeValueMemberS{Value: personaSKPrefix},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store: list personas: %w", err)
	}
	out := make([]UserPersona, 0, len(raw))
	for _, r := range raw {
		var p UserPersona
		if err := attributevalue.UnmarshalMap(r, &p); err != nil {
			return nil, fmt.Errorf("store: unmarshal persona: %w", err)
		}
		out = append(out, p)
	}
	return out, nil
}

// UpdateUserPersona replaces an existing persona's content (the handler
// merges the client's partial edit into the loaded item first). The put
// is conditional on the item existing — ErrNotFound otherwise — and when
// the persona is shared the CATALOG mirror is refreshed write-through
// (ownerName attributes the mirror).
func (s *Store) UpdateUserPersona(ctx context.Context, userID, ownerName string, p *UserPersona) error {
	if p == nil {
		return errors.New("store: persona is required")
	}
	if err := validatePersonaFields(userID, p.PersonaID); err != nil {
		return err
	}
	if strings.TrimSpace(p.Name) == "" || strings.TrimSpace(p.Instructions) == "" {
		return errors.New("store: persona name and instructions are required")
	}
	p.UpdatedAt = time.Now().UTC().Format(time.RFC3339)

	av, err := attributevalue.MarshalMap(p)
	if err != nil {
		return fmt.Errorf("store: marshal persona: %w", err)
	}
	av["pk"] = &types.AttributeValueMemberS{Value: userPK(userID)}
	av["sk"] = &types.AttributeValueMemberS{Value: personaSK(p.PersonaID)}

	_, err = s.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:           aws.String(s.table),
		Item:                av,
		ConditionExpression: aws.String("attribute_exists(pk) AND attribute_exists(sk)"),
	})
	if err != nil {
		var condErr *types.ConditionalCheckFailedException
		if errors.As(err, &condErr) {
			return ErrNotFound
		}
		return fmt.Errorf("store: update persona: %w", err)
	}

	if p.Shared {
		if err := s.putCatalogPersona(ctx, userID, ownerName, p); err != nil {
			return err
		}
	}
	return nil
}

// SetUserPersonaShared flips a persona's shared flag and applies the
// write-through mirror (put on share, delete on unshare). Idempotent:
// re-sharing refreshes the mirror; re-unsharing is a no-op delete.
// Returns the updated persona, or ErrNotFound.
func (s *Store) SetUserPersonaShared(ctx context.Context, userID, ownerName, personaID string, shared bool) (*UserPersona, error) {
	if err := validatePersonaFields(userID, personaID); err != nil {
		return nil, err
	}
	p, err := s.GetUserPersona(ctx, userID, personaID)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, ErrNotFound
	}
	p.Shared = shared
	p.UpdatedAt = time.Now().UTC().Format(time.RFC3339)

	_, err = s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.table),
		Key: map[string]types.AttributeValue{
			"pk": &types.AttributeValueMemberS{Value: userPK(userID)},
			"sk": &types.AttributeValueMemberS{Value: personaSK(personaID)},
		},
		ConditionExpression: aws.String("attribute_exists(pk)"),
		UpdateExpression:    aws.String("SET shared = :sh, updatedAt = :ts"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":sh": &types.AttributeValueMemberBOOL{Value: shared},
			":ts": &types.AttributeValueMemberS{Value: p.UpdatedAt},
		},
	})
	if err != nil {
		var condErr *types.ConditionalCheckFailedException
		if errors.As(err, &condErr) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("store: set persona shared: %w", err)
	}

	if shared {
		if err := s.putCatalogPersona(ctx, userID, ownerName, p); err != nil {
			return nil, err
		}
	} else {
		if err := s.deleteCatalogPersona(ctx, personaID); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// DeleteUserPersona removes a persona and its CATALOG mirror (write-
// through). The handler has already verified existence/ownership via
// GetUserPersona; deleting an already-absent row is an idempotent no-op
// (the mirror can only be the owner's — IDs are server-random, so no
// cross-user sk collision exists).
func (s *Store) DeleteUserPersona(ctx context.Context, userID, personaID string) error {
	if err := validatePersonaFields(userID, personaID); err != nil {
		return err
	}
	if _, err := s.client.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(s.table),
		Key: map[string]types.AttributeValue{
			"pk": &types.AttributeValueMemberS{Value: userPK(userID)},
			"sk": &types.AttributeValueMemberS{Value: personaSK(personaID)},
		},
	}); err != nil {
		return fmt.Errorf("store: delete persona: %w", err)
	}
	return s.deleteCatalogPersona(ctx, personaID)
}

// ListSharedPersonas returns every shared-catalog mirror (single-
// partition Query on CATALOG + begins_with PERSONA#; the catalog is
// bounded by the allowlisted-user population's shared personas).
func (s *Store) ListSharedPersonas(ctx context.Context) ([]CatalogPersona, error) {
	raw, err := s.queryAllPages(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(s.table),
		KeyConditionExpression: aws.String("pk = :pk AND begins_with(sk, :pfx)"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk":  &types.AttributeValueMemberS{Value: pkPersonaCatalog},
			":pfx": &types.AttributeValueMemberS{Value: personaSKPrefix},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store: list shared personas: %w", err)
	}
	out := make([]CatalogPersona, 0, len(raw))
	for _, r := range raw {
		var p CatalogPersona
		if err := attributevalue.UnmarshalMap(r, &p); err != nil {
			return nil, fmt.Errorf("store: unmarshal catalog persona: %w", err)
		}
		out = append(out, p)
	}
	return out, nil
}

// GetCatalogPersona fetches one shared-catalog mirror. (nil, nil) when
// absent.
func (s *Store) GetCatalogPersona(ctx context.Context, personaID string) (*CatalogPersona, error) {
	if personaID == "" {
		return nil, errors.New("store: personaID is required")
	}
	out, err := s.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.table),
		Key: map[string]types.AttributeValue{
			"pk": &types.AttributeValueMemberS{Value: pkPersonaCatalog},
			"sk": &types.AttributeValueMemberS{Value: personaSK(personaID)},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store: get catalog persona: %w", err)
	}
	if out.Item == nil {
		return nil, nil
	}
	var p CatalogPersona
	if err := attributevalue.UnmarshalMap(out.Item, &p); err != nil {
		return nil, fmt.Errorf("store: unmarshal catalog persona: %w", err)
	}
	return &p, nil
}

func (s *Store) putCatalogPersona(ctx context.Context, ownerID, ownerName string, p *UserPersona) error {
	mirror := CatalogPersona{
		PersonaID:    p.PersonaID,
		Name:         p.Name,
		Description:  p.Description,
		Instructions: p.Instructions,
		Voice:        p.Voice,
		Shared:       true,
		OwnerID:      ownerID,
		OwnerName:    ownerName,
		SharedAt:     time.Now().UTC().Format(time.RFC3339),
	}
	av, err := attributevalue.MarshalMap(mirror)
	if err != nil {
		return fmt.Errorf("store: marshal catalog persona: %w", err)
	}
	av["pk"] = &types.AttributeValueMemberS{Value: pkPersonaCatalog}
	av["sk"] = &types.AttributeValueMemberS{Value: personaSK(p.PersonaID)}

	if _, err := s.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.table),
		Item:      av,
	}); err != nil {
		return fmt.Errorf("store: put catalog persona: %w", err)
	}
	return nil
}

func (s *Store) deleteCatalogPersona(ctx context.Context, personaID string) error {
	if _, err := s.client.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(s.table),
		Key: map[string]types.AttributeValue{
			"pk": &types.AttributeValueMemberS{Value: pkPersonaCatalog},
			"sk": &types.AttributeValueMemberS{Value: personaSK(personaID)},
		},
	}); err != nil {
		return fmt.Errorf("store: delete catalog persona: %w", err)
	}
	return nil
}
