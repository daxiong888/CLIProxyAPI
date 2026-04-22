package fastpath

import (
	"context"
	"strings"
	"testing"

	codexclaude "github.com/router-for-me/CLIProxyAPI/v6/internal/translator/codex/claude"
	"github.com/tidwall/gjson"
)

func TestCodexFormatMapper_IsEligible(t *testing.T) {
	m := &CodexFormatMapper{}

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
			name:     "base64 image",
			payload:  `{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"abc"}}]}]}`,
			eligible: true,
		},
		{
			name:     "url image rejected",
			payload:  `{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"https://example.com/img.png"}}]}]}`,
			eligible: false,
		},
		{
			name:     "unsupported content type",
			payload:  `{"messages":[{"role":"user","content":[{"type":"document","source":{"type":"base64"}}]}]}`,
			eligible: false,
		},
		{
			name:     "unsupported tool schema oneOf",
			payload:  `{"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}],"tools":[{"name":"search","input_schema":{"type":"object","properties":{"q":{"oneOf":[{"type":"string"},{"type":"number"}]}}}}]}`,
			eligible: false,
		},
		{
			name:     "no messages",
			payload:  `{"model":"claude-sonnet-4-20250514"}`,
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

func TestCodexFormatMapper_MapRequest_BasicText(t *testing.T) {
	m := &CodexFormatMapper{}
	input := []byte(`{"model":"claude-sonnet-4-20250514","messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}],"system":"You are helpful."}`)

	body, originalTranslated, err := m.MapRequest(input, input, "claude-sonnet-4-20250514")
	if err != nil {
		t.Fatalf("MapRequest error: %v", err)
	}

	// Verify model
	if got := gjson.GetBytes(body, "model").String(); got != "claude-sonnet-4-20250514" {
		t.Errorf("model = %q, want claude-sonnet-4-20250514", got)
	}

	// Verify system → developer input
	firstInput := gjson.GetBytes(body, "input.0")
	if firstInput.Get("role").String() != "developer" {
		t.Errorf("first input role = %q, want developer", firstInput.Get("role").String())
	}
	if firstInput.Get("content.0.type").String() != "input_text" {
		t.Errorf("first content type = %q, want input_text", firstInput.Get("content.0.type").String())
	}
	if firstInput.Get("content.0.text").String() != "You are helpful." {
		t.Errorf("system text = %q, want 'You are helpful.'", firstInput.Get("content.0.text").String())
	}

	// Verify user message → input_text
	secondInput := gjson.GetBytes(body, "input.1")
	if secondInput.Get("role").String() != "user" {
		t.Errorf("second input role = %q, want user", secondInput.Get("role").String())
	}
	if secondInput.Get("content.0.type").String() != "input_text" {
		t.Errorf("user content type = %q, want input_text", secondInput.Get("content.0.type").String())
	}

	// Verify stream and store defaults
	if !gjson.GetBytes(body, "stream").Bool() {
		t.Error("stream should be true")
	}
	if gjson.GetBytes(body, "store").Bool() {
		t.Error("store should be false")
	}

	// Both outputs should be equal when same input
	if string(body) != string(originalTranslated) {
		t.Error("body and originalTranslated should be equal for same input")
	}
}

func TestCodexFormatMapper_MapRequest_ParityWithTranslator(t *testing.T) {
	m := &CodexFormatMapper{}
	input := []byte(`{
		"model":"claude-sonnet-4-20250514",
		"system":[{"type":"text","text":"You are helpful."}],
		"messages":[
			{"role":"assistant","content":[{"type":"text","text":"Checking..."},{"type":"tool_use","id":"call_1","name":"mcp__weather__GetWeather","input":{"city":"SF"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":[{"type":"text","text":"Sunny"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"abc"}}]}]}
		],
		"tools":[{"name":"mcp__weather__GetWeather","description":"Get weather","input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}],
		"tool_choice":{"disable_parallel_tool_use":true},
		"thinking":{"type":"enabled","budget_tokens":8000}
	}`)

	body, _, err := m.MapRequest(input, input, "gpt-5")
	if err != nil {
		t.Fatalf("MapRequest error: %v", err)
	}
	want := codexclaude.ConvertClaudeRequestToCodex("gpt-5", input, false)
	if string(body) != string(want) {
		t.Fatalf("fast path request differs from translator\nfast: %s\nwant: %s", string(body), string(want))
	}
}

func TestCodexFormatMapper_MapRequest_ToolUse(t *testing.T) {
	m := &CodexFormatMapper{}
	input := []byte(`{
		"model":"claude-sonnet-4-20250514",
		"messages":[
			{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"Paris"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"22°C sunny"}]}
		],
		"tools":[{"name":"get_weather","description":"Get weather","input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}]
	}`)

	body, _, err := m.MapRequest(input, input, "claude-sonnet-4-20250514")
	if err != nil {
		t.Fatalf("MapRequest error: %v", err)
	}

	// tool_use → function_call
	fc := gjson.GetBytes(body, "input.0")
	if fc.Get("type").String() != "function_call" {
		t.Errorf("tool_use should map to function_call, got %q", fc.Get("type").String())
	}
	if fc.Get("call_id").String() != "toolu_1" {
		t.Errorf("call_id = %q, want toolu_1", fc.Get("call_id").String())
	}
	if fc.Get("name").String() != "get_weather" {
		t.Errorf("name = %q, want get_weather", fc.Get("name").String())
	}

	// tool_result → function_call_output
	fco := gjson.GetBytes(body, "input.1")
	if fco.Get("type").String() != "function_call_output" {
		t.Errorf("tool_result should map to function_call_output, got %q", fco.Get("type").String())
	}

	// tools declaration
	tool := gjson.GetBytes(body, "tools.0")
	if tool.Get("type").String() != "function" {
		t.Errorf("tool type = %q, want function", tool.Get("type").String())
	}
	if tool.Get("parameters.type").String() != "object" {
		t.Errorf("parameters.type = %q, want object", tool.Get("parameters.type").String())
	}
	if tool.Get("strict").Bool() {
		t.Error("strict should be false")
	}
}

func TestCodexFormatMapper_MapRequest_ToolNameShortening(t *testing.T) {
	m := &CodexFormatMapper{}
	longName := "mcp__my_very_long_server_name__my_very_long_tool_name_that_exceeds_limit"
	input := []byte(`{
		"model":"test",
		"messages":[
			{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"` + longName + `","input":{}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}
		],
		"tools":[{"name":"` + longName + `","description":"test","input_schema":{"type":"object"}}]
	}`)

	body, _, err := m.MapRequest(input, input, "test")
	if err != nil {
		t.Fatalf("MapRequest error: %v", err)
	}

	// Tool declaration name should be shortened
	toolName := gjson.GetBytes(body, "tools.0.name").String()
	if len(toolName) > 64 {
		t.Errorf("tool name length %d > 64: %s", len(toolName), toolName)
	}

	// function_call name should also be shortened
	fcName := gjson.GetBytes(body, "input.0.name").String()
	if len(fcName) > 64 {
		t.Errorf("function_call name length %d > 64: %s", len(fcName), fcName)
	}

	// Both should be the same short name
	if toolName != fcName {
		t.Errorf("tool name %q != function_call name %q", toolName, fcName)
	}
}

func TestCodexFormatMapper_MapRequest_DualPayload(t *testing.T) {
	m := &CodexFormatMapper{}
	payload := []byte(`{"model":"test","messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`)
	original := []byte(`{"model":"test","messages":[{"role":"user","content":[{"type":"text","text":"original"}]}]}`)

	body, originalTranslated, err := m.MapRequest(payload, original, "test")
	if err != nil {
		t.Fatalf("MapRequest error: %v", err)
	}

	// body should contain "hello"
	bodyText := gjson.GetBytes(body, "input.0.content.0.text").String()
	if bodyText != "hello" {
		t.Errorf("body text = %q, want hello", bodyText)
	}

	// originalTranslated should contain "original"
	origText := gjson.GetBytes(originalTranslated, "input.0.content.0.text").String()
	if origText != "original" {
		t.Errorf("original text = %q, want original", origText)
	}
}

func TestCodexFormatMapper_MapNonStreamResponse(t *testing.T) {
	m := &CodexFormatMapper{}
	originalRequest := []byte(`{"tools":[{"name":"get_weather","input_schema":{"type":"object"}}]}`)
	codexResponse := []byte(`{
		"type":"response.completed",
		"response":{
			"id":"resp_1",
			"model":"gpt-5",
			"output":[
				{"type":"message","content":[{"type":"output_text","text":"Hello!"}]},
				{"type":"function_call","name":"get_weather","call_id":"fc_1","arguments":"{\"city\":\"Paris\"}"}
			],
			"stop_reason":"",
			"usage":{"input_tokens":100,"output_tokens":50}
		}
	}`)

	out, err := m.MapNonStreamResponse(originalRequest, codexResponse)
	if err != nil {
		t.Fatalf("MapNonStreamResponse error: %v", err)
	}

	// Verify structure
	if gjson.GetBytes(out, "type").String() != "message" {
		t.Errorf("type = %q, want message", gjson.GetBytes(out, "type").String())
	}
	if gjson.GetBytes(out, "id").String() != "resp_1" {
		t.Errorf("id = %q, want resp_1", gjson.GetBytes(out, "id").String())
	}
	if gjson.GetBytes(out, "role").String() != "assistant" {
		t.Errorf("role = %q, want assistant", gjson.GetBytes(out, "role").String())
	}

	// Verify text content
	content := gjson.GetBytes(out, "content")
	if !content.IsArray() || len(content.Array()) != 2 {
		t.Fatalf("expected 2 content blocks, got %d", len(content.Array()))
	}
	if content.Array()[0].Get("type").String() != "text" {
		t.Errorf("first content type = %q, want text", content.Array()[0].Get("type").String())
	}
	if content.Array()[0].Get("text").String() != "Hello!" {
		t.Errorf("text = %q, want Hello!", content.Array()[0].Get("text").String())
	}

	// Verify tool_use content
	if content.Array()[1].Get("type").String() != "tool_use" {
		t.Errorf("second content type = %q, want tool_use", content.Array()[1].Get("type").String())
	}
	if content.Array()[1].Get("name").String() != "get_weather" {
		t.Errorf("tool name = %q, want get_weather", content.Array()[1].Get("name").String())
	}

	// Verify stop_reason
	if gjson.GetBytes(out, "stop_reason").String() != "tool_use" {
		t.Errorf("stop_reason = %q, want tool_use", gjson.GetBytes(out, "stop_reason").String())
	}

	// Verify usage
	if gjson.GetBytes(out, "usage.input_tokens").Int() != 100 {
		t.Errorf("input_tokens = %d, want 100", gjson.GetBytes(out, "usage.input_tokens").Int())
	}
	if gjson.GetBytes(out, "usage.output_tokens").Int() != 50 {
		t.Errorf("output_tokens = %d, want 50", gjson.GetBytes(out, "usage.output_tokens").Int())
	}
}

func TestCodexFormatMapper_MapNonStreamResponse_Reasoning(t *testing.T) {
	m := &CodexFormatMapper{}
	originalRequest := []byte(`{}`)
	codexResponse := []byte(`{
		"type":"response.completed",
		"response":{
			"id":"resp_2",
			"model":"gpt-5",
			"output":[
				{"type":"reasoning","summary":[{"text":"thinking about it..."}],"encrypted_content":"enc_sig_123"},
				{"type":"message","content":[{"type":"output_text","text":"Answer"}]}
			],
			"usage":{"input_tokens":10,"output_tokens":20}
		}
	}`)

	out, err := m.MapNonStreamResponse(originalRequest, codexResponse)
	if err != nil {
		t.Fatalf("MapNonStreamResponse error: %v", err)
	}

	content := gjson.GetBytes(out, "content")
	if len(content.Array()) != 2 {
		t.Fatalf("expected 2 content blocks, got %d", len(content.Array()))
	}

	// thinking block
	thinking := content.Array()[0]
	if thinking.Get("type").String() != "thinking" {
		t.Errorf("first content type = %q, want thinking", thinking.Get("type").String())
	}
	if thinking.Get("thinking").String() != "thinking about it..." {
		t.Errorf("thinking text = %q", thinking.Get("thinking").String())
	}
	if thinking.Get("signature").String() != "enc_sig_123" {
		t.Errorf("signature = %q, want enc_sig_123", thinking.Get("signature").String())
	}

	// text block
	if content.Array()[1].Get("type").String() != "text" {
		t.Errorf("second content type = %q, want text", content.Array()[1].Get("type").String())
	}
}

func TestCodexFormatMapper_MapNonStreamResponse_NotCompleted(t *testing.T) {
	m := &CodexFormatMapper{}
	_, err := m.MapNonStreamResponse([]byte(`{}`), []byte(`{"type":"response.created"}`))
	if err == nil {
		t.Error("expected error for non-completed response")
	}
}

func TestCodexFormatMapper_MapRequest_ThinkingEnabled(t *testing.T) {
	m := &CodexFormatMapper{}
	input := []byte(`{"model":"test","messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":8192}}`)

	body, _, err := m.MapRequest(input, input, "test")
	if err != nil {
		t.Fatalf("MapRequest error: %v", err)
	}

	effort := gjson.GetBytes(body, "reasoning.effort").String()
	if effort == "" {
		t.Error("reasoning.effort should be set")
	}
}

func TestCodexFormatMapper_MapRequest_ThinkingDisabled(t *testing.T) {
	m := &CodexFormatMapper{}
	input := []byte(`{"model":"test","messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled"}}`)

	body, _, err := m.MapRequest(input, input, "test")
	if err != nil {
		t.Fatalf("MapRequest error: %v", err)
	}

	effort := gjson.GetBytes(body, "reasoning.effort").String()
	// disabled thinking should still produce a reasoning.effort (mapped from budget 0)
	if effort == "" {
		t.Error("reasoning.effort should be set even when thinking is disabled")
	}
}

func TestGetMapper(t *testing.T) {
	// claude→codex should be registered via init()
	m := GetMapper("claude", "codex")
	if m == nil {
		t.Fatal("expected mapper for claude→codex")
	}

	// unknown route
	m = GetMapper("gemini", "codex")
	if m != nil {
		t.Error("expected nil for gemini→codex")
	}
}

func TestCodexFormatMapper_MapRequest_WebSearchTool(t *testing.T) {
	m := &CodexFormatMapper{}
	input := []byte(`{
		"model":"test",
		"messages":[{"role":"user","content":"search for cats"}],
		"tools":[
			{"type":"web_search_20250305"},
			{"name":"get_info","description":"info","input_schema":{"type":"object"}}
		]
	}`)

	body, _, err := m.MapRequest(input, input, "test")
	if err != nil {
		t.Fatalf("MapRequest error: %v", err)
	}

	tools := gjson.GetBytes(body, "tools")
	if len(tools.Array()) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(tools.Array()))
	}

	// web_search_20250305 → web_search
	if tools.Array()[0].Get("type").String() != "web_search" {
		t.Errorf("first tool type = %q, want web_search", tools.Array()[0].Get("type").String())
	}

	// regular tool
	if tools.Array()[1].Get("type").String() != "function" {
		t.Errorf("second tool type = %q, want function", tools.Array()[1].Get("type").String())
	}
}

