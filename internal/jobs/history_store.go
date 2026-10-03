package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

type fileSnapshot struct {
	FormatVersion int                     `json:"formatVersion"`
	Records       map[string]Record       `json:"records"`
	History       map[string]HistoryEntry `json:"history"`
}

func (s *FileStore) ListHistory(ctx context.Context, uid, id string, q HistoryQuery) (HistoryPage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.check(ctx); e != nil {
		return emptyHistoryPage(), e
	}
	anchor, e := validateHistoryQuery(uid, id, q)
	if e != nil {
		return emptyHistoryPage(), e
	}
	if _, exists := s.records[recordKey(uid, id)]; !exists {
		return emptyHistoryPage(), ErrNotFound
	}
	entries := []HistoryEntry{}
	prefix := recordKey(uid, historyPrefix(id))
	for k, entry := range s.history {
		if !strings.HasPrefix(k, prefix) || q.Kind != "" && entry.Kind != q.Kind {
			continue
		}
		sk := historyKey(entry)
		if q.Cursor != "" && sk >= anchor || q.After != "" && sk <= anchor {
			continue
		}
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool {
		if q.After != "" {
			return entries[i].Sequence < entries[j].Sequence
		}
		return entries[i].Sequence > entries[j].Sequence
	})
	limit := pageLimit(q.Limit)
	more := len(entries) > limit
	if more {
		entries = entries[:limit]
	}
	// Copy pointer-bearing snapshots: callers cannot modify persisted history.
	b, _ := json.Marshal(entries)
	var cloned []HistoryEntry
	_ = json.Unmarshal(b, &cloned)
	return historyPage(uid, cloned, q, more), nil
}

func (s *DynamoStore) historyWrites(ctx context.Context, r *Record, expected int64) ([]types.TransactWriteItem, error) {
	var before *Record
	if expected > 0 {
		var e error
		before, e = s.Get(ctx, r.UserID, r.Job.ID)
		if errors.Is(e, ErrNotFound) {
			return nil, ErrConflict
		}
		if e != nil {
			return nil, e
		}
		if before.Job.Version != expected {
			return nil, ErrConflict
		}
	}
	entries, e := buildHistory(before, r)
	if e != nil {
		return nil, e
	}
	writes := make([]types.TransactWriteItem, 0, len(entries))
	for _, entry := range entries {
		b, e := json.Marshal(entry)
		if e != nil {
			return nil, e
		}
		writes = append(writes, types.TransactWriteItem{Put: &types.Put{TableName: aws.String(s.table), Item: map[string]types.AttributeValue{"pk": avs("USER#" + r.UserID), "sk": avs(historyKey(entry)), "payload": avs(string(b)), "kind": avs(entry.Kind)}, ConditionExpression: aws.String("attribute_not_exists(pk)")}})
	}
	return writes, nil
}

func (s *DynamoStore) ListHistory(ctx context.Context, uid, id string, q HistoryQuery) (HistoryPage, error) {
	anchor, e := validateHistoryQuery(uid, id, q)
	if e != nil {
		return emptyHistoryPage(), e
	}
	limit := pageLimit(q.Limit)
	prefix := historyPrefix(id)
	in := &dynamodb.QueryInput{TableName: aws.String(s.table), ConsistentRead: aws.Bool(true), KeyConditionExpression: aws.String("pk = :pk AND begins_with(sk, :prefix)"), ExpressionAttributeValues: map[string]types.AttributeValue{":pk": avs("USER#" + uid), ":prefix": avs(prefix)}, ScanIndexForward: aws.Bool(q.After != ""), Limit: aws.Int32(int32(limit + 1))}
	if q.Cursor != "" {
		in.ExclusiveStartKey = map[string]types.AttributeValue{"pk": avs("USER#" + uid), "sk": avs(anchor)}
	}
	if q.After != "" {
		in.KeyConditionExpression = aws.String("pk = :pk AND sk BETWEEN :lo AND :hi")
		delete(in.ExpressionAttributeValues, ":prefix")
		in.ExpressionAttributeValues[":lo"] = avs(anchor + "!")
		in.ExpressionAttributeValues[":hi"] = avs(prefix + "~")
	}
	if q.Kind != "" {
		in.FilterExpression = aws.String("#kind = :kind")
		in.ExpressionAttributeNames = map[string]string{"#kind": "kind"}
		in.ExpressionAttributeValues[":kind"] = avs(q.Kind)
	}
	entries := []HistoryEntry{}
	var last map[string]types.AttributeValue
	for page := 0; page < 10; page++ {
		out, e := s.client.Query(ctx, in)
		if e != nil {
			return emptyHistoryPage(), e
		}
		for _, item := range out.Items {
			raw, ok := item["payload"].(*types.AttributeValueMemberS)
			if !ok {
				return emptyHistoryPage(), ErrCorrupt
			}
			var entry HistoryEntry
			if json.Unmarshal([]byte(raw.Value), &entry) != nil || entry.JobID != id || entry.ID == "" || entry.Sequence < 1 || attributeString(item, "pk") != "USER#"+uid || attributeString(item, "sk") != historyKey(entry) {
				return emptyHistoryPage(), ErrCorrupt
			}
			entries = append(entries, entry)
		}
		last = out.LastEvaluatedKey
		if len(entries) > limit || len(last) == 0 {
			break
		}
		in.ExclusiveStartKey = last
		in.Limit = aws.Int32(int32(limit + 1 - len(entries)))
	}
	more := len(entries) > limit || len(last) > 0
	if len(entries) > limit {
		entries = entries[:limit]
	}
	p := historyPage(uid, entries, q, more)
	// Sparse filtered pages still advance a scoped cursor through the last key.
	if len(entries) == 0 && len(last) > 0 {
		sk := attributeString(last, "sk")
		dummy := HistoryEntry{JobID: id}
		if attributeString(last, "pk") != "USER#"+uid || !strings.HasPrefix(sk, prefix) || len(sk) != len(prefix)+20 {
			return emptyHistoryPage(), ErrCorrupt
		}
		sequence, err := strconv.ParseInt(strings.TrimPrefix(sk, prefix), 10, 64)
		if err != nil || sequence < 1 {
			return emptyHistoryPage(), ErrCorrupt
		}
		dummy.Sequence = sequence
		if q.After != "" {
			p.NewerCursor = encodeHistoryCursor(uid, dummy, q.Kind)
		} else {
			p.OlderCursor = encodeHistoryCursor(uid, dummy, q.Kind)
		}
	}
	return p, nil
}
