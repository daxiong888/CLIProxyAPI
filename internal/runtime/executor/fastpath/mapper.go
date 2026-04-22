// Package fastpath provides optimized request/response translation for hot routes.
//
// When a Claude-format request targets a Codex or OpenAI upstream, the fast path
// replaces the generic translator registry lookup with a direct, byte-level mapper.
// The generic translator pipeline is used as fallback for ineligible requests.
package fastpath

import "context"

// FormatMapper encapsulates all fast-path logic for a specific target format.
type FormatMapper interface {
	// IsEligible checks if the Claude payload can be handled by this fast path.
	// Returns (eligible, reason) where reason explains why not if ineligible.
	IsEligible(claudePayload []byte) (bool, string)

	// MapRequest converts Claude format to target format at the byte level.
	// Returns (mappedBody, mappedOriginal, error). Both are needed for ApplyPayloadConfigWithRoot.
	MapRequest(claudePayload, originalPayload []byte, model string) ([]byte, []byte, error)

	// NewStreamBridge creates a typed stream state machine for response translation.
	NewStreamBridge(originalRequest []byte) StreamBridge

	// MapNonStreamResponse converts a target-format response to Claude format.
	MapNonStreamResponse(originalRequest, targetResponse []byte) ([]byte, error)
}

// StreamBridge processes upstream SSE lines and emits Claude-format SSE chunks.
type StreamBridge interface {
	// ProcessLine handles one SSE line. Returns zero or more Claude SSE chunks.
	ProcessLine(ctx context.Context, line []byte) [][]byte
	// Finalize emits any pending close events (e.g., message_stop).
	Finalize() [][]byte
}

// mappers holds registered fast-path mappers keyed by "from:to".
var mappers = map[string]FormatMapper{}

// RegisterMapper registers a FormatMapper for a specific from→to route.
func RegisterMapper(from, to string, m FormatMapper) {
	mappers[from+":"+to] = m
}

// GetMapper returns the FormatMapper for a route, or nil if none registered.
func GetMapper(from, to string) FormatMapper {
	return mappers[from+":"+to]
}