func TestCodexFormatMapper_MapNonStreamResponse_CachedTokens(t *testing.T) {
	m := &CodexFormatMapper{}
	codexResponse := []byte(`{
		"type":"response.completed",
		"response":{
			"id":"resp_3",
			"model":"gpt-5",
			"output":[{"type":"message","content":[{"type":"output_text","text":"hi"}]}],
			"usage":{"input_tokens":200,"output_tokens":50,"input_tokens_details":{"cached_tokens":150}}
		}
	}`)

	out, err := m.MapNonStreamResponse([]byte(`{}`), codexResponse)
	if err != nil {
		t.Fatalf("MapNonStreamResponse error: %v", err)
	}

	// input_tokens should be 200-150=50 (cached subtracted)
	if gjson.GetBytes(out, "usage.input_tokens").Int() != 50 {
		t.Errorf("input_tokens = %d, want 50", gjson.GetBytes(out, "usage.input_tokens").Int())
	}
	if gjson.GetBytes(out, "usage.cache_read_input_tokens").Int() != 150 {
		t.Errorf("cache_read_input_tokens = %d, want 150", gjson.GetBytes(out, "usage.cache_read_input_tokens").Int())
	}
}

// --- Stream bridge tests ---

func feedLine(b *CodexToClaudeStreamBridge, event string) string {
	chunks := b.ProcessLine(context.Background(), []byte("data: "+event))
	var sb strings.Builder
	for _, c := range chunks {
		sb.Write(c)
	}
	return sb.String()
}

