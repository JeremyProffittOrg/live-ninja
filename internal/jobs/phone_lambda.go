package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
)

type PhoneLambda interface {
	Invoke(context.Context, *lambda.InvokeInput, ...func(*lambda.Options)) (*lambda.InvokeOutput, error)
}
type phoneLambdaProvider struct {
	client   PhoneLambda
	function string
}

func NewPhoneResearchProvider(client PhoneLambda, function string) (PhoneResearchProvider, error) {
	if client == nil || !regexp.MustCompile(`^arn:aws:lambda:us-east-1:[0-9]{12}:function:live-ninja-phone-trial-recorder$`).MatchString(function) {
		return nil, errors.New("invalid phone research recorder")
	}
	return &phoneLambdaProvider{client: client, function: function}, nil
}
func (p *phoneLambdaProvider) invoke(ctx context.Context, operation string, value any, out any) error {
	raw, e := json.Marshal(value)
	if e != nil {
		return e
	}
	var fields map[string]any
	if e = json.Unmarshal(raw, &fields); e != nil {
		return e
	}
	fields["operation"] = operation
	raw, e = json.Marshal(fields)
	if e != nil {
		return e
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	result, e := p.client.Invoke(ctx, &lambda.InvokeInput{FunctionName: aws.String(p.function), Payload: raw})
	if e != nil || result == nil || result.FunctionError != nil || len(result.Payload) > 512000 {
		return errors.New("phone research result is unknown; retry the same request")
	}
	var envelope struct {
		StatusCode int    `json:"statusCode"`
		Body       string `json:"body"`
	}
	if json.Unmarshal(result.Payload, &envelope) != nil {
		return ErrCorrupt
	}
	switch envelope.StatusCode {
	case 200:
		if json.Unmarshal([]byte(envelope.Body), out) != nil {
			return ErrCorrupt
		}
		return nil
	case 401, 403:
		return ErrForbidden
	case 404:
		return ErrNotFound
	case 400:
		return ErrValidation
	case 409:
		return ErrConflict
	default:
		return errors.New("phone research is unavailable; no additional call was approved")
	}
}
func (p *phoneLambdaProvider) Catalog(ctx context.Context, uid string) (PhoneCatalog, error) {
	var out PhoneCatalog
	e := p.invoke(ctx, "phone_research_catalog", map[string]string{"ownerUserId": uid}, &out)
	return out, e
}
func (p *phoneLambdaProvider) Advance(ctx context.Context, command PhoneCommand) (PhoneResult, error) {
	var out PhoneResult
	e := p.invoke(ctx, "phone_research_advance", command, &out)
	return out, e
}
func (p *phoneLambdaProvider) Status(ctx context.Context, uid, jobID, runID string) (PhoneResult, error) {
	var out PhoneResult
	e := p.invoke(ctx, "phone_research_status", map[string]string{"ownerUserId": uid, "jobId": jobID, "runId": runID}, &out)
	return out, e
}
