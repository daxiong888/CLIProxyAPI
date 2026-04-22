package fastpath

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	translatorcommon "github.com/router-for-me/CLIProxyAPI/v6/internal/translator/common"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func init() {
	RegisterMapper("claude", "codex", &CodexFormatMapper{})
}

// CodexFormatMapper implements FormatMapper for Claude → Codex translation.
type CodexFormatMapper struct{}

// IsEligible checks whether a Claude payload can use the Codex fast path.
func (m *CodexFormatMapper) IsEligible(claudePayload []byte) (bool, string) {
	if len(claudePayload) == 0 {
		return false, "empty payload"
	}
	root := gjson.ParseBytes(claudePayload)
	if unsupported, reason := hasUnsupportedToolSchema(claudePayload); unsupported {
		return false, reason
	}

	// Check messages for unsupported content types.
	messages := root.Get("messages")
	if messages.IsArray() {
		for _, msg := range messages.Array() {
			content := msg.Get("content")
			if !content.IsArray() {
				continue
			}
			for _, part := range content.Array() {
				ct := part.Get("type").String()
				switch ct {
				case "text", "tool_use", "tool_result":
					// supported
				case "image":
					src := part.Get("source")
					if !src.Exists() {
						return false, "image without source"
					}
					srcType := src.Get("type").String()
					if srcType != "" && srcType != "base64" {
						// Fast path only handles base64 images; URL images fall back to generic translator.
						return false, "image source type " + srcType + " not supported by fast path, falling back to generic translator"
					}
				default:
					return false, "unsupported content type: " + ct
				}
			}
		}
	}

	return true, ""
}

// MapRequest converts a Claude-format payload to Codex format.
// Returns (mappedBody, mappedOriginal, error).
func (m *CodexFormatMapper) MapRequest(claudePayload, originalPayload []byte, model string) ([]byte, []byte, error) {
	body := convertClaudeToCodex(claudePayload, model)
	originalTranslated := body
	if len(originalPayload) > 0 && &originalPayload[0] != &claudePayload[0] {
		originalTranslated = convertClaudeToCodex(originalPayload, model)
	}
	return body, originalTranslated, nil
}

// NewStreamBridge creates a stream bridge for Codex → Claude SSE translation.
func (m *CodexFormatMapper) NewStreamBridge(originalRequest []byte) StreamBridge {
	return &CodexToClaudeStreamBridge{
		revNames: BuildReverseNameMap(originalRequest),
	}
}

// MapNonStreamResponse converts a Codex response.completed event to a Claude Messages response.
func (m *CodexFormatMapper) MapNonStreamResponse(originalRequest, targetResponse []byte) ([]byte, error) {
	revNames := BuildReverseNameMap(originalRequest)
	out := convertCodexCompletedToClaude(targetResponse, revNames)
	if len(out) == 0 {
		return nil, fmt.Errorf("empty response from codex completed event")
	}
	return out, nil
}