func TestStreamBridge_NonDataLine(t *testing.T) {
	b := &CodexToClaudeStreamBridge{}
	if chunks := b.ProcessLine(context.Background(), []byte("event: ping")); chunks != nil {
		t.Errorf("expected nil for non-data line, got %d chunks", len(chunks))
	}
	if chunks := b.ProcessLine(context.Background(), []byte("")); chunks != nil {
		t.Errorf("expected nil for empty line, got %d chunks", len(chunks))
	}
}

func TestStreamBridge_ResponseCreated(t *testing.T) {
	b := &CodexToClaudeStreamBridge{}
	out := feedLine(b, `{"type":"response.created","response":{"id":"resp_1","model":"gpt-5"}}`)
	if !strings.Contains(out, "message_start") {
		t.Error("expected message_start event")
	}
	if !strings.Contains(out, `"id":"resp_1"`) {
		t.Error("expected response id")
	}
	if !strings.Contains(out, `"model":"gpt-5"`) {
		t.Error("expected model name")
	}
}

func TestStreamBridge_TextFlow(t *testing.T) {
	b := &CodexToClaudeStreamBridge{}

	out := feedLine(b, `{"type":"response.content_part.added","part":{"type":"output_text","text":""}}`)
	if !strings.Contains(out, "content_block_start") {
		t.Error("expected content_block_start for text")
	}
	if !strings.Contains(out, `"type":"text"`) {
		t.Error("expected text block type")
	}

	out = feedLine(b, `{"type":"response.output_text.delta","delta":"Hello"}`)
	if !strings.Contains(out, "content_block_delta") {
		t.Error("expected content_block_delta")
	}
	if !strings.Contains(out, `"type":"text_delta"`) {
		t.Error("expected text_delta type")
	}

	out = feedLine(b, `{"type":"response.content_part.done","part":{"type":"output_text","text":"Hello"}}`)
	if !strings.Contains(out, "content_block_stop") {
		t.Error("expected content_block_stop")
	}
	if b.blockIndex != 1 {
		t.Errorf("blockIndex = %d, want 1", b.blockIndex)
	}
}

