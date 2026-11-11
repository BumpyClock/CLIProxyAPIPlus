package translator

import (
	"context"
	"errors"
	"testing"
)

func TestPipelineTranslateRequestPropagatesConverterFailure(t *testing.T) {
	for _, withMiddleware := range []bool{false, true} {
		r := NewRegistry()
		failure := errors.New("synthetic converter rejection")
		r.Register(FormatOpenAI, FormatClaude, func(string, []byte, bool) ([]byte, error) {
			return nil, failure
		}, ResponseTransform{})
		pipeline := NewPipeline(r)
		var observed error
		if withMiddleware {
			pipeline.UseRequest(func(ctx context.Context, req RequestEnvelope, next RequestHandler) (RequestEnvelope, error) {
				out, err := next(ctx, req)
				observed = err
				return out, err
			})
		}
		out, err := pipeline.TranslateRequest(context.Background(), FormatOpenAI, FormatClaude, RequestEnvelope{
			Format: FormatOpenAI,
			Model:  "synthetic-model",
			Body:   []byte(`{"model":"synthetic-model"}`),
		})
		if !errors.Is(err, failure) || !errors.Is(out.Err, failure) {
			t.Fatalf("middleware=%v: error=%v envelope error=%v, want converter rejection", withMiddleware, err, out.Err)
		}
		if withMiddleware && !errors.Is(observed, failure) {
			t.Fatalf("middleware observed %v, want converter rejection", observed)
		}
	}
}

func TestPipelineTranslateRequestPreservesSuccess(t *testing.T) {
	r := NewRegistry()
	r.Register(FormatOpenAI, FormatClaude, func(string, []byte, bool) ([]byte, error) {
		return []byte(`{"translated":true}`), nil
	}, ResponseTransform{})
	out, err := NewPipeline(r).TranslateRequest(context.Background(), FormatOpenAI, FormatClaude, RequestEnvelope{
		Format: FormatOpenAI,
		Model:  "synthetic-model",
		Stream: true,
		Body:   []byte(`{"model":"synthetic-model"}`),
	})
	if err != nil || out.Err != nil || out.Format != FormatClaude || out.Model != "synthetic-model" || !out.Stream || string(out.Body) != `{"translated":true}` {
		t.Fatalf("successful translation changed: envelope=%+v error=%v", out, err)
	}
}
