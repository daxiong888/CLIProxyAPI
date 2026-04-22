package fastpath

import (
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
)

const toolNameLimit = 64

// ShortenNameIfNeeded applies a simple shortening rule for a single tool name.
// Codex/OpenAI APIs have a 64-character limit on function names.
func ShortenNameIfNeeded(name string) string {
	if len(name) <= toolNameLimit {
		return name
	}
	if strings.HasPrefix(name, "mcp__") {
		idx := strings.LastIndex(name, "__")
		if idx > 0 {
			cand := "mcp__" + name[idx+2:]
			if len(cand) > toolNameLimit {
				return cand[:toolNameLimit]
			}
			return cand
		}
	}
	return name[:toolNameLimit]
}

// BuildShortNameMap ensures uniqueness of shortened names within a request.
// Returns a map from original name to unique short name.
func BuildShortNameMap(names []string) map[string]string {
	used := map[string]struct{}{}
	m := map[string]string{}

	baseCandidate := func(n string) string {
		if len(n) <= toolNameLimit {
			return n
		}
		if strings.HasPrefix(n, "mcp__") {
			idx := strings.LastIndex(n, "__")
			if idx > 0 {
				cand := "mcp__" + n[idx+2:]
				if len(cand) > toolNameLimit {
					cand = cand[:toolNameLimit]
				}
				return cand
			}
		}
		return n[:toolNameLimit]
	}

	makeUnique := func(cand string) string {
		if _, ok := used[cand]; !ok {
			return cand
		}
		base := cand
		for i := 1; ; i++ {
			suffix := "_" + strconv.Itoa(i)
			allowed := toolNameLimit - len(suffix)
			if allowed < 0 {
				allowed = 0
			}
			tmp := base
			if len(tmp) > allowed {
				tmp = tmp[:allowed]
			}
			tmp = tmp + suffix
			if _, ok := used[tmp]; !ok {
				return tmp
			}
		}
	}

	for _, n := range names {
		cand := baseCandidate(n)
		uniq := makeUnique(cand)
		used[uniq] = struct{}{}
		m[n] = uniq
	}
	return m
}

// BuildReverseNameMap builds a short→original reverse map from Claude request tools.
func BuildReverseNameMap(claudePayload []byte) map[string]string {
	tools := gjson.GetBytes(claudePayload, "tools")
	rev := map[string]string{}
	if !tools.IsArray() {
		return rev
	}
	var names []string
	arr := tools.Array()
	for i := 0; i < len(arr); i++ {
		n := arr[i].Get("name").String()
		if n != "" {
			names = append(names, n)
		}
	}
	if len(names) > 0 {
		m := BuildShortNameMap(names)
		for orig, short := range m {
			rev[short] = orig
		}
	}
	return rev
}

// BuildForwardNameMap builds an original→short forward map from Claude request tools.
func BuildForwardNameMap(claudePayload []byte) map[string]string {
	tools := gjson.GetBytes(claudePayload, "tools")
	if !tools.IsArray() {
		return nil
	}
	var names []string
	arr := tools.Array()
	for i := 0; i < len(arr); i++ {
		n := arr[i].Get("name").String()
		if n != "" {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return nil
	}
	return BuildShortNameMap(names)
}