func TestStreamBridge_FunctionCallFlow(t *testing.T) {
	origReq := []byte(`{"tools":[{"name":"get_weather","input_schema":{"type":"object"}}]}`)
	m := &CodexFormatMapper{}
	b := m.NewStreamBridge(origReq).(*CodexToClaudeStreamBridge)

	out := feedLine(b, `{"type":"response.output_item.added","item":{"type":"function_call","call_id":"fc_1","name":"get_weather"}}`)
	if !strings.Contains(out, "content_block_start") {
		t.Error("expected content_block_start for tool_use")
	}
	if !strings.Contains(out, `"type":"tool_use"`) {
		t.Error("expected tool_use type")
	}
	if !strings.Contains(out, `"name":"get_weather"`) {
		t.Error("expected tool name")
	}
	if !strings.Contains(out, "input_json_delta") {
		t.Error("expected initial empty input_json_delta")
	}
	if !b.hasToolCall {
		t.Error("hasToolCall should be true")
	}

	out = feedLine(b, `{"type":"response.function_call_arguments.delta","delta":"{\"city\":"}`)
	if !strings.Contains(out, "input_json_delta") {
		t.Error("expected input_json_delta for args delta")
	}

	out = feedLine(b, `{"type":"response.output_item.done","item":{"type":"function_call","call_id":"fc_1","name":"get_weather","arguments":"{\"city\":\"Paris\"}"}}`)
	if !strings.Contains(out, "content_block_stop") {
		t.Error("expected content_block_stop for function_call done")
	}
}

