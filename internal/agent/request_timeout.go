package agent

import (
	"context"
	"time"

	"charm.land/fantasy"
)

// requestTimeoutModel wraps a [fantasy.LanguageModel] so each request,
// including the full duration of a streamed response, is bounded by a
// deadline. The deadline is applied per call, so when fantasy's retry loop
// re-runs a failed request, every attempt starts with a fresh budget — the
// same per-request semantics the provider SDKs expose.
type requestTimeoutModel struct {
	fantasy.LanguageModel
	timeout time.Duration
}

// newRequestTimeoutModel bounds each request to m with the given timeout. A
// timeout of zero or less, or a nil model, returns m unchanged.
func newRequestTimeoutModel(m fantasy.LanguageModel, timeout time.Duration) fantasy.LanguageModel {
	if m == nil || timeout <= 0 {
		return m
	}
	return requestTimeoutModel{LanguageModel: m, timeout: timeout}
}

// Generate implements [fantasy.LanguageModel].
func (m requestTimeoutModel) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	ctx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()
	return m.LanguageModel.Generate(ctx, call)
}

// Stream implements [fantasy.LanguageModel].
//
// The stream is consumed after Stream returns, so the deadline must outlive
// this call: cancelling on return would abort the in-flight response body.
// The cancel func is instead released when iteration ends, whether the
// stream finishes, breaks, or the deadline aborts it.
func (m requestTimeoutModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, m.timeout)
	inner, err := m.LanguageModel.Stream(ctx, call)
	if err != nil {
		cancel()
		return nil, err
	}
	return func(yield func(fantasy.StreamPart) bool) {
		defer cancel()
		inner(yield)
	}, nil
}
