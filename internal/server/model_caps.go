package server

// ── 模型能力探测（文本 / 工具调用 / 图片识别）───────────────────────────────
//
// # 需求（用户 2026-11-05，参考共享对话的实测缺陷）
//
//  1. 拉取上游模型目录时，先分别测试每个模型的**文本**、**工具调用**、
//     **图片识别**能力；若**既无文本又无工具**能力，不纳入列表
//     （/v1/models 与模型目录同一判据，保持 T09 相等不变式）。
//  2. 模型管理页的「来源」列改为「输入类型」：文本 / 文本和图片。
//
// # 为什么判据是「两者皆无才剔除」
//
// 需求二的取值里「文本」本身就是类别（对话请求的输入永远是文本），
// 单看「文本」无法构成排除判据 —— 能成立的是「文本和图片」相对「文本」的
// 增量（vision 结果决定标注），而排除必须落在「连最基本的一次对话都发不动
// 且不会调工具」的模型上。所以：
//
//	剔除 ⇔ ¬text ∧ ¬tools；  标注 ⇔ vision ? "文本和图片" : "文本"
//
// # 探测为什么必须异步（不能同步塞进 modelListWithScrub）
//
// 目录拉取本身就是慢路径（实测直连上游 36.2s/23.8s 不收敛，见 handler.go
// modelsForProviderCached 的注释），请求路径纪律是「只读缓存，后台灌热」
// （models_prewarm.go）。探测比拉目录更贵（每个模型 2~4 次小请求），
// 所以同样只在后台 sweep 里做；出口层与页面读的是内存缓存。
//
// # 结果永久有效（用户修订 2026-11-05：判死不重探）
//
// 探测结果写盘（store options 表），内存缓存在进程存续期内**永不过期**：
// 无新模型、也无不可达标记时 sweep 无事可做，判死的模型不会被重探。
// 入口是「目录里出现了没有确定性结果的模型」（新模型，或上轮不可达的）。
// 要人工复活某个判死模型：删掉 options 表里它的 caps.<id> 键（或直接 UPDATE
// 该值）再等下一轮。
// 注：目录 id 超过 64 字符的模型落不了盘（optionMaxKeyLen），结果只在内存，
// 重启后会重新探测一次 —— 已知且可接受的退化。
//
// # 未知 ≠ 判死（2026-11-05 二次修订：未知要写明「不可达」；
// 三次修订 m01624：不可达直接从目录剔除）
//
// 拿不到账号 / 传输层错误 / 5xx / 429 —— **不是模型能力的结论**：不判死、
// 不落盘，每轮 sweep 都会重探，上游恢复即自动回到目录。但用户明确
// 「已失效的直接不显示」，所以这类模型同样被剔出目录与 /v1/models，
// 只是剔除原因逐条写网关日志（capsProbe/capsSweep 行），而不是含糊的
// 「未探测」。只有拿到明确的应答（2xx 或上游明确 4xx 拒绝探测请求）
// 才落确定性 capResult。误判死一个可用模型比放过一个死模型伤害大得多，
// 因此判死只认确定性应答。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/store"
	"workbuddy2api/internal/wire"
)

// ── 配置 ────────────────────────────────────────────────────────────────────

// CapsProbeConfig 能力探测的接线参数（server.Config.Caps）。
//
// nil = 整条机制关闭（既有测试与未装配部署的行为与改造前逐字一致）。
type CapsProbeConfig struct {
	Enabled bool
	// SweepEvery 后台扫描间隔，<=0 回落 capsSweepEveryDefault。
	SweepEvery time.Duration
	// Concurrency 同时在探的模型数，<=0 回落 capsConcurrencyDefault。
	Concurrency int
	// Timeout 单次探测请求的超时，<=0 回落 capsProbeTimeoutDefault。
	Timeout time.Duration
}

const (
	capsSweepEveryDefault      = 60 * time.Minute
	capsConcurrencyDefault     = 2
	capsProbeTimeoutDefault    = 90 * time.Second
	capsOptionKeyPrefix        = "caps."       // store options 持久化键前缀
	capsPersistedAtMinUnixSecs = 1_000_000_000 // 2001-09 起，用于校验 at 字段
)

