// Command agentmemory-smoke proves the deployed AgentCore Memory end to end
// from a developer machine (plan.md agentcore-memory): it checks the memory
// is ACTIVE, writes one exchange for a throwaway actor, waits for AWS's
// built-in strategies to extract a record, retrieves it back through the
// same namespacePath query the broker uses, and purges the actor.
// `-actor <id>` instead probes namespace vs namespacePath semantics against
// an existing actor's records (how the exact-match finding was made) and
// purges that actor unless -keep is set.
//
//	AGENTCORE_MEMORY_ID=live_ninja_memory-xxxx go run ./scripts/agentmemory-smoke -wait 4m
//
// Exit 0 when a record came back, 2 when the wait ran out (the event was
// still written and purged), 1 on any API error. Costs: one event, a few
// retrievals — cents.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockagentcore"
	"github.com/aws/aws-sdk-go-v2/service/bedrockagentcore/types"
	"github.com/aws/aws-sdk-go-v2/service/bedrockagentcorecontrol"

	"github.com/JeremyProffittOrg/live-ninja/internal/agentmemory"
)

func main() {
	memoryID := flag.String("memory", os.Getenv("AGENTCORE_MEMORY_ID"), "memory id (default AGENTCORE_MEMORY_ID)")
	wait := flag.Duration("wait", 4*time.Minute, "how long to wait for extraction")
	keep := flag.Bool("keep", false, "leave the smoke actor's data in place (default: purge)")
	probeActor := flag.String("actor", "", "probe namespace/namespacePath semantics against an existing actor and exit")
	flag.Parse()
	if *memoryID == "" {
		fmt.Fprintln(os.Stderr, "agentmemory-smoke: -memory or AGENTCORE_MEMORY_ID is required")
		os.Exit(1)
	}

	ctx := context.Background()
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion("us-east-1"))
	if err != nil {
		fail("load aws config: %v", err)
	}

	control := bedrockagentcorecontrol.NewFromConfig(cfg)
	mem, err := control.GetMemory(ctx, &bedrockagentcorecontrol.GetMemoryInput{MemoryId: memoryID})
	if err != nil {
		fail("GetMemory: %v", err)
	}
	fmt.Printf("memory %s status=%s expiry=%dd strategies=%d\n", aws.ToString(mem.Memory.Id), mem.Memory.Status,
		aws.ToInt32(mem.Memory.EventExpiryDuration), len(mem.Memory.Strategies))
	for _, st := range mem.Memory.Strategies {
		fmt.Printf("  strategy %s type=%s status=%s namespaces=%v\n", aws.ToString(st.Name), st.Type, st.Status, st.Namespaces)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	svc := agentmemory.New(bedrockagentcore.NewFromConfig(cfg),
		agentmemory.Config{MemoryID: *memoryID, Mode: agentmemory.ModeAll}, logger)

	actor := "smoke-" + time.Now().UTC().Format("20060102-150405")
	if *probeActor != "" {
		probeNamespaces(ctx, cfg, *memoryID, *probeActor)
		if !*keep {
			events, records, perr := svc.PurgeActor(ctx, *probeActor)
			if perr != nil {
				fail("PurgeActor: %v", perr)
			}
			fmt.Printf("purged %s: %d event(s), %d record(s)\n", *probeActor, events, records)
		}
		return
	}
	fmt.Printf("actor %s namespace %s\n", actor, agentmemory.Namespace(actor))

	n, err := svc.RecordExchanges(ctx, actor, "smoke-session", []agentmemory.Turn{
		{Seq: 1, Role: "user", Text: "Quick note so you remember: my sister Sarah lives in Austin, and I always want temperatures in Celsius."},
		{Seq: 2, Role: "assistant", Text: "Got it. Sarah is your sister in Austin, and you prefer Celsius. I will remember both."},
	})
	if err != nil {
		fail("CreateEvent: %v", err)
	}
	fmt.Printf("events written: %d\n", n)

	found := false
	deadline := time.Now().Add(*wait)
	for time.Now().Before(deadline) {
		time.Sleep(15 * time.Second)
		recs, rerr := svc.Retrieve(ctx, actor, "Where does the user's sister live, and which temperature unit do they prefer?", 5)
		if rerr != nil {
			fail("RetrieveMemoryRecords: %v", rerr)
		}
		fmt.Printf("%s retrieve: %d record(s)\n", time.Now().UTC().Format(time.TimeOnly), len(recs))
		for _, r := range recs {
			fmt.Printf("  [%.3f] %s  %q\n", r.Score, r.Namespace, r.Text)
		}
		if len(recs) > 0 {
			found = true
			break
		}
		// Probe the two exact strategy namespaces too, so a prefix that is
		// not honoured shows up as "exact has it, prefix does not".
		raw := bedrockagentcore.NewFromConfig(cfg)
		for _, ns := range []string{agentmemory.Namespace(actor) + "facts/", agentmemory.Namespace(actor) + "preferences/"} {
			out, lerr := raw.ListMemoryRecords(ctx, &bedrockagentcore.ListMemoryRecordsInput{
				MemoryId: memoryID, Namespace: aws.String(ns), MaxResults: aws.Int32(10),
			})
			if lerr != nil {
				fmt.Printf("  exact %s: error %v\n", ns, lerr)
				continue
			}
			if len(out.MemoryRecordSummaries) > 0 {
				fmt.Printf("  exact %s: %d record(s)\n", ns, len(out.MemoryRecordSummaries))
			}
		}
	}

	all, lerr := svc.ListRecords(ctx, actor, 50)
	if lerr != nil {
		fail("ListMemoryRecords: %v", lerr)
	}
	fmt.Printf("list: %d record(s) under the actor namespace\n", len(all))

	if !*keep {
		events, records, perr := svc.PurgeActor(ctx, actor)
		if perr != nil {
			fail("PurgeActor: %v", perr)
		}
		fmt.Printf("purged: %d event(s), %d record(s)\n", events, records)
	}
	if !found {
		fmt.Println("RESULT: no record within the wait; extraction may still be pending")
		os.Exit(2)
	}
	fmt.Println("RESULT: OK — namespacePath retrieval returned an extracted record")
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "agentmemory-smoke: "+format+"\n", args...)
	os.Exit(1)
}

