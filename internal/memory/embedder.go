// Package memory is the M10 memory core: semantic entity recall over the
// per-user DynamoDB embedding partition (see internal/store/entities.go
// for the locked item shapes and the S3 Vectors verification note —
// the s3vectors SDK exists, but no vector bucket/index is provisioned in
// this deploy's template delta, so the DynamoDB-native fallback is used).
//
// Embeddings come from Bedrock amazon.titan-embed-text-v2:0 in
// us-east-1 (cheap, keyless — IAM only: the calling function needs
// bedrock:InvokeModel on that one foundation-model ARN).
package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
)

// EmbedModelID is the Bedrock model used for all memory embeddings. The
// EMB item records it per vector so a future model migration can detect
// stale vectors.
const EmbedModelID = "amazon.titan-embed-text-v2:0"

// EmbedDim is the requested output dimensionality. The locked decision
// caps vectors at 768 dims, but Titan Text Embeddings V2 only supports
// 256 / 512 / 1024 — so this uses 512, the largest supported size under
// the 768 cap (documented deviation; 768 itself is not a valid Titan v2
// dimension).
const EmbedDim = 512

// maxEmbedChars defensively truncates input text well under Titan v2's
// 50k-char / 8192-token input limit.
const maxEmbedChars = 8000

// Embedder is the seam the memory core uses to turn text into a vector,
// so tests (and any future engine swap) can inject a fake.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// bedrockAPI is the one-method subset of the Bedrock runtime client the
// embedder depends on.
type bedrockAPI interface {
	InvokeModel(ctx context.Context, params *bedrockruntime.InvokeModelInput, optFns ...func(*bedrockruntime.Options)) (*bedrockruntime.InvokeModelOutput, error)
}

// BedrockEmbedder calls Titan Text Embeddings V2 via bedrock-runtime
// InvokeModel.
type BedrockEmbedder struct {
	client bedrockAPI
}

// NewBedrockEmbedder builds an embedder on the ambient AWS config,
// pinned to us-east-1 (the locked region for the Titan embed model).
func NewBedrockEmbedder(ctx context.Context) (*BedrockEmbedder, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion("us-east-1"))
	if err != nil {
		return nil, fmt.Errorf("memory: load aws config: %w", err)
	}
	return &BedrockEmbedder{client: bedrockruntime.NewFromConfig(cfg)}, nil
}

// NewBedrockEmbedderWithClient injects an already-built client (or a
// test fake implementing bedrockAPI).
func NewBedrockEmbedderWithClient(client bedrockAPI) *BedrockEmbedder {
	return &BedrockEmbedder{client: client}
}

// titanEmbedRequest / titanEmbedResponse are the Titan v2 InvokeModel
// body shapes.
type titanEmbedRequest struct {
	InputText  string `json:"inputText"`
	Dimensions int    `json:"dimensions"`
	Normalize  bool   `json:"normalize"`
}

type titanEmbedResponse struct {
	Embedding           []float32 `json:"embedding"`
	InputTextTokenCount int       `json:"inputTextTokenCount"`
}

// Embed returns the normalized EmbedDim-dimensional embedding of text.
func (b *BedrockEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	if text == "" {
		return nil, errors.New("memory: embed text is required")
	}
	if len(text) > maxEmbedChars {
		text = text[:maxEmbedChars]
	}

	body, err := json.Marshal(titanEmbedRequest{
		InputText:  text,
		Dimensions: EmbedDim,
		Normalize:  true,
	})
	if err != nil {
		return nil, fmt.Errorf("memory: marshal embed request: %w", err)
	}

	out, err := b.client.InvokeModel(ctx, &bedrockruntime.InvokeModelInput{
		ModelId:     aws.String(EmbedModelID),
		ContentType: aws.String("application/json"),
		Accept:      aws.String("application/json"),
		Body:        body,
	})
	if err != nil {
		return nil, fmt.Errorf("memory: bedrock invoke %s: %w", EmbedModelID, err)
	}

	var resp titanEmbedResponse
	if err := json.Unmarshal(out.Body, &resp); err != nil {
		return nil, fmt.Errorf("memory: unmarshal embed response: %w", err)
	}
	if len(resp.Embedding) == 0 {
		return nil, errors.New("memory: bedrock returned an empty embedding")
	}
	return resp.Embedding, nil
}
