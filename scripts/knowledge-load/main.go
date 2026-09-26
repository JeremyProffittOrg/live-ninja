// Command knowledge-load loads a folder of curated markdown documents into
// the deployed AgentCore Memory as long-term records under the owner's
// knowledge namespace, /knowledge/<actor>/<collection>/ (see
// agentmemory.KnowledgeNamespace). knowledge_search retrieves them by meaning.
//
// Each *.md file is one document: line 1 is its title, and every "## "
// section becomes one or more records, each prefixed with the title and the
// section heading so a record retrieved alone still says what it is about.
// Records are written directly with BatchCreateMemoryRecords, so no event is
// billed and no extraction model rewrites them.
//
//	AGENTCORE_MEMORY_ID=live_ninja_memory-xxxx go run ./scripts/knowledge-load \
//	    -user <userId> -collection tahoe-2027 -dir "C:\path\to\notes" [-replace] [-dry-run] [-limit 1]
//
// -replace deletes the collection's existing records first, so a reload
// never leaves duplicates. -query "..." skips loading and runs one retrieval
// against the knowledge namespace. Exit 1 on any API error or failed record.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockagentcore"
	"github.com/aws/aws-sdk-go-v2/service/bedrockagentcore/types"

	"github.com/JeremyProffittOrg/live-ninja/internal/agentmemory"
)

// maxRecordRunes bounds one record's text. Sections longer than this split
// on paragraph boundaries, each part keeping the title and heading prefix.
const maxRecordRunes = 1800

const batchSize = 25

type record struct {
	id   string
	text string
}

func main() {
	memoryID := flag.String("memory", os.Getenv("AGENTCORE_MEMORY_ID"), "memory id (default AGENTCORE_MEMORY_ID)")
	userID := flag.String("user", "", "Live Ninja user id that owns the knowledge (required)")
	collection := flag.String("collection", "", "collection name, [a-z0-9-] (required unless -query)")
	dir := flag.String("dir", "", "folder of *.md documents to load")
	replace := flag.Bool("replace", false, "delete the collection's existing records before loading")
	dryRun := flag.Bool("dry-run", false, "print the records that would be written and exit")
	limit := flag.Int("limit", 0, "write at most this many records (0 = all)")
	query := flag.String("query", "", "run one knowledge retrieval and exit")
	flag.Parse()

	if *memoryID == "" || *userID == "" {
		fail("-memory (or AGENTCORE_MEMORY_ID) and -user are required")
	}
	ctx := context.Background()
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion("us-east-1"))
	if err != nil {
		fail("aws config: %v", err)
	}
	client := bedrockagentcore.NewFromConfig(cfg)
	base := agentmemory.KnowledgeNamespace(*userID)

	if *query != "" {
		out, err := client.RetrieveMemoryRecords(ctx, &bedrockagentcore.RetrieveMemoryRecordsInput{
			MemoryId:       aws.String(*memoryID),
			NamespacePath:  aws.String(base),
			SearchCriteria: &types.SearchCriteria{SearchQuery: aws.String(*query), TopK: aws.Int32(5)},
			MaxResults:     aws.Int32(5),
		})
		if err != nil {
			fail("retrieve: %v", err)
		}
		for _, s := range out.MemoryRecordSummaries {
			t, _ := s.Content.(*types.MemoryContentMemberText)
			txt := ""
			if t != nil {
				txt = t.Value
			}
			fmt.Printf("%.3f  %s\n       %s\n", aws.ToFloat64(s.Score), strings.Join(s.Namespaces, ","), clip(oneLine(txt), 160))
		}
		return
	}

	if !validCollection(*collection) || *dir == "" {
		fail("-collection ([a-z0-9-]) and -dir are required")
	}
	ns := base + *collection + "/"
	recs, err := buildRecords(*dir, *collection)
	if err != nil {
		fail("%v", err)
	}
	if *limit > 0 && len(recs) > *limit {
		recs = recs[:*limit]
	}
	fmt.Printf("namespace %s: %d records from %s\n", ns, len(recs), *dir)
	if *dryRun {
		for _, r := range recs {
			fmt.Printf("--- %s (%d chars)\n%s\n", r.id, utf8.RuneCountInString(r.text), clip(r.text, 300))
		}
		return
	}

	if *replace {
		n, err := deleteNamespace(ctx, client, *memoryID, ns)
		if err != nil {
			fail("replace: %v", err)
		}
		fmt.Printf("deleted %d existing records\n", n)
	}

	now := time.Now().UTC()
	written, failed := 0, 0
	for start := 0; start < len(recs); start += batchSize {
		end := min(start+batchSize, len(recs))
		in := make([]types.MemoryRecordCreateInput, 0, end-start)
		for _, r := range recs[start:end] {
			in = append(in, types.MemoryRecordCreateInput{
				Content:           &types.MemoryContentMemberText{Value: r.text},
				Namespaces:        []string{ns},
				RequestIdentifier: aws.String(r.id),
				Timestamp:         aws.Time(now),
			})
		}
		out, err := client.BatchCreateMemoryRecords(ctx, &bedrockagentcore.BatchCreateMemoryRecordsInput{
			MemoryId:    aws.String(*memoryID),
			Records:     in,
			ClientToken: aws.String(token(ns, recs[start].id, end)),
		})
		if err != nil {
			fail("batch %d-%d: %v", start, end, err)
		}
		written += len(out.SuccessfulRecords)
		for _, f := range out.FailedRecords {
			failed++
			fmt.Printf("FAILED %s: %d %s\n", aws.ToString(f.RequestIdentifier), aws.ToInt32(f.ErrorCode), aws.ToString(f.ErrorMessage))
		}
		fmt.Printf("batch %d-%d: %d written, %d failed\n", start, end, len(out.SuccessfulRecords), len(out.FailedRecords))
	}
	fmt.Printf("done: %d written, %d failed\n", written, failed)
	if failed > 0 {
		os.Exit(1)
	}
}

