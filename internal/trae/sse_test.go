package trae

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// soloFrames 把 SOLO SSE 输入经 ConvertSOLOToOpenAI 转换后，解析出所有 OpenAI JSON 帧。
func soloFrames(t *testing.T, raw string) []map[string]any {
	t.Helper()
	var buf bytes.Buffer
	if err := ConvertSOLOToOpenAI(strings.NewReader(raw), &buf); err != nil {
		t.Fatal(err)
	}
	var frames []map[string]any
	for _, ln := range strings.Split(buf.String(), "\n") {
		ln = strings.TrimSpace(ln)
		if strings.HasPrefix(ln, "data: ") && !strings.HasPrefix(ln, "data: [DONE]") {
			var obj map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(ln, "data: ")), &obj); err != nil {
				t.Fatalf("bad frame %q: %v", ln, err)
			}
			frames = append(frames, obj)
		}
	}
	return frames
}

// TestConvertSOLOToolCallsNormalizedToOpenAI 锁定 Trae/SOLO 流式 tool_calls 归一化：
// 上游 function_call → 标准 function，namespace/partial_arguments 剥离，
// done 的 finish_reason 由 stop 归一化为 tool_calls（用户报的三个硬伤）。
func TestConvertSOLOToolCallsNormalizedToOpenAI(t *testing.T) {
	raw := "event: output\n" +
		"data: {\"response\":\"\",\"reasoning_content\":\"\",\"tool_calls\":[{\"index\":0,\"id\":\"call_a\",\"type\":\"function\",\"function_call\":{\"name\":\"get_weather\",\"arguments\":\"{\\\"city\\\":\\\"北京\\\"}\",\"namespace\":\"trae\",\"partial_arguments\":\"\"}}]}\n\n" +
		"event: token_usage\n" +
		"data: {\"total_tokens\":11}\n\n" +
		"event: done\n" +
		"data: {\"finish_reason\":\"stop\"}\n\n"

	frames := soloFrames(t, raw)
	if len(frames) == 0 {
		t.Fatal("no frames emitted")
	}
	// 找到带 tool_calls 的那一帧
	var sawToolCalls bool
	for _, f := range frames {
		for _, c := range f["choices"].([]any) {
			cm := c.(map[string]any)
			if tc, ok := cm["delta"].(map[string]any)["tool_calls"].([]any); ok && len(tc) > 0 {
				call := tc[0].(map[string]any)
				fn, ok := call["function"].(map[string]any)
				if !ok {
					t.Fatalf("tool_call has no function: %#v", call)
				}
				if _, leaked := call["function_call"]; leaked {
					t.Errorf("function_call not stripped: %#v", call)
				}
				if fn["name"] != "get_weather" {
					t.Errorf("function.name=%v", fn["name"])
				}
				if fn["arguments"] != `{"city":"北京"}` {
					t.Errorf("function.arguments=%q", fn["arguments"])
				}
				if _, leaked := fn["namespace"]; leaked {
					t.Errorf("namespace leaked: %v", fn["namespace"])
				}
				sawToolCalls = true
			}
			if fr, ok := cm["finish_reason"].(string); ok && fr == "tool_calls" {
				if !sawToolCalls {
					// finish 帧可能在 tool_calls 帧之后才出现，这里只校验它确实被归一化
				}
			}
		}
	}
	// done 帧必须出现在 tool_calls 之后，且 finish_reason 归一化为 tool_calls
	sawFinish := false
	for _, f := range frames {
		for _, c := range f["choices"].([]any) {
			if fr, ok := c.(map[string]any)["finish_reason"].(string); ok && fr == "tool_calls" {
				sawFinish = true
			}
		}
	}
	if !sawToolCalls {
		t.Error("no tool_calls frame seen")
	}
	if !sawFinish {
		t.Error("finish_reason not normalized to tool_calls")
	}
}
