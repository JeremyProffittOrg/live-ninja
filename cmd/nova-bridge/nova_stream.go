package main

import (
	"context"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	brtypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
)

// NovaModelID is the only model Bedrock's bidirectional stream accepts today
// (verified against the pinned SDK's InvokeModelWithBidirectionalStreamInput
// doc comment) and the one the plan confirmed available in us-east-1.
const NovaModelID = "amazon.nova-sonic-v1:0"

// novaStream is the transport the bridge pump talks to: send a Nova protocol
// document, receive the next one, close. Abstracting the concrete Bedrock
// event stream behind this interface is what lets the pump be unit-tested
// with a fake, and keeps every SDK detail in the adapter below.
type novaStream interface {
	// Send writes one Nova input document (a wrapped {"event": {...}}).
	Send(ctx context.Context, payload []byte) error
	// Recv returns the next Nova output document, or io.EOF once the stream
	// is exhausted/closed.
	Recv(ctx context.Context) ([]byte, error)
	// Close tears down the stream.
	Close() error
}

// bedrockClient is the subset of the Bedrock Runtime client the bridge uses;
// it lets main wire the real client and tests skip it entirely.
type bedrockClient interface {
	InvokeModelWithBidirectionalStream(
		ctx context.Context,
		params *bedrockruntime.InvokeModelWithBidirectionalStreamInput,
		optFns ...func(*bedrockruntime.Options),
	) (*bedrockruntime.InvokeModelWithBidirectionalStreamOutput, error)
}

// openNovaStream opens a Bedrock bidirectional stream for Nova Sonic and
// wraps it in the novaStream interface.
func openNovaStream(ctx context.Context, client bedrockClient) (novaStream, error) {
	out, err := client.InvokeModelWithBidirectionalStream(ctx,
		&bedrockruntime.InvokeModelWithBidirectionalStreamInput{
			ModelId: aws.String(NovaModelID),
		})
	if err != nil {
		return nil, err
	}
	es := out.GetStream()
	return &bedrockNovaStream{es: es, events: es.Events()}, nil
}

// bedrockNovaStream adapts the SDK's event stream to novaStream. The SDK
// models the two halves as a writer (Send) and a channel of output events
// (Events); we present a blocking Recv over that channel and surface the
// stream's terminal error when the channel drains.
type bedrockNovaStream struct {
	es     *bedrockruntime.InvokeModelWithBidirectionalStreamEventStream
	events <-chan brtypes.InvokeModelWithBidirectionalStreamOutput
}

func (s *bedrockNovaStream) Send(ctx context.Context, payload []byte) error {
	return s.es.Send(ctx, &brtypes.InvokeModelWithBidirectionalStreamInputMemberChunk{
		Value: brtypes.BidirectionalInputPayloadPart{Bytes: payload},
	})
}

func (s *bedrockNovaStream) Recv(ctx context.Context) ([]byte, error) {
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case evt, ok := <-s.events:
			if !ok {
				if err := s.es.Err(); err != nil {
					return nil, err
				}
				return nil, io.EOF
			}
			chunk, isChunk := evt.(*brtypes.InvokeModelWithBidirectionalStreamOutputMemberChunk)
			if !isChunk {
				// UnknownUnionMember or a future member — skip and keep reading.
				continue
			}
			return chunk.Value.Bytes, nil
		}
	}
}

func (s *bedrockNovaStream) Close() error { return s.es.Close() }
