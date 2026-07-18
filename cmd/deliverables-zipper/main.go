// Command deliverables-zipper builds deliverable ZIP bundles (M9,
// FR-DLV-02). It is invoked asynchronously (InvocationType=Event) by the
// web function's deliverable_zip path (internal/deliv.Service.Zip) with a
// deliv.ZipJob payload, streams the caller's source objects from the
// deliverables bucket through archive/zip into the target object
// (io.Pipe → S3 multipart upload; nothing buffered in memory or /tmp),
// and writes the outcome back onto the pending DELIV# index item
// (status=ready + sizeBytes, or status=failed).
//
// Security: the job re-validates that the target key and every source
// key sit under deliverables/<userId>/ (deliv.ZipJob.Validate) — the
// zipper never reads or writes outside the job user's own prefix, even
// on a forged event.
//
// Env (set in template.yaml by the infra workstream): TABLE_NAME,
// DELIVERABLES_BUCKET. arm64 / provided.al2023, built via the shared
// Makefile pattern.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/aws/aws-lambda-go/lambda"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/JeremyProffittOrg/live-ninja/internal/config"
	"github.com/JeremyProffittOrg/live-ninja/internal/deliv"
	"github.com/JeremyProffittOrg/live-ninja/internal/observ"
	"github.com/JeremyProffittOrg/live-ninja/internal/store"
)

var (
	logger   = observ.NewLogger(os.Stdout, config.FromEnv().LogLevel)
	st       *store.Store
	getter   deliv.GetObjectAPI
	uploader deliv.UploadAPI
	bucket   string
)

// handler runs one zip job. Any error after validation is also recorded
// on the index item as status=failed so the pending row never dangles;
// the returned error additionally lands the async invoke in Lambda's
// failure metrics/destinations.
func handler(ctx context.Context, job deliv.ZipJob) error {
	l := logger.With(
		slog.String("userId", job.UserID),
		slog.String("deliverableId", job.DeliverableID),
		slog.Int("sources", len(job.Sources)),
	)

	if err := job.Validate(); err != nil {
		// A payload that fails validation may still reference a real
		// pending item; try the failed write-back when it's addressable.
		l.Error("zipper: invalid job", slog.String("error", err.Error()))
		if job.UserID != "" && job.SK != "" {
			if uerr := st.UpdateDeliverableStatus(ctx, job.UserID, job.SK, store.DeliverableStatusFailed, 0); uerr != nil {
				l.Error("zipper: failed-status write-back failed", slog.String("error", uerr.Error()))
			}
		}
		return err
	}

	size, err := deliv.RunZipJob(ctx, getter, uploader, bucket, job)
	if err != nil {
		l.Error("zipper: zip build failed", slog.String("error", err.Error()))
		if uerr := st.UpdateDeliverableStatus(ctx, job.UserID, job.SK, store.DeliverableStatusFailed, 0); uerr != nil {
			l.Error("zipper: failed-status write-back failed", slog.String("error", uerr.Error()))
		}
		observ.EmitMetric("LiveNinja/Deliverables", "ZipJobs", 1, "Count", map[string]string{"Outcome": "error"})
		return err
	}

	if err := st.UpdateDeliverableStatus(ctx, job.UserID, job.SK, store.DeliverableStatusReady, size); err != nil {
		l.Error("zipper: ready-status write-back failed", slog.String("error", err.Error()))
		return fmt.Errorf("zip built but status write-back failed: %w", err)
	}

	l.Info("zipper: zip built", slog.Int64("sizeBytes", size), slog.String("key", job.Key))
	observ.EmitMetric("LiveNinja/Deliverables", "ZipJobs", 1, "Count", map[string]string{"Outcome": "ok"})
	return nil
}

func main() {
	ctx := context.Background()
	cfg := config.FromEnv()

	bucket = os.Getenv("DELIVERABLES_BUCKET")
	if bucket == "" {
		logger.Error("zipper: DELIVERABLES_BUCKET is required")
		os.Exit(1)
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		logger.Error("zipper: aws config load failed", slog.String("error", err.Error()))
		os.Exit(1)
	}

	st = store.NewWithClient(dynamodb.NewFromConfig(awsCfg), cfg.TableName)
	s3c := s3.NewFromConfig(awsCfg)
	getter = s3c
	uploader = manager.NewUploader(s3c)

	lambda.Start(handler)
}
