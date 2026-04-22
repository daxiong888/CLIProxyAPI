package fastpath

import (
	"context"
	"strings"
	"testing"

	openaiclaude "github.com/router-for-me/CLIProxyAPI/v6/internal/translator/openai/claude"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

// ---------------------------------------------------------------------------
// IsEligible
// ---------------------------------------------------------------------------

func TestOpenAIFormatMapper_IsEligible(t *testing.T) {
	m := &OpenAIFormatMapper{}

	tests := []struct {
		name     string
		payload  string
		eligible bool
	}{
		{
			name:     "empty payload",
			payload:  "",
			eligible: false,
		},
		{
			name:     "simple text message",
			payload:  `{"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`,
			eligible: true,
		},
		{
			name:     "tool_use and tool_result",
			payload:  `{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"fn","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}]}`,
			eligible: true,
		},
		{
			name:     "image content",
			payload:  `{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"abc"}}]}]}`,
			eligible: true,
		},
		{
			name:     "url image supported",
			payload:  `{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"https://example.com/img.png"}}]}]}`,
			eligible: true,
		},
		{
			name:     "thinking supported",
			payload:  `{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"Let me think..."}]}]}`,
			eligible: true,
		},
		{
			name:     "redacted_thinking supported",
			payload:  `{"messages":[{"role":"assistant","content":[{"type":"redacted_thinking","data":"abc"}]}]}`,
			eligible: true,
		},
		{
			name:     "unsupported content type",
			payload:  `{"messages":[{"role":"user","content":[{"type":"document","source":{"type":"base64"}}]}]}`,
			eligible: false,
		},
		{
			name:     "unsupported tool schema anyOf",
			payload:  `{"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}],"tools":[{"name":"search","input_schema":{"type":"object","properties":{"q":{"anyOf":[{"type":"string"},{"type":"number"}]}}}}]}`,
			eligible: false,
		},
		{
			name:     "no messages",
			payload:  `{"model":"gpt-4o"}`,
			eligible: true,
		},
		{
			name:     "string content (not array)",
			payload:  `{"messages":[{"role":"user","content":"hello world"}]}`,
			eligible: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			eligible, reason := m.IsEligible([]byte(tt.payload))
			if eligible != tt.eligible {
				t.Errorf("IsEligible() = %v (reason: %s), want %v", eligible, reason, tt.eligible)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// MapRequest
// ---------------------------------------------------------------------------

func TestOpenAIFormatMapper_MapRequest_BasicText(t *testing.T) {
	m := &OpenAIFormatMapper{}
	input := []byte(`{"model":"claude-sonnet-4-20250514","messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}],"system":"You are helpful.","max_tokens":1024}`)

	body, originalTranslated, err := m.MapRequest(input, input, "gpt-4o")
	if err != nil {
		t.Fatalf("MapRequest error: %v", err)
	}

	root := gjson.ParseBytes(body)

	if root.Get("model").String() != "gpt-4o" {
		t.Errorf("model = %s, want gpt-4o", root.Get("model").String())
	}
	if root.Get("max_tokens").Int() != 1024 {
		t.Errorf("max_tokens = %d, want 1024", root.Get("max_tokens").Int())
	}
	if root.Get("stream").Bool() != false {
		t.Errorf("stream = %v, want false", root.Get("stream").Bool())
	}

	// System message should be first in messages array
	msgs := root.Get("messages").Array()
	if len(msgs) < 2 {
		t.Fatalf("expected at least 2 messages, got %d", len(msgs))
	}
	if msgs[0].Get("role").String() != "system" {
		t.Errorf("first message role = %s, want system", msgs[0].Get("role").String())
	}
	if msgs[1].Get("role").String() != "user" {
		t.Errorf("second message role = %s, want user", msgs[1].Get("role").String())
	}

	// Same input -> same output for both
	if string(body) != string(originalTranslated) {
		t.Errorf("body and originalTranslated should be identical for same input")
	}
}

func TestOpenAIFormatMapper_MapRequest_ToolUseAndResult(t *testing.T) {
	m := &OpenAIFormatMapper{}
	input := []byte(`{"model":"claude-sonnet-4-20250514","messages":[
		{"role":"assistant","content":[{"type":"tool_use","id":"call_123","name":"get_weather","input":{"city":"SF"}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_123","content":"Sunny, 72F"}]}
	]}`)

	body, _, err := m.MapRequest(input, input, "gpt-4o")
	if err != nil {
		t.Fatalf("MapRequest error: %v", err)
	}

	root := gjson.ParseBytes(body)
	msgs := root.Get("messages").Array()

	// tool_result should be emitted before the assistant message
	if len(msgs) < 2 {
		t.Fatalf("expected at least 2 messages, got %d", len(msgs))
	}

	// Find the tool result message
	foundTool := false
	foundAssistant := false
	for _, msg := range msgs {
		if msg.Get("role").String() == "tool" {
			foundTool = true
			if msg.Get("tool_call_id").String() != "call_123" {
				t.Errorf("tool_call_id = %s, want call_123", msg.Get("tool_call_id").String())
			}
		}
		if msg.Get("role").String() == "assistant" {
			foundAssistant = true
			toolCalls := msg.Get("tool_calls").Array()
			if len(toolCalls) == 0 {
				t.Error("expected tool_calls on assistant message")
			} else {
				if toolCalls[0].Get("function.name").String() != "get_weather" {
					t.Errorf("tool call name = %s, want get_weather", toolCalls[0].Get("function.name").String())
				}
			}
		}
	}
	if !foundTool {
		t.Error("expected tool message")
	}
	if !foundAssistant {
		t.Error("expected assistant message")
	}
}

func TestOpenAIFormatMapper_MapRequest_Tools(t *testing.T) {
	m := &OpenAIFormatMapper{}
	input := []byte(`{"model":"claude-sonnet-4-20250514","messages":[{"role":"user","content":"hi"}],"tools":[{"name":"calc","description":"A calculator","input_schema":{"type":"object","properties":{"expr":{"type":"string"}}}}],"tool_choice":{"type":"any"}}`)

	body, _, err := m.MapRequest(input, input, "gpt-4o")
	if err != nil {
		t.Fatalf("MapRequest error: %v", err)
	}

	root := gjson.ParseBytes(body)

	tools := root.Get("tools").Array()
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}
	if tools[0].Get("function.name").String() != "calc" {
		t.Errorf("tool name = %s, want calc", tools[0].Get("function.name").String())
	}
	if !tools[0].Get("function.parameters").Exists() {
		t.Error("expected function.parameters to exist")
	}
	if root.Get("tool_choice").String() != "required" {
		t.Errorf("tool_choice = %s, want required", root.Get("tool_choice").String())
	}
}

func TestOpenAIFormatMapper_MapRequest_Thinking(t *testing.T) {
	m := &OpenAIFormatMapper{}
	input := []byte(`{"model":"claude-sonnet-4-20250514","messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"Let me think..."},{"type":"text","text":"Sure!"}]},{"role":"user","content":[{"type":"text","text":"continue"}]}],"thinking":{"type":"enabled","budget_tokens":10000}}`)

	body, _, err := m.MapRequest(input, input, "gpt-4o")
	if err != nil {
		t.Fatalf("MapRequest error: %v", err)
	}

	root := gjson.ParseBytes(body)

	if !root.Get("reasoning_effort").Exists() {
		t.Error("expected reasoning_effort to be set")
	}

	// Check that assistant message has reasoning_content
	msgs := root.Get("messages").Array()
	for _, msg := range msgs {
		if msg.Get("role").String() == "assistant" {
			if msg.Get("reasoning_content").String() != "Let me think..." {
				t.Errorf("reasoning_content = %s, want 'Let me think...'", msg.Get("reasoning_content").String())
			}
		}
	}
}

func TestOpenAIFormatMapper_MapRequest_Image(t *testing.T) {
	m := &OpenAIFormatMapper{}
	input := []byte(`{"model":"claude-sonnet-4-20250514","messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBOR..."}}]}]}`)

	body, _, err := m.MapRequest(input, input, "gpt-4o")
	if err != nil {
		t.Fatalf("MapRequest error: %v", err)
	}

	root := gjson.ParseBytes(body)
	msgs := root.Get("messages").Array()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	contentParts := msgs[0].Get("content").Array()
	if len(contentParts) != 1 {
		t.Fatalf("expected 1 content part, got %d", len(contentParts))
	}
	if contentParts[0].Get("type").String() != "image_url" {
		t.Errorf("content type = %s, want image_url", contentParts[0].Get("type").String())
	}
	url := contentParts[0].Get("image_url.url").String()
	if !strings.HasPrefix(url, "data:image/png;base64,") {
		t.Errorf("image_url.url should start with data:image/png;base64,, got %s", url[:30])
	}
}

func TestOpenAIFormatMapper_MapRequest_ParityWithTranslator(t *testing.T) {
	m := &OpenAIFormatMapper{}
	input := []byte(`{
		"model":"claude-sonnet-4-20250514",
		"system":[{"type":"text","text":"You are helpful."}],
		"messages":[
			{"role":"assistant","content":[{"type":"thinking","thinking":"Let me reason"},{"type":"text","text":"Checking..."},{"type":"tool_use","id":"call_1","name":"GetWeather","input":{"city":"SF"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":[{"type":"text","text":"Sunny"}]}]}
		],
		"tools":[{"name":"GetWeather","description":"Get weather","input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}],
		"tool_choice":{"type":"tool","name":"GetWeather"},
		"thinking":{"type":"enabled","budget_tokens":8000},
		"max_tokens":2048
	}`)

	body, _, err := m.MapRequest(input, input, "gpt-4o")
	if err != nil {
		t.Fatalf("MapRequest error: %v", err)
	}
	want := openaiclaude.ConvertClaudeRequestToOpenAI("gpt-4o", input, false)
	if string(body) != string(want) {
		t.Fatalf("fast path request differs from translator\nfast: %s\nwant: %s", string(body), string(want))
	}
}

func TestOpenAIFormatMapper_MapNonStreamResponse_ParityWithTranslator(t *testing.T) {
	m := &OpenAIFormatMapper{}
	originalReq := []byte(`{"messages":[{"role":"user","content":"hi"}],"tools":[{"name":"GetWeather","description":"Get weather","input_schema":{"type":"object"}}]}`)
	response := []byte(`{
		"id":"chatcmpl-9",
		"model":"gpt-4o",
		"choices":[{
			"index":0,
			"message":{
				"role":"assistant",
				"reasoning_content":"Thinking...",
				"content":"Done",
				"tool_calls":[{"id":"call_abc","type":"function","function":{"name":"getweather","arguments":"{'city':'SF'}"}}]
			},
			"finish_reason":"tool_calls"
		}],
		"usage":{"prompt_tokens":100,"completion_tokens":8,"prompt_tokens_details":{"cached_tokens":60}}
	}`)

	got, err := m.MapNonStreamResponse(originalReq, response)
	if err != nil {
		t.Fatalf("MapNonStreamResponse error: %v", err)
	}
	var param any
	want := sdktranslator.TranslateNonStream(context.Background(), sdktranslator.FromString("openai"), sdktranslator.FromString("claude"), "gpt-4o", originalReq, nil, response, &param)
	if string(got) != string(want) {
		t.Fatalf("fast path non-stream differs from translator\nfast: %s\nwant: %s", string(got), string(want))
	}
}

// ---------------------------------------------------------------------------
// MapNonStreamResponse
// ---------------------------------------------------------------------------

func TestOpenAIFormatMapper_MapNonStreamResponse_Text(t *testing.T) {
	m := &OpenAIFormatMapper{}
	originalReq := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	response := []byte(`{"id":"chatcmpl-1","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"Hello!"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`)

	out, err := m.MapNonStreamResponse(originalReq, response)
	if err != nil {
		t.Fatalf("MapNonStreamResponse error: %v", err)
	}

	root := gjson.ParseBytes(out)
	if root.Get("id").String() != "chatcmpl-1" {
		t.Errorf("id = %s, want chatcmpl-1", root.Get("id").String())
	}
	if root.Get("type").String() != "message" {
		t.Errorf("type = %s, want message", root.Get("type").String())
	}
	if root.Get("stop_reason").String() != "end_turn" {
		t.Errorf("stop_reason = %s, want end_turn", root.Get("stop_reason").String())
	}

	content := root.Get("content").Array()
	if len(content) != 1 {
		t.Fatalf("expected 1 content block, got %d", len(content))
	}
	if content[0].Get("type").String() != "text" {
		t.Errorf("content type = %s, want text", content[0].Get("type").String())
	}
	if content[0].Get("text").String() != "Hello!" {
		t.Errorf("text = %s, want Hello!", content[0].Get("text").String())
	}

	if root.Get("usage.input_tokens").Int() != 10 {
		t.Errorf("input_tokens = %d, want 10", root.Get("usage.input_tokens").Int())
	}
	if root.Get("usage.output_tokens").Int() != 5 {
		t.Errorf("output_tokens = %d, want 5", root.Get("usage.output_tokens").Int())
	}
}

func TestOpenAIFormatMapper_MapNonStreamResponse_ToolCalls(t *testing.T) {
	m := &OpenAIFormatMapper{}
	originalReq := []byte(`{"messages":[{"role":"user","content":"hi"}],"tools":[{"name":"get_weather","description":"Get weather","input_schema":{"type":"object"}}]}`)
	response := []byte(`{"id":"chatcmpl-2","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_abc","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"SF\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`)

	out, err := m.MapNonStreamResponse(originalReq, response)
	if err != nil {
		t.Fatalf("MapNonStreamResponse error: %v", err)
	}

	root := gjson.ParseBytes(out)
	if root.Get("stop_reason").String() != "tool_use" {
		t.Errorf("stop_reason = %s, want tool_use", root.Get("stop_reason").String())
	}

	content := root.Get("content").Array()
	if len(content) != 1 {
		t.Fatalf("expected 1 content block, got %d", len(content))
	}
	if content[0].Get("type").String() != "tool_use" {
		t.Errorf("content type = %s, want tool_use", content[0].Get("type").String())
	}
	if content[0].Get("name").String() != "get_weather" {
		t.Errorf("name = %s, want get_weather", content[0].Get("name").String())
	}
	if content[0].Get("input.city").String() != "SF" {
		t.Errorf("input.city = %s, want SF", content[0].Get("input.city").String())
	}
}

func TestOpenAIFormatMapper_MapNonStreamResponse_CachedTokens(t *testing.T) {
	m := &OpenAIFormatMapper{}
	originalReq := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	response := []byte(`{"id":"chatcmpl-3","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"Hi!"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":60}}}`)

	out, err := m.MapNonStreamResponse(originalReq, response)
	if err != nil {
		t.Fatalf("MapNonStreamResponse error: %v", err)
	}

	root := gjson.ParseBytes(out)
	// input_tokens = prompt_tokens - cached_tokens = 100 - 60 = 40
	if root.Get("usage.input_tokens").Int() != 40 {
		t.Errorf("input_tokens = %d, want 40", root.Get("usage.input_tokens").Int())
	}
	if root.Get("usage.cache_read_input_tokens").Int() != 60 {
		t.Errorf("cache_read_input_tokens = %d, want 60", root.Get("usage.cache_read_input_tokens").Int())
	}
}

// ---------------------------------------------------------------------------
// Stream bridge helpers
// ---------------------------------------------------------------------------

func feedOpenAILine(t *testing.T, b *OpenAIToClaudeStreamBridge, line string) string {
	t.Helper()
	chunks := b.ProcessLine(context.Background(), []byte(line))
	var sb strings.Builder
	for _, c := range chunks {
		sb.Write(c)
	}
	return sb.String()
}

// ---------------------------------------------------------------------------
// StreamBridge tests
// ---------------------------------------------------------------------------

func TestOpenAIStreamBridge_NonDataLine(t *testing.T) {
	b := &OpenAIToClaudeStreamBridge{
		textContentBlockIndex:     -1,
		thinkingContentBlockIndex: -1,
		toolCallBlockIndexes:      make(map[int]int),
	}
	result := b.ProcessLine(context.Background(), []byte("event: message"))
	if result != nil {
		t.Errorf("expected nil for non-data line, got %v", result)
	}
}

func TestOpenAIStreamBridge_TextFlow(t *testing.T) {
	b := &OpenAIToClaudeStreamBridge{
		textContentBlockIndex:     -1,
		thinkingContentBlockIndex: -1,
		toolCallBlockIndexes:      make(map[int]int),
	}

	// First chunk: should emit message_start + content_block_start + text_delta
	out := feedOpenAILine(t, b, `data: {"id":"chatcmpl-1","model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":"Hello"}}]}`)
	if !strings.Contains(out, "message_start") {
		t.Error("expected message_start")
	}
	if !strings.Contains(out, "content_block_start") {
		t.Error("expected content_block_start")
	}
	if !strings.Contains(out, `"type":"text"`) {
		t.Error("expected text content block")
	}
	if !strings.Contains(out, "text_delta") {
		t.Error("expected text_delta")
	}

	// Second chunk: just text_delta
	out = feedOpenAILine(t, b, `data: {"id":"chatcmpl-1","model":"gpt-4o","choices":[{"index":0,"delta":{"content":" world"}}]}`)
	if strings.Contains(out, "message_start") {
		t.Error("should not emit message_start again")
	}
	if !strings.Contains(out, "text_delta") {
		t.Error("expected text_delta")
	}

	// finish_reason
	out = feedOpenAILine(t, b, `data: {"id":"chatcmpl-1","model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
	if !strings.Contains(out, "content_block_stop") {
		t.Error("expected content_block_stop")
	}

	// Usage chunk
	out = feedOpenAILine(t, b, `data: {"id":"chatcmpl-1","model":"gpt-4o","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":3}}`)
	if !strings.Contains(out, "message_delta") {
		t.Error("expected message_delta with usage")
	}
	if !strings.Contains(out, "end_turn") {
		t.Error("expected stop_reason end_turn")
	}
	if !strings.Contains(out, "message_stop") {
		t.Error("expected message_stop")
	}
}

func TestOpenAIStreamBridge_ToolCallFlow(t *testing.T) {
	b := &OpenAIToClaudeStreamBridge{
		toolNameMap:               map[string]string{"get_weather": "get_weather"},
		textContentBlockIndex:     -1,
		thinkingContentBlockIndex: -1,
		toolCallBlockIndexes:      make(map[int]int),
	}

	// First chunk with tool_calls name
	out := feedOpenAILine(t, b, `data: {"id":"chatcmpl-1","model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_123","type":"function","function":{"name":"get_weather"}}]}}]}`)
	if !strings.Contains(out, "message_start") {
		t.Error("expected message_start")
	}
	if !strings.Contains(out, `"type":"tool_use"`) {
		t.Error("expected tool_use content_block_start")
	}
	if !strings.Contains(out, "call_123") {
		t.Error("expected tool call id")
	}

	// Arguments chunk
	out = feedOpenAILine(t, b, `data: {"id":"chatcmpl-1","model":"gpt-4o","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]}}]}`)
	// Arguments are accumulated, not emitted yet
	if strings.Contains(out, "input_json_delta") {
		t.Error("should not emit input_json_delta during streaming (accumulated)")
	}

	// finish_reason with tool_calls
	out = feedOpenAILine(t, b, `data: {"id":"chatcmpl-1","model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`)
	if !strings.Contains(out, "input_json_delta") {
		t.Error("expected input_json_delta with accumulated arguments on finish")
	}
	if !strings.Contains(out, "content_block_stop") {
		t.Error("expected content_block_stop")
	}

	// [DONE]
	out = feedOpenAILine(t, b, `data: [DONE]`)
	if !strings.Contains(out, "message_delta") {
		t.Error("expected message_delta on [DONE]")
	}
	if !strings.Contains(out, "tool_use") {
		t.Error("expected stop_reason tool_use")
	}
	if !strings.Contains(out, "message_stop") {
		t.Error("expected message_stop")
	}
}