// ── 结果 ────────────────────────────────────────────────────────────────────

// capResult 一个模型的一次探测结果。两种形态：
//
//   - **确定性**（Unreachable=false）：三个能力位是真判据，永久有效、不重探；
//     ¬text ∧ ¬tools 即判死剔除。
//   - **不可达标记**（Unreachable=true）：尝试过但拿不到结论（没号/链路失败/
//     5xx/429/解析失败）。不判死、不剔除，页面显示「不可达」；**每轮 sweep
//     重探**，上游恢复即被确定性结果覆盖。Reason 只在内存，供日志定位。
type capResult struct {
	Text        bool // 最小文本请求拿到了非空应答
	Tools       bool // 工具调用探测拿到了带函数名的 tool_calls
	Vision      bool // 带图请求被接受并拿到非空应答（见 probeVision 的局限说明）
	Unreachable bool // 尝试过但无结论（见上）；true 时三个能力位无意义
	Reason      string
	At          time.Time
}

// conclusive 是否为确定性结果（永久有效、不再重探）。
func (c capResult) conclusive() bool { return !c.Unreachable }

// inputType 返回需求二要的类别值。
//
// 工具探测的输入本身就是文本消息，tools=true 即证明文本输入可用
// （即便正文为空、只回了 tool_calls —— 推理模型正文空是常态）。
func (c capResult) inputType() string {
	if c.Unreachable {
		return InputTypeUnreachable
	}
	if c.Vision {
		return InputTypeTextImage
	}
	return InputTypeText
}

// 前端契约取值（catalogViewModel.input_type）。页面把它们映射成中文徽章；
// 空串 = 还没探到（未入队/在途），页面显示「未探测」。
// InputTypeUnreachable 正常情况下**不会出现在目录数据里**——不可达行已被
// modelVisible 剔除（m01624）；保留该取值是防御（装配时序缝隙），页面也照
// 「不可达」渲染。
const (
	InputTypeText        = "text"
	InputTypeTextImage   = "text+image"
	InputTypeUnreachable = "unreachable"
)

// capsEnabled 探测机制是否装配开启。
func (h *Handler) capsEnabled() bool {
	return h.cfg.Caps != nil && h.cfg.Caps.Enabled
}

func (h *Handler) capsSweepEvery() time.Duration {
	if h.cfg.Caps != nil && h.cfg.Caps.SweepEvery > 0 {
		return h.cfg.Caps.SweepEvery
	}
	return capsSweepEveryDefault
}

func (h *Handler) capsConcurrency() int {
	if h.cfg.Caps != nil && h.cfg.Caps.Concurrency > 0 {
		return h.cfg.Caps.Concurrency
	}
	return capsConcurrencyDefault
}

func (h *Handler) capsTimeout() time.Duration {
	if h.cfg.Caps != nil && h.cfg.Caps.Timeout > 0 {
		return h.cfg.Caps.Timeout
	}
	return capsProbeTimeoutDefault
}

// ── 缓存读写 ────────────────────────────────────────────────────────────────

// capsKnown 返回 id 的探测结果（永不过期，见文件头「结果永久有效」）。
func (h *Handler) capsKnown(id string) (capResult, bool) {
	h.capsMu.RLock()
	res, ok := h.caps[id]
	h.capsMu.RUnlock()
	return res, ok
}

// recordCaps 写内存缓存并尽力持久化（见 persistCaps 的说明）。
func (h *Handler) recordCaps(id string, res capResult) {
	h.capsMu.Lock()
	if h.caps == nil {
		h.caps = map[string]capResult{}
	}
	h.caps[id] = res
	delete(h.capsProbing, id)
	h.capsMu.Unlock()
	h.persistCaps(id, res)
}

// clearProbing 探测失败路径也要清标记，否则该模型永远排不进队列。
func (h *Handler) clearProbing(id string) {
	h.capsMu.Lock()
	delete(h.capsProbing, id)
	h.capsMu.Unlock()
}