// buildRecords turns every *.md file in dir into section records.
func buildRecords(dir, collection string) ([]record, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.md"))
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no *.md files in %s", dir)
	}
	sort.Strings(files)
	var out []record
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		base := strings.TrimSuffix(filepath.Base(f), ".md")
		for i, text := range splitDocument(string(b)) {
			out = append(out, record{id: fmt.Sprintf("%s-%s-%03d", collection, base, i), text: text})
		}
	}
	return out, nil
}

// splitDocument returns the record texts for one document: the title line,
// then one record per "## " section (long sections split by paragraph).
// Text before the first section (the vehicle/source line) rides along as
// context on every record.
func splitDocument(doc string) []string {
	doc = strings.ReplaceAll(doc, "\r\n", "\n")
	lines := strings.Split(doc, "\n")
	if len(lines) == 0 {
		return nil
	}
	title := strings.TrimSpace(lines[0])
	var preamble []string
	type section struct {
		heading string
		body    []string
	}
	var secs []section
	for _, l := range lines[1:] {
		if strings.HasPrefix(l, "## ") {
			secs = append(secs, section{heading: strings.TrimSpace(strings.TrimPrefix(l, "## "))})
			continue
		}
		if len(secs) == 0 {
			if strings.TrimSpace(l) != "" {
				preamble = append(preamble, strings.TrimSpace(l))
			}
			continue
		}
		secs[len(secs)-1].body = append(secs[len(secs)-1].body, l)
	}
	context := ""
	if len(preamble) > 0 {
		context = clip(strings.Join(preamble, " "), 300)
	}
	var out []string
	if len(secs) == 0 {
		return []string{strings.TrimSpace(title + "\n" + context)}
	}
	for _, s := range secs {
		prefix := title + " — " + s.heading + "\n"
		if context != "" {
			prefix += context + "\n"
		}
		budget := maxRecordRunes - utf8.RuneCountInString(prefix)
		for _, part := range packParagraphs(strings.TrimSpace(strings.Join(s.body, "\n")), budget) {
			out = append(out, prefix+part)
		}
	}
	return out
}

// packParagraphs groups paragraphs into parts of at most budget runes; a
// single paragraph longer than budget is split on line boundaries, then hard.
func packParagraphs(body string, budget int) []string {
	if body == "" {
		return nil
	}
	var units []string
	for _, p := range strings.Split(body, "\n\n") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if utf8.RuneCountInString(p) <= budget {
			units = append(units, p)
			continue
		}
		for _, l := range strings.Split(p, "\n") {
			for utf8.RuneCountInString(l) > budget {
				r := []rune(l)
				units = append(units, string(r[:budget]))
				l = string(r[budget:])
			}
			if strings.TrimSpace(l) != "" {
				units = append(units, l)
			}
		}
	}
	var parts []string
	cur := ""
	for _, u := range units {
		if cur != "" && utf8.RuneCountInString(cur)+2+utf8.RuneCountInString(u) > budget {
			parts = append(parts, cur)
			cur = ""
		}
		if cur == "" {
			cur = u
		} else {
			cur += "\n\n" + u
		}
	}
	if cur != "" {
		parts = append(parts, cur)
	}
	return parts
}

// deleteNamespace removes every record under ns (exact namespace).
func deleteNamespace(ctx context.Context, client *bedrockagentcore.Client, memoryID, ns string) (int, error) {
	var ids []string
	var next *string
	for {
		out, err := client.ListMemoryRecords(ctx, &bedrockagentcore.ListMemoryRecordsInput{
			MemoryId: aws.String(memoryID), Namespace: aws.String(ns), MaxResults: aws.Int32(100), NextToken: next,
		})
		if err != nil {
			return 0, err
		}
		for _, s := range out.MemoryRecordSummaries {
			ids = append(ids, aws.ToString(s.MemoryRecordId))
		}
		if out.NextToken == nil {
			break
		}
		next = out.NextToken
	}
	deleted := 0
	for start := 0; start < len(ids); start += 100 {
		end := min(start+100, len(ids))
		in := make([]types.MemoryRecordDeleteInput, 0, end-start)
		for _, id := range ids[start:end] {
			in = append(in, types.MemoryRecordDeleteInput{MemoryRecordId: aws.String(id)})
		}
		out, err := client.BatchDeleteMemoryRecords(ctx, &bedrockagentcore.BatchDeleteMemoryRecordsInput{
			MemoryId: aws.String(memoryID), Records: in,
		})
		if err != nil {
			return deleted, err
		}
		deleted += len(out.SuccessfulRecords)
		if len(out.FailedRecords) > 0 {
			return deleted, fmt.Errorf("%d records failed to delete", len(out.FailedRecords))
		}
	}
	return deleted, nil
}

func validCollection(c string) bool {
	if c == "" || len(c) > 64 {
		return false
	}
	for _, r := range c {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

func token(parts ...any) string {
	sum := sha256.Sum256([]byte(fmt.Sprint(parts...)))
	return hex.EncodeToString(sum[:])[:32]
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "knowledge-load: "+format+"\n", a...)
	os.Exit(1)
}
