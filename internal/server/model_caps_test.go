package server

// model_caps_test：能力探测的**纯函数判据**测试 —— 不碰网络、不碰账号池。
//
// 探测路径（probeModel/probeAttempt）依赖装配好的 Provider/Pool，本文件不覆盖；
// 覆盖的是所有"决定模型去留与标注"的判据函数：这些错了才会真的误杀模型。

import (
	"strings"
	"testing"
	"time"
)

func newCapsHandler(t *testing.T, enabled bool) *Handler {
	t.Helper()
	h := &Handler{caps: map[string]capResult{}, capsProbing: map[string]bool{}}
	if enabled {
		h.cfg.Caps = &CapsProbeConfig{Enabled: true}
	}
	return h
}

func TestParseCapsValueRoundtrip(t *testing.T) {
	want := capResult{Text: true, Tools: false, Vision: true, At: time.Unix(1_760_000_000, 0)}
	v := formatCapsValue(want)
	got, ok := parseCapsValue(v)
	if !ok {
		t.Fatalf("parseCapsValue(%q) not ok", v)
	}
	if got.Text != want.Text || got.Tools != want.Tools || got.Vision != want.Vision || !got.At.Equal(want.At) {
		t.Fatalf("roundtrip mismatch: %+v vs %+v", got, want)
	}
}

func TestParseCapsValueRejectsGarbage(t *testing.T) {
	for _, v := range []string{
		"", "1", "1,0", "1,0,1", // 段数不足
		"1,0,1,x",                // at 非数字
		"1,0,1,999",              // at 早于下限：视为脏数据
		"a,0,1,1760000000",       // text 非数字
		"1,0,1,1760000000,extra", // 段数过多
		" 1,0,1,1760000000",      // 前导空白（Split 后首段非数字）
	} {
		if _, ok := parseCapsValue(v); ok {
			t.Fatalf("parseCapsValue(%q) should fail", v)
		}
	}
}

func TestCapsOptionKeyLimits(t *testing.T) {
	if k, ok := capsOptionKey("trae/kimi-k2"); !ok || k != "caps.trae/kimi-k2" {
		t.Fatalf("normal key: %q ok=%v", k, ok)
	}
	if _, ok := capsOptionKey(strings.Repeat("x", 60)); ok {
		t.Fatal("64-char key must be rejected (prefix makes it >64)")
	}
	if _, ok := capsOptionKey("a b"); ok {
		t.Fatal("key with space must be rejected")
	}
}

func TestApplyCapsFilter(t *testing.T) {
	h := newCapsHandler(t, true)
	now := time.Now()
	h.caps["trae/dead"] = capResult{At: now} // ¬text ∧ ¬tools，确定性判死
	h.caps["trae/text"] = capResult{Text: true, At: now}
	h.caps["trae/toolsonly"] = capResult{Tools: true, At: now}
	h.caps["trae/old-dead"] = capResult{At: now.Add(-30 * 24 * time.Hour)} // 判死永久有效（无 TTL 复活）

	entry := func(id string, image bool) map[string]any {
		e := map[string]any{"id": id}
		if image {
			e["capabilities"] = []string{"image_generation"}
		}
		return e
	}
	in := []map[string]any{
		entry("trae/dead", false),
		entry("trae/text", false),
		entry("trae/toolsonly", false),
		entry("trae/unknown", false),
		entry("trae/old-dead", false),
		entry("trae/draw", true), // 生图豁免：即便没有任何探测结果也必须保留
	}
	out := h.applyCapsFilter(in)

	want := map[string]bool{
		"trae/text": true, "trae/toolsonly": true, "trae/unknown": true,
		"trae/draw": true,
	}
	if len(out) != len(want) {
		t.Fatalf("len=%d want %d", len(out), len(want))
	}
	for _, e := range out {
		id, _ := e["id"].(string)
		if !want[id] {
			t.Fatalf("unexpected kept id %q", id)
		}
	}
	// 原地压缩坑回归：输入切片的元素顺序与内容不得被改写。
	if id0, _ := in[0]["id"].(string); id0 != "trae/dead" {
		t.Fatalf("input slice mutated: in[0]=%v", in[0])
	}
}

func TestCapsFilterDisabledIsIdentity(t *testing.T) {
	h := newCapsHandler(t, false)
	in := []map[string]any{{"id": "trae/dead"}}
	if out := h.applyCapsFilter(in); len(out) != 1 {
		t.Fatalf("disabled filter must not touch list, got %d", len(out))
	}
	if !h.modelVisible("trae/dead") || h.inputTypeOf("trae/dead") != "" {
		t.Fatal("disabled probe must be fully inert")
	}
}