func TestOpenAIStreamBridge_ReasoningFlow(t *testing.T) {
	b := &OpenAIToClaudeStreamBridge{
		textContentBlockIndex:     -1,
		thinkingContentBlockIndex: -1,
		toolCallBlockIndexes:      make(map[int]int),
	}

	// Reasoning delta
	out := feedOpenAILine(t, b, `data: {"id":"chatcmpl-1","model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"Let me think"}}]}`)
	if !strings.Contains(out, "message_start") {
		t.Error("expected message_start")
	}
	if !strings.Contains(out, `"type":"thinking"`) {
		t.Error("expected thinking content_block_start")
	}
	if !strings.Contains(out, "thinking_delta") {
		t.Error("expected thinking_delta")
	}

	// Text content after thinking
	out = feedOpenAILine(t, b, `data: {"id":"chatcmpl-1","model":"gpt-4o","choices":[{"index":0,"delta":{"content":"The answer is 42"}}]}`)
	if !strings.Contains(out, "content_block_stop") {
		t.Error("expected content_block_stop for thinking before text")
	}
	if !strings.Contains(out, `"type":"text"`) {
		t.Error("expected text content_block_start")
	}
	if !strings.Contains(out, "text_delta") {
		t.Error("expected text_delta")
	}
}

func TestOpenAIStreamBridge_Done(t *testing.T) {
	b := &OpenAIToClaudeStreamBridge{
		textContentBlockIndex:     -1,
		thinkingContentBlockIndex: -1,
		toolCallBlockIndexes:      make(map[int]int),
	}

	// Simple text + finish + [DONE]
	feedOpenAILine(t, b, `data: {"id":"chatcmpl-1","model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":"Hi"}}]}`)
	feedOpenAILine(t, b, `data: {"id":"chatcmpl-1","model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)

	out := feedOpenAILine(t, b, `data: [DONE]`)
	if !strings.Contains(out, "message_delta") {
		t.Error("expected message_delta on [DONE]")
	}
	if !strings.Contains(out, "message_stop") {
		t.Error("expected message_stop on [DONE]")
	}
}

func TestOpenAIStreamBridge_DoneIdempotent(t *testing.T) {
	b := &OpenAIToClaudeStreamBridge{
		textContentBlockIndex:     -1,
		thinkingContentBlockIndex: -1,
		toolCallBlockIndexes:      make(map[int]int),
	}

	feedOpenAILine(t, b, `data: {"id":"chatcmpl-1","model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":"Hi"}}]}`)
	feedOpenAILine(t, b, `data: {"id":"chatcmpl-1","model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
	feedOpenAILine(t, b, `data: {"id":"chatcmpl-1","model":"gpt-4o","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":1}}`)

	// [DONE] after usage already sent message_stop
	out := feedOpenAILine(t, b, `data: [DONE]`)
	if out != "" {
		t.Errorf("expected empty output for idempotent [DONE], got: %s", out)
	}
}

