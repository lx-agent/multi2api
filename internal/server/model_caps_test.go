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

// v2 七段往返（2026-11-06，用户 m00003：上下文/最大输出/输出模态随探针缓存持久化）。
func TestParseCapsValueV2Roundtrip(t *testing.T) {
	want := capResult{
		Text: true, Vision: true, At: time.Unix(1_760_000_000, 0),
		Context: 131072, MaxOut: 8192, OutMod: OutputModalityText,
	}
	v := formatCapsValue(want)
	got, ok := parseCapsValue(v)
	if !ok {
		t.Fatalf("parseCapsValue(%q) not ok", v)
	}
	if got.Context != want.Context || got.MaxOut != want.MaxOut || got.OutMod != want.OutMod {
		t.Fatalf("meta roundtrip mismatch: got %+v want %+v (raw %q)", got, want, v)
	}
	// 生图档：能力位全零 + OutMod=image，同样要能完整往返（sweep 落的就是它）。
	img := capResult{At: time.Unix(1_760_000_000, 0), OutMod: OutputModalityImage}
	got2, ok := parseCapsValue(formatCapsValue(img))
	if !ok || got2.OutMod != OutputModalityImage || got2.Context != 0 || got2.Text {
		t.Fatalf("image meta roundtrip mismatch: ok=%v got %+v", ok, got2)
	}
}

// v1 四段兼容：旧档解析成功、元数据为零值（sweep 会原地回填，见 capsSweepOnce）。
func TestParseCapsValueV1Compat(t *testing.T) {
	got, ok := parseCapsValue("1,0,1,1760000000")
	if !ok {
		t.Fatal("v1 value must still parse")
	}
	if !got.Text || got.Tools || !got.Vision {
		t.Fatalf("v1 capability bits wrong: %+v", got)
	}
	if got.Context != 0 || got.MaxOut != 0 || got.OutMod != "" {
		t.Fatalf("v1 metadata must be zero-valued, got %+v", got)
	}
}

// v2 档自身的脏值要拒：坏 mod、负数窗口、非数字窗口。
func TestParseCapsValueV2RejectsGarbage(t *testing.T) {
	for _, v := range []string{
		"1,0,1,1760000000,131072,8192,banana", // mod 非法值：别猜
		"1,0,1,1760000000,-1,8192,text",       // 负上下文
		"1,0,1,1760000000,131072,-8192,text",  // 负最大输出
		"1,0,1,1760000000,x,8192,text",        // ctx 非数字
		"1,0,1,1760000000,131072,x,text",      // maxout 非数字
		"1,0,1,1760000000,131072",             // 段数 5：既不是 v1 也不是 v2
	} {
		if _, ok := parseCapsValue(v); ok {
			t.Fatalf("parseCapsValue(%q) should fail", v)
		}
	}
}

// 生图元数据结果（OutMod=image，无 chat 能力位）在三处判据的守卫：
// modelVisible 放行（别被 ¬text∧¬tools 误杀）、inputTypeOf 空串（别把「没探
// chat」说成「文本输入」）、capsMetaOf 把 mod 交给目录列。
func TestImageMetaResultGuards(t *testing.T) {
	h := newCapsHandler(t, true)
	now := time.Now()
	h.caps["draw"] = capResult{OutMod: OutputModalityImage, At: now}

	if !h.modelVisible("draw") {
		t.Fatal("image meta-only result must stay visible (no chat probe for image models)")
	}
	if got := h.inputTypeOf("draw"); got != "" {
		t.Fatalf("image meta result inputType=%q, want empty", got)
	}
	ctx, maxOut, mod := h.capsMetaOf("draw")
	if ctx != 0 || maxOut != 0 || mod != OutputModalityImage {
		t.Fatalf("capsMetaOf(draw) = (%d,%d,%q)", ctx, maxOut, mod)
	}
	// 选人判据：生图模型没有 chat 结论，仍不该出现在 chat 候选里（手填兜底）。
	if h.modelProbedAvailable("draw") {
		t.Fatal("image meta-only result must not be a chat candidate")
	}
	// applyCapsFilter：生图条目即便只有 meta 结果也必须保留。
	in := []map[string]any{{"id": "draw", "capabilities": []string{"image_generation"}}}
	if out := h.applyCapsFilter(in); len(out) != 1 {
		t.Fatalf("image entry with meta result must survive filter, got %d", len(out))
	}
}