func TestStreamBridge_FunctionCallArgsDone_NoDeltas(t *testing.T) {
	b := &CodexToClaudeStreamBridge{}
	// Simulate function_call_arguments.done without prior deltas.
	b.hasReceivedArgsDelta = false
	out := feedLine(b, `{"type":"response.function_call_arguments.done","arguments":"{\"x\":1}"}`)
	if !strings.Contains(out, "input_json_delta") {
		t.Error("expected input_json_delta backfill when no deltas received")
	}
}

func TestStreamBridge_ThinkingFlow(t *testing.T) {
	b := &CodexToClaudeStreamBridge{}

	// reasoning item added with signature
	feedLine(b, `{"type":"response.output_item.added","item":{"type":"reasoning","encrypted_content":"sig_abc"}}`)
	if b.thinkingSignature != "sig_abc" {
		t.Errorf("thinkingSignature = %q, want sig_abc", b.thinkingSignature)
	}

	out := feedLine(b, `{"type":"response.reasoning_summary_part.added"}`)
	if !strings.Contains(out, "content_block_start") {
		t.Error("expected content_block_start for thinking")
	}
	if !strings.Contains(out, `"type":"thinking"`) {
		t.Error("expected thinking type")
	}

	out = feedLine(b, `{"type":"response.reasoning_summary_text.delta","delta":"let me think..."}`)
	if !strings.Contains(out, "thinking_delta") {
		t.Error("expected thinking_delta")
	}

	// summary done → thinkingStopPending, signature exists → finalize immediately
	out = feedLine(b, `{"type":"response.reasoning_summary_part.done"}`)
	if !strings.Contains(out, "signature_delta") {
		t.Error("expected signature_delta on finalize")
	}
	if !strings.Contains(out, "content_block_stop") {
		t.Error("expected content_block_stop on finalize")
	}
}

