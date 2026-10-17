package webapp

import (
	"context"
	"errors"
	"os"
	"time"
)

func exportPhoneAccount(ctx context.Context, deps *Deps, owner string) ([]map[string]any, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	function := os.Getenv("PHONE_MESSAGES_FUNCTION_ARN")
	if function == "" || deps.PhoneAccount == nil {
		return nil, errors.New("phone account export is not configured")
	}
	service := &phoneMessages{client: deps.PhoneAccount, function: function}
	var records []map[string]any
	cursor := ""
	seen := map[string]bool{}
	for page := 0; page < 100; page++ {
		if ctx.Err() != nil {
			return nil, errors.New("phone account export deadline exceeded")
		}
		result, err := service.invoke(ctx, map[string]any{"operation": "owner_account_export", "ownerUserId": owner, "cursor": cursor})
		if err != nil {
			return nil, errors.New("phone account export is incomplete")
		}
		items, ok := result["items"].([]any)
		complete, completeOK := result["complete"].(bool)
		next, nextOK := result["nextCursor"].(string)
		if !ok || !completeOK || !nextOK || len(items) > 25 || complete != (next == "") {
			return nil, errors.New("invalid phone export page")
		}
		for _, value := range items {
			item, ok := value.(map[string]any)
			if !ok || item["kind"] != "phone_record" {
				return nil, errors.New("invalid phone export record")
			}
			records = append(records, item)
		}
		if complete {
			return records, nil
		}
		if seen[next] || len(next) > 8192 {
			return nil, errors.New("phone export cursor did not advance")
		}
		seen[next] = true
		cursor = next
	}
	return nil, errors.New("phone export exceeds page limit")
}
