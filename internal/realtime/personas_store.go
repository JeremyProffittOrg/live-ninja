package realtime

// Stored-persona resolution for the session mint (personas platform
// feature): user-created personas live at USER#<uid>/PERSONA#<pid> and
// shared ones are mirrored at CATALOG/PERSONA#<pid> (internal/store/
// personas.go — write-through on share/unshare/delete). The broker
// resolves them here at mint time via GetItem (key lookups only, never a
// Scan), which is the live re-check the feature requires: a persona
// deleted or un-shared after the client's picker loaded falls back to the
// default persona instead of minting with stale instructions.
//
// The anti-injection contract is preserved end to end:
//   - clients only ever send a bare persona ID;
//   - the WEB function (which verified the caller's identity) qualifies
//     that ID into one of the refs below — it refuses client IDs that
//     contain ':' so a client can never fabricate a ref to another user's
//     partition;
//   - instruction text flows DynamoDB -> broker -> OpenAI and never
//     through a client;
//   - user-authored style text is composed onto the shared operational
//     core (ComposeCustomInstructions) so it can shape tone but never
//     rewrite tool/safety policy.

import (
	"context"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// Ref prefixes. Composed ONLY server-side (webapp qualifies verified
// identities into these; see UserPersonaRef/SharedPersonaRef).
const (
	userPersonaRefPrefix   = "user:"
	sharedPersonaRefPrefix = "shared:"
)

// personaSKPrefix mirrors internal/store's PERSONA# sort-key prefix (the
// broker deliberately does not import the store package — it stays inside
// the OpenAI-key isolate, mirroring guides.go/mint.go's posture).
const personaSKPrefix = "PERSONA#"

// personaCatalogPK mirrors internal/store's CATALOG partition for shared
// persona mirrors.
const personaCatalogPK = "CATALOG"

// personaLookupTimeout bounds the per-mint stored-persona GetItem so a
// DynamoDB hiccup degrades to the default persona instead of stalling the
// mint.
const personaLookupTimeout = 3 * time.Second

// UserPersonaRef composes the mint ref for the caller's OWN custom
// persona. userID must come from a verified auth context.
func UserPersonaRef(userID, personaID string) string {
	return userPersonaRefPrefix + userID + ":" + personaID
}

// SharedPersonaRef composes the mint ref for a shared-catalog persona.
func SharedPersonaRef(personaID string) string {
	return sharedPersonaRefPrefix + personaID
}

// PersonaItemGetter is the single DynamoDB read stored-persona resolution
// needs. A *dynamodb.Client satisfies it; tests inject a fake.
type PersonaItemGetter interface {
	GetItem(ctx context.Context, params *dynamodb.GetItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
}

var (
	personaStoreMu     sync.RWMutex
	personaStoreGetter PersonaItemGetter
	personaStoreTable  string
	// personaStoreConfigured records that SetPersonaStore was called
	// explicitly (even with nil, which disables stored-persona resolution
	// outright) — the lazy env self-wiring below only ever runs when no
	// explicit configuration happened, so tests can never fall through to
	// a real AWS client.
	personaStoreConfigured bool
	personaStoreOnce       sync.Once
)

// SetPersonaStore installs the DynamoDB source for stored-persona refs.
// Tests inject a fake here (nil disables resolution); in the deployed
// broker the source is lazily self-wired from the ambient AWS config +
// TABLE_NAME on first use (so cmd/realtime-broker needs no new wiring for
// this feature).
func SetPersonaStore(g PersonaItemGetter, table string) {
	personaStoreMu.Lock()
	defer personaStoreMu.Unlock()
	personaStoreGetter = g
	personaStoreTable = table
	personaStoreConfigured = true
}

// personaStoreSource returns the installed (or lazily self-wired) source.
// Returns (nil, "") when no source can be built — resolution then falls
// back to the default persona, never an error (voice must not go down
// over a persona lookup).
func personaStoreSource() (PersonaItemGetter, string) {
	personaStoreMu.RLock()
	g, t, configured := personaStoreGetter, personaStoreTable, personaStoreConfigured
	personaStoreMu.RUnlock()
	if configured {
		return g, t
	}

	personaStoreOnce.Do(func() {
		cfg, err := awsconfig.LoadDefaultConfig(context.Background())
		if err != nil {
			return
		}
		table := os.Getenv("TABLE_NAME")
		if table == "" {
			table = "live-ninja" // config.FromEnv's default
		}
		SetPersonaStore(dynamodb.NewFromConfig(cfg), table)
	})

	personaStoreMu.RLock()
	defer personaStoreMu.RUnlock()
	return personaStoreGetter, personaStoreTable
}

// storedPersonaItem is the attribute subset resolution reads off a
// USER#/PERSONA# or CATALOG/PERSONA# item.
type storedPersonaItem struct {
	PersonaID    string `dynamodbav:"personaId"`
	Name         string `dynamodbav:"name"`
	Instructions string `dynamodbav:"instructions"`
	Voice        string `dynamodbav:"voice"`
	Shared       bool   `dynamodbav:"shared"`
}

// resolveStoredPersonaRef resolves a server-composed persona ref against
// live DynamoDB state. ok=false for anything that is not a valid,
// currently-visible stored persona (the caller then falls back to the
// default persona):
//
//	user:<uid>:<pid>  -> GetItem USER#<uid>/PERSONA#<pid>; the item's
//	                     existence in the owner's partition IS the
//	                     ownership re-check (delete removes it).
//	shared:<pid>      -> GetItem CATALOG/PERSONA#<pid> and require
//	                     shared=true; unshare/delete write-through removes
//	                     or flips the mirror, so visibility is re-checked
//	                     at mint.
func resolveStoredPersonaRef(id string) (Persona, bool) {
	var pk, sk string
	requireShared := false
	switch {
	case strings.HasPrefix(id, userPersonaRefPrefix):
		rest := id[len(userPersonaRefPrefix):]
		uid, pid, ok := strings.Cut(rest, ":")
		if !ok || uid == "" || pid == "" {
			return Persona{}, false
		}
		pk, sk = "USER#"+uid, personaSKPrefix+pid
	case strings.HasPrefix(id, sharedPersonaRefPrefix):
		pid := id[len(sharedPersonaRefPrefix):]
		if pid == "" {
			return Persona{}, false
		}
		pk, sk = personaCatalogPK, personaSKPrefix+pid
		requireShared = true
	default:
		return Persona{}, false
	}

	getter, table := personaStoreSource()
	if getter == nil || table == "" {
		return Persona{}, false
	}

	ctx, cancel := context.WithTimeout(context.Background(), personaLookupTimeout)
	defer cancel()
	out, err := getter.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(table),
		Key: map[string]ddbtypes.AttributeValue{
			"pk": &ddbtypes.AttributeValueMemberS{Value: pk},
			"sk": &ddbtypes.AttributeValueMemberS{Value: sk},
		},
	})
	if err != nil || len(out.Item) == 0 {
		return Persona{}, false
	}

	var item storedPersonaItem
	if err := attributevalue.UnmarshalMap(out.Item, &item); err != nil {
		return Persona{}, false
	}
	if strings.TrimSpace(item.Instructions) == "" {
		return Persona{}, false
	}
	if requireShared && !item.Shared {
		return Persona{}, false
	}

	name := item.Name
	if name == "" {
		name = item.PersonaID
	}
	return Persona{
		ID:           id,
		Name:         name,
		Voice:        item.Voice,
		Style:        item.Instructions,
		Instructions: ComposeCustomInstructions(item.Instructions),
	}, true
}