// convertClaudeToCodex translates a Claude Messages request to Codex Responses format.
func convertClaudeToCodex(rawJSON []byte, modelName string) []byte {
	template := []byte(`{"model":"","instructions":"","input":[]}`)
	root := gjson.ParseBytes(rawJSON)
	template, _ = sjson.SetBytes(template, "model", modelName)

	// Build short name map once for reuse in both tools and tool_use messages.
	shortMap := BuildForwardNameMap(rawJSON)

	// Process system messages → developer input.
	if systemsResult := root.Get("system"); systemsResult.Exists() {
		message := []byte(`{"type":"message","role":"developer","content":[]}`)
		contentIndex := 0

		appendSystemText := func(text string) {
			// Skip billing metadata injected by Anthropic SDKs; forwarding it to Codex is meaningless.
			if text == "" || strings.HasPrefix(text, "x-anthropic-billing-header: ") {
				return
			}
			message, _ = sjson.SetBytes(message, fmt.Sprintf("content.%d.type", contentIndex), "input_text")
			message, _ = sjson.SetBytes(message, fmt.Sprintf("content.%d.text", contentIndex), text)
			contentIndex++
		}

		if systemsResult.Type == gjson.String {
			appendSystemText(systemsResult.String())
		} else if systemsResult.IsArray() {
			for _, sys := range systemsResult.Array() {
				if sys.Get("type").String() == "text" {
					appendSystemText(sys.Get("text").String())
				}
			}
		}

		if contentIndex > 0 {
			template, _ = sjson.SetRawBytes(template, "input.-1", message)
		}
	}

	// Process messages.
	if messagesResult := root.Get("messages"); messagesResult.IsArray() {
		for _, messageResult := range messagesResult.Array() {
			messageRole := messageResult.Get("role").String()

			newMessage := func() []byte {
				msg := []byte(`{"type":"message","role":"","content":[]}`)
				msg, _ = sjson.SetBytes(msg, "role", messageRole)
				return msg
			}

			message := newMessage()
			contentIndex := 0
			hasContent := false

			flushMessage := func() {
				if hasContent {
					template, _ = sjson.SetRawBytes(template, "input.-1", message)
					message = newMessage()
					contentIndex = 0
					hasContent = false
				}
			}

			appendTextContent := func(text string) {
				partType := "input_text"
				if messageRole == "assistant" {
					partType = "output_text"
				}
				message, _ = sjson.SetBytes(message, fmt.Sprintf("content.%d.type", contentIndex), partType)
				message, _ = sjson.SetBytes(message, fmt.Sprintf("content.%d.text", contentIndex), text)
				contentIndex++
				hasContent = true
			}

			appendImageContent := func(dataURL string) {
				message, _ = sjson.SetBytes(message, fmt.Sprintf("content.%d.type", contentIndex), "input_image")
				message, _ = sjson.SetBytes(message, fmt.Sprintf("content.%d.image_url", contentIndex), dataURL)
				contentIndex++
				hasContent = true
			}

			messageContentsResult := messageResult.Get("content")
			if messageContentsResult.IsArray() {
				for _, part := range messageContentsResult.Array() {
					contentType := part.Get("type").String()
					switch contentType {
					case "text":
						appendTextContent(part.Get("text").String())
					case "image":
						sourceResult := part.Get("source")
						if sourceResult.Exists() {
							data := sourceResult.Get("data").String()
							if data == "" {
								data = sourceResult.Get("base64").String()
							}
							if data != "" {
								mediaType := sourceResult.Get("media_type").String()
								if mediaType == "" {
									mediaType = sourceResult.Get("mime_type").String()
								}
								if mediaType == "" {
									mediaType = "application/octet-stream"
								}
								appendImageContent(fmt.Sprintf("data:%s;base64,%s", mediaType, data))
							}
						}
					case "tool_use":
						flushMessage()
						fc := []byte(`{"type":"function_call"}`)
						fc, _ = sjson.SetBytes(fc, "call_id", part.Get("id").String())
						name := part.Get("name").String()
						if shortMap != nil {
							if short, ok := shortMap[name]; ok {
								name = short
							} else {
								name = ShortenNameIfNeeded(name)
							}
						}
						fc, _ = sjson.SetBytes(fc, "name", name)
						fc, _ = sjson.SetBytes(fc, "arguments", part.Get("input").Raw)
						template, _ = sjson.SetRawBytes(template, "input.-1", fc)
					case "tool_result":
						flushMessage()
						fco := []byte(`{"type":"function_call_output"}`)
						fco, _ = sjson.SetBytes(fco, "call_id", part.Get("tool_use_id").String())
						contentResult := part.Get("content")
						if contentResult.IsArray() {
							toolResultContent := []byte(`[]`)
							toolResultContentIndex := 0
							for _, cr := range contentResult.Array() {
								crType := cr.Get("type").String()
								if crType == "image" {
									sourceResult := cr.Get("source")
									if sourceResult.Exists() {
										data := sourceResult.Get("data").String()
										if data == "" {
											data = sourceResult.Get("base64").String()
										}
										if data != "" {
											mediaType := sourceResult.Get("media_type").String()
											if mediaType == "" {
												mediaType = sourceResult.Get("mime_type").String()
											}
											if mediaType == "" {
												mediaType = "application/octet-stream"
											}
											toolResultContent, _ = sjson.SetBytes(toolResultContent, fmt.Sprintf("%d.type", toolResultContentIndex), "input_image")
											toolResultContent, _ = sjson.SetBytes(toolResultContent, fmt.Sprintf("%d.image_url", toolResultContentIndex), fmt.Sprintf("data:%s;base64,%s", mediaType, data))
											toolResultContentIndex++
										}
									}
								} else if crType == "text" {
									toolResultContent, _ = sjson.SetBytes(toolResultContent, fmt.Sprintf("%d.type", toolResultContentIndex), "input_text")
									toolResultContent, _ = sjson.SetBytes(toolResultContent, fmt.Sprintf("%d.text", toolResultContentIndex), cr.Get("text").String())
									toolResultContentIndex++
								}
							}
							if toolResultContentIndex > 0 {
								fco, _ = sjson.SetRawBytes(fco, "output", toolResultContent)
							} else {
								fco, _ = sjson.SetBytes(fco, "output", part.Get("content").String())
							}
						} else {
							fco, _ = sjson.SetBytes(fco, "output", part.Get("content").String())
						}
						template, _ = sjson.SetRawBytes(template, "input.-1", fco)
					}
				}
				flushMessage()
			} else if messageContentsResult.Type == gjson.String {
				appendTextContent(messageContentsResult.String())
				flushMessage()
			}
		}
	}

	// Convert tools declarations.
	if toolsResult := root.Get("tools"); toolsResult.IsArray() {
		template, _ = sjson.SetRawBytes(template, "tools", []byte(`[]`))
		template, _ = sjson.SetBytes(template, "tool_choice", `auto`)
		for _, toolResult := range toolsResult.Array() {
			if toolResult.Get("type").String() == "web_search_20250305" {
				template, _ = sjson.SetRawBytes(template, "tools.-1", []byte(`{"type":"web_search"}`))
				continue
			}
			tool := []byte(toolResult.Raw)
			tool, _ = sjson.SetBytes(tool, "type", "function")
			if v := toolResult.Get("name"); v.Exists() {
				name := v.String()
				if shortMap != nil {
					if short, ok := shortMap[name]; ok {
						name = short
					} else {
						name = ShortenNameIfNeeded(name)
					}
				}
				tool, _ = sjson.SetBytes(tool, "name", name)
			}
			tool, _ = sjson.SetRawBytes(tool, "parameters", []byte(normalizeToolParameters(toolResult.Get("input_schema").Raw)))
			tool, _ = sjson.DeleteBytes(tool, "input_schema")
			tool, _ = sjson.DeleteBytes(tool, "parameters.$schema")
			tool, _ = sjson.DeleteBytes(tool, "cache_control")
			tool, _ = sjson.DeleteBytes(tool, "defer_loading")
			tool, _ = sjson.SetBytes(tool, "strict", false)
			template, _ = sjson.SetRawBytes(template, "tools.-1", tool)
		}
	}

	// Parallel tool calls.
	parallelToolCalls := true
	if disableParallelToolUse := root.Get("tool_choice.disable_parallel_tool_use"); disableParallelToolUse.Exists() {
		parallelToolCalls = !disableParallelToolUse.Bool()
	}
	template, _ = sjson.SetBytes(template, "parallel_tool_calls", parallelToolCalls)

	// Convert thinking → reasoning.
	reasoningEffort := "medium"
	if thinkingConfig := root.Get("thinking"); thinkingConfig.Exists() && thinkingConfig.IsObject() {
		switch thinkingConfig.Get("type").String() {
		case "enabled":
			if budgetTokens := thinkingConfig.Get("budget_tokens"); budgetTokens.Exists() {
				budget := int(budgetTokens.Int())
				if effort, ok := thinking.ConvertBudgetToLevel(budget); ok && effort != "" {
					reasoningEffort = effort
				}
			}
		case "adaptive", "auto":
			effort := ""
			if v := root.Get("output_config.effort"); v.Exists() && v.Type == gjson.String {
				effort = strings.ToLower(strings.TrimSpace(v.String()))
			}
			if effort != "" {
				reasoningEffort = effort
			} else {
				reasoningEffort = string(thinking.LevelXHigh)
			}
		case "disabled":
			if effort, ok := thinking.ConvertBudgetToLevel(0); ok && effort != "" {
				reasoningEffort = effort
			}
		}
	}
	template, _ = sjson.SetBytes(template, "reasoning.effort", reasoningEffort)
	template, _ = sjson.SetBytes(template, "reasoning.summary", "auto")
	template, _ = sjson.SetBytes(template, "stream", true)
	template, _ = sjson.SetBytes(template, "store", false)
	template, _ = sjson.SetBytes(template, "include", []string{"reasoning.encrypted_content"})

	return template
}