func TestOpenAIStreamBridge_Finalize(t *testing.T) {
	b := &OpenAIToClaudeStreamBridge{
		textContentBlockIndex:     -1,
		thinkingContentBlockIndex: -1,
		toolCallBlockIndexes:      make(map[int]int),
	}

	feedOpenAILine(t, b, `data: {"id":"chatcmpl-1","model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":"Hi"}}]}`)
	feedOpenAILine(t, b, `data: {"id":"chatcmpl-1","model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)

	// Finalize without [DONE]
	chunks := b.Finalize()
	var sb strings.Builder
	for _, c := range chunks {
		sb.Write(c)
	}
	out := sb.String()
	if !strings.Contains(out, "message_stop") {
		t.Error("expected Finalize to emit message_stop")
	}
}

func TestOpenAIStreamBridge_FinalizeAfterDone(t *testing.T) {
	b := &OpenAIToClaudeStreamBridge{
		textContentBlockIndex:     -1,
		thinkingContentBlockIndex: -1,
		toolCallBlockIndexes:      make(map[int]int),
	}

	feedOpenAILine(t, b, `data: {"id":"chatcmpl-1","model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":"Hi"}}]}`)
	feedOpenAILine(t, b, `data: {"id":"chatcmpl-1","model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
	feedOpenAILine(t, b, `data: [DONE]`)

	// Finalize should be no-op
	chunks := b.Finalize()
	if chunks != nil {
		t.Errorf("expected nil from Finalize after [DONE], got %d chunks", len(chunks))
	}
}

