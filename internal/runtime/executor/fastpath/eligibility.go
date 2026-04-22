package fastpath

import (
	"strings"

	"github.com/tidwall/gjson"
)

func hasUnsupportedToolSchema(claudePayload []byte) (bool, string) {
	tools := gjson.GetBytes(claudePayload, "tools")
	if !tools.IsArray() {
		return false, ""
	}
	for _, tool := range tools.Array() {
		name := strings.TrimSpace(tool.Get("name").String())
		schema := tool.Get("input_schema")
		if !schema.Exists() {
			continue
		}
		if keyword := findUnsupportedSchemaKeyword(schema); keyword != "" {
			if name == "" {
				name = "<unnamed>"
			}
			return true, "unsupported tool schema keyword " + keyword + " in tool " + name
		}
	}
	return false, ""
}

func findUnsupportedSchemaKeyword(node gjson.Result) string {
	if !node.Exists() {
		return ""
	}
	if node.IsArray() {
		for _, item := range node.Array() {
			if keyword := findUnsupportedSchemaKeyword(item); keyword != "" {
				return keyword
			}
		}
		return ""
	}
	if node.Type != gjson.JSON {
		return ""
	}
	var found string
	isObject := false
	node.ForEach(func(key, value gjson.Result) bool {
		isObject = true
		switch key.String() {
		case "oneOf", "anyOf", "allOf", "$ref":
			found = key.String()
			return false
		}
		if keyword := findUnsupportedSchemaKeyword(value); keyword != "" {
			found = keyword
			return false
		}
		return true
	})
	if !isObject {
		for _, item := range node.Array() {
			if keyword := findUnsupportedSchemaKeyword(item); keyword != "" {
				return keyword
			}
		}
	}
	return found
}
