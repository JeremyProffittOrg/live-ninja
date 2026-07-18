package memory

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeBedrock captures the InvokeModel request and plays back a canned
// Titan v2 response.
type fakeBedrock struct {
	lastInput *bedrockruntime.InvokeModelInput
	respBody  string
	err       error
}

func (f *fakeBedrock) InvokeModel(ctx context.Context, params *bedrockruntime.InvokeModelInput, optFns ...func(*bedrockruntime.Options)) (*bedrockruntime.InvokeModelOutput, error) {
	f.lastInput = params
	if f.err != nil {
		return nil, f.err
	}
	return &bedrockruntime.InvokeModelOutput{Body: []byte(f.respBody)}, nil
}

func TestBedrockEmbedderRequestAndResponse(t *testing.T) {
	fb := &fakeBedrock{respBody: `{"embedding":[0.1,-0.2,0.3],"inputTextTokenCount":4}`}
	e := NewBedrockEmbedderWithClient(fb)

	vec, err := e.Embed(context.Background(), "Rex the dog")
	require.NoError(t, err)
	assert.Equal(t, []float32{0.1, -0.2, 0.3}, vec)

	require.NotNil(t, fb.lastInput)
	assert.Equal(t, EmbedModelID, *fb.lastInput.ModelId)
	assert.Equal(t, "application/json", *fb.lastInput.ContentType)

	var req titanEmbedRequest
	require.NoError(t, json.Unmarshal(fb.lastInput.Body, &req))
	assert.Equal(t, "Rex the dog", req.InputText)
	assert.Equal(t, EmbedDim, req.Dimensions)
	assert.True(t, req.Normalize)
}

func TestBedrockEmbedderTruncatesLongInput(t *testing.T) {
	fb := &fakeBedrock{respBody: `{"embedding":[1]}`}
	e := NewBedrockEmbedderWithClient(fb)

	long := strings.Repeat("x", maxEmbedChars+500)
	_, err := e.Embed(context.Background(), long)
	require.NoError(t, err)

	var req titanEmbedRequest
	require.NoError(t, json.Unmarshal(fb.lastInput.Body, &req))
	assert.Len(t, req.InputText, maxEmbedChars)
}

func TestBedrockEmbedderErrors(t *testing.T) {
	e := NewBedrockEmbedderWithClient(&fakeBedrock{respBody: `{"embedding":[]}`})

	_, err := e.Embed(context.Background(), "")
	require.Error(t, err) // empty text rejected before any API call

	_, err = e.Embed(context.Background(), "hi")
	require.Error(t, err) // empty embedding array is a hard error

	e = NewBedrockEmbedderWithClient(&fakeBedrock{respBody: `not-json`})
	_, err = e.Embed(context.Background(), "hi")
	require.Error(t, err)
}
