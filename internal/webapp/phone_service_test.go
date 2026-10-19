package webapp

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/JeremyProffittOrg/live-ninja/internal/tools"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"testing"
)

type phoneLambdaFake struct {
	payload map[string]any
	status  int
	fail    bool
}

func (f *phoneLambdaFake) Invoke(_ context.Context, in *lambda.InvokeInput, _ ...func(*lambda.Options)) (*lambda.InvokeOutput, error) {
	json.Unmarshal(in.Payload, &f.payload)
	if f.fail {
		return nil, errors.New("network")
	}
	body, _ := json.Marshal(map[string]any{"requestId": f.payload["requestId"], "accepted": true, "providerStatus": "queued"})
	result, _ := json.Marshal(map[string]any{"statusCode": f.status, "body": string(body)})
	return &lambda.InvokeOutput{Payload: result}, nil
}
func TestOwnerPhoneAdapterIdentityAndUnknownOutcome(t *testing.T) {
	fake := &phoneLambdaFake{status: 200}
	p := &phoneMessages{client: fake, function: "function"}
	result, e := p.Send(context.Background(), "owner-uuid", "request-1", "primary", "Hello", false, "")
	if e != nil || result["providerStatus"] != "queued" || fake.payload["ownerUserId"] != "owner-uuid" || fake.payload["operation"] != "owner_message_send" {
		t.Fatal(result, e, fake.payload)
	}
	_, e = p.Send(context.Background(), "owner-uuid", "live-request", "primary", "Summary", true, `{"total":12}`)
	if e != nil || fake.payload["live"] != true || fake.payload["payload"] != `{"total":12}` {
		t.Fatal("live payload lost", e)
	}
	fake.fail = true
	_, e = p.Send(context.Background(), "owner-uuid", "request-1", "primary", "Hello", false, "")
	if e == nil || e.Code != tools.CodeUpstreamError || e.Details["requestId"] != "request-1" {
		t.Fatal(e)
	}
	fake.fail = false
	fake.status = 403
	_, e = p.Status(context.Background(), "owner-uuid", "request-1")
	if e == nil || e.Code != tools.CodeForbidden || fake.payload["operation"] != "owner_message_status" {
		t.Fatal(e, fake.payload)
	}
}