// modelVisible 目录过滤判据（需求一 + 用户修订 2026-11-05 m01624）。
//
// 未装配探测 / 该模型无结果（未入队/在途/渠道合成名）→ 放行。
// 确定性 ¬text ∧ ¬tools（判死）→ 剔除。
// **不可达标记 → 同样剔除**（用户明确：已失效的不显示）。与判死的区别：
// 不可达不落盘、每轮 sweep 重探，上游恢复即自动回到目录；判死永久有效、
// 不重探。剔除原因逐条写网关日志（capsSweep/capsProbe 行），页面不再兜留。
// 生图模型不走 chat 探测，由调用方（applyCapsFilter）单独放行。
func (h *Handler) modelVisible(id string) bool {
	if !h.capsEnabled() {
		return true
	}
	res, ok := h.capsKnown(id)
	if !ok {
		return true
	}
	if res.Unreachable {
		return false
	}
	return res.Text || res.Tools
}

// modelProbedAvailable 分组编辑候选的过滤判据（用户 m02135：只列探测可用的模型）。
//
// 与 modelVisible 的区别：那条是目录展示判据，未探测**放行**（不能因为探测
// 没跑到就说网关没有模型）；这里是**选人**判据，语义反过来 —— 没探测出
// 结论的模型不该出现在候选里（选了也大概率调不通），只有拿到确定性结果
// 且有文本或工具能力的才列出。生图模型不走 chat 探测、没有 chat 结论，
// 同样不列（需要时走手填入口）。
//
// 探测未启用（未装配 / Enabled=false）→ 一律放行：过滤机制不该在探测
// 关闭时把候选池清空。
func (h *Handler) modelProbedAvailable(id string) bool {
	if !h.capsEnabled() {
		return true
	}
	res, ok := h.capsKnown(id)
	if !ok {
		return false
	}
	if res.Unreachable {
		return false
	}
	return res.Text || res.Tools
}

// ModelProbedAvailable 是 modelProbedAvailable 的导出包装：装配层（main.go）
// 把它注入 admin.Config.ModelProbedAvailable，供 /admin/groups/catalog?probed=only
// 过滤候选。未接线时调用方拿 nil 回调、自行退化为不过滤。
func (h *Handler) ModelProbedAvailable(id string) bool {
	return h.modelProbedAvailable(id)
}

// inputTypeOf 需求二的列值：目录行 → "text"/"text+image"/""。
func (h *Handler) inputTypeOf(id string) string {
	if !h.capsEnabled() {
		return ""
	}
	res, ok := h.capsKnown(id)
	if !ok {
		return ""
	}
	return res.inputType()
}

// capsIsImageEntry 目录条目是否被标为生图模型（capabilities 含 image_generation）。
// 它们不能走 chat/completions，chat 探测必然失败 —— 必须豁免过滤。
func capsIsImageEntry(e map[string]any) bool {
	caps, ok := e["capabilities"].([]string)
	if !ok {
		return false
	}
	for _, c := range caps {
		if c == "image_generation" {
			return true
		}
	}
	return false
}

// applyCapsFilter 按 modelVisible 过滤 /v1/models 形态的目录列表。
//
// ⚠ 返回**新切片**：append(x[:0], ...) 原地压缩会写坏调用方持有的底层数组
// （appendChannelModels 的注释里记过一次同样的坑）。
func (h *Handler) applyCapsFilter(list []map[string]any) []map[string]any {
	if !h.capsEnabled() {
		return list
	}
	out := make([]map[string]any, 0, len(list))
	for _, e := range list {
		if capsIsImageEntry(e) {
			out = append(out, e)
			continue
		}
		id, _ := e["id"].(string)
		if id == "" || h.modelVisible(id) {
			out = append(out, e)
		}
	}
	return out
}

// ── 持久化（store options KV；表结构冻结，只能存这里）────────────────────────
//
// 键 "caps."+目录id（id 含 "provider/model"，长度常 > 前缀，仍受 optionMaxKeyLen=64
// 约束）；值 "text,tools,vision,atUnix"。任何一步不满足（键太长/含空白/值太长/
// 库未接线）都**静默跳过**：这类模型的结果只在内存，重启后会重探一次 ——
// 已知退化（见文件头）。落盘的模型则跨重启不重探（用户修订：判死不重探）。