func TestStreamBridge_TextBackfill(t *testing.T) {
	b := &CodexToClaudeStreamBridge{}
	// output_item.done with message type, no prior text deltas → backfill
	out := feedLine(b, `{"type":"response.output_item.done","item":{"type":"message","content":[{"type":"output_text","text":"Backfilled!"}]}}`)
	if !strings.Contains(out, "content_block_start") {
		t.Error("expected content_block_start for backfill")
	}
	if !strings.Contains(out, "text_delta") {
		t.Error("expected text_delta for backfill")
	}
	if !strings.Contains(out, "Backfilled!") {
		t.Error("expected backfilled text content")
	}
	if !strings.Contains(out, "content_block_stop") {
		t.Error("expected content_block_stop for backfill")
	}
	if !b.hasTextDelta {
		t.Error("hasTextDelta should be true after backfill")
	}
}

func TestStreamBridge_TextBackfill_SkippedWhenDeltaReceived(t *testing.T) {
	b := &CodexToClaudeStreamBridge{}
	b.hasTextDelta = true
	out := feedLine(b, `{"type":"response.output_item.done","item":{"type":"message","content":[{"type":"output_text","text":"should be skipped"}]}}`)
	if strings.Contains(out, "should be skipped") {
		t.Error("backfill should be skipped when hasTextDelta is true")
	}
}

