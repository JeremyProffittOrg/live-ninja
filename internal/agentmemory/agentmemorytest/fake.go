// Package agentmemorytest is the shared fake AgentCore client for tests in
// the packages that consume internal/agentmemory (webapp, broker, tools).
package agentmemorytest

import (
	"context"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockagentcore"
	"github.com/aws/aws-sdk-go-v2/service/bedrockagentcore/types"
)

// Fake records every call and answers from canned data. Zero value is usable.
type Fake struct {
	mu sync.Mutex

	// Events collects every CreateEvent input; CreateErr fails them all.
	Events    []*bedrockagentcore.CreateEventInput
	CreateErr error

	// Records answers RetrieveMemoryRecords and ListMemoryRecords (one page)
	// and GetMemoryRecord (by id). RetrieveDelay makes retrieval slow so a
	// caller's deadline can be exercised; RetrieveErr fails it.
	Records       []types.MemoryRecordSummary
	RetrieveDelay time.Duration
	RetrieveErr   error
	Retrieves     []*bedrockagentcore.RetrieveMemoryRecordsInput

	// Deleted collects DeleteMemoryRecord + BatchDeleteMemoryRecords ids.
	Deleted []string
}

// Summary builds one record summary for Records.
func Summary(id, namespace, text string, score float64) types.MemoryRecordSummary {
	return types.MemoryRecordSummary{
		MemoryRecordId: aws.String(id),
		Namespaces:     []string{namespace},
		Content:        &types.MemoryContentMemberText{Value: text},
		Score:          aws.Float64(score),
		CreatedAt:      aws.Time(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)),
	}
}

func (f *Fake) CreateEvent(_ context.Context, in *bedrockagentcore.CreateEventInput, _ ...func(*bedrockagentcore.Options)) (*bedrockagentcore.CreateEventOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.CreateErr != nil {
		return nil, f.CreateErr
	}
	f.Events = append(f.Events, in)
	return &bedrockagentcore.CreateEventOutput{}, nil
}

func (f *Fake) RetrieveMemoryRecords(ctx context.Context, in *bedrockagentcore.RetrieveMemoryRecordsInput, _ ...func(*bedrockagentcore.Options)) (*bedrockagentcore.RetrieveMemoryRecordsOutput, error) {
	f.mu.Lock()
	f.Retrieves = append(f.Retrieves, in)
	delay, err, recs := f.RetrieveDelay, f.RetrieveErr, append([]types.MemoryRecordSummary(nil), f.Records...)
	f.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err != nil {
		return nil, err
	}
	return &bedrockagentcore.RetrieveMemoryRecordsOutput{MemoryRecordSummaries: recs}, nil
}

func (f *Fake) ListMemoryRecords(_ context.Context, _ *bedrockagentcore.ListMemoryRecordsInput, _ ...func(*bedrockagentcore.Options)) (*bedrockagentcore.ListMemoryRecordsOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &bedrockagentcore.ListMemoryRecordsOutput{MemoryRecordSummaries: append([]types.MemoryRecordSummary(nil), f.Records...)}, nil
}

func (f *Fake) GetMemoryRecord(_ context.Context, in *bedrockagentcore.GetMemoryRecordInput, _ ...func(*bedrockagentcore.Options)) (*bedrockagentcore.GetMemoryRecordOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.Records {
		if aws.ToString(r.MemoryRecordId) == aws.ToString(in.MemoryRecordId) {
			return &bedrockagentcore.GetMemoryRecordOutput{MemoryRecord: &types.MemoryRecord{
				MemoryRecordId: r.MemoryRecordId, Namespaces: r.Namespaces, Content: r.Content, CreatedAt: r.CreatedAt,
			}}, nil
		}
	}
	return nil, &types.ResourceNotFoundException{Message: aws.String("no such record")}
}

func (f *Fake) DeleteMemoryRecord(_ context.Context, in *bedrockagentcore.DeleteMemoryRecordInput, _ ...func(*bedrockagentcore.Options)) (*bedrockagentcore.DeleteMemoryRecordOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Deleted = append(f.Deleted, aws.ToString(in.MemoryRecordId))
	return &bedrockagentcore.DeleteMemoryRecordOutput{}, nil
}

func (f *Fake) BatchDeleteMemoryRecords(_ context.Context, in *bedrockagentcore.BatchDeleteMemoryRecordsInput, _ ...func(*bedrockagentcore.Options)) (*bedrockagentcore.BatchDeleteMemoryRecordsOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range in.Records {
		f.Deleted = append(f.Deleted, aws.ToString(r.MemoryRecordId))
	}
	return &bedrockagentcore.BatchDeleteMemoryRecordsOutput{}, nil
}

func (f *Fake) ListSessions(_ context.Context, _ *bedrockagentcore.ListSessionsInput, _ ...func(*bedrockagentcore.Options)) (*bedrockagentcore.ListSessionsOutput, error) {
	return &bedrockagentcore.ListSessionsOutput{}, nil
}

func (f *Fake) ListEvents(_ context.Context, _ *bedrockagentcore.ListEventsInput, _ ...func(*bedrockagentcore.Options)) (*bedrockagentcore.ListEventsOutput, error) {
	return &bedrockagentcore.ListEventsOutput{}, nil
}

func (f *Fake) DeleteEvent(_ context.Context, _ *bedrockagentcore.DeleteEventInput, _ ...func(*bedrockagentcore.Options)) (*bedrockagentcore.DeleteEventOutput, error) {
	return &bedrockagentcore.DeleteEventOutput{}, nil
}