func capsOptionKey(id string) (string, bool) {
	k := capsOptionKeyPrefix + id
	if len(k) == 0 || len(k) > 64 {
		return "", false
	}
	for i := 0; i < len(k); i++ {
		switch k[i] {
		case ' ', '\t', '\n', '\r':
			return "", false
		}
	}
	return k, true
}

func (h *Handler) persistCaps(id string, res capResult) {
	if store.DB == nil {
		return
	}
	if res.Unreachable {
		return // 不可达标记不写盘：它本来就要重探，落盘只会把「瞬时故障」伪装成「持久状态」；
		// 重启后第一轮 sweep 立即重探，代价为零（内存标记见 probeModel）。
	}
	key, ok := capsOptionKey(id)
	if !ok {
		return
	}
	val := formatCapsValue(res)
	if len(val) > 4096 { // 实际 <30，防御式检查，与 option.go 的上限同口径
		return
	}
	_ = store.PutOption(key, val) // 失败静默：见上方定位
}

// loadCapsFromStore NewHandler 构造时回填上次探测结果（重启不重烧上游额度）。
func (h *Handler) loadCapsFromStore() {
	if store.DB == nil {
		return
	}
	opts, err := store.LoadOptions()
	if err != nil || len(opts) == 0 {
		return
	}
	for k, v := range opts {
		if !strings.HasPrefix(k, capsOptionKeyPrefix) {
			continue
		}
		id := strings.TrimPrefix(k, capsOptionKeyPrefix)
		if id == "" {
			continue
		}
		res, ok := parseCapsValue(v)
		if !ok {
			continue
		}
		// 结果永久有效：不管 At 多久以前，一律回填（判死不重探）。
		h.capsMu.Lock()
		h.caps[id] = res
		h.capsMu.Unlock()
	}
}

// formatCapsValue/parseCapsValue 是一对：persistCaps 写出的就是 parseCapsValue
// 吃的格式（测试靠这条往返，别让两侧各自漂移）。
func formatCapsValue(res capResult) string {
	return fmt.Sprintf("%d,%d,%d,%d", b2i(res.Text), b2i(res.Tools), b2i(res.Vision), res.At.Unix())
}

func parseCapsValue(v string) (capResult, bool) {
	parts := strings.Split(v, ",")
	if len(parts) != 4 {
		return capResult{}, false
	}
	var tt, uu, ww, ss int64
	atoi := func(s string, out *int64) bool {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return false
		}
		*out = n
		return true
	}
	if !atoi(parts[0], &tt) || !atoi(parts[1], &uu) || !atoi(parts[2], &ww) || !atoi(parts[3], &ss) {
		return capResult{}, false
	}
	if parts[0] != "0" && parts[0] != "1" || parts[1] != "0" && parts[1] != "1" || parts[2] != "0" && parts[2] != "1" {
		return capResult{}, false // 布尔位只认 0/1，其它值是写坏的档，别猜
	}
	if ss < capsPersistedAtMinUnixSecs {
		return capResult{}, false
	}
	return capResult{Text: tt == 1, Tools: uu == 1, Vision: ww == 1, At: time.Unix(ss, 0)}, true
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ── 后台扫描 ────────────────────────────────────────────────────────────────

// StartModelCapsProbe 后台探测循环（与 StartModelsPrewarm 同一范式、同一装配点）。
//
// 幂等；ctx 退出即停；请求路径永不触发探测（纪律：后台灌热、出口只读缓存）。
func (h *Handler) StartModelCapsProbe(ctx context.Context) {
	if !h.capsEnabled() {
		return
	}
	h.capsSweepMu.Lock()
	if h.capsSweepOn {
		h.capsSweepMu.Unlock()
		return
	}
	h.capsSweepOn = true
	h.capsSweepMu.Unlock()

	interval := h.capsSweepEvery()
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		h.capsSweepOnce(ctx) // 启动先跑一次（与 prewarm 一致：目录到位就立刻有活可干）
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				h.capsSweepOnce(ctx)
			}
		}
	}()
}

