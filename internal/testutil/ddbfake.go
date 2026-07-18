// Package testutil holds shared test fakes for the live-ninja test suite.
//
// FakeDynamo is an ephemeral in-memory DynamoDB implementing exactly the
// call surface the production code uses (PutItem / GetItem / Query /
// UpdateItem / DeleteItem / TransactWriteItems) with just enough of the
// expression grammar those callers actually emit:
//
//	Condition:      attribute_exists(x) | attribute_not_exists(x) |
//	                a = :v | a > :v (AND-joined), or a single top-level
//	                OR of those clauses (used by settings optimistic
//	                concurrency: attribute_not_exists(pk) OR version = :v)
//	Update:         SET a = :v, b = :w [ADD c :n]
//	Key condition:  pk = :pk [AND sk = :sk | AND begins_with(sk, :pfx) |
//	                AND sk BETWEEN :lo AND :hi]
//	Filter:         a = :v (AND-joined)
//	Pagination:     ExclusiveStartKey / LastEvaluatedKey on the sort attr
//
// It is deliberately NOT a general DynamoDB emulator — an unsupported
// expression fails the test loudly instead of silently passing.
package testutil

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// FakeDynamo is a concurrency-safe in-memory single-table fake. The zero
// value is not usable — construct with NewFakeDynamo.
type FakeDynamo struct {
	mu    sync.Mutex
	items map[string]map[string]types.AttributeValue
}

// NewFakeDynamo returns an empty in-memory table.
func NewFakeDynamo() *FakeDynamo {
	return &FakeDynamo{items: make(map[string]map[string]types.AttributeValue)}
}

// SeedItem inserts an item directly (no conditions), for test arrangement.
// The item must carry S-typed "pk" and "sk" attributes.
func (f *FakeDynamo) SeedItem(item map[string]types.AttributeValue) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items[itemKey(item)] = copyItem(item)
}

// RawItem returns a copy of the stored item at pk/sk, or nil.
func (f *FakeDynamo) RawItem(pk, sk string) map[string]types.AttributeValue {
	f.mu.Lock()
	defer f.mu.Unlock()
	it, ok := f.items[pk+"|"+sk]
	if !ok {
		return nil
	}
	return copyItem(it)
}

// Len reports how many items the table holds.
func (f *FakeDynamo) Len() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.items)
}

func itemKey(item map[string]types.AttributeValue) string {
	return sVal(item["pk"]) + "|" + sVal(item["sk"])
}

func keyOfKey(key map[string]types.AttributeValue) string {
	return sVal(key["pk"]) + "|" + sVal(key["sk"])
}

func sVal(av types.AttributeValue) string {
	if s, ok := av.(*types.AttributeValueMemberS); ok {
		return s.Value
	}
	return ""
}

func copyItem(item map[string]types.AttributeValue) map[string]types.AttributeValue {
	out := make(map[string]types.AttributeValue, len(item))
	for k, v := range item {
		out[k] = v
	}
	return out
}

// ---- expression helpers ----

func resolveName(name string, names map[string]string) string {
	name = strings.TrimSpace(name)
	if strings.HasPrefix(name, "#") {
		if resolved, ok := names[name]; ok {
			return resolved
		}
		panic(fmt.Sprintf("testutil: unresolved expression name %q", name))
	}
	return name
}

func avEqual(a, b types.AttributeValue) bool {
	switch x := a.(type) {
	case *types.AttributeValueMemberS:
		y, ok := b.(*types.AttributeValueMemberS)
		return ok && x.Value == y.Value
	case *types.AttributeValueMemberN:
		y, ok := b.(*types.AttributeValueMemberN)
		if !ok {
			return false
		}
		xf, xe := strconv.ParseFloat(x.Value, 64)
		yf, ye := strconv.ParseFloat(y.Value, 64)
		return xe == nil && ye == nil && xf == yf
	case *types.AttributeValueMemberBOOL:
		y, ok := b.(*types.AttributeValueMemberBOOL)
		return ok && x.Value == y.Value
	}
	return false
}

func avNumber(av types.AttributeValue) (float64, bool) {
	n, ok := av.(*types.AttributeValueMemberN)
	if !ok {
		return 0, false
	}
	f, err := strconv.ParseFloat(n.Value, 64)
	return f, err == nil
}

