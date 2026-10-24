package alexa

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"

	"github.com/JeremyProffittOrg/live-ninja/internal/config"
	"github.com/JeremyProffittOrg/live-ninja/internal/mcpauth"
	"github.com/JeremyProffittOrg/live-ninja/internal/store"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

type Runtime struct {
	Service *Service
	Handler *Handler
	Store   *store.Store
	Auth    *mcpauth.Service
}

func NewRuntime(ctx context.Context) (*Runtime, error) {
	app := config.FromEnv()
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, err
	}
	db := dynamodb.NewFromConfig(cfg)
	st := store.NewWithClient(db, app.TableName)
	loader, err := config.NewLoader(ctx)
	if err != nil {
		return nil, err
	}
	client, err := mcpauth.LoadAlexaClient(ctx, loader)
	if err != nil {
		return nil, err
	}
	authRepo := &mcpauth.Dynamo{Client: db, Table: app.TableName}
	auth := &mcpauth.Service{Repo: authRepo, OwnerActive: authRepo.OwnerActive, AlexaClient: client}
	skill := os.Getenv("ALEXA_SKILL_ID")
	queue := os.Getenv("ALEXA_QUEUE_URL")
	if skill == "" || queue == "" {
		return nil, errors.New("Alexa skill/queue configuration is missing")
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	service := &Service{Repo: NewDynamo(db, app.TableName), Log: logger}
	service.Authorize = func(ctx context.Context, id Identity) error {
		grant, e := auth.VerifyGrant(ctx, id.Grant, mcpauth.AlexaResource)
		if e != nil || grant.UserID != id.Owner {
			return ErrUnauthorized
		}
		device, e := st.GetDevice(ctx, id.Device)
		if e != nil {
			return e
		}
		if device == nil || device.UserID != id.Owner || device.Surface != store.SurfaceAlexa || device.Status != store.DeviceStatusActive || device.AlexaGeneration == "" || device.AlexaGeneration != id.Generation {
			return ErrUnauthorized
		}
		return nil
	}
	service.Settings = func(ctx context.Context, id Identity) (Settings, error) {
		return loadSettings(ctx, st, id.Owner)
	}
	queueClient := sqs.NewFromConfig(cfg, func(o *sqs.Options) { o.RetryMaxAttempts = 1 })
	service.Publish = func(ctx context.Context, job Job) error {
		body, e := json.Marshal(job)
		if e != nil {
			return e
		}
		_, e = queueClient.SendMessage(ctx, &sqs.SendMessageInput{QueueUrl: aws.String(queue), MessageBody: aws.String(string(body))})
		return e
	}
	handler := &Handler{SkillID: skill, Service: service, Log: logger}
	handler.Resolve = func(ctx context.Context, token, providerDevice string) (Identity, error) {
		linked, e := auth.VerifyAlexa(ctx, token)
		if e != nil {
			return Identity{}, ErrUnauthorized
		}
		device, e := st.EnsureAlexaDevice(ctx, linked.UserID, skill, providerDevice)
		if e != nil {
			if errors.Is(e, store.ErrDeviceRevoked) || errors.Is(e, store.ErrDeviceOwnership) {
				return Identity{}, ErrUnauthorized
			}
			return Identity{}, e
		}
		return Identity{Owner: linked.UserID, Device: device.DeviceID, Grant: linked.GrantID, Generation: device.AlexaGeneration}, nil
	}
	return &Runtime{Service: service, Handler: handler, Store: st, Auth: auth}, nil
}

func loadSettings(ctx context.Context, st *store.Store, owner string) (Settings, error) {
	doc, err := st.GetSettings(ctx, owner)
	if err != nil {
		return Settings{}, err
	}
	effective := store.EffectiveSettings(doc, store.AlexaSettingsDeviceID(owner))
	rawVersion, _ := json.Marshal(doc["version"])
	var version int64
	if json.Unmarshal(rawVersion, &version) != nil {
		return Settings{}, errors.New("invalid settings version")
	}
	result := Settings{Version: version}
	if persona, ok := effective["persona"].(map[string]any); ok {
		result.Persona, _ = persona["presetId"].(string)
	}
	if privacy, ok := effective["privacy"].(map[string]any); ok {
		keep, _ := privacy["storeTranscripts"].(bool)
		encoded, _ := json.Marshal(privacy["retentionDays"])
		_ = json.Unmarshal(encoded, &result.RetentionDays)
		result.KeepTranscript = keep && result.RetentionDays > 0
	}
	return result, nil
}
