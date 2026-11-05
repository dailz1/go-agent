package openai

import (
	"context"
	"fmt"
	"net/http"

	"github.com/dailz1/go-agent/llm"
)

// NewEmbedder returns a Provider usable as an [llm.Embedder] against any
// OpenAI-compatible embeddings endpoint (POST {baseURL}/embeddings). The
// same base URL, HTTP client, and logger options as [NewProvider] apply,
// so chat and embedding calls share one provider configuration.
func NewEmbedder(apiKey, model string, opts ...ProviderOption) *Provider {
	return NewProvider(apiKey, model, opts...)
}

type embeddingsRequest struct {
	Model      string   `json:"model"`
	Input      []string `json:"input"`
	Dimensions int      `json:"dimensions,omitempty"`
}

type embeddingsResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Usage struct {
		PromptTokens int64 `json:"prompt_tokens"`
		TotalTokens  int64 `json:"total_tokens"`
	} `json:"usage"`
}

// Embed implements [llm.Embedder]. Result[i] vectorizes input[i]: the
// response vectors are reordered by their index field, never by arrival
// order, and an index set that does not cover [0,len(input)) exactly once
// is a typed error rather than a silent misalignment.
func (p *Provider) Embed(ctx context.Context, input []string, opts ...llm.EmbedOption) ([][]float32, llm.Usage, error) {
	if len(input) == 0 {
		return nil, llm.Usage{}, fmt.Errorf("openai embed: empty input")
	}
	options := llm.EmbedOptions{Model: p.model}
	for _, opt := range opts {
		opt(&options)
	}
	if options.Model == "" {
		return nil, llm.Usage{}, fmt.Errorf("openai embed: no model configured")
	}

	var resp embeddingsResponse
	err := llm.DoJSONRequest(ctx, p.httpClient, llm.RequestConfig{
		Method:  http.MethodPost,
		URL:     p.baseURL + "/embeddings",
		Headers: p.apiHeaders(),
	}, embeddingsRequest{
		Model:      options.Model,
		Input:      input,
		Dimensions: options.Dimensions,
	}, &resp)
	if err != nil {
		return nil, llm.Usage{}, err
	}

	if len(resp.Data) != len(input) {
		return nil, llm.Usage{}, fmt.Errorf("openai embed: response has %d vectors for %d inputs", len(resp.Data), len(input))
	}
	vectors := make([][]float32, len(input))
	for _, d := range resp.Data {
		if d.Index < 0 || d.Index >= len(input) || vectors[d.Index] != nil {
			return nil, llm.Usage{}, fmt.Errorf("openai embed: duplicate or out-of-range embedding index %d", d.Index)
		}
		vectors[d.Index] = d.Embedding
	}
	for i, v := range vectors {
		if v == nil {
			return nil, llm.Usage{}, fmt.Errorf("openai embed: missing embedding index %d", i)
		}
	}
	return vectors, llm.Usage{InputTokens: resp.Usage.PromptTokens}, nil
}