func TestOpenAIStreamBridge_ToolNameRestore(t *testing.T) {
	b := &OpenAIToClaudeStreamBridge{
		toolNameMap:               map[string]string{"get_weather": "Get_Weather"},
		textContentBlockIndex:     -1,
		thinkingContentBlockIndex: -1,
		toolCallBlockIndexes:      make(map[int]int),
	}

	out := feedOpenAILine(t, b, `data: {"id":"chatcmpl-1","model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather"}}]}}]}`)
	if !strings.Contains(out, "Get_Weather") {
		t.Error("expected restored tool name Get_Weather")
	}
}

func TestOpenAIStreamBridge_UnknownEventType(t *testing.T) {
	b := &OpenAIToClaudeStreamBridge{
		textContentBlockIndex:     -1,
		thinkingContentBlockIndex: -1,
		toolCallBlockIndexes:      make(map[int]int),
	}

	// A chunk with no delta and no finish_reason
	result := b.ProcessLine(context.Background(), []byte(`data: {"id":"chatcmpl-1","model":"gpt-4o","object":"chat.completion.chunk"}`))
	if result != nil {
		t.Errorf("expected nil for chunk without delta, got %v", result)
	}
}

func TestOpenAIStreamBridge_MaxTokensFinishReason(t *testing.T) {
	b := &OpenAIToClaudeStreamBridge{
		textContentBlockIndex:     -1,
		thinkingContentBlockIndex: -1,
		toolCallBlockIndexes:      make(map[int]int),
	}

	feedOpenAILine(t, b, `data: {"id":"chatcmpl-1","model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":"Hi"}}]}`)
	feedOpenAILine(t, b, `data: {"id":"chatcmpl-1","model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"length"}]}`)

	out := feedOpenAILine(t, b, `data: [DONE]`)
	if !strings.Contains(out, "max_tokens") {
		t.Error("expected stop_reason max_tokens for finish_reason=length")
	}
}