// evalCondition evaluates an AND-joined condition expression against an
// existing item (nil means "no item at that key"). A single top-level
// " OR " (no mixing with AND — panics if both appear, like every other
// unsupported shape) succeeds when any branch does.
func evalCondition(expr string, item map[string]types.AttributeValue,
	names map[string]string, values map[string]types.AttributeValue) bool {
	if strings.Contains(expr, " OR ") {
		if strings.Contains(expr, " AND ") {
			panic(fmt.Sprintf("testutil: unsupported mixed AND/OR condition %q", expr))
		}
		for _, branch := range strings.Split(expr, " OR ") {
			if evalCondition(strings.TrimSpace(branch), item, names, values) {
				return true
			}
		}
		return false
	}
	for _, clause := range strings.Split(expr, " AND ") {
		clause = strings.TrimSpace(clause)
		switch {
		case strings.HasPrefix(clause, "attribute_not_exists(") && strings.HasSuffix(clause, ")"):
			attr := resolveName(clause[len("attribute_not_exists("):len(clause)-1], names)
			if item != nil {
				if _, present := item[attr]; present {
					return false
				}
			}
		case strings.HasPrefix(clause, "attribute_exists(") && strings.HasSuffix(clause, ")"):
			attr := resolveName(clause[len("attribute_exists("):len(clause)-1], names)
			if item == nil {
				return false
			}
			if _, present := item[attr]; !present {
				return false
			}
		case strings.Contains(clause, " = "):
			parts := strings.SplitN(clause, " = ", 2)
			attr := resolveName(parts[0], names)
			want, ok := values[strings.TrimSpace(parts[1])]
			if !ok {
				panic(fmt.Sprintf("testutil: unresolved expression value in clause %q", clause))
			}
			if item == nil {
				return false
			}
			have, present := item[attr]
			if !present || !avEqual(have, want) {
				return false
			}
		case strings.Contains(clause, " > "):
			parts := strings.SplitN(clause, " > ", 2)
			attr := resolveName(parts[0], names)
			want, ok := values[strings.TrimSpace(parts[1])]
			if !ok {
				panic(fmt.Sprintf("testutil: unresolved expression value in clause %q", clause))
			}
			if item == nil {
				return false
			}
			haveF, ok1 := avNumber(item[attr])
			wantF, ok2 := avNumber(want)
			if !ok1 || !ok2 || !(haveF > wantF) {
				return false
			}
		default:
			panic(fmt.Sprintf("testutil: unsupported condition clause %q", clause))
		}
	}
	return true
}

// applyUpdate applies "SET a = :v, b = :w [ADD c :n]" to item in place.
func applyUpdate(expr string, item map[string]types.AttributeValue,
	names map[string]string, values map[string]types.AttributeValue) {
	expr = strings.TrimSpace(expr)

	setPart, addPart := "", ""
	if idx := strings.Index(expr, "ADD "); idx >= 0 {
		setPart = strings.TrimSpace(expr[:idx])
		addPart = strings.TrimSpace(expr[idx+len("ADD "):])
	} else {
		setPart = expr
	}

	if setPart != "" {
		if !strings.HasPrefix(setPart, "SET ") {
			panic(fmt.Sprintf("testutil: unsupported update expression %q", expr))
		}
		for _, clause := range strings.Split(setPart[len("SET "):], ",") {
			parts := strings.SplitN(clause, "=", 2)
			if len(parts) != 2 {
				panic(fmt.Sprintf("testutil: unsupported SET clause %q", clause))
			}
			attr := resolveName(parts[0], names)
			val, ok := values[strings.TrimSpace(parts[1])]
			if !ok {
				panic(fmt.Sprintf("testutil: unresolved value in SET clause %q", clause))
			}
			item[attr] = val
		}
	}

	if addPart != "" {
		fields := strings.Fields(addPart)
		if len(fields) != 2 {
			panic(fmt.Sprintf("testutil: unsupported ADD clause %q", addPart))
		}
		attr := resolveName(fields[0], names)
		val, ok := values[fields[1]]
		if !ok {
			panic(fmt.Sprintf("testutil: unresolved value in ADD clause %q", addPart))
		}
		delta, ok := avNumber(val)
		if !ok {
			panic(fmt.Sprintf("testutil: non-numeric ADD value in %q", addPart))
		}
		current := 0.0
		if existing, present := item[attr]; present {
			if f, isNum := avNumber(existing); isNum {
				current = f
			}
		}
		item[attr] = &types.AttributeValueMemberN{
			Value: strconv.FormatFloat(current+delta, 'f', -1, 64),
		}
	}
}

func condFailedErr() error {
	return &types.ConditionalCheckFailedException{Message: aws.String("The conditional request failed")}
}

// ---- ddb call surface ----

// PutItem implements the DynamoDB PutItem call (full-item replace with an
// optional condition against the existing item at that key).
func (f *FakeDynamo) PutItem(ctx context.Context, params *dynamodb.PutItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	key := itemKey(params.Item)
	existing := f.items[key]
	if params.ConditionExpression != nil {
		if !evalCondition(*params.ConditionExpression, existing,
			params.ExpressionAttributeNames, params.ExpressionAttributeValues) {
			return nil, condFailedErr()
		}
	}
	f.items[key] = copyItem(params.Item)
	return &dynamodb.PutItemOutput{}, nil
}

