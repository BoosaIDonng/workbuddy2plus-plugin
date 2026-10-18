package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSyncRequestRepairs(t *testing.T) {
	body := []byte(`{"model":"gpt-6-sol","max_completion_tokens":1,"messages":[{"role":"user","content":[{"type":"image_url","image_url":"https://example.com/image.png"}]}],"tools":[{"type":"function","function":{"name":"run","parameters":{"type":"string","pattern":"^agent\\_run"}}}]}`)
	var obj map[string]any
	if err := json.Unmarshal(PrepareBodyOpt(body, false), &obj); err != nil {
		t.Fatal(err)
	}
	if obj["max_tokens"] != float64(16) {
		t.Fatalf("max=%v", obj["max_tokens"])
	}
	tool := obj["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if tool["parameters"].(map[string]any)["pattern"] != "^agent_run" {
		t.Fatal("pattern repair missing")
	}
	part := obj["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)
	if part["image_url"].(map[string]any)["url"] != "https://example.com/image.png" {
		t.Fatal("image changed")
	}
	var nonGPT map[string]any
	json.Unmarshal(PrepareBodyOpt([]byte(`{"model":"hy4","max_tokens":1}`), false), &nonGPT)
	if nonGPT["max_tokens"] != float64(1) {
		t.Fatal("non-GPT output changed")
	}
}

func TestSyncEmptyToolNameDropsBothSidesAndKeepsValidPair(t *testing.T) {
	body := []byte(`{"messages":[{"role":"assistant","tool_calls":[{"id":"bad","function":{"name":"  ","arguments":"{}"}},{"id":"good","function":{"name":"run","arguments":"{}"}}]},{"role":"tool","tool_call_id":"bad","content":"bad-result"},{"role":"tool","tool_call_id":"good","content":"good-result"}]}`)
	result := string(PrepareBodyOpt(body, false))
	if strings.Contains(result, "bad-result") || strings.Contains(result, `"id":"bad"`) {
		t.Fatalf("bad pair retained: %s", result)
	}
	if !strings.Contains(result, "good-result") || !strings.Contains(result, `"name":"run"`) {
		t.Fatalf("valid pair lost: %s", result)
	}
}

func TestMarkupChunksSplitMarkersAndFinishOrder(t *testing.T) {
	n := NewMarkupChunkNormalizer([]byte(`{"tools":[{"function":{"name":"run"}}]}`))
	markup := markupOpen + `<` + markupPipe + `DSML` + markupPipe + ` invoke name="run">` + markupInvokeClose + markupClose
	var frames []string
	for _, txt := range []string{"hello " + markup[:10], markup[10:]} {
		raw, _ := json.Marshal(map[string]any{"id": "stream-id", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": txt}}}})
		result, _, _ := n.NormalizeFrames(string(raw))
		frames = append(frames, result...)
	}
	result, _, _ := n.NormalizeFrames(`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
	frames = append(frames, result...)
	joined := strings.Join(frames, "\n")
	if strings.Contains(joined, "DSML") || !strings.Contains(joined, `"name":"run"`) || !strings.Contains(joined, `"content":"hello "`) {
		t.Fatalf("frames=%s", joined)
	}
	if strings.Index(joined, `"name":"run"`) > strings.LastIndex(joined, `"finish_reason":"tool_calls"`) {
		t.Fatal("finish before tool call")
	}
	if len(n.FlushFrames()) != 0 {
		t.Fatal("duplicate final frames")
	}
}

func TestMarkupChunksUnclosedTextSurvivesEOFAndStructuredCalls(t *testing.T) {
	n := NewMarkupChunkNormalizer(nil)
	text := "plain " + markupOpen + "unfinished"
	raw, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": text}}}})
	frames, _, _ := n.NormalizeFrames(string(raw))
	frames = append(frames, n.FlushFrames()...)
	var visible string
	for _, frame := range frames {
		var obj map[string]any
		json.Unmarshal([]byte(frame), &obj)
		c := obj["choices"].([]any)[0].(map[string]any)
		d, _ := c["delta"].(map[string]any)
		if txt, ok := d["content"].(string); ok {
			visible += txt
		}
	}
	if visible != text {
		t.Fatalf("visible=%q want=%q", visible, text)
	}
	n = NewMarkupChunkNormalizer(nil)
	calls, valid, _ := n.NormalizeFrames(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"real","function":{"name":"run","arguments":"{}"}}]}}]}`)
	if !valid || !strings.Contains(strings.Join(calls, ""), `"id":"real"`) {
		t.Fatal("structured call lost")
	}
}