// convertCodexCompletedToClaude converts a Codex response.completed event to a Claude Messages response.
func convertCodexCompletedToClaude(rawJSON []byte, revNames map[string]string) []byte {
	rootResult := gjson.ParseBytes(rawJSON)
	if rootResult.Get("type").String() != "response.completed" {
		return nil
	}

	responseData := rootResult.Get("response")
	if !responseData.Exists() {
		return nil
	}

	out := []byte(`{"id":"","type":"message","role":"assistant","model":"","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}`)
	out, _ = sjson.SetBytes(out, "id", responseData.Get("id").String())
	out, _ = sjson.SetBytes(out, "model", responseData.Get("model").String())
	inputTokens, outputTokens, cachedTokens := extractResponsesUsage(responseData.Get("usage"))
	out, _ = sjson.SetBytes(out, "usage.input_tokens", inputTokens)
	out, _ = sjson.SetBytes(out, "usage.output_tokens", outputTokens)
	if cachedTokens > 0 {
		out, _ = sjson.SetBytes(out, "usage.cache_read_input_tokens", cachedTokens)
	}

	hasToolCall := false

	if output := responseData.Get("output"); output.Exists() && output.IsArray() {
		output.ForEach(func(_, item gjson.Result) bool {
			switch item.Get("type").String() {
			case "reasoning":
				var thinkingBuilder strings.Builder
				signature := item.Get("encrypted_content").String()
				if summary := item.Get("summary"); summary.Exists() {
					if summary.IsArray() {
						summary.ForEach(func(_, part gjson.Result) bool {
							if txt := part.Get("text"); txt.Exists() {
								thinkingBuilder.WriteString(txt.String())
							} else {
								thinkingBuilder.WriteString(part.String())
							}
							return true
						})
					} else {
						thinkingBuilder.WriteString(summary.String())
					}
				}
				if thinkingBuilder.Len() == 0 {
					if content := item.Get("content"); content.Exists() {
						if content.IsArray() {
							content.ForEach(func(_, part gjson.Result) bool {
								if txt := part.Get("text"); txt.Exists() {
									thinkingBuilder.WriteString(txt.String())
								} else {
									thinkingBuilder.WriteString(part.String())
								}
								return true
							})
						} else {
							thinkingBuilder.WriteString(content.String())
						}
					}
				}
				if thinkingBuilder.Len() > 0 || signature != "" {
					block := []byte(`{"type":"thinking","thinking":""}`)
					block, _ = sjson.SetBytes(block, "thinking", thinkingBuilder.String())
					if signature != "" {
						block, _ = sjson.SetBytes(block, "signature", signature)
					}
					out, _ = sjson.SetRawBytes(out, "content.-1", block)
				}
			case "message":
				if content := item.Get("content"); content.Exists() {
					if content.IsArray() {
						content.ForEach(func(_, part gjson.Result) bool {
							if part.Get("type").String() == "output_text" {
								if text := part.Get("text").String(); text != "" {
									block := []byte(`{"type":"text","text":""}`)
									block, _ = sjson.SetBytes(block, "text", text)
									out, _ = sjson.SetRawBytes(out, "content.-1", block)
								}
							}
							return true
						})
					} else {
						if text := content.String(); text != "" {
							block := []byte(`{"type":"text","text":""}`)
							block, _ = sjson.SetBytes(block, "text", text)
							out, _ = sjson.SetRawBytes(out, "content.-1", block)
						}
					}
				}
			case "function_call":
				hasToolCall = true
				name := item.Get("name").String()
				if original, ok := revNames[name]; ok {
					name = original
				}
				toolBlock := []byte(`{"type":"tool_use","id":"","name":"","input":{}}`)
				toolBlock, _ = sjson.SetBytes(toolBlock, "id", util.SanitizeClaudeToolID(item.Get("call_id").String()))
				toolBlock, _ = sjson.SetBytes(toolBlock, "name", name)
				inputRaw := "{}"
				if argsStr := item.Get("arguments").String(); argsStr != "" && gjson.Valid(argsStr) {
					argsJSON := gjson.Parse(argsStr)
					if argsJSON.IsObject() {
						inputRaw = argsJSON.Raw
					}
				}
				toolBlock, _ = sjson.SetRawBytes(toolBlock, "input", []byte(inputRaw))
				out, _ = sjson.SetRawBytes(out, "content.-1", toolBlock)
			}
			return true
		})
	}

	if stopReason := responseData.Get("stop_reason"); stopReason.Exists() && stopReason.String() != "" {
		out, _ = sjson.SetBytes(out, "stop_reason", stopReason.String())
	} else if hasToolCall {
		out, _ = sjson.SetBytes(out, "stop_reason", "tool_use")
	} else {
		out, _ = sjson.SetBytes(out, "stop_reason", "end_turn")
	}

	if stopSequence := responseData.Get("stop_sequence"); stopSequence.Exists() && stopSequence.String() != "" {
		out, _ = sjson.SetRawBytes(out, "stop_sequence", []byte(stopSequence.Raw))
	}

	return out
}

