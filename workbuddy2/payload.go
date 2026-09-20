// payload.go keeps only the model-alias rewrite: the rest of the old
// hand-rolled outbound pipeline (stream forcing, tool_choice normalization,
// system template sanitization, Global system injection) is superseded by the
// vendored workbuddy2api pipeline invoked from wb_upstream.go, which is a
// strict superset of the old behavior.
package main

import (
	"encoding/json"
	"strings"
)

// rewriteModelInBody replaces the "model" field of a chat-completions body
// with the resolved upstream model ID.
func rewriteModelInBody(body []byte, upstreamModel string) []byte {
	if len(body) == 0 || strings.TrimSpace(upstreamModel) == "" {
		return body
	}
	var obj map[string]any
	if json.Unmarshal(body, &obj) != nil {
		return body
	}
	cur, _ := obj["model"].(string)
	if strings.EqualFold(strings.TrimSpace(cur), strings.TrimSpace(upstreamModel)) {
		return body
	}
	obj["model"] = upstreamModel
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}
