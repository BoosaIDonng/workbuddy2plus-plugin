package upstream

import "encoding/json"

// MarkupChunkNormalizer adapts the upstream markup repair to CPA's per-frame
// stream bridge. Keep the ordinary ChunkNormalizer as the final wire sanitizer.
// One input frame can yield text, tool calls and a finish frame, in that order.
type MarkupChunkNormalizer struct {
	normalizer *ChunkNormalizer
	repair     *MarkupRepair
	last       map[string]any
	nextIndex  int
	finished   bool
}

func NewMarkupChunkNormalizer(body []byte) *MarkupChunkNormalizer {
	allowed := ToolNameAllowlist(body)
	return &MarkupChunkNormalizer{normalizer: NewChunkNormalizer(), repair: NewMarkupRepair(allowed, len(allowed) == 0)}
}

func (n *MarkupChunkNormalizer) encode(objects []map[string]any) []string {
	var frames []string
	for _, obj := range objects {
		raw, err := json.Marshal(obj)
		if err != nil {
			continue
		}
		text, _, _ := n.normalizer.Normalize(string(raw))
		if text != "" {
			frames = append(frames, text)
		}
	}
	return frames
}

func (n *MarkupChunkNormalizer) NormalizeFrames(payload string) ([]string, bool, bool) {
	if payload == "[DONE]" {
		return n.FlushFrames(), false, true
	}
	var obj map[string]any
	if json.Unmarshal([]byte(payload), &obj) != nil || obj == nil {
		text, valid, done := n.normalizer.Normalize(payload)
		return []string{text}, valid, done
	}
	n.last = obj
	if _, ok := obj["error"]; ok {
		return append(n.FlushFrames(), payload), true, false
	}
	choices, _ := obj["choices"].([]any)
	// Repair only a single-choice stream; flush held text before a multi-choice fallback.
	if len(choices) > 1 {
		frames := n.FlushFrames()
		n.repair = nil
		text, valid, done := n.normalizer.Normalize(payload)
		return append(frames, text), valid, done
	}
	// A real structured call is authoritative. Flush unclassified text before it.
	for _, value := range choices {
		c, _ := value.(map[string]any)
		if c == nil {
			continue
		}
		delta, _ := c["delta"].(map[string]any)
		if calls, _ := delta["tool_calls"].([]any); len(calls) > 0 {
			frames := n.FlushFrames()
			n.repair = nil
			text, valid, done := n.normalizer.Normalize(payload)
			return append(frames, text), valid, done
		}
	}
	if !n.repair.Enabled() || len(choices) != 1 {
		return n.encode([]map[string]any{obj}), true, false
	}
	choice, _ := choices[0].(map[string]any)
	if choice == nil {
		return n.encode([]map[string]any{obj}), true, false
	}
	delta, _ := choice["delta"].(map[string]any)
	text, _ := delta["content"].(string)
	visible, calls := n.repair.Feed(text)
	finish, _ := choice["finish_reason"].(string)
	if finish != "" {
		visible += n.repair.Flush()
		n.finished = true
	}
	if delta != nil {
		if visible == "" {
			delete(delta, "content")
		} else {
			delta["content"] = visible
		}
	}
	frames := []map[string]any{obj}
	if len(calls) > 0 {
		delete(choice, "finish_reason")
		for _, call := range calls {
			frames = append(frames, markupCallFrame(obj, n.nextIndex, call))
			n.nextIndex++
		}
		if finish != "" {
			if finish == "stop" {
				finish = "tool_calls"
			}
			frames = append(frames, finishOnlyFrame(obj, finish))
		}
	} else if n.nextIndex > 0 && finish == "stop" {
		choice["finish_reason"] = "tool_calls"
	}
	return n.encode(frames), true, false
}

// FlushFrames returns an incomplete marker verbatim on both EOF and [DONE].
func (n *MarkupChunkNormalizer) FlushFrames() []string {
	if !n.repair.Enabled() {
		return nil
	}
	var frames []map[string]any
	if tail := n.repair.Flush(); tail != "" {
		frame := finishOnlyFrame(n.last, "")
		choices := frame["choices"].([]any)
		choice := choices[0].(map[string]any)
		delete(choice, "finish_reason")
		choice["delta"] = map[string]any{"content": tail}
		frames = append(frames, frame)
	}
	if n.nextIndex > 0 && !n.finished {
		frames = append(frames, finishOnlyFrame(n.last, "tool_calls"))
		n.finished = true
	}
	return n.encode(frames)
}

func markupCallFrame(src map[string]any, idx int, call MarkupCall) map[string]any {
	frame := map[string]any{
		"object": "chat.completion.chunk",
		"choices": []any{map[string]any{
			"index": 0,
			"delta": map[string]any{
				"tool_calls": markupToolCalls([]MarkupCall{call}, idx),
			},
		}},
	}
	for _, k := range []string{"created", "model"} {
		if v, ok := src[k]; ok && v != nil {
			frame[k] = v
		}
	}
	return frame
}

func finishOnlyFrame(src map[string]any, finish string) map[string]any {
	frame := map[string]any{
		"object": "chat.completion.chunk",
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": finish,
		}},
	}
	for _, k := range []string{"created", "model"} {
		if v, ok := src[k]; ok && v != nil {
			frame[k] = v
		}
	}
	return frame
}