func extractResponsesUsage(usage gjson.Result) (int64, int64, int64) {
	if !usage.Exists() || usage.Type == gjson.Null {
		return 0, 0, 0
	}
	inputTokens := usage.Get("input_tokens").Int()
	outputTokens := usage.Get("output_tokens").Int()
	cachedTokens := usage.Get("input_tokens_details.cached_tokens").Int()
	if cachedTokens > 0 {
		if inputTokens >= cachedTokens {
			inputTokens -= cachedTokens
		} else {
			inputTokens = 0
		}
	}
	return inputTokens, outputTokens, cachedTokens
}

func normalizeToolParameters(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" || !gjson.Valid(raw) {
		return `{"type":"object","properties":{}}`
	}
	result := gjson.Parse(raw)
	schema := []byte(raw)
	schemaType := result.Get("type").String()
	if schemaType == "" {
		schema, _ = sjson.SetBytes(schema, "type", "object")
		schemaType = "object"
	}
	if schemaType == "object" && !result.Get("properties").Exists() {
		schema, _ = sjson.SetRawBytes(schema, "properties", []byte(`{}`))
	}
	return string(schema)
}

var sseDataPrefix = []byte("data:")

// CodexToClaudeStreamBridge translates Codex SSE events to Claude SSE format.
type CodexToClaudeStreamBridge struct {
	revNames map[string]string

	blockIndex           int
	hasToolCall          bool
	hasReceivedArgsDelta bool
	hasTextDelta         bool
	textBlockOpen        bool
	thinkingBlockOpen    bool
	thinkingStopPending  bool
	thinkingSignature    string
}

