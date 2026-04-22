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
	RegisterMapper("claude", "openai", &OpenAIFormatMapper{})
}

// OpenAIFormatMapper implements FormatMapper for Claude -> OpenAI Chat Completions translation.
type OpenAIFormatMapper struct{}

// IsEligible checks whether a Claude payload can use the OpenAI fast path.
func (m *OpenAIFormatMapper) IsEligible(claudePayload []byte) (bool, string) {
	if len(claudePayload) == 0 {
		return false, "empty payload"
	}
	root := gjson.ParseBytes(claudePayload)
	if unsupported, reason := hasUnsupportedToolSchema(claudePayload); unsupported {
		return false, reason
	}
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
				case "text", "image", "tool_use", "tool_result", "thinking", "redacted_thinking":
					// supported
				default:
					return false, "unsupported content type: " + ct
				}
			}
		}
	}
	return true, ""
}

// MapRequest converts a Claude-format payload to OpenAI Chat Completions format.
// Returns (mappedBody, mappedOriginal, error).
func (m *OpenAIFormatMapper) MapRequest(claudePayload, originalPayload []byte, model string) ([]byte, []byte, error) {
	body := convertClaudeToOpenAI(claudePayload, model)
	originalTranslated := body
	if len(originalPayload) > 0 && &originalPayload[0] != &claudePayload[0] {
		originalTranslated = convertClaudeToOpenAI(originalPayload, model)
	}
	return body, originalTranslated, nil
}

// NewStreamBridge creates a stream bridge for OpenAI -> Claude SSE translation.
func (m *OpenAIFormatMapper) NewStreamBridge(originalRequest []byte) StreamBridge {
	return &OpenAIToClaudeStreamBridge{
		toolNameMap:               util.ToolNameMapFromClaudeRequest(originalRequest),
		textContentBlockIndex:     -1,
		thinkingContentBlockIndex: -1,
		toolCallBlockIndexes:      make(map[int]int),
	}
}