// capsSweepOnce 一轮扫描：枚举**过滤前**的原始目录，只对**尚无结果**且不在探的
// 模型并行探测（结果永久有效 ⇒ 稳态下这里通常是空队列）。
func (h *Handler) capsSweepOnce(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("capsSweep: recovered panic: %v", r)
		}
	}()
	if h.cfg.Provider == nil {
		return // 单上游模式不探测：那条路径没有 provider id 语义（见 modelListRawWithScrub 的单上游分支）
	}
	raw := h.modelListRawWithScrub(false)

	type job struct{ id, provider, bare string }
	jobs := make([]job, 0, len(raw))
	h.capsMu.Lock()
	if h.caps == nil {
		h.caps = map[string]capResult{}
	}
	if h.capsProbing == nil {
		h.capsProbing = map[string]bool{}
	}
	for _, e := range raw {
		if capsIsImageEntry(e) {
			continue // 生图模型不走 chat，探测必然误判死 —— 豁免
		}
		id, _ := e["id"].(string)
		if id == "" {
			continue
		}
		if res, ok := h.caps[id]; ok && res.conclusive() {
			continue // 确定性结果（含判死）永久跳过 —— 用户修订 2026-11-05：判死不重探、无新模型不探。
			// 不可达标记**不算有结果**：每轮重探，上游恢复即被确定性结果覆盖（用户 m01504：不可用要写清楚）。
		}
		if h.capsProbing[id] {
			continue // 上一轮还在探（极端慢上游）
		}
		h.capsProbing[id] = true
		provider, bare := id, id
		if p, m, has := gateway.SplitModel(id); has && p != "" {
			provider, bare = p, m
		} else if i := strings.IndexByte(id, '/'); i > 0 {
			provider, bare = id[:i], id[i+1:]
		}
		jobs = append(jobs, job{id, provider, bare})
	}
	h.capsMu.Unlock()

	// sort 让日志/测试的调度顺序确定（map 遍历顺序不参与判据，但确定性零成本）。
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].id < jobs[j].id })

	sem := make(chan struct{}, h.capsConcurrency())
	var wg sync.WaitGroup
	var statMu sync.Mutex
	var doneC, doneU int
	for _, j := range jobs {
		if ctx.Err() != nil {
			h.clearProbing(j.id)
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(j job) {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() {
				if r := recover(); r != nil {
					h.clearProbing(j.id)
					log.Printf("capsProbe %s: recovered panic: %v", j.id, r)
				}
			}()
			res := h.probeModel(ctx, j.provider, j.bare) // ctx 是 sweep 入参（循环外单值），闭包捕获安全
			h.recordCaps(j.id, res)
			statMu.Lock()
			if res.conclusive() {
				doneC++
			} else {
				doneU++
				log.Printf("capsProbe %s: unreachable (%s)", j.id, res.Reason)
			}
			statMu.Unlock()
		}(j)
	}
	wg.Wait()
	if len(jobs) > 0 {
		log.Printf("capsSweep: probed %d · conclusive %d · unreachable %d", len(jobs), doneC, doneU)
	}
}

// ── 单模型探测 ──────────────────────────────────────────────────────────────