// GetItem implements the DynamoDB GetItem call.
func (f *FakeDynamo) GetItem(ctx context.Context, params *dynamodb.GetItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	it, ok := f.items[keyOfKey(params.Key)]
	if !ok {
		return &dynamodb.GetItemOutput{}, nil
	}
	return &dynamodb.GetItemOutput{Item: copyItem(it)}, nil
}

// DeleteItem implements the DynamoDB DeleteItem call (with ALL_OLD).
func (f *FakeDynamo) DeleteItem(ctx context.Context, params *dynamodb.DeleteItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	key := keyOfKey(params.Key)
	existing, ok := f.items[key]
	delete(f.items, key)

	out := &dynamodb.DeleteItemOutput{}
	if ok && params.ReturnValues == types.ReturnValueAllOld {
		out.Attributes = copyItem(existing)
	}
	return out, nil
}

// UpdateItem implements the DynamoDB UpdateItem call. Like real DynamoDB,
// an update on an absent key creates the item (unless a condition forbids
// it).
func (f *FakeDynamo) UpdateItem(ctx context.Context, params *dynamodb.UpdateItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	key := keyOfKey(params.Key)
	existing := f.items[key]
	if params.ConditionExpression != nil {
		if !evalCondition(*params.ConditionExpression, existing,
			params.ExpressionAttributeNames, params.ExpressionAttributeValues) {
			return nil, condFailedErr()
		}
	}

	var item map[string]types.AttributeValue
	if existing != nil {
		item = copyItem(existing)
	} else {
		item = copyItem(params.Key)
	}
	if params.UpdateExpression != nil {
		applyUpdate(*params.UpdateExpression, item,
			params.ExpressionAttributeNames, params.ExpressionAttributeValues)
	}
	f.items[key] = item
	return &dynamodb.UpdateItemOutput{}, nil
}

// TransactWriteItems implements the (Update-only) transactional surface
// RotateRefresh uses: all conditions checked first, then all updates
// applied; any failed condition cancels the whole transaction with a
// ConditionalCheckFailed cancellation reason.
func (f *FakeDynamo) TransactWriteItems(ctx context.Context, params *dynamodb.TransactWriteItemsInput, optFns ...func(*dynamodb.Options)) (*dynamodb.TransactWriteItemsOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	reasons := make([]types.CancellationReason, len(params.TransactItems))
	failed := false
	for i, ti := range params.TransactItems {
		if ti.Update == nil {
			panic("testutil: only Update transact items are supported")
		}
		reasons[i] = types.CancellationReason{Code: aws.String("None")}
		existing := f.items[keyOfKey(ti.Update.Key)]
		if ti.Update.ConditionExpression != nil {
			if !evalCondition(*ti.Update.ConditionExpression, existing,
				ti.Update.ExpressionAttributeNames, ti.Update.ExpressionAttributeValues) {
				reasons[i] = types.CancellationReason{
					Code:    aws.String("ConditionalCheckFailed"),
					Message: aws.String("The conditional request failed"),
				}
				failed = true
			}
		}
	}
	if failed {
		return nil, &types.TransactionCanceledException{
			Message:             aws.String("Transaction cancelled"),
			CancellationReasons: reasons,
		}
	}

	for _, ti := range params.TransactItems {
		key := keyOfKey(ti.Update.Key)
		existing := f.items[key]
		var item map[string]types.AttributeValue
		if existing != nil {
			item = copyItem(existing)
		} else {
			item = copyItem(ti.Update.Key)
		}
		applyUpdate(*ti.Update.UpdateExpression, item,
			ti.Update.ExpressionAttributeNames, ti.Update.ExpressionAttributeValues)
		f.items[key] = item
	}
	return &dynamodb.TransactWriteItemsOutput{}, nil
}