func TestCapsMetaOf(t *testing.T) {
	h := newCapsHandler(t, true)
	now := time.Now()
	h.caps["full"] = capResult{Text: true, At: now, Context: 131072, MaxOut: 8192, OutMod: OutputModalityText}
	h.caps["noMeta"] = capResult{Text: true, At: now} // v1 旧档回填前的中间态

	ctx, maxOut, mod := h.capsMetaOf("full")
	if ctx != 131072 || maxOut != 8192 || mod != OutputModalityText {
		t.Fatalf("capsMetaOf(full) = (%d,%d,%q)", ctx, maxOut, mod)
	}
	if ctx, maxOut, mod := h.capsMetaOf("noMeta"); ctx != 0 || maxOut != 0 || mod != "" {
		t.Fatalf("capsMetaOf(noMeta) = (%d,%d,%q), want zeros", ctx, maxOut, mod)
	}
	if ctx, maxOut, mod := h.capsMetaOf("never"); ctx != 0 || maxOut != 0 || mod != "" {
		t.Fatalf("capsMetaOf(never) = (%d,%d,%q), want zeros", ctx, maxOut, mod)
	}
	// 探测关闭 ⇒ 惰性：目录列全空，不该凭空出现元数据。
	off := newCapsHandler(t, false)
	if ctx, maxOut, mod := off.capsMetaOf("full"); ctx != 0 || maxOut != 0 || mod != "" {
		t.Fatalf("disabled probe must be inert, got (%d,%d,%q)", ctx, maxOut, mod)
	}
}

// stampChatMeta：探针结果落盘前盖元数据戳。
func TestStampChatMeta(t *testing.T) {
	res := capResult{Text: true}
	stampChatMeta(&res, 131072, 8192)
	if res.Context != 131072 || res.MaxOut != 8192 {
		t.Fatalf("meta not stamped: %+v", res)
	}
	if res.OutMod != OutputModalityText {
		t.Fatalf("conclusive chat result must mark OutMod=text, got %q", res.OutMod)
	}

	// 上游没给的窗口字段不覆盖（0 = 未知，不是 0 = 已知为 0）；但只要拿到了
	// 文本结论，输出模态就是「文本」—— 模态由探测结论决定，与窗口无关。
	res2 := capResult{Text: true}
	stampChatMeta(&res2, 0, 0)
	if res2.Context != 0 || res2.MaxOut != 0 || res2.OutMod != OutputModalityText {
		t.Fatalf("zero window meta must stay unknown, modality follows probe result: %+v", res2)
	}

	// 判死（¬text ∧ ¬tools）与不可达不许标 text —— 那是把「探不出」说成「文本输出」。
	dead := capResult{At: time.Now()}
	stampChatMeta(&dead, 4096, 1024)
	if dead.OutMod != "" || dead.Context != 4096 {
		t.Fatalf("dead result must keep OutMod empty, got %+v", dead)
	}

	// 已有 OutMod 不改写（image 条目混进探测路径时保住原值）。
	img := capResult{OutMod: OutputModalityImage, Text: true}
	stampChatMeta(&img, 8, 8)
	if img.OutMod != OutputModalityImage {
		t.Fatalf("existing OutMod must not be overwritten, got %q", img.OutMod)
	}
}

