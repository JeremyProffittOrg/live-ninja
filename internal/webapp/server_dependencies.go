package webapp

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/JeremyProffittOrg/live-ninja/internal/agentmemory"
	"github.com/JeremyProffittOrg/live-ninja/internal/codeapproval"
	"github.com/JeremyProffittOrg/live-ninja/internal/codeupdate"
	"github.com/JeremyProffittOrg/live-ninja/internal/config"
	"github.com/JeremyProffittOrg/live-ninja/internal/deliv"
	"github.com/JeremyProffittOrg/live-ninja/internal/ghost"
	"github.com/JeremyProffittOrg/live-ninja/internal/jobs"
	"github.com/JeremyProffittOrg/live-ninja/internal/store"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/firehose"
	lambdasvc "github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

// NewServerDependencies is shared by the web application and Alexa worker.
// It builds the existing tools' dependencies, without web login or JWT signing.
func NewServerDependencies(ctx context.Context, cfg config.App, logger *slog.Logger) (*Deps, error) {
	st, err := store.New(ctx, cfg.TableName)
	if err != nil {
		return nil, err
	}

	loader, err := config.NewLoader(ctx)
	if err != nil {
		return nil, err
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, err
	}

	lambdaClient := lambdasvc.NewFromConfig(awsCfg)
	s3Client := s3.NewFromConfig(awsCfg)
	ddbClient := dynamodb.NewFromConfig(awsCfg)
	deps := &Deps{
		Store:                  st,
		Cfg:                    cfg,
		Secrets:                loader,
		Log:                    logger,
		BrokerFn:               os.Getenv("BROKER_FUNCTION_NAME"),
		SQSEmailURL:            cfg.EmailQueueURL,
		SQSRcaURL:              os.Getenv("RCA_QUEUE_URL"),
		SQS:                    sqs.NewFromConfig(awsCfg),
		Lambda:                 lambdaClient,
		PhoneAccount:           lambdasvc.NewFromConfig(awsCfg, func(o *lambdasvc.Options) { o.Region = "us-east-1" }),
		Firehose:               firehose.NewFromConfig(awsCfg),
		TelemetryStreamName:    os.Getenv("TELEMETRY_FIREHOSE_STREAM_NAME"),
		AndroidArtifacts:       s3Client,
		AndroidArtifactsBucket: os.Getenv("ANDROID_RELEASES_BUCKET"),
		AndroidLatestKey:       os.Getenv("ANDROID_LATEST_KEY"),
		AndroidAssetLinksKey:   os.Getenv("ANDROID_ASSETLINKS_KEY"),
		CodeUpdateQueueURL:     os.Getenv("CODE_UPDATE_QUEUE_URL"),
	}
	jobStore, err := jobs.NewDynamoStore(ctx, cfg.TableName)
	if err != nil {
		return nil, err
	}
	deps.Jobs = jobs.NewService(jobStore)
	if phoneFunction := os.Getenv("PHONE_MESSAGES_FUNCTION_ARN"); phoneFunction != "" {
		provider, e := jobs.NewPhoneResearchProvider(deps.PhoneAccount, phoneFunction)
		if e != nil {
			return nil, e
		}
		deps.Jobs = deps.Jobs.WithPhoneResearch(provider)
	}
	deps.JobsSchedulingEnabled = os.Getenv("JOBS_SCHEDULING_ENABLED") == "true"
	// agentcore-memory: nil when AGENTCORE_MEMORY_ID is unset or the mode is
	// off; every caller treats nil as "not configured". The counter feeds the
	// per-user day row usage-rollup sums (plan.md cost-guard).
	deps.AgentMemory = agentmemory.NewFromAWSConfig(awsCfg, agentmemory.ConfigFromEnv(), logger)
	deps.AgentMemory.SetCounter(func(ctx context.Context, userID string, events, retrievals int64) {
		if err := st.AddDayMemoryUsage(ctx, userID, store.DayPeriod(time.Now()), events, retrievals); err != nil {
			logger.Warn("memory usage counter failed", slog.String("error", err.Error()), slog.String("userId", userID))
		}
	})
	if deps.AgentMemory != nil {
		logger.Info("agentcore memory enabled", slog.String("mode", string(deps.AgentMemory.Config().Mode)))
	}
	// Voice-driven code updates. All three pieces are wired together or not at
	// all: the ghost client reaches the fleet, the store tracks a request, and
	// the dispatcher supplies the progress endpoint's email leg. Missing config
	// leaves the code_update_* tools reporting not_configured and the progress
	// route answering 503 — never a silent no-op.
	if fn := os.Getenv("GHOST_COMMAND_FUNCTION_ARN"); fn != "" {
		deps.Ghost = ghost.New(ghost.Config{API: lambdaClient, Function: fn, Log: logger})
		deps.CodeUpdate = codeupdate.NewStore(ddbClient, cfg.TableName, nil)
		deps.CodeUpdateDispatcher = &codeupdate.Dispatcher{
			Ghost:         deps.Ghost,
			Store:         deps.CodeUpdate,
			SQS:           deps.SQS,
			EmailQueueURL: cfg.EmailQueueURL,
			OwnerEmail:    os.Getenv("OWNER_EMAIL"),
			Log:           logger,
		}
		if deps.CodeUpdateQueueURL == "" {
			logger.Warn("code_update_start disabled (CODE_UPDATE_QUEUE_URL not set)")
		}
	} else {
		logger.Warn("voice code updates disabled (GHOST_COMMAND_FUNCTION_ARN not set)")
	}
	deps.CodeApprovals = codeapproval.NewService(codeapproval.NewDynamoStoreWithClient(ddbClient, cfg.TableName), deps.Ghost)
	if deps.TelemetryStreamName == "" {
		logger.Warn("telemetry lake disabled (TELEMETRY_FIREHOSE_STREAM_NAME not set)")
	}
	// M17: RcaQueue is created unconditionally by template.yaml (only the
	// analyzer function is gated on RcaEnable), so an empty RCA_QUEUE_URL here
	// means the env var was dropped from the template, not that RCA is off. Warn
	// loudly — the symptom otherwise is silence: tools keep working and no tool
	// failure is ever analysed.
	if deps.SQSRcaURL == "" {
		logger.Warn("tool-failure RCA enqueue disabled (RCA_QUEUE_URL not set)")
	}

	// M9 deliverables service: only wired when the dedicated bucket is
	// configured (DELIVERABLES_BUCKET + ZIPPER_FUNCTION_NAME env, set by
	// template.yaml). Absent config degrades cleanly: deliverables routes
	// answer 503 and the deliverable_* tools report not_configured.
	if bucket := os.Getenv("DELIVERABLES_BUCKET"); bucket != "" {
		svc, err := deliv.New(deliv.Config{
			S3:           s3Client,
			Presign:      s3.NewPresignClient(s3Client),
			Lambda:       lambdaClient,
			Store:        st,
			Bucket:       bucket,
			ZipperFn:     os.Getenv("ZIPPER_FUNCTION_NAME"),
			EnqueueEmail: deps.EnqueueEmail,
			Log:          logger,
		})
		if err != nil {
			return nil, err
		}
		deps.Deliv = svc
	} else {
		logger.Warn("deliverables store disabled (DELIVERABLES_BUCKET not set)")
	}

	return deps, nil
}