func TestStreamBridge_Completed_EndTurn(t *testing.T) {
	b := &CodexToClaudeStreamBridge{}
	out := feedLine(b, `{"type":"response.completed","response":{"stop_reason":"","usage":{"input_tokens":100,"output_tokens":50}}}`)
	if !strings.Contains(out, "message_delta") {
		t.Error("expected message_delta")
	}
	if !strings.Contains(out, `"stop_reason":"end_turn"`) {
		t.Error("expected end_turn stop reason")
	}
	if !strings.Contains(out, "message_stop") {
		t.Error("expected message_stop")
	}
}

func TestStreamBridge_Completed_ToolUse(t *testing.T) {
	b := &CodexToClaudeStreamBridge{hasToolCall: true}
	out := feedLine(b, `{"type":"response.completed","response":{"usage":{"input_tokens":10,"output_tokens":5}}}`)
	if !strings.Contains(out, `"stop_reason":"tool_use"`) {
		t.Error("expected tool_use stop reason")
	}
}

func TestStreamBridge_Completed_MaxTokens(t *testing.T) {
	b := &CodexToClaudeStreamBridge{}
	out := feedLine(b, `{"type":"response.completed","response":{"stop_reason":"max_tokens","usage":{"input_tokens":10,"output_tokens":5}}}`)
	if !strings.Contains(out, `"stop_reason":"max_tokens"`) {
		t.Error("expected max_tokens stop reason")
	}
}

func TestStreamBridge_ToolNameReverse(t *testing.T) {
	longName := "mcp__my_very_long_server_name__my_very_long_tool_name_that_exceeds_limit"
	origReq := []byte(`{"tools":[{"name":"` + longName + `","input_schema":{"type":"object"}}]}`)
	m := &CodexFormatMapper{}
	b := m.NewStreamBridge(origReq).(*CodexToClaudeStreamBridge)

	// The short name used by Codex should be reversed to the original long name.
	shortName := ShortenNameIfNeeded(longName)
	out := feedLine(b, `{"type":"response.output_item.added","item":{"type":"function_call","call_id":"fc_1","name":"`+shortName+`"}}`)
	if !strings.Contains(out, longName) {
		t.Errorf("expected original long name %q in output, got: %s", longName, out)
	}
}

func TestStreamBridge_Finalize(t *testing.T) {
	b := &CodexToClaudeStreamBridge{}
	if chunks := b.Finalize(); chunks != nil {
		t.Errorf("expected nil from Finalize, got %d chunks", len(chunks))
	}
}

func TestStreamBridge_UnknownEventType(t *testing.T) {
	b := &CodexToClaudeStreamBridge{}
	chunks := b.ProcessLine(context.Background(), []byte(`data: {"type":"response.some_future_event","data":"ignored"}`))
	if chunks != nil {
		t.Errorf("expected nil for unknown event type, got %d chunks", len(chunks))
	}
}

func TestStreamBridge_DeferredThinkingFinalize(t *testing.T) {
	b := &CodexToClaudeStreamBridge{
		thinkingBlockOpen:   true,
		thinkingStopPending: true,
		thinkingSignature:   "sig_xyz",
	}
	// content_part.added should trigger deferred thinking finalization
	out := feedLine(b, `{"type":"response.content_part.added","part":{"type":"output_text"}}`)
	if !strings.Contains(out, "signature_delta") {
		t.Error("expected signature_delta from deferred thinking finalize")
	}
	if !strings.Contains(out, "content_block_stop") {
		t.Error("expected content_block_stop from deferred thinking finalize")
	}
	if b.thinkingBlockOpen {
		t.Error("thinkingBlockOpen should be false after finalization")
	}
}
