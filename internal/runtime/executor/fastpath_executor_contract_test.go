package executor

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type trackedReadCloser struct {
	inner     io.ReadCloser
	closed    chan struct{}
	closeOnce sync.Once
}

func newTrackedReadCloser(body string) *trackedReadCloser {
	return &trackedReadCloser{
		inner:  io.NopCloser(strings.NewReader(body)),
		closed: make(chan struct{}),
	}
}

func (t *trackedReadCloser) Read(p []byte) (int, error) {
	return t.inner.Read(p)
}

func (t *trackedReadCloser) Close() error {
	var err error
	t.closeOnce.Do(func() {
		close(t.closed)
		err = t.inner.Close()
	})
	return err
}

func newRoundTripperWithResponse(status int, contentType string, body io.ReadCloser) http.RoundTripper {
	return roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": []string{contentType}},
			Body:       body,
			Request:    req,
		}, nil
	})
}

func claudeFastPathPayload() []byte {
	return []byte(`{"model":"claude-opus-4-6","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
}

func codexStreamBurst(deltaCount int) string {
	var sb strings.Builder
	sb.WriteString("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-5\"}}\n\n")
	sb.WriteString("data: {\"type\":\"response.content_part.added\",\"part\":{\"type\":\"output_text\",\"text\":\"\"}}\n\n")
	for i := 0; i < deltaCount; i++ {
		sb.WriteString("data: {\"type\":\"response.output_text.delta\",\"delta\":\"x\"}\n\n")
	}
	return sb.String()
}

func openAIStreamBurst(deltaCount int) string {
	var sb strings.Builder
	sb.WriteString("data: {\"id\":\"chatcmpl-1\",\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"x\"}}]}\n\n")
	for i := 1; i < deltaCount; i++ {
		sb.WriteString("data: {\"id\":\"chatcmpl-1\",\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"}}]}\n\n")
	}
	return sb.String()
}

func requireStatusCode(t *testing.T, err error, want int) {
	t.Helper()
	statusProvider, ok := err.(interface{ StatusCode() int })
	if !ok {
		t.Fatalf("error %T does not expose StatusCode()", err)
	}
	if got := statusProvider.StatusCode(); got != want {
		t.Fatalf("StatusCode() = %d, want %d", got, want)
	}
}

func requireRetryAfter(t *testing.T, err error, want time.Duration) {
	t.Helper()
	retryProvider, ok := err.(interface{ RetryAfter() *time.Duration })
	if !ok {
		t.Fatalf("error %T does not expose RetryAfter()", err)
	}
	retryAfter := retryProvider.RetryAfter()
	if retryAfter == nil {
		t.Fatalf("RetryAfter() = nil, want %v", want)
	}
	if *retryAfter != want {
		t.Fatalf("RetryAfter() = %v, want %v", *retryAfter, want)
	}
}

func requireNilRetryAfter(t *testing.T, err error) {
	t.Helper()
	retryProvider, ok := err.(interface{ RetryAfter() *time.Duration })
	if !ok {
		t.Fatalf("error %T does not expose RetryAfter()", err)
	}
	if retryProvider.RetryAfter() != nil {
		t.Fatalf("RetryAfter() = %v, want nil", *retryProvider.RetryAfter())
	}
}

func TestCodexExecutorFastPath_StatusErrorsExposeRetryMetadata(t *testing.T) {
	tests := []struct {
		name           string
		status         int
		body           string
		invokeStream   bool
		wantRetryAfter *time.Duration
	}{
		{
			name:         "execute 429 retry-after",
			status:       http.StatusTooManyRequests,
			body:         `{"error":{"type":"usage_limit_reached","message":"quota exhausted","resets_in_seconds":42}}`,
			invokeStream: false,
			wantRetryAfter: func() *time.Duration {
				d := 42 * time.Second
				return &d
			}(),
		},
		{
			name:           "execute 503 no retry-after",
			status:         http.StatusServiceUnavailable,
			body:           `{"error":{"type":"server_error","message":"upstream unavailable"}}`,
			invokeStream:   false,
			wantRetryAfter: nil,
		},
		{
			name:         "stream 429 retry-after",
			status:       http.StatusTooManyRequests,
			body:         `{"error":{"type":"usage_limit_reached","message":"quota exhausted","resets_in_seconds":42}}`,
			invokeStream: true,
			wantRetryAfter: func() *time.Duration {
				d := 42 * time.Second
				return &d
			}(),
		},
		{
			name:           "stream 503 no retry-after",
			status:         http.StatusServiceUnavailable,
			body:           `{"error":{"type":"server_error","message":"upstream unavailable"}}`,
			invokeStream:   true,
			wantRetryAfter: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", newRoundTripperWithResponse(tt.status, "application/json", io.NopCloser(strings.NewReader(tt.body))))
			executor := NewCodexExecutor(&config.Config{})
			auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": "https://codex.test", "api_key": "test"}}
			req := cliproxyexecutor.Request{Model: "gpt-5", Payload: claudeFastPathPayload()}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")}

			var err error
			if tt.invokeStream {
				_, err = executor.ExecuteStream(ctx, auth, req, opts)
			} else {
				_, err = executor.Execute(ctx, auth, req, opts)
			}
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			requireStatusCode(t, err, tt.status)
			if tt.wantRetryAfter != nil {
				requireRetryAfter(t, err, *tt.wantRetryAfter)
			} else {
				requireNilRetryAfter(t, err)
			}
		})
	}
}

func TestOpenAICompatExecutorFastPath_StatusErrorsExposeStatusCode(t *testing.T) {
	tests := []struct {
		name         string
		status       int
		invokeStream bool
	}{
		{name: "execute 429", status: http.StatusTooManyRequests, invokeStream: false},
		{name: "execute 503", status: http.StatusServiceUnavailable, invokeStream: false},
		{name: "stream 429", status: http.StatusTooManyRequests, invokeStream: true},
		{name: "stream 503", status: http.StatusServiceUnavailable, invokeStream: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", newRoundTripperWithResponse(tt.status, "application/json", io.NopCloser(strings.NewReader(`{"error":{"message":"upstream failed"}}`))))
			executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
			auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": "https://openai.test/v1", "api_key": "test"}}
			req := cliproxyexecutor.Request{Model: "gpt-4o", Payload: claudeFastPathPayload()}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")}

			var err error
			if tt.invokeStream {
				_, err = executor.ExecuteStream(ctx, auth, req, opts)
			} else {
				_, err = executor.Execute(ctx, auth, req, opts)
			}
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			requireStatusCode(t, err, tt.status)
		})
	}
}

func TestCodexExecutorFastPath_ExecuteStream_CancelClosesBodyAndChannel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	trackedBody := newTrackedReadCloser(codexStreamBurst(32))
	ctx = context.WithValue(ctx, "cliproxy.roundtripper", newRoundTripperWithResponse(http.StatusOK, "text/event-stream", trackedBody))

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": "https://codex.test", "api_key": "test"}}
	result, err := executor.ExecuteStream(ctx, auth, cliproxyexecutor.Request{
		Model:   "gpt-5",
		Payload: claudeFastPathPayload(),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case <-trackedBody.closed:
	case <-time.After(1 * time.Second):
		t.Fatal("response body was not closed after cancellation")
	}

	drained := make(chan struct{})
	go func() {
		for range result.Chunks {
		}
		close(drained)
	}()

	select {
	case <-drained:
	case <-time.After(1 * time.Second):
		t.Fatal("stream channel did not close after cancellation")
	}
}

func TestOpenAICompatExecutorFastPath_ExecuteStream_CancelClosesBodyAndChannel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	trackedBody := newTrackedReadCloser(openAIStreamBurst(32))
	ctx = context.WithValue(ctx, "cliproxy.roundtripper", newRoundTripperWithResponse(http.StatusOK, "text/event-stream", trackedBody))

	executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": "https://openai.test/v1", "api_key": "test"}}
	result, err := executor.ExecuteStream(ctx, auth, cliproxyexecutor.Request{
		Model:   "gpt-4o",
		Payload: claudeFastPathPayload(),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case <-trackedBody.closed:
	case <-time.After(1 * time.Second):
		t.Fatal("response body was not closed after cancellation")
	}

	drained := make(chan struct{})
	go func() {
		for range result.Chunks {
		}
		close(drained)
	}()

	select {
	case <-drained:
	case <-time.After(1 * time.Second):
		t.Fatal("stream channel did not close after cancellation")
	}
}

func TestOpenAICompatExecutor_Execute_PayloadOverrideWinsOverThinking(t *testing.T) {
	var gotBody []byte
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		gotBody = body
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"id":"chatcmpl-1","object":"chat.completion","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)),
			Request:    req,
		}, nil
	}))

	executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{
		Payload: config.PayloadConfig{
			Override: []config.PayloadRule{{
				Models: []config.PayloadModelRule{{Name: "gpt-4o", Protocol: "openai"}},
				Params: map[string]any{"reasoning_effort": "low"},
			}},
		},
	})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": "https://openai.test/v1", "api_key": "test"}}

	_, err := executor.Execute(ctx, auth, cliproxyexecutor.Request{
		Model:   "gpt-4o(high)",
		Payload: claudeFastPathPayload(),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}

	if got := gjson.GetBytes(gotBody, "reasoning_effort").String(); got != "low" {
		t.Fatalf("reasoning_effort = %q, want %q, body=%s", got, "low", string(gotBody))
	}
}