// Query implements the single-partition Query patterns the store uses
// (base table pk [+ sk equality or begins_with]; GSI equality on the
// index keys) plus AND-joined equality FilterExpressions, sorting by the
// relevant range attribute and honoring Limit/ScanIndexForward.
func (f *FakeDynamo) Query(ctx context.Context, params *dynamodb.QueryInput, optFns ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	type cond struct {
		attr      string
		val       types.AttributeValue
		beginWith bool
		between   bool
		lo, hi    string
	}
	// Split on " AND ", then re-join the "x BETWEEN :lo AND :hi" triple the
	// split tore apart (BETWEEN's operand separator is also AND).
	rawClauses := strings.Split(*params.KeyConditionExpression, " AND ")
	var clauses []string
	for i := 0; i < len(rawClauses); i++ {
		clause := strings.TrimSpace(rawClauses[i])
		if strings.Contains(clause, " BETWEEN ") && i+1 < len(rawClauses) {
			clause = clause + " AND " + strings.TrimSpace(rawClauses[i+1])
			i++
		}
		clauses = append(clauses, clause)
	}
	var conds []cond
	for _, clause := range clauses {
		switch {
		case strings.Contains(clause, " BETWEEN "):
			parts := strings.SplitN(clause, " BETWEEN ", 2)
			attr := resolveName(parts[0], params.ExpressionAttributeNames)
			bounds := strings.SplitN(parts[1], " AND ", 2)
			if len(bounds) != 2 {
				panic(fmt.Sprintf("testutil: unsupported BETWEEN clause %q", clause))
			}
			loAV, okLo := params.ExpressionAttributeValues[strings.TrimSpace(bounds[0])]
			hiAV, okHi := params.ExpressionAttributeValues[strings.TrimSpace(bounds[1])]
			if !okLo || !okHi {
				panic(fmt.Sprintf("testutil: unresolved value in %q", clause))
			}
			conds = append(conds, cond{attr: attr, between: true, lo: sVal(loAV), hi: sVal(hiAV)})
		case strings.HasPrefix(clause, "begins_with(") && strings.HasSuffix(clause, ")"):
			inner := clause[len("begins_with(") : len(clause)-1]
			parts := strings.SplitN(inner, ",", 2)
			attr := resolveName(parts[0], params.ExpressionAttributeNames)
			val, ok := params.ExpressionAttributeValues[strings.TrimSpace(parts[1])]
			if !ok {
				panic(fmt.Sprintf("testutil: unresolved value in %q", clause))
			}
			conds = append(conds, cond{attr: attr, val: val, beginWith: true})
		case strings.Contains(clause, " = "):
			parts := strings.SplitN(clause, " = ", 2)
			attr := resolveName(parts[0], params.ExpressionAttributeNames)
			val, ok := params.ExpressionAttributeValues[strings.TrimSpace(parts[1])]
			if !ok {
				panic(fmt.Sprintf("testutil: unresolved value in %q", clause))
			}
			conds = append(conds, cond{attr: attr, val: val})
		default:
			panic(fmt.Sprintf("testutil: unsupported key condition clause %q", clause))
		}
	}

	var matched []map[string]types.AttributeValue
	for _, item := range f.items {
		ok := true
		for _, c := range conds {
			have, present := item[c.attr]
			if !present {
				ok = false
				break
			}
			switch {
			case c.between:
				v := sVal(have)
				if v < c.lo || v > c.hi {
					ok = false
				}
			case c.beginWith:
				hs, isS := have.(*types.AttributeValueMemberS)
				ps, isPS := c.val.(*types.AttributeValueMemberS)
				if !isS || !isPS || !strings.HasPrefix(hs.Value, ps.Value) {
					ok = false
				}
			default:
				if !avEqual(have, c.val) {
					ok = false
				}
			}
			if !ok {
				break
			}
		}
		if !ok {
			continue
		}
		if params.FilterExpression != nil {
			if !evalCondition(*params.FilterExpression, item,
				params.ExpressionAttributeNames, params.ExpressionAttributeValues) {
				continue
			}
		}
		matched = append(matched, copyItem(item))
	}

	sortAttr := "sk"
	if params.IndexName != nil {
		switch *params.IndexName {
		case "GSI1":
			sortAttr = "gsi1sk"
		case "GSI2":
			sortAttr = "gsi2sk"
		}
	}
	sort.Slice(matched, func(i, j int) bool {
		return sVal(matched[i][sortAttr]) < sVal(matched[j][sortAttr])
	})
	if params.ScanIndexForward != nil && !*params.ScanIndexForward {
		for i, j := 0, len(matched)-1; i < j; i, j = i+1, j-1 {
			matched[i], matched[j] = matched[j], matched[i]
		}
	}
	// Pagination: ExclusiveStartKey positions strictly past the given sort
	// value (in the active scan direction), and LastEvaluatedKey is
	// returned whenever Limit truncated the result — the two halves of the
	// cursor round-trip ListDeliverables and friends depend on.
	if len(params.ExclusiveStartKey) > 0 {
		start := sVal(params.ExclusiveStartKey[sortAttr])
		forward := params.ScanIndexForward == nil || *params.ScanIndexForward
		kept := matched[:0]
		for _, it := range matched {
			v := sVal(it[sortAttr])
			if (forward && v > start) || (!forward && v < start) {
				kept = append(kept, it)
			}
		}
		matched = kept
	}
	var lastKey map[string]types.AttributeValue
	if params.Limit != nil && len(matched) > int(*params.Limit) {
		matched = matched[:int(*params.Limit)]
		last := matched[len(matched)-1]
		lastKey = map[string]types.AttributeValue{"pk": last["pk"], "sk": last["sk"]}
	}
	return &dynamodb.QueryOutput{Items: matched, Count: int32(len(matched)), LastEvaluatedKey: lastKey}, nil
}