// probeNamespaces compares the ways of scoping a read to one actor, so the
// service's Retrieve/List can be built on what the API actually honours.
func probeNamespaces(ctx context.Context, cfg aws.Config, memoryID, actor string) {
	raw := bedrockagentcore.NewFromConfig(cfg)
	base := agentmemory.Namespace(actor)
	query := aws.String("Where does the user's sister live, and which temperature unit do they prefer?")
	show := func(label string, out *bedrockagentcore.RetrieveMemoryRecordsOutput, err error) {
		if err != nil {
			fmt.Printf("%-45s error: %v\n", label, err)
			return
		}
		fmt.Printf("%-45s %d record(s)\n", label, len(out.MemoryRecordSummaries))
		for _, r := range out.MemoryRecordSummaries {
			if t, ok := r.Content.(*types.MemoryContentMemberText); ok {
				fmt.Printf("    %v %q\n", r.Namespaces, t.Value)
			}
		}
	}
	for _, ns := range []string{base, strings.TrimSuffix(base, "/"), base + "facts/", base + "preferences/"} {
		out, err := raw.RetrieveMemoryRecords(ctx, &bedrockagentcore.RetrieveMemoryRecordsInput{
			MemoryId: aws.String(memoryID), Namespace: aws.String(ns),
			SearchCriteria: &types.SearchCriteria{SearchQuery: query, TopK: aws.Int32(10)}, MaxResults: aws.Int32(10),
		})
		show("retrieve namespace="+ns, out, err)
	}
	for _, ns := range []string{base, strings.TrimSuffix(base, "/")} {
		out, err := raw.RetrieveMemoryRecords(ctx, &bedrockagentcore.RetrieveMemoryRecordsInput{
			MemoryId: aws.String(memoryID), NamespacePath: aws.String(ns),
			SearchCriteria: &types.SearchCriteria{SearchQuery: query, TopK: aws.Int32(10)}, MaxResults: aws.Int32(10),
		})
		show("retrieve namespacePath="+ns, out, err)
		lout, lerr := raw.ListMemoryRecords(ctx, &bedrockagentcore.ListMemoryRecordsInput{
			MemoryId: aws.String(memoryID), NamespacePath: aws.String(ns), MaxResults: aws.Int32(10),
		})
		if lerr != nil {
			fmt.Printf("%-45s error: %v\n", "list namespacePath="+ns, lerr)
		} else {
			fmt.Printf("%-45s %d record(s)\n", "list namespacePath="+ns, len(lout.MemoryRecordSummaries))
		}
	}
}
