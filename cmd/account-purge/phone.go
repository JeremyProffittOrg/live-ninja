package main

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
)

type phoneAccountLambda interface {
	Invoke(context.Context, *lambda.InvokeInput, ...func(*lambda.Options)) (*lambda.InvokeOutput, error)
}

func (p *Purger) purgePhoneAccount(ctx context.Context, owner string) error {
	if p.PhoneClient == nil || p.PhoneFunction == "" {
		return errors.New("account-purge: phone lifecycle is not configured")
	}
	ctx, cancel := context.WithTimeout(ctx, 240*time.Second)
	defer cancel()
	payload, _ := json.Marshal(map[string]string{"operation": "owner_account_purge", "ownerUserId": owner})
	waits := 0
	for page := 0; page < 1000; page++ {
		if ctx.Err() != nil {
			return errors.New("account-purge: phone cleanup deadline; retry required")
		}
		out, err := p.PhoneClient.Invoke(ctx, &lambda.InvokeInput{FunctionName: aws.String(p.PhoneFunction), Payload: payload})
		if err != nil || out == nil || out.FunctionError != nil {
			return errors.New("account-purge: phone cleanup invocation failed")
		}
		var envelope struct {
			StatusCode int    `json:"statusCode"`
			Body       string `json:"body"`
		}
		var result struct {
			Complete   *bool `json:"complete"`
			RetryAfter int   `json:"retryAfterSeconds"`
		}
		if json.Unmarshal(out.Payload, &envelope) != nil || json.Unmarshal([]byte(envelope.Body), &result) != nil {
			return errors.New("account-purge: invalid phone cleanup response")
		}
		if envelope.StatusCode == 503 && result.RetryAfter > 0 && result.RetryAfter <= 120 && waits < 2 {
			waits++
			p.Sleep(time.Duration(result.RetryAfter) * time.Second)
			continue
		}
		if envelope.StatusCode != 200 || result.Complete == nil {
			return errors.New("account-purge: phone cleanup incomplete; retry required")
		}
		if *result.Complete {
			return nil
		}
	}
	return errors.New("account-purge: phone cleanup page limit; retry required")
}