// probeModel 对 (provider, bare) 跑三判据，返回确定性结果或「不可达」标记。
//
// 不可达标记（capResult.Unreachable）= 尝试过但拿不到能力结论：没号/号被占/
// 没凭证/链路失败/5xx/429/401/403。**401/403 是账号侧问题，绝不能拿来判死
// 模型** —— 否则凭证一过期整个上游的活模型会被集体误杀剔除。
// 一旦文本判据拿到确定结果（成或败），整轮按「确定」处理：
// tools/vision 的基础设施错误降级为 false（宁可标注保守，不可反复重烧额度）。
func (h *Handler) probeModel(ctx context.Context, provider, bare string) capResult {
	mark := func(reason string) capResult {
		return capResult{Unreachable: true, Reason: reason, At: time.Now()}
	}
	if h.cfg.Pool == nil {
		return mark("no_pool")
	}
	acct := h.cfg.Pool.PickFor(provider, bare, nil)
	if acct == nil {
		return mark("no_account") // 该上游此刻没有可用账号：不是模型的问题
	}
	if !h.cfg.Pool.Acquire(acct.UID) {
		return mark("account_busy") // 号被在途请求占着：下轮再说
	}
	defer h.cfg.Pool.Release(acct.UID)

	cred, ok := h.credentialFor(provider, acct.UID)
	if !ok {
		return mark("no_credential")
	}

	var res capResult

	// 1) 文本
	status, agg, netOK := h.probeAttempt(ctx, provider, cred, probeTextBody(bare))
	if !netOK {
		return mark("text transport/parse")
	}
	// 上游 HTTP 200 但 SSE 里嵌 error：提取错误码分类
	if e, ok := agg["error"].(map[string]any); ok {
		code, _ := e["code"].(string)
		msg, _ := e["message"].(string)
		return mark(fmt.Sprintf("text upstream error: %s %s", code, msg))
	}
	switch {
	case status >= 200 && status < 300:
		res.Text = probeHasReply(agg)
	case status == 401 || status == 403:
		return mark(fmt.Sprintf("text http %d (auth)", status)) // 账号侧故障：判不了模型，更不该判死
	case status >= 500 || status == 429:
		return mark(fmt.Sprintf("text http %d", status)) // 上游侧故障，判不了模型
	default: // 其余 4xx：上游明确拒绝了这个最小请求 —— 保守判「无文本能力」
		res.Text = false
	}

	// 2) 工具调用（required 首发；4xx 时回落 auto 再试一次）
	status, agg, netOK = h.probeAttempt(ctx, provider, cred, probeToolsBody(bare, "required"))
	if netOK {
		if status >= 200 && status < 300 {
			res.Tools = probeToolCalled(agg)
		} else if status >= 500 || status == 429 {
			status, agg, netOK = h.probeAttempt(ctx, provider, cred, probeToolsBody(bare, "auto"))
			if netOK && status >= 200 && status < 300 {
				res.Tools = probeToolCalled(agg)
			}
		} else {
			// required 不被支持 ≠ 没有工具能力：用 auto 重试一次再定论。
			if s2, a2, ok2 := h.probeAttempt(ctx, provider, cred, probeToolsBody(bare, "auto")); ok2 && s2 >= 200 && s2 < 300 {
				res.Tools = probeToolCalled(a2)
			}
		}
	}

	// 3) 图片识别
	if status, agg, netOK := h.probeAttempt(ctx, provider, cred, probeVisionBody(bare)); netOK {
		if status >= 200 && status < 300 {
			res.Vision = probeHasReply(agg)
		}
	}

	res.At = time.Now()
	return res
}

// probeAttempt 发一次探测请求并聚合 SSE。
//
// 返回 (状态码, 聚合响应, netOK)。netOK=false 只表示**我们这边/链路**没拿到
// 应答（transport 失败、读流失败、解析失败）；true 时 status 是上游真答的码。
func (h *Handler) probeAttempt(ctx context.Context, provider string, cred gateway.Credential, body []byte) (int, map[string]any, bool) {
	cctx, cancel := context.WithTimeout(ctx, h.capsTimeout())
	defer cancel()
	cs, err := h.chatProvider(cctx, provider, cred, body)
	if err != nil {
		log.Printf("capsProbe %s: chatProvider err: %v", provider, err)
		return 0, nil, false
	}
	if cs.Body == nil {
		log.Printf("capsProbe %s: chatProvider nil Body (status %d)", provider, cs.Status)
		return cs.Status, nil, false
	}
	defer cs.Body.Close()
	raw, rerr := io.ReadAll(io.LimitReader(cs.Body, maxToolLoopRespBytes))
	if rerr != nil {
		log.Printf("capsProbe %s: read body err: %v", provider, rerr)
		return cs.Status, nil, false
	}
	// SSE 与 JSON 两种落地都要能吃：上游对 stream=false 的请求可能直接回 JSON。
	agg := aggregateProbeResponse(raw)
	if agg == nil {
		log.Printf("capsProbe %s: aggregate parse failed, raw head: %.300q", provider, string(raw))
		return cs.Status, nil, false
	}
	return cs.Status, agg, true
}