// capsEntryMeta：目录条目元数据抽取的**类型容忍** —— entryOf 写进去的可能是
// int / int64 / float64（JSON 反序列化的常见落点），只认 >0。
func TestCapsEntryMeta(t *testing.T) {
	if ctx, maxOut := capsEntryMeta(map[string]any{
		"context_length": 131072, "max_output_tokens": int64(8192),
	}); ctx != 131072 || maxOut != 8192 {
		t.Fatalf("typed values: (%d,%d)", ctx, maxOut)
	}
	if ctx, maxOut := capsEntryMeta(map[string]any{
		"context_length": 131072.0, "max_output_tokens": 8192.0, // encoding/json → float64
	}); ctx != 131072 || maxOut != 8192 {
		t.Fatalf("float64 values: (%d,%d)", ctx, maxOut)
	}
	if ctx, maxOut := capsEntryMeta(map[string]any{
		"context_length": 0, "max_output_tokens": -1, // 0 与负数都是「没说」
	}); ctx != 0 || maxOut != 0 {
		t.Fatalf("non-positive must be unknown: (%d,%d)", ctx, maxOut)
	}
	if ctx, maxOut := capsEntryMeta(map[string]any{
		"context_length": "131072", // 字符串：不是数字类型，别猜
	}); ctx != 0 || maxOut != 0 {
		t.Fatalf("string value must be ignored: (%d,%d)", ctx, maxOut)
	}
	if ctx, maxOut := capsEntryMeta(map[string]any{}); ctx != 0 || maxOut != 0 {
		t.Fatalf("empty entry: (%d,%d)", ctx, maxOut)
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
	h.caps["m5"] = capResult{Unreachable: true, Reason: "text http 503", At: now}
	if got := h.inputTypeOf("m5"); got != InputTypeUnreachable {
		t.Fatalf("unreachable m5=%q", got)
	}
}

// 不可达语义（m01624）：从目录剔除，但不是判死 —— 无落盘值、conclusive()=false，
// sweep 每轮重探，恢复后自动回归。
func TestUnreachableHiddenButRetryable(t *testing.T) {
	h := newCapsHandler(t, true)
	h.caps["trae/dead"] = capResult{At: time.Now()} // 确定性 ¬text ∧ ¬tools
	h.caps["trae/gone"] = capResult{Unreachable: true, Reason: "text transport", At: time.Now()}

	if h.modelVisible("trae/gone") {
		t.Fatal("unreachable must be hidden from catalog (m01624)")
	}
	if h.modelVisible("trae/dead") {
		t.Fatal("deterministic dead must stay hidden")
	}
	if h.modelVisible("trae/never") {
		// 无结果 = 未探测，仍放行（首次 sweep 前不能把目录清空）。
		t.Log("unknown kept visible")
	}
	if (capResult{Unreachable: true}).conclusive() {
		t.Fatal("unreachable must not be conclusive (else it never re-probes)")
	}
	in := []map[string]any{{"id": "trae/gone"}, {"id": "trae/never"}}
	if out := h.applyCapsFilter(in); len(out) != 1 || out[0]["id"] != "trae/never" {
		t.Fatalf("filter must drop only unreachable, got %v", out)
	}
}

// modelProbedAvailable（用户 m02135）：分组编辑候选的选人判据。
// 与 modelVisible 相反的一点是**未探测也排除**——编辑器里列出一个
// 还没验证过的模型，选中后调不调得通全凭运气；手填入口保留兜底。
func TestModelProbedAvailable(t *testing.T) {
	h := newCapsHandler(t, true)
	now := time.Now()
	h.caps["m-text"] = capResult{Text: true, At: now}
	h.caps["m-tools"] = capResult{Tools: true, At: now}
	h.caps["m-dead"] = capResult{At: now} // 确定性 ¬text ∧ ¬tools
	h.caps["m-gone"] = capResult{Unreachable: true, Reason: "text transport", At: now}

	if !h.modelProbedAvailable("m-text") || !h.modelProbedAvailable("m-tools") {
		t.Fatal("conclusive Text/Tools must be selectable")
	}
	if h.modelProbedAvailable("m-dead") {
		t.Fatal("deterministic negative must not be selectable")
	}
	if h.modelProbedAvailable("m-gone") {
		t.Fatal("unreachable must not be selectable")
	}
	if h.modelProbedAvailable("m-never") {
		t.Fatal("unprobed must NOT be selectable (stricter than modelVisible)")
	}
	if h.ModelProbedAvailable("m-text") != h.modelProbedAvailable("m-text") {
		t.Fatal("exported wrapper must agree")
	}

	// 探测关闭 ⇒ 谓词恒真：候选池退化为全量，编辑器照常可用。
	off := newCapsHandler(t, false)
	if !off.modelProbedAvailable("anything") {
		t.Fatal("disabled probe must make the predicate inert (no filtering)")
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

func TestExtractSSEError(t *testing.T) {
	// 嵌套 error 对象（trae/workbuddy 格式）
	traeSSE := `data: {"choices":[{"delta":{},"finish_reason":"stop","index":0}],"created":1793851950,"id":"chatcmpl-1","model":"","object":"chat.completion.chunk"}

data: {"error":{"code":"4001","message":"We're sorry, the param is invalid. Please try with a valid param."}}

data: [DONE]
`
	got := extractSSEError(traeSSE)
	if got == nil {
		t.Fatal("trae nested error not extracted")
	}
	e, ok := got["error"].(map[string]any)
	if !ok {
		t.Fatalf("error key not a map: %v", got)
	}
	if e["code"] != "4001" {
		t.Fatalf("code=%v want 4001", e["code"])
	}
	msg, _ := e["message"].(string)
	if !strings.Contains(msg, "param is invalid") {
		t.Fatalf("message=%q", msg)
	}

	// 顶层 error_code/error_msg（codearts 格式）
	codeartsSSE := `data:{"text":"[DONE]","error_code":"TM.000010051","error_msg":"您当前无权调用该模型，请联系企业管理员开通。"}
`
	got = extractSSEError(codeartsSSE)
	if got == nil {
		t.Fatal("codearts error not extracted")
	}
	e = got["error"].(map[string]any)
	if e["code"] != "TM.000010051" {
		t.Fatalf("code=%v want TM.000010051", e["code"])
	}
	msg, _ = e["message"].(string)
	if !strings.Contains(msg, "无权调用") {
		t.Fatalf("message=%q", msg)
	}

	// 无 error 的 SSE → nil
	if extractSSEError(`data: {"choices":[{"delta":{"content":"hi"}}]}

data: [DONE]
`) != nil {
		t.Fatal("no error should return nil")
	}
	// [DONE] 单独 → nil
	if extractSSEError("data: [DONE]\n") != nil {
		t.Fatal("[DONE] should return nil")
	}
	// 空 → nil
	if extractSSEError("") != nil {
		t.Fatal("empty should return nil")
	}
	// garbage → nil
	if extractSSEError("not json") != nil {
		t.Fatal("garbage should return nil")
	}
}