// ProcessLine handles one SSE line and returns zero or more Claude SSE chunks.
func (b *CodexToClaudeStreamBridge) ProcessLine(_ context.Context, line []byte) [][]byte {
	if !bytes.HasPrefix(line, sseDataPrefix) {
		return nil
	}
	rawJSON := bytes.TrimSpace(line[len(sseDataPrefix):])

	output := make([]byte, 0, 512)
	root := gjson.ParseBytes(rawJSON)

	// Deferred thinking block finalization on transition to new content.
	if b.thinkingBlockOpen && b.thinkingStopPending {
		switch root.Get("type").String() {
		case "response.content_part.added", "response.completed":
			output = append(output, b.finalizeThinkingBlock()...)
		}
	}

	switch root.Get("type").String() {
	case "response.created":
		tpl := []byte(`{"type":"message_start","message":{"id":"","type":"message","role":"assistant","model":"","stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0},"content":[],"stop_reason":null}}`)
		tpl, _ = sjson.SetBytes(tpl, "message.model", root.Get("response.model").String())
		tpl, _ = sjson.SetBytes(tpl, "message.id", root.Get("response.id").String())
		output = translatorcommon.AppendSSEEventBytes(output, "message_start", tpl, 2)

	case "response.reasoning_summary_part.added":
		if b.thinkingBlockOpen && b.thinkingStopPending {
			output = append(output, b.finalizeThinkingBlock()...)
		}
		tpl := []byte(`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`)
		tpl, _ = sjson.SetBytes(tpl, "index", b.blockIndex)
		b.thinkingBlockOpen = true
		b.thinkingStopPending = false
		output = translatorcommon.AppendSSEEventBytes(output, "content_block_start", tpl, 2)

	case "response.reasoning_summary_text.delta":
		tpl := []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":""}}`)
		tpl, _ = sjson.SetBytes(tpl, "index", b.blockIndex)
		tpl, _ = sjson.SetBytes(tpl, "delta.thinking", root.Get("delta").String())
		output = translatorcommon.AppendSSEEventBytes(output, "content_block_delta", tpl, 2)

	case "response.reasoning_summary_part.done":
		b.thinkingStopPending = true
		if b.thinkingSignature != "" {
			output = append(output, b.finalizeThinkingBlock()...)
		}

	case "response.content_part.added":
		tpl := []byte(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
		tpl, _ = sjson.SetBytes(tpl, "index", b.blockIndex)
		b.textBlockOpen = true
		output = translatorcommon.AppendSSEEventBytes(output, "content_block_start", tpl, 2)

	case "response.output_text.delta":
		b.hasTextDelta = true
		tpl := []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":""}}`)
		tpl, _ = sjson.SetBytes(tpl, "index", b.blockIndex)
		tpl, _ = sjson.SetBytes(tpl, "delta.text", root.Get("delta").String())
		output = translatorcommon.AppendSSEEventBytes(output, "content_block_delta", tpl, 2)

	case "response.content_part.done":
		tpl := []byte(`{"type":"content_block_stop","index":0}`)
		tpl, _ = sjson.SetBytes(tpl, "index", b.blockIndex)
		b.textBlockOpen = false
		b.blockIndex++
		output = translatorcommon.AppendSSEEventBytes(output, "content_block_stop", tpl, 2)

	case "response.completed":
		tpl := []byte(`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"input_tokens":0,"output_tokens":0}}`)
		if b.hasToolCall {
			tpl, _ = sjson.SetBytes(tpl, "delta.stop_reason", "tool_use")
		} else if sr := root.Get("response.stop_reason").String(); sr == "max_tokens" || sr == "stop" {
			tpl, _ = sjson.SetBytes(tpl, "delta.stop_reason", sr)
		}
		inputTokens, outputTokens, cachedTokens := extractResponsesUsage(root.Get("response.usage"))
		tpl, _ = sjson.SetBytes(tpl, "usage.input_tokens", inputTokens)
		tpl, _ = sjson.SetBytes(tpl, "usage.output_tokens", outputTokens)
		if cachedTokens > 0 {
			tpl, _ = sjson.SetBytes(tpl, "usage.cache_read_input_tokens", cachedTokens)
		}
		output = translatorcommon.AppendSSEEventBytes(output, "message_delta", tpl, 2)
		output = translatorcommon.AppendSSEEventBytes(output, "message_stop", []byte(`{"type":"message_stop"}`), 2)

	case "response.output_item.added":
		item := root.Get("item")
		switch item.Get("type").String() {
		case "function_call":
			output = append(output, b.finalizeThinkingBlock()...)
			b.hasToolCall = true
			b.hasReceivedArgsDelta = false
			tpl := []byte(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"","name":"","input":{}}}`)
			tpl, _ = sjson.SetBytes(tpl, "index", b.blockIndex)
			tpl, _ = sjson.SetBytes(tpl, "content_block.id", util.SanitizeClaudeToolID(item.Get("call_id").String()))
			name := item.Get("name").String()
			if orig, ok := b.revNames[name]; ok {
				name = orig
			}
			tpl, _ = sjson.SetBytes(tpl, "content_block.name", name)
			output = translatorcommon.AppendSSEEventBytes(output, "content_block_start", tpl, 2)
			delta := []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":""}}`)
			delta, _ = sjson.SetBytes(delta, "index", b.blockIndex)
			output = translatorcommon.AppendSSEEventBytes(output, "content_block_delta", delta, 2)
		case "reasoning":
			b.thinkingSignature = item.Get("encrypted_content").String()
			if b.thinkingStopPending {
				output = append(output, b.finalizeThinkingBlock()...)
			}
		}

	case "response.output_item.done":
		item := root.Get("item")
		switch item.Get("type").String() {
		case "message":
			if !b.hasTextDelta {
				content := item.Get("content")
				if content.Exists() && content.IsArray() {
					var tb strings.Builder
					content.ForEach(func(_, part gjson.Result) bool {
						if part.Get("type").String() == "output_text" {
							if txt := part.Get("text").String(); txt != "" {
								tb.WriteString(txt)
							}
						}
						return true
					})
					if text := tb.String(); text != "" {
						output = append(output, b.finalizeThinkingBlock()...)
						if !b.textBlockOpen {
							start := []byte(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
							start, _ = sjson.SetBytes(start, "index", b.blockIndex)
							b.textBlockOpen = true
							output = translatorcommon.AppendSSEEventBytes(output, "content_block_start", start, 2)
						}
						d := []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":""}}`)
						d, _ = sjson.SetBytes(d, "index", b.blockIndex)
						d, _ = sjson.SetBytes(d, "delta.text", text)
						output = translatorcommon.AppendSSEEventBytes(output, "content_block_delta", d, 2)
						stop := []byte(`{"type":"content_block_stop","index":0}`)
						stop, _ = sjson.SetBytes(stop, "index", b.blockIndex)
						b.textBlockOpen = false
						b.blockIndex++
						b.hasTextDelta = true
						output = translatorcommon.AppendSSEEventBytes(output, "content_block_stop", stop, 2)
					}
				}
			}
		case "function_call":
			tpl := []byte(`{"type":"content_block_stop","index":0}`)
			tpl, _ = sjson.SetBytes(tpl, "index", b.blockIndex)
			b.blockIndex++
			output = translatorcommon.AppendSSEEventBytes(output, "content_block_stop", tpl, 2)
		case "reasoning":
			if sig := item.Get("encrypted_content").String(); sig != "" {
				b.thinkingSignature = sig
			}
			output = append(output, b.finalizeThinkingBlock()...)
			b.thinkingSignature = ""
		}

	case "response.function_call_arguments.delta":
		b.hasReceivedArgsDelta = true
		tpl := []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":""}}`)
		tpl, _ = sjson.SetBytes(tpl, "index", b.blockIndex)
		tpl, _ = sjson.SetBytes(tpl, "delta.partial_json", root.Get("delta").String())
		output = translatorcommon.AppendSSEEventBytes(output, "content_block_delta", tpl, 2)

	case "response.function_call_arguments.done":
		if !b.hasReceivedArgsDelta {
			if args := root.Get("arguments").String(); args != "" {
				tpl := []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":""}}`)
				tpl, _ = sjson.SetBytes(tpl, "index", b.blockIndex)
				tpl, _ = sjson.SetBytes(tpl, "delta.partial_json", args)
				output = translatorcommon.AppendSSEEventBytes(output, "content_block_delta", tpl, 2)
			}
		}
	}

	if len(output) == 0 {
		return nil
	}
	return [][]byte{output}
}

// Finalize emits any pending close events.
func (b *CodexToClaudeStreamBridge) Finalize() [][]byte {
	return nil
}

func (b *CodexToClaudeStreamBridge) finalizeThinkingBlock() []byte {
	if !b.thinkingBlockOpen {
		return nil
	}
	output := make([]byte, 0, 256)
	if b.thinkingSignature != "" {
		sigDelta := []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":""}}`)
		sigDelta, _ = sjson.SetBytes(sigDelta, "index", b.blockIndex)
		sigDelta, _ = sjson.SetBytes(sigDelta, "delta.signature", b.thinkingSignature)
		output = translatorcommon.AppendSSEEventBytes(output, "content_block_delta", sigDelta, 2)
	}
	stop := []byte(`{"type":"content_block_stop","index":0}`)
	stop, _ = sjson.SetBytes(stop, "index", b.blockIndex)
	output = translatorcommon.AppendSSEEventBytes(output, "content_block_stop", stop, 2)
	b.blockIndex++
	b.thinkingBlockOpen = false
	b.thinkingStopPending = false
	return output
}

// Ensure interface compliance.
var _ FormatMapper = (*CodexFormatMapper)(nil)
var _ StreamBridge = (*CodexToClaudeStreamBridge)(nil)