// aggregateProbeResponse 把响应字节聚合成 chat.completion 形态 map。
//
// wire.Aggregate 吃 SSE；非 SSE（上游真非流式回 JSON）时退回直接解析。
// SSE 里嵌 error 时（上游 HTTP 200 但流里带错误）提取错误信息返回，
// 让 probeModel 能分类，而不是一律 transport/parse。
// 两者都失败返回 nil（调用方按 netOK=false 处理）。
func aggregateProbeResponse(raw []byte) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	trimmed := strings.TrimSpace(string(raw))
	if strings.HasPrefix(trimmed, "{") {
		var m map[string]any
		if json.Unmarshal([]byte(trimmed), &m) == nil {
			return m
		}
		return nil
	}
	agg, err := wire.Aggregate(strings.NewReader(trimmed))
	if err != nil || agg == nil {
		// SSE 里嵌 error：提取错误信息让 probeModel 能分类
		return extractSSEError(trimmed)
	}
	return agg
}

// extractSSEError 从 SSE 流中提取错误信息。
//
// 上游 HTTP 200 但 SSE 里带 error 字段时，wire.Aggregate 无法产出有效
// completion，返回 nil。这里扫描 data: 行找 error 字段，提取成 map 供
// probeModel 分类为「不可达（上游报错）」，而不是 transport/parse。
// 支持两种格式：
//   - 嵌套 error 对象（trae/workbuddy）：{"error":{"code":"4001","message":"..."}}
//   - 顶层 error_code/error_msg（codearts）：{"error_code":"TM...","error_msg":"..."}
func extractSSEError(raw string) map[string]any {
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" || payload == "" {
			continue
		}
		var obj map[string]any
		if json.Unmarshal([]byte(payload), &obj) != nil {
			continue
		}
		// 嵌套 error 对象（trae/workbuddy 格式）
		if e, ok := obj["error"].(map[string]any); ok {
			return map[string]any{"error": e}
		}
		// 顶层 error_code/error_msg（codearts 格式）
		if code, ok := obj["error_code"].(string); ok {
			msg, _ := obj["error_msg"].(string)
			return map[string]any{"error": map[string]any{"code": code, "message": msg}}
		}
	}
	return nil
}

// ── 探测请求体 ──────────────────────────────────────────────────────────────
//
// ⚠ body 的 model 必须是**裸名**：Provider 契约如此（handler.go 出站只在请求
// 带前缀时 rewriteModel 成裸名；见 provider_router.go rewriteModel 的注释）。
// max_tokens 给小值：探测只为「能不能、回没回」，不为生成质量。

func probeMarshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil { // 常量结构不可能失败；失败说明改动引入 bug，panic 给 sweep 的 recover 兜住
		panic(fmt.Sprintf("caps probe body: %v", err))
	}
	return b
}

func probeTextBody(bare string) []byte {
	return probeMarshal(map[string]any{
		"model": bare,
		"messages": []map[string]any{
			{"role": "user", "content": "Reply with exactly one word: OK"},
		},
		"max_tokens": 16,
		"stream":     false,
	})
}

func probeToolsBody(bare, choice string) []byte {
	return probeMarshal(map[string]any{
		"model": bare,
		"messages": []map[string]any{
			{"role": "user", "content": "北京今天天气怎么样？必须调用 get_weather 工具查询后再回答。"},
		},
		"tools": []map[string]any{
			{"type": "function", "function": map[string]any{
				"name":        "get_weather",
				"description": "查询指定城市的当前天气",
				"parameters": map[string]any{
					"type":       "object",
					"properties": map[string]any{"city": map[string]any{"type": "string", "description": "城市名"}},
					"required":   []string{"city"},
				},
			}},
		},
		"tool_choice": choice,
		"max_tokens":  256,
		"stream":      false,
	})
}

