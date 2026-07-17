// Command usage-rollup runs hourly off an EventBridge rate(1 hour)
// schedule (see template.yaml).
//
// M0 real behavior (per plan.md): Query today's USAGE#<day> partition
// (empty until M2 starts writing per-session usage records into it) and
// emit an EMF metric UsageRollupRuns proving the schedule fires. Never a
// table Scan.
package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"

	"github.com/JeremyProffittOrg/live-ninja/internal/config"
	"github.com/JeremyProffittOrg/live-ninja/internal/observ"
	"github.com/JeremyProffittOrg/live-ninja/internal/store"
)

var (
	logger = observ.NewLogger(os.Stdout, config.FromEnv().LogLevel)
	st     *store.Store
)

func handler(ctx context.Context, _ events.CloudWatchEvent) error {
	l := observ.WithRequest(logger, "", "", "system")

	day := time.Now().UTC().Format("2006-01-02")
	items, err := st.QueryUsageToday(ctx, day)
	if err != nil {
		l.Error("usage-rollup: query failed", slog.String("error", err.Error()), slog.String("day", day))
		return err
	}

	l.Info("usage-rollup: run complete", slog.String("day", day), slog.Int("itemCount", len(items)))
	observ.EmitMetric("LiveNinja/UsageRollup", "UsageRollupRuns", 1, "Count",
		map[string]string{"Day": day})
	return nil
}

func main() {
	ctx := context.Background()
	cfg := config.FromEnv()

	s, err := store.New(ctx, cfg.TableName)
	if err != nil {
		logger.Error("usage-rollup: store init failed", slog.String("error", err.Error()))
		os.Exit(1)
	}
	st = s

	lambda.Start(handler)
}