func TestOpenAIStreamBridge_UsageWithCachedTokens(t *testing.T) {
	b := &OpenAIToClaudeStreamBridge{
		textContentBlockIndex:     -1,
		thinkingContentBlockIndex: -1,
		toolCallBlockIndexes:      make(map[int]int),
	}

	feedOpenAILine(t, b, `data: {"id":"chatcmpl-1","model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":"Hi"}}]}`)
	feedOpenAILine(t, b, `data: {"id":"chatcmpl-1","model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)

	out := feedOpenAILine(t, b, `data: {"id":"chatcmpl-1","model":"gpt-4o","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":60}}}`)
	if !strings.Contains(out, "message_delta") {
		t.Error("expected message_delta with usage")
	}
	// Check that input_tokens = 100 - 60 = 40
	root := gjson.Parse(strings.TrimPrefix(strings.Split(out, "\n\n")[0], "event: message_delta\ndata: "))
	if root.Get("usage.input_tokens").Int() != 40 {
		t.Errorf("input_tokens = %d, want 40", root.Get("usage.input_tokens").Int())
	}
	if root.Get("usage.cache_read_input_tokens").Int() != 60 {
		t.Errorf("cache_read_input_tokens = %d, want 60", root.Get("usage.cache_read_input_tokens").Int())
	}
}

// ---------------------------------------------------------------------------
// Interface compliance
// ---------------------------------------------------------------------------

func TestOpenAIFormatMapper_InterfaceCompliance(t *testing.T) {
	var _ FormatMapper = (*OpenAIFormatMapper)(nil)
	var _ StreamBridge = (*OpenAIToClaudeStreamBridge)(nil)
}

// ---------------------------------------------------------------------------
// Registration
// ---------------------------------------------------------------------------

func TestOpenAIFormatMapper_Registered(t *testing.T) {
	m := GetMapper("claude", "openai")
	if m == nil {
		t.Fatal("expected claude:openai mapper to be registered")
	}
	if _, ok := m.(*OpenAIFormatMapper); !ok {
		t.Fatalf("expected *OpenAIFormatMapper, got %T", m)
	}
}