func TestInputTypeOf(t *testing.T) {
	h := newCapsHandler(t, true)
	now := time.Now()
	h.caps["m1"] = capResult{Text: true, At: now}
	h.caps["m2"] = capResult{Text: true, Vision: true, At: now}
	h.caps["m3"] = capResult{Tools: true, At: now} // 工具可用即文本输入可用（判据见 inputType 注释）
	if got := h.inputTypeOf("m1"); got != InputTypeText {
		t.Fatalf("m1=%q", got)
	}
	if got := h.inputTypeOf("m2"); got != InputTypeTextImage {
		t.Fatalf("m2=%q", got)
	}
	if got := h.inputTypeOf("m3"); got != InputTypeText {
		t.Fatalf("m3=%q", got)
	}
	if got := h.inputTypeOf("m4"); got != "" {
		t.Fatalf("unknown m4=%q", got)
	}
}

func TestProbeHasReply(t *testing.T) {
	mk := func(content any, reasoning string) map[string]any {
		msg := map[string]any{"content": content}
		if reasoning != "" {
			msg["reasoning_content"] = reasoning
		}
		return map[string]any{"choices": []any{map[string]any{"message": msg}}}
	}
	if !probeHasReply(mk("OK", "")) {
		t.Fatal("plain text reply")
	}
	if !probeHasReply(mk("", "thinking...")) {
		t.Fatal("reasoning-only reply counts")
	}
	if !probeHasReply(mk([]any{map[string]any{"type": "text", "text": "red"}}, "")) {
		t.Fatal("array content counts")
	}
	if probeHasReply(mk("", "")) {
		t.Fatal("empty reply must not count")
	}
	if probeHasReply(map[string]any{}) {
		t.Fatal("no choices must not count")
	}
}

func TestProbeToolCalled(t *testing.T) {
	withName := map[string]any{"choices": []any{map[string]any{
		"finish_reason": "tool_calls",
		"message": map[string]any{"tool_calls": []any{
			map[string]any{"function": map[string]any{"name": "get_weather", "arguments": `{"city":"北京"}`}},
		}},
	}}}
	if !probeToolCalled(withName) {
		t.Fatal("named tool call with finish_reason=tool_calls")
	}
	// 修复前的 Trae 形状：finish_reason=stop 且 tool_calls[0] 没有 function —— 必须判 false。
	traeBroken := map[string]any{"choices": []any{map[string]any{
		"finish_reason": "stop",
		"message": map[string]any{"tool_calls": []any{
			map[string]any{"id": "call_1", "index": 0, "type": "function"},
		}},
	}}}
	if probeToolCalled(traeBroken) {
		t.Fatal("function-less tool_calls must NOT count as tools capability")
	}
	// finish=tool_calls 但函数名缺失（残缺形状）也不算 —— 判死宁严勿松。
	nameless := map[string]any{"choices": []any{map[string]any{
		"finish_reason": "tool_calls",
		"message":       map[string]any{"tool_calls": []any{map[string]any{"id": "c1"}}},
	}}}
	if probeToolCalled(nameless) {
		t.Fatal("tool_calls without function.name must not count")
	}
	// finish_reason=stop 但 message 里带完整 tool_calls（部分上游不发专用 finish）：算。
	stopButCalled := map[string]any{"choices": []any{map[string]any{
		"finish_reason": "stop",
		"message": map[string]any{"tool_calls": []any{
			map[string]any{"function": map[string]any{"name": "get_weather"}},
		}},
	}}}
	if !probeToolCalled(stopButCalled) {
		t.Fatal("complete tool_calls under finish=stop still proves the capability")
	}
}

func TestAggregateProbeResponse(t *testing.T) {
	// JSON 直解路径。
	j := aggregateProbeResponse([]byte(`{"choices":[{"message":{"content":"OK"}}]}`))
	if j == nil || !probeHasReply(j) {
		t.Fatalf("json path: %v", j)
	}
	// SSE 路径（OpenAI 标准帧 + [DONE]）。
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"O\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"K\"},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	a := aggregateProbeResponse([]byte(sse))
	if a == nil {
		t.Fatal("sse path returned nil")
	}
	msg := firstChoiceMessage(a)
	if msg == nil {
		t.Fatalf("sse aggregate has no message: %v", a)
	}
	if s, _ := msg["content"].(string); s != "OK" {
		t.Fatalf("sse content=%q want OK", s)
	}
	if aggregateProbeResponse(nil) != nil || aggregateProbeResponse([]byte("   ")) != nil {
		t.Fatal("empty input must return nil (netOK=false), not an empty map")
	}
	if aggregateProbeResponse([]byte("not json at all")) != nil {
		t.Fatal("garbage must return nil")
	}
}

func TestProbeBodiesUseBareModel(t *testing.T) {
	for name, b := range map[string][]byte{
		"text":   probeTextBody("kimi-k2"),
		"tools":  probeToolsBody("kimi-k2", "required"),
		"vision": probeVisionBody("kimi-k2"),
	} {
		if !strings.Contains(string(b), `"model":"kimi-k2"`) {
			t.Fatalf("%s body must carry bare model id: %s", name, b)
		}
		if strings.Contains(string(b), "trae/") {
			t.Fatalf("%s body leaked prefixed model: %s", name, b)
		}
	}
}