// MapNonStreamResponse converts an OpenAI Chat Completions response to a Claude Messages response.
func (m *OpenAIFormatMapper) MapNonStreamResponse(originalRequest, targetResponse []byte) ([]byte, error) {
	toolNameMap := util.ToolNameMapFromClaudeRequest(originalRequest)
	out := convertOpenAINonStreamToClaude(targetResponse, toolNameMap)
	if len(out) == 0 {
		return nil, fmt.Errorf("empty response from OpenAI")
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Request translation: Claude -> OpenAI
// ---------------------------------------------------------------------------

func convertClaudeToOpenAI(rawJSON []byte, modelName string) []byte {
	out := []byte(`{"model":"","messages":[]}`)
	root := gjson.ParseBytes(rawJSON)

	out, _ = sjson.SetBytes(out, "model", modelName)

	if maxTokens := root.Get("max_tokens"); maxTokens.Exists() {
		out, _ = sjson.SetBytes(out, "max_tokens", maxTokens.Int())
	}

	if temp := root.Get("temperature"); temp.Exists() {
		out, _ = sjson.SetBytes(out, "temperature", temp.Float())
	} else if topP := root.Get("top_p"); topP.Exists() {
		out, _ = sjson.SetBytes(out, "top_p", topP.Float())
	}

	if stopSequences := root.Get("stop_sequences"); stopSequences.Exists() && stopSequences.IsArray() {
		var stops []string
		stopSequences.ForEach(func(_, value gjson.Result) bool {
			stops = append(stops, value.String())
			return true
		})
		if len(stops) == 1 {
			out, _ = sjson.SetBytes(out, "stop", stops[0])
		} else if len(stops) > 1 {
			out, _ = sjson.SetBytes(out, "stop", stops)
		}
	}

	// Default stream=false; executor overrides for streaming paths.
	out, _ = sjson.SetBytes(out, "stream", false)

	// Thinking -> reasoning_effort
	if thinkingConfig := root.Get("thinking"); thinkingConfig.Exists() && thinkingConfig.IsObject() {
		if thinkingType := thinkingConfig.Get("type"); thinkingType.Exists() {
			switch thinkingType.String() {
			case "enabled":
				if budgetTokens := thinkingConfig.Get("budget_tokens"); budgetTokens.Exists() {
					budget := int(budgetTokens.Int())
					if effort, ok := thinking.ConvertBudgetToLevel(budget); ok && effort != "" {
						out, _ = sjson.SetBytes(out, "reasoning_effort", effort)
					}
				} else {
					if effort, ok := thinking.ConvertBudgetToLevel(-1); ok && effort != "" {
						out, _ = sjson.SetBytes(out, "reasoning_effort", effort)
					}
				}
			case "adaptive", "auto":
				effort := ""
				if v := root.Get("output_config.effort"); v.Exists() && v.Type == gjson.String {
					effort = strings.ToLower(strings.TrimSpace(v.String()))
				}
				if effort != "" {
					out, _ = sjson.SetBytes(out, "reasoning_effort", effort)
				} else {
					out, _ = sjson.SetBytes(out, "reasoning_effort", string(thinking.LevelXHigh))
				}
			case "disabled":
				if effort, ok := thinking.ConvertBudgetToLevel(0); ok && effort != "" {
					out, _ = sjson.SetBytes(out, "reasoning_effort", effort)
				}
			}
		}
	}

	// Build messages
	messagesJSON := []byte(`[]`)

	// System message
	if system := root.Get("system"); system.Exists() {
		systemMsgJSON := []byte(`{"role":"system","content":[]}`)
		hasSystemContent := false
		if system.Type == gjson.String {
			if system.String() != "" {
				textContent := []byte(`{"type":"text","text":""}`)
				textContent, _ = sjson.SetBytes(textContent, "text", system.String())
				systemMsgJSON, _ = sjson.SetRawBytes(systemMsgJSON, "content.-1", textContent)
				hasSystemContent = true
			}
		} else if system.IsArray() {
			for _, sys := range system.Array() {
				if contentItem, ok := convertClaudeContentPartToOpenAI(sys); ok {
					systemMsgJSON, _ = sjson.SetRawBytes(systemMsgJSON, "content.-1", []byte(contentItem))
					hasSystemContent = true
				}
			}
		}
		if hasSystemContent {
			messagesJSON, _ = sjson.SetRawBytes(messagesJSON, "-1", systemMsgJSON)
		}
	}

	// Process conversation messages
	if messages := root.Get("messages"); messages.Exists() && messages.IsArray() {
		messages.ForEach(func(_, message gjson.Result) bool {
			role := message.Get("role").String()
			contentResult := message.Get("content")

			if contentResult.Exists() && contentResult.IsArray() {
				var contentItems [][]byte
				var reasoningParts []string
				var toolCalls []interface{}
				var toolResults [][]byte

				contentResult.ForEach(func(_, part gjson.Result) bool {
					switch part.Get("type").String() {
					case "thinking":
						if role == "assistant" {
							thinkingText := thinking.GetThinkingText(part)
							if strings.TrimSpace(thinkingText) != "" {
								reasoningParts = append(reasoningParts, thinkingText)
							}
						}
					case "redacted_thinking":
						// ignore
					case "text", "image":
						if contentItem, ok := convertClaudeContentPartToOpenAI(part); ok {
							contentItems = append(contentItems, []byte(contentItem))
						}
					case "tool_use":
						if role == "assistant" {
							tc := []byte(`{"id":"","type":"function","function":{"name":"","arguments":""}}`)
							tc, _ = sjson.SetBytes(tc, "id", part.Get("id").String())
							tc, _ = sjson.SetBytes(tc, "function.name", part.Get("name").String())
							if input := part.Get("input"); input.Exists() {
								tc, _ = sjson.SetBytes(tc, "function.arguments", input.Raw)
							} else {
								tc, _ = sjson.SetBytes(tc, "function.arguments", "{}")
							}
							toolCalls = append(toolCalls, gjson.ParseBytes(tc).Value())
						}
					case "tool_result":
						tr := []byte(`{"role":"tool","tool_call_id":"","content":""}`)
						tr, _ = sjson.SetBytes(tr, "tool_call_id", part.Get("tool_use_id").String())
						trContent, trContentRaw := convertClaudeToolResultContentToOpenAI(part.Get("content"))
						if trContentRaw {
							tr, _ = sjson.SetRawBytes(tr, "content", []byte(trContent))
						} else {
							tr, _ = sjson.SetBytes(tr, "content", trContent)
						}
						toolResults = append(toolResults, tr)
					}
					return true
				})

				reasoningContent := ""
				if len(reasoningParts) > 0 {
					reasoningContent = strings.Join(reasoningParts, "\n\n")
				}

				hasContent := len(contentItems) > 0
				hasReasoning := reasoningContent != ""
				hasToolCalls := len(toolCalls) > 0

				// Tool results must precede the message they respond to.
				for _, tr := range toolResults {
					messagesJSON, _ = sjson.SetRawBytes(messagesJSON, "-1", tr)
				}

				if role == "assistant" {
					if hasContent || hasReasoning || hasToolCalls {
						msgJSON := []byte(`{"role":"assistant"}`)
						if hasContent {
							arr := []byte(`[]`)
							for _, ci := range contentItems {
								arr, _ = sjson.SetRawBytes(arr, "-1", ci)
							}
							msgJSON, _ = sjson.SetRawBytes(msgJSON, "content", arr)
						} else {
							msgJSON, _ = sjson.SetBytes(msgJSON, "content", "")
						}
						if hasReasoning {
							msgJSON, _ = sjson.SetBytes(msgJSON, "reasoning_content", reasoningContent)
						}
						if hasToolCalls {
							msgJSON, _ = sjson.SetBytes(msgJSON, "tool_calls", toolCalls)
						}
						messagesJSON, _ = sjson.SetRawBytes(messagesJSON, "-1", msgJSON)
					}
				} else if hasContent {
					msgJSON := []byte(`{"role":""}`)
					msgJSON, _ = sjson.SetBytes(msgJSON, "role", role)
					arr := []byte(`[]`)
					for _, ci := range contentItems {
						arr, _ = sjson.SetRawBytes(arr, "-1", ci)
					}
					msgJSON, _ = sjson.SetRawBytes(msgJSON, "content", arr)
					messagesJSON, _ = sjson.SetRawBytes(messagesJSON, "-1", msgJSON)
				}

			} else if contentResult.Exists() && contentResult.Type == gjson.String {
				msgJSON := []byte(`{"role":"","content":""}`)
				msgJSON, _ = sjson.SetBytes(msgJSON, "role", role)
				msgJSON, _ = sjson.SetBytes(msgJSON, "content", contentResult.String())
				messagesJSON, _ = sjson.SetRawBytes(messagesJSON, "-1", msgJSON)
			}
			return true
		})
	}

	if msgs := gjson.ParseBytes(messagesJSON); msgs.IsArray() && len(msgs.Array()) > 0 {
		out, _ = sjson.SetRawBytes(out, "messages", messagesJSON)
	}

	// Tools
	if tools := root.Get("tools"); tools.Exists() && tools.IsArray() {
		toolsJSON := []byte(`[]`)
		tools.ForEach(func(_, tool gjson.Result) bool {
			t := []byte(`{"type":"function","function":{"name":"","description":""}}`)
			t, _ = sjson.SetBytes(t, "function.name", tool.Get("name").String())
			t, _ = sjson.SetBytes(t, "function.description", tool.Get("description").String())
			if inputSchema := tool.Get("input_schema"); inputSchema.Exists() {
				t, _ = sjson.SetBytes(t, "function.parameters", inputSchema.Value())
			}
			toolsJSON, _ = sjson.SetRawBytes(toolsJSON, "-1", t)
			return true
		})
		if parsed := gjson.ParseBytes(toolsJSON); parsed.IsArray() && len(parsed.Array()) > 0 {
			out, _ = sjson.SetRawBytes(out, "tools", toolsJSON)
		}
	}

	// Tool choice
	if toolChoice := root.Get("tool_choice"); toolChoice.Exists() {
		switch toolChoice.Get("type").String() {
		case "auto":
			out, _ = sjson.SetBytes(out, "tool_choice", "auto")
		case "any":
			out, _ = sjson.SetBytes(out, "tool_choice", "required")
		case "tool":
			tc := []byte(`{"type":"function","function":{"name":""}}`)
			tc, _ = sjson.SetBytes(tc, "function.name", toolChoice.Get("name").String())
			out, _ = sjson.SetRawBytes(out, "tool_choice", tc)
		default:
			out, _ = sjson.SetBytes(out, "tool_choice", "auto")
		}
	}

	// User
	if user := root.Get("user"); user.Exists() {
		out, _ = sjson.SetBytes(out, "user", user.String())
	}

	return out
}

func convertClaudeContentPartToOpenAI(part gjson.Result) (string, bool) {
	switch part.Get("type").String() {
	case "text":
		text := part.Get("text").String()
		if strings.TrimSpace(text) == "" {
			return "", false
		}
		c := []byte(`{"type":"text","text":""}`)
		c, _ = sjson.SetBytes(c, "text", text)
		return string(c), true
	case "image":
		var imageURL string
		if source := part.Get("source"); source.Exists() {
			switch source.Get("type").String() {
			case "base64":
				mediaType := source.Get("media_type").String()
				if mediaType == "" {
					mediaType = "application/octet-stream"
				}
				data := source.Get("data").String()
				if data != "" {
					imageURL = "data:" + mediaType + ";base64," + data
				}
			case "url":
				imageURL = source.Get("url").String()
			}
		}
		if imageURL == "" {
			imageURL = part.Get("url").String()
		}
		if imageURL == "" {
			return "", false
		}
		c := []byte(`{"type":"image_url","image_url":{"url":""}}`)
		c, _ = sjson.SetBytes(c, "image_url.url", imageURL)
		return string(c), true
	default:
		return "", false
	}
}

func convertClaudeToolResultContentToOpenAI(content gjson.Result) (string, bool) {
	if !content.Exists() {
		return "", false
	}
	if content.Type == gjson.String {
		return content.String(), false
	}
	if content.IsArray() {
		var parts []string
		contentJSON := []byte(`[]`)
		hasImagePart := false
		content.ForEach(func(_, item gjson.Result) bool {
			switch {
			case item.Type == gjson.String:
				text := item.String()
				parts = append(parts, text)
				tc := []byte(`{"type":"text","text":""}`)
				tc, _ = sjson.SetBytes(tc, "text", text)
				contentJSON, _ = sjson.SetRawBytes(contentJSON, "-1", tc)
			case item.IsObject() && item.Get("type").String() == "text":
				text := item.Get("text").String()
				parts = append(parts, text)
				tc := []byte(`{"type":"text","text":""}`)
				tc, _ = sjson.SetBytes(tc, "text", text)
				contentJSON, _ = sjson.SetRawBytes(contentJSON, "-1", tc)
			case item.IsObject() && item.Get("type").String() == "image":
				contentItem, ok := convertClaudeContentPartToOpenAI(item)
				if ok {
					contentJSON, _ = sjson.SetRawBytes(contentJSON, "-1", []byte(contentItem))
					hasImagePart = true
				} else {
					parts = append(parts, item.Raw)
				}
			case item.IsObject() && item.Get("text").Exists() && item.Get("text").Type == gjson.String:
				parts = append(parts, item.Get("text").String())
			default:
				parts = append(parts, item.Raw)
			}
			return true
		})
		if hasImagePart {
			return string(contentJSON), true
		}
		joined := strings.Join(parts, "\n\n")
		if strings.TrimSpace(joined) != "" {
			return joined, false
		}
		return content.Raw, false
	}
	if content.IsObject() {
		if content.Get("type").String() == "image" {
			contentItem, ok := convertClaudeContentPartToOpenAI(content)
			if ok {
				arr := []byte(`[]`)
				arr, _ = sjson.SetRawBytes(arr, "-1", []byte(contentItem))
				return string(arr), true
			}
		}
		if text := content.Get("text"); text.Exists() && text.Type == gjson.String {
			return text.String(), false
		}
		return content.Raw, false
	}
	return content.Raw, false
}

// ---------------------------------------------------------------------------
// Non-streaming response: OpenAI -> Claude
// ---------------------------------------------------------------------------

func convertOpenAINonStreamToClaude(rawJSON []byte, toolNameMap map[string]string) []byte {
	root := gjson.ParseBytes(rawJSON)
	out := []byte(`{"id":"","type":"message","role":"assistant","model":"","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}`)
	out, _ = sjson.SetBytes(out, "id", root.Get("id").String())
	out, _ = sjson.SetBytes(out, "model", root.Get("model").String())

	hasToolCall := false
	stopReasonSet := false

	if choices := root.Get("choices"); choices.Exists() && choices.IsArray() && len(choices.Array()) > 0 {
		choice := choices.Array()[0]

		if finishReason := choice.Get("finish_reason"); finishReason.Exists() {
			out, _ = sjson.SetBytes(out, "stop_reason", mapOpenAIFinishReason(finishReason.String()))
			stopReasonSet = true
		}

		if message := choice.Get("message"); message.Exists() {
			// Structured content array
			if contentResult := message.Get("content"); contentResult.Exists() {
				if contentResult.IsArray() {
					var textBuilder strings.Builder
					var thinkingBuilder strings.Builder

					flushText := func() {
						if textBuilder.Len() == 0 {
							return
						}
						block := []byte(`{"type":"text","text":""}`)
						block, _ = sjson.SetBytes(block, "text", textBuilder.String())
						out, _ = sjson.SetRawBytes(out, "content.-1", block)
						textBuilder.Reset()
					}

					flushThinking := func() {
						if thinkingBuilder.Len() == 0 {
							return
						}
						block := []byte(`{"type":"thinking","thinking":""}`)
						block, _ = sjson.SetBytes(block, "thinking", thinkingBuilder.String())
						out, _ = sjson.SetRawBytes(out, "content.-1", block)
						thinkingBuilder.Reset()
					}

					for _, item := range contentResult.Array() {
						switch item.Get("type").String() {
						case "text":
							flushThinking()
							textBuilder.WriteString(item.Get("text").String())
						case "tool_calls":
							flushThinking()
							flushText()
							tcs := item.Get("tool_calls")
							if tcs.IsArray() {
								tcs.ForEach(func(_, tc gjson.Result) bool {
									hasToolCall = true
									toolUse := []byte(`{"type":"tool_use","id":"","name":"","input":{}}`)
									toolUse, _ = sjson.SetBytes(toolUse, "id", util.SanitizeClaudeToolID(tc.Get("id").String()))
									toolUse, _ = sjson.SetBytes(toolUse, "name", util.MapToolName(toolNameMap, tc.Get("function.name").String()))
									argsStr := util.FixJSON(tc.Get("function.arguments").String())
									if argsStr != "" && gjson.Valid(argsStr) {
										argsJSON := gjson.Parse(argsStr)
										if argsJSON.IsObject() {
											toolUse, _ = sjson.SetRawBytes(toolUse, "input", []byte(argsJSON.Raw))
										}
									}
									out, _ = sjson.SetRawBytes(out, "content.-1", toolUse)
									return true
								})
							}
						case "reasoning":
							flushText()
							if t := item.Get("text"); t.Exists() {
								thinkingBuilder.WriteString(t.String())
							}
						default:
							flushThinking()
							flushText()
						}
					}
					flushThinking()
					flushText()

				} else if contentResult.Type == gjson.String {
					textContent := contentResult.String()
					if textContent != "" {
						block := []byte(`{"type":"text","text":""}`)
						block, _ = sjson.SetBytes(block, "text", textContent)
						out, _ = sjson.SetRawBytes(out, "content.-1", block)
					}
				}
			}

			// reasoning_content on message
			if reasoning := message.Get("reasoning_content"); reasoning.Exists() {
				for _, reasoningText := range collectOpenAIReasoningTexts(reasoning) {
					if reasoningText == "" {
						continue
					}
					block := []byte(`{"type":"thinking","thinking":""}`)
					block, _ = sjson.SetBytes(block, "thinking", reasoningText)
					out, _ = sjson.SetRawBytes(out, "content.-1", block)
				}
			}

			// tool_calls on message
			if toolCalls := message.Get("tool_calls"); toolCalls.Exists() && toolCalls.IsArray() {
				toolCalls.ForEach(func(_, toolCall gjson.Result) bool {
					hasToolCall = true
					toolUseBlock := []byte(`{"type":"tool_use","id":"","name":"","input":{}}`)
					toolUseBlock, _ = sjson.SetBytes(toolUseBlock, "id", util.SanitizeClaudeToolID(toolCall.Get("id").String()))
					toolUseBlock, _ = sjson.SetBytes(toolUseBlock, "name", util.MapToolName(toolNameMap, toolCall.Get("function.name").String()))
					argsStr := util.FixJSON(toolCall.Get("function.arguments").String())
					if argsStr != "" && gjson.Valid(argsStr) {
						argsJSON := gjson.Parse(argsStr)
						if argsJSON.IsObject() {
							toolUseBlock, _ = sjson.SetRawBytes(toolUseBlock, "input", []byte(argsJSON.Raw))
						}
					}
					out, _ = sjson.SetRawBytes(out, "content.-1", toolUseBlock)
					return true
				})
			}
		}
	}

	// Usage
	if usage := root.Get("usage"); usage.Exists() {
		inputTokens, outputTokens, cachedTokens := extractOpenAIUsage(usage)
		out, _ = sjson.SetBytes(out, "usage.input_tokens", inputTokens)
		out, _ = sjson.SetBytes(out, "usage.output_tokens", outputTokens)
		if cachedTokens > 0 {
			out, _ = sjson.SetBytes(out, "usage.cache_read_input_tokens", cachedTokens)
		}
	}

	if !stopReasonSet {
		if hasToolCall {
			out, _ = sjson.SetBytes(out, "stop_reason", "tool_use")
		} else {
			out, _ = sjson.SetBytes(out, "stop_reason", "end_turn")
		}
	}

	return out
}

// ---------------------------------------------------------------------------
// OpenAI helpers
// ---------------------------------------------------------------------------

func mapOpenAIFinishReason(reason string) string {
	switch reason {
	case "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	case "content_filter":
		return "end_turn"
	case "function_call":
		return "tool_use"
	default:
		return "end_turn"
	}
}

func extractOpenAIUsage(usage gjson.Result) (int64, int64, int64) {
	if !usage.Exists() || usage.Type == gjson.Null {
		return 0, 0, 0
	}
	inputTokens := usage.Get("prompt_tokens").Int()
	outputTokens := usage.Get("completion_tokens").Int()
	cachedTokens := usage.Get("prompt_tokens_details.cached_tokens").Int()
	if cachedTokens > 0 {
		if inputTokens >= cachedTokens {
			inputTokens -= cachedTokens
		} else {
			inputTokens = 0
		}
	}
	return inputTokens, outputTokens, cachedTokens
}

func collectOpenAIReasoningTexts(node gjson.Result) []string {
	var texts []string
	if !node.Exists() {
		return texts
	}
	if node.IsArray() {
		node.ForEach(func(_, value gjson.Result) bool {
			texts = append(texts, collectOpenAIReasoningTexts(value)...)
			return true
		})
		return texts
	}
	switch node.Type {
	case gjson.String:
		if text := node.String(); text != "" {
			texts = append(texts, text)
		}
	case gjson.JSON:
		if text := node.Get("text"); text.Exists() {
			if textStr := text.String(); textStr != "" {
				texts = append(texts, textStr)
			}
		} else if raw := node.Raw; raw != "" && !strings.HasPrefix(raw, "{") && !strings.HasPrefix(raw, "[") {
			texts = append(texts, raw)
		}
	}
	return texts
}

// ---------------------------------------------------------------------------
// Streaming bridge: OpenAI SSE -> Claude SSE
// ---------------------------------------------------------------------------

type openAIToolCallAccumulator struct {
	ID        string
	Name      string
	Arguments strings.Builder
}

// OpenAIToClaudeStreamBridge translates OpenAI streaming chunks to Claude SSE format.
type OpenAIToClaudeStreamBridge struct {
	toolNameMap map[string]string

	messageID string
	model     string

	sawToolCall                 bool
	contentAccumulator          strings.Builder
	toolCallsAccumulator        map[int]*openAIToolCallAccumulator
	textContentBlockStarted     bool
	thinkingContentBlockStarted bool
	finishReason                string
	contentBlocksStopped        bool
	messageDeltaSent            bool
	messageStarted              bool
	messageStopSent             bool
	toolCallBlockIndexes        map[int]int
	textContentBlockIndex       int
	thinkingContentBlockIndex   int
	nextContentBlockIndex       int
}

// ProcessLine handles one SSE line and returns zero or more Claude SSE chunks.
func (b *OpenAIToClaudeStreamBridge) ProcessLine(_ context.Context, line []byte) [][]byte {
	if !bytes.HasPrefix(line, sseDataPrefix) {
		return nil
	}
	rawJSON := bytes.TrimSpace(line[len(sseDataPrefix):])

	if bytes.Equal(bytes.TrimSpace(rawJSON), []byte("[DONE]")) {
		return b.handleDone()
	}

	return b.handleStreamingChunk(rawJSON)
}

func (b *OpenAIToClaudeStreamBridge) handleStreamingChunk(rawJSON []byte) [][]byte {
	root := gjson.ParseBytes(rawJSON)
	var results [][]byte

	if b.messageID == "" {
		b.messageID = root.Get("id").String()
	}
	if b.model == "" {
		b.model = root.Get("model").String()
	}

	delta := root.Get("choices.0.delta")
	if delta.Exists() {
		// Emit message_start on first chunk with delta
		if !b.messageStarted {
			tpl := []byte(`{"type":"message_start","message":{"id":"","type":"message","role":"assistant","model":"","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}}`)
			tpl, _ = sjson.SetBytes(tpl, "message.id", b.messageID)
			tpl, _ = sjson.SetBytes(tpl, "message.model", b.model)
			results = append(results, translatorcommon.AppendSSEEventBytes(nil, "message_start", tpl, 2))
			b.messageStarted = true
		}

		// reasoning_content delta
		if reasoning := delta.Get("reasoning_content"); reasoning.Exists() {
			for _, reasoningText := range collectOpenAIReasoningTexts(reasoning) {
				if reasoningText == "" {
					continue
				}
				b.stopTextBlock(&results)
				if !b.thinkingContentBlockStarted {
					if b.thinkingContentBlockIndex == -1 {
						b.thinkingContentBlockIndex = b.nextContentBlockIndex
						b.nextContentBlockIndex++
					}
					tpl := []byte(`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`)
					tpl, _ = sjson.SetBytes(tpl, "index", b.thinkingContentBlockIndex)
					results = append(results, translatorcommon.AppendSSEEventBytes(nil, "content_block_start", tpl, 2))
					b.thinkingContentBlockStarted = true
				}
				tpl := []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":""}}`)
				tpl, _ = sjson.SetBytes(tpl, "index", b.thinkingContentBlockIndex)
				tpl, _ = sjson.SetBytes(tpl, "delta.thinking", reasoningText)
				results = append(results, translatorcommon.AppendSSEEventBytes(nil, "content_block_delta", tpl, 2))
			}
		}

		// content delta
		if content := delta.Get("content"); content.Exists() && content.String() != "" {
			if !b.textContentBlockStarted {
				b.stopThinkingBlock(&results)
				if b.textContentBlockIndex == -1 {
					b.textContentBlockIndex = b.nextContentBlockIndex
					b.nextContentBlockIndex++
				}
				tpl := []byte(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
				tpl, _ = sjson.SetBytes(tpl, "index", b.textContentBlockIndex)
				results = append(results, translatorcommon.AppendSSEEventBytes(nil, "content_block_start", tpl, 2))
				b.textContentBlockStarted = true
			}
			tpl := []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":""}}`)
			tpl, _ = sjson.SetBytes(tpl, "index", b.textContentBlockIndex)
			tpl, _ = sjson.SetBytes(tpl, "delta.text", content.String())
			results = append(results, translatorcommon.AppendSSEEventBytes(nil, "content_block_delta", tpl, 2))
			b.contentAccumulator.WriteString(content.String())
		}

		// tool_calls delta
		if toolCalls := delta.Get("tool_calls"); toolCalls.Exists() && toolCalls.IsArray() {
			if b.toolCallsAccumulator == nil {
				b.toolCallsAccumulator = make(map[int]*openAIToolCallAccumulator)
			}
			toolCalls.ForEach(func(_, toolCall gjson.Result) bool {
				b.sawToolCall = true
				index := int(toolCall.Get("index").Int())
				blockIndex := b.toolBlockIndex(index)

				if _, exists := b.toolCallsAccumulator[index]; !exists {
					b.toolCallsAccumulator[index] = &openAIToolCallAccumulator{}
				}
				acc := b.toolCallsAccumulator[index]

				if id := toolCall.Get("id"); id.Exists() {
					acc.ID = id.String()
				}

				if function := toolCall.Get("function"); function.Exists() {
					if name := function.Get("name"); name.Exists() {
						acc.Name = util.MapToolName(b.toolNameMap, name.String())

						b.stopThinkingBlock(&results)
						b.stopTextBlock(&results)

						tpl := []byte(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"","name":"","input":{}}}`)
						tpl, _ = sjson.SetBytes(tpl, "index", blockIndex)
						tpl, _ = sjson.SetBytes(tpl, "content_block.id", util.SanitizeClaudeToolID(acc.ID))
						tpl, _ = sjson.SetBytes(tpl, "content_block.name", acc.Name)
						results = append(results, translatorcommon.AppendSSEEventBytes(nil, "content_block_start", tpl, 2))
					}

					if args := function.Get("arguments"); args.Exists() {
						if argsText := args.String(); argsText != "" {
							acc.Arguments.WriteString(argsText)
						}
					}
				}
				return true
			})
		}
	}

	// finish_reason: close all open content blocks
	if finishReason := root.Get("choices.0.finish_reason"); finishReason.Exists() && finishReason.String() != "" {
		reason := finishReason.String()
		if b.sawToolCall {
			b.finishReason = "tool_calls"
		} else {
			b.finishReason = reason
		}

		if b.thinkingContentBlockStarted {
			tpl := []byte(`{"type":"content_block_stop","index":0}`)
			tpl, _ = sjson.SetBytes(tpl, "index", b.thinkingContentBlockIndex)
			results = append(results, translatorcommon.AppendSSEEventBytes(nil, "content_block_stop", tpl, 2))
			b.thinkingContentBlockStarted = false
			b.thinkingContentBlockIndex = -1
		}

		b.stopTextBlock(&results)

		if !b.contentBlocksStopped {
			for index := range b.toolCallsAccumulator {
				acc := b.toolCallsAccumulator[index]
				blockIndex := b.toolBlockIndex(index)

				if acc.Arguments.Len() > 0 {
					inputDelta := []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":""}}`)
					inputDelta, _ = sjson.SetBytes(inputDelta, "index", blockIndex)
					inputDelta, _ = sjson.SetBytes(inputDelta, "delta.partial_json", util.FixJSON(acc.Arguments.String()))
					results = append(results, translatorcommon.AppendSSEEventBytes(nil, "content_block_delta", inputDelta, 2))
				}

				tpl := []byte(`{"type":"content_block_stop","index":0}`)
				tpl, _ = sjson.SetBytes(tpl, "index", blockIndex)
				results = append(results, translatorcommon.AppendSSEEventBytes(nil, "content_block_stop", tpl, 2))
				delete(b.toolCallBlockIndexes, index)
			}
			b.contentBlocksStopped = true
		}
	}

	// Usage (arrives in a separate chunk after finish_reason)
	if b.finishReason != "" {
		usage := root.Get("usage")
		if usage.Exists() && usage.Type != gjson.Null {
			inputTokens, outputTokens, cachedTokens := extractOpenAIUsage(usage)
			msgDelta := []byte(`{"type":"message_delta","delta":{"stop_reason":"","stop_sequence":null},"usage":{"input_tokens":0,"output_tokens":0}}`)
			msgDelta, _ = sjson.SetBytes(msgDelta, "delta.stop_reason", mapOpenAIFinishReason(b.effectiveFinishReason()))
			msgDelta, _ = sjson.SetBytes(msgDelta, "usage.input_tokens", inputTokens)
			msgDelta, _ = sjson.SetBytes(msgDelta, "usage.output_tokens", outputTokens)
			if cachedTokens > 0 {
				msgDelta, _ = sjson.SetBytes(msgDelta, "usage.cache_read_input_tokens", cachedTokens)
			}
			results = append(results, translatorcommon.AppendSSEEventBytes(nil, "message_delta", msgDelta, 2))
			b.messageDeltaSent = true

			if !b.messageStopSent {
				results = append(results, translatorcommon.AppendSSEEventBytes(nil, "message_stop", []byte(`{"type":"message_stop"}`), 2))
				b.messageStopSent = true
			}
		}
	}

	if len(results) == 0 {
		return nil
	}
	return results
}

func (b *OpenAIToClaudeStreamBridge) handleDone() [][]byte {
	var results [][]byte

	if b.thinkingContentBlockStarted {
		tpl := []byte(`{"type":"content_block_stop","index":0}`)
		tpl, _ = sjson.SetBytes(tpl, "index", b.thinkingContentBlockIndex)
		results = append(results, translatorcommon.AppendSSEEventBytes(nil, "content_block_stop", tpl, 2))
		b.thinkingContentBlockStarted = false
		b.thinkingContentBlockIndex = -1
	}

	b.stopTextBlock(&results)

	if !b.contentBlocksStopped {
		for index := range b.toolCallsAccumulator {
			acc := b.toolCallsAccumulator[index]
			blockIndex := b.toolBlockIndex(index)

			if acc.Arguments.Len() > 0 {
				inputDelta := []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":""}}`)
				inputDelta, _ = sjson.SetBytes(inputDelta, "index", blockIndex)
				inputDelta, _ = sjson.SetBytes(inputDelta, "delta.partial_json", util.FixJSON(acc.Arguments.String()))
				results = append(results, translatorcommon.AppendSSEEventBytes(nil, "content_block_delta", inputDelta, 2))
			}

			tpl := []byte(`{"type":"content_block_stop","index":0}`)
			tpl, _ = sjson.SetBytes(tpl, "index", blockIndex)
			results = append(results, translatorcommon.AppendSSEEventBytes(nil, "content_block_stop", tpl, 2))
			delete(b.toolCallBlockIndexes, index)
		}
		b.contentBlocksStopped = true
	}

	if b.finishReason != "" && !b.messageDeltaSent {
		msgDelta := []byte(`{"type":"message_delta","delta":{"stop_reason":"","stop_sequence":null},"usage":{"input_tokens":0,"output_tokens":0}}`)
		msgDelta, _ = sjson.SetBytes(msgDelta, "delta.stop_reason", mapOpenAIFinishReason(b.effectiveFinishReason()))
		results = append(results, translatorcommon.AppendSSEEventBytes(nil, "message_delta", msgDelta, 2))
		b.messageDeltaSent = true
	}

	if !b.messageStopSent {
		results = append(results, translatorcommon.AppendSSEEventBytes(nil, "message_stop", []byte(`{"type":"message_stop"}`), 2))
		b.messageStopSent = true
	}

	if len(results) == 0 {
		return nil
	}
	return results
}

// Finalize emits any pending close events. Acts as a safety net if [DONE] was never received.
func (b *OpenAIToClaudeStreamBridge) Finalize() [][]byte {
	if b.messageStopSent {
		return nil
	}
	return b.handleDone()
}

func (b *OpenAIToClaudeStreamBridge) effectiveFinishReason() string {
	if b.sawToolCall {
		return "tool_calls"
	}
	return b.finishReason
}

func (b *OpenAIToClaudeStreamBridge) toolBlockIndex(openAIToolIndex int) int {
	if idx, ok := b.toolCallBlockIndexes[openAIToolIndex]; ok {
		return idx
	}
	idx := b.nextContentBlockIndex
	b.nextContentBlockIndex++
	b.toolCallBlockIndexes[openAIToolIndex] = idx
	return idx
}

func (b *OpenAIToClaudeStreamBridge) stopThinkingBlock(results *[][]byte) {
	if !b.thinkingContentBlockStarted {
		return
	}
	tpl := []byte(`{"type":"content_block_stop","index":0}`)
	tpl, _ = sjson.SetBytes(tpl, "index", b.thinkingContentBlockIndex)
	*results = append(*results, translatorcommon.AppendSSEEventBytes(nil, "content_block_stop", tpl, 2))
	b.thinkingContentBlockStarted = false
	b.thinkingContentBlockIndex = -1
}

func (b *OpenAIToClaudeStreamBridge) stopTextBlock(results *[][]byte) {
	if !b.textContentBlockStarted {
		return
	}
	tpl := []byte(`{"type":"content_block_stop","index":0}`)
	tpl, _ = sjson.SetBytes(tpl, "index", b.textContentBlockIndex)
	*results = append(*results, translatorcommon.AppendSSEEventBytes(nil, "content_block_stop", tpl, 2))
	b.textContentBlockStarted = false
	b.textContentBlockIndex = -1
}

// Ensure interface compliance.
var _ FormatMapper = (*OpenAIFormatMapper)(nil)
var _ StreamBridge = (*OpenAIToClaudeStreamBridge)(nil)