// probeVisionBody 带一张 1x1 红色 PNG 的最小多模态请求。
//
// ⚠ 局限（如实记录）：判据是「上游接受带图请求并给出非空应答」。
// 网关无法验证模型**真的看懂了**图（答案对不对因模型而异），
// 所以这里探测的是「输入类型」——需求二恰好问的就是这个（文本 vs 文本和图片）。
// 真正拒收多模态的上游会以 4xx 回这里，vision=false。
func probeVisionBody(bare string) []byte {
	return probeMarshal(map[string]any{
		"model": bare,
		"messages": []map[string]any{
			{"role": "user", "content": []map[string]any{
				{"type": "text", "text": "这是什么颜色？只回答一个词。"},
				{"type": "image_url", "image_url": map[string]string{"url": "data:image/png;base64," + capsProbePNG}},
			}},
		},
		"max_tokens": 16,
		"stream":     false,
	})
}

// capsProbePNG 1x1 纯红 PNG 的 base64（PowerShell GDI+ 生成，68B）。
const capsProbePNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAAAXNSR0IArs4c6QAAAARnQU1BAACxjwv8YQUAAAAJcEhZcwAADsMAAA7DAcdvqGQAAAANSURBVBhXY7gjYvMfAAUoAiw/zdObAAAAAElFTkSuQmCC"

// ── 应答判据 ────────────────────────────────────────────────────────────────

// probeHasReply 聚合响应里是否有非空正文/思考正文。
func probeHasReply(agg map[string]any) bool {
	msg := firstChoiceMessage(agg)
	if msg == nil {
		return false
	}
	if s, _ := msg["content"].(string); strings.TrimSpace(s) != "" {
		return true
	}
	// reasoning_content：部分推理模型正文可能为空，思考非空也算模型活着能应答。
	if s, _ := msg["reasoning_content"].(string); strings.TrimSpace(s) != "" {
		return true
	}
	// content 为数组形态（少数上游 echo 多模态结构）也算有正文。
	if arr, ok := msg["content"].([]any); ok && len(arr) > 0 {
		return true
	}
	return false
}

// probeToolCalled 聚合响应里是否真的产生了带函数名的 tool_calls。
//
// 这正是本次会话修好的链路：修复前 Trae 的 tool_calls 没有 function 字段，
// 这里就会正确地判 tools=false —— 探测与修复互为验证。
func probeToolCalled(agg map[string]any) bool {
	choice := firstChoice(agg)
	if choice == nil {
		return false
	}
	if fr, _ := choice["finish_reason"].(string); fr == "tool_calls" {
		// finish 说调了工具：函数名必须在 message.tool_calls[0].function.name 里找到才算数
		// （找不到就是残缺形状——正是修复前 Trae 的输出，判 false）。
		if msg, ok := messageOf(choice); ok {
			name, _ := firstToolCallName(msg)
			return name != ""
		}
		return false
	}
	msg := firstChoiceMessage(agg)
	if msg == nil {
		return false
	}
	name, ok := firstToolCallName(msg)
	return ok && name != ""
}

func messageOf(choice map[string]any) (map[string]any, bool) {
	msg, ok := choice["message"].(map[string]any)
	return msg, ok
}

func firstChoice(agg map[string]any) map[string]any {
	choices, _ := agg["choices"].([]any)
	if len(choices) == 0 {
		return nil
	}
	c, _ := choices[0].(map[string]any)
	return c
}

func firstChoiceMessage(agg map[string]any) map[string]any {
	c := firstChoice(agg)
	if c == nil {
		return nil
	}
	msg, _ := c["message"].(map[string]any)
	return msg
}

// firstToolCallName 从持有 tool_calls 的对象里取第一个工具调用的函数名。
func firstToolCallName(holder map[string]any) (string, bool) {
	tcs, _ := holder["tool_calls"].([]any)
	if len(tcs) == 0 {
		return "", false
	}
	tc, _ := tcs[0].(map[string]any)
	if tc == nil {
		return "", false
	}
	fn, _ := tc["function"].(map[string]any)
	if fn == nil {
		return "", false
	}
	name, _ := fn["name"].(string)
	return name, true
}
