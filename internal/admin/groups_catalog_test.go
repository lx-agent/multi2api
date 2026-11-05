// groups_catalog_test.go：GET /admin/groups/catalog 的 ?probed=only 过滤。
//
// 用户 m02135 要求分组编辑弹窗只列「探测可用」的模型。判据不在 admin 侧
// （它不认识探测），admin 只认注入的谓词回调。这里验证三件事：
//
//	① 不带参数 ⇒ 全量，且响应里没有 probed_* 字段（一致性检查依赖全量）；
//	② 带参数且回调已接线 ⇒ models 与 entries 按**同一谓词**过滤，
//	   probed_excluded 给出被隐藏的条数；
//	③ 带参数但回调未接线 ⇒ 退化为不过滤 —— 宁给全量不给空列表
//	   （空候选池会让分组编辑器直接不可用，比多列几个死模型更伤）。
package admin

import (
	"net/http"
	"testing"
)

// catalogFixture 在 pruneFixture 之上注入一个可控目录。
// live 里的模型判「可用」，其余判「不可用」。
func catalogFixture(t *testing.T, names []string, live map[string]bool, wire bool) *pruneFixture {
	t.Helper()
	f := newPruneFixture(t)
	entries := make([]ModelCatalogEntry, 0, len(names))
	for _, n := range names {
		entries = append(entries, ModelCatalogEntry{ID: n, Owner: "p"})
	}
	f.h.cfg.ModelNames = func() ModelCatalogSnapshot {
		return ModelCatalogSnapshot{Names: names, Entries: entries}
	}
	if wire {
		f.h.cfg.ModelProbedAvailable = func(name string) bool { return live[name] }
	}
	return f
}

// ① 默认不带参数：全量返回，不出现 probed_* 字段。
func TestGroupCatalogWithoutProbedReturnsFullList(t *testing.T) {
	f := catalogFixture(t, []string{"m-a", "m-b"}, map[string]bool{"m-a": true}, true)
	code, out := f.call(t, http.MethodGet, "/admin/groups/catalog", "")
	if code != http.StatusOK {
		t.Fatalf("应 200，实际 %d", code)
	}
	models, _ := out["models"].([]any)
	if len(models) != 2 {
		t.Fatalf("不带参数必须给全量 2 条，实际 %v", out["models"])
	}
	if _, ok := out["probed_only"]; ok {
		t.Errorf("不带参数不应出现 probed_only 字段：%v", out)
	}
	if _, ok := out["probed_excluded"]; ok {
		t.Errorf("不带参数不应出现 probed_excluded 字段：%v", out)
	}
}

// ② 带 ?probed=only 且回调接线：models/entries 同谓词过滤 + 计数。
func TestGroupCatalogProbedOnlyFiltersBothModelsAndEntries(t *testing.T) {
	f := catalogFixture(t, []string{"m-a", "m-b", "m-c"},
		map[string]bool{"m-a": true, "m-c": true}, true)
	code, out := f.call(t, http.MethodGet, "/admin/groups/catalog?probed=only", "")
	if code != http.StatusOK {
		t.Fatalf("应 200，实际 %d", code)
	}
	models, _ := out["models"].([]any)
	if len(models) != 2 || models[0] != "m-a" || models[1] != "m-c" {
		t.Fatalf("models 应过滤为 [m-a m-c]，实际 %v", out["models"])
	}
	entries, _ := out["entries"].([]any)
	if len(entries) != 2 {
		t.Fatalf("entries 必须用同一谓词过滤到 2 条，实际 %v", out["entries"])
	}
	for _, e := range entries {
		ent, _ := e.(map[string]any)
		if id, _ := ent["id"].(string); id != "m-a" && id != "m-c" {
			t.Errorf("entries 混进了未过滤的 %v", ent)
		}
	}
	if out["probed_only"] != true {
		t.Errorf("probed_only 应为 true，实际 %v", out["probed_only"])
	}
	if out["probed_excluded"] != float64(1) {
		t.Errorf("probed_excluded 应为 1，实际 %v", out["probed_excluded"])
	}
	if out["ready"] != true {
		t.Errorf("过滤后仍有候选时 ready 应为 true，实际 %v", out["ready"])
	}
}

// ②b 全部被过滤：models 空、ready=false、excluded=全量 ——
// 前端据此区分「探测过滤为空」与「目录没拉到」两种文案。
func TestGroupCatalogProbedOnlyEverythingFiltered(t *testing.T) {
	f := catalogFixture(t, []string{"m-a", "m-b"}, map[string]bool{}, true)
	code, out := f.call(t, http.MethodGet, "/admin/groups/catalog?probed=only", "")
	if code != http.StatusOK {
		t.Fatalf("应 200，实际 %d", code)
	}
	models, _ := out["models"].([]any)
	if len(models) != 0 {
		t.Fatalf("全不可用时 models 应为空，实际 %v", out["models"])
	}
	if out["ready"] != false {
		t.Errorf("全被过滤时 ready 应为 false，实际 %v", out["ready"])
	}
	if out["probed_excluded"] != float64(2) {
		t.Errorf("probed_excluded 应为 2，实际 %v", out["probed_excluded"])
	}
}

// ③ 回调未接线：带参数也退化为不过滤（保守），但仍回显 probed_only。
func TestGroupCatalogProbedOnlyWithoutCallbackIsNoop(t *testing.T) {
	f := catalogFixture(t, []string{"m-a", "m-b"}, nil, false)
	code, out := f.call(t, http.MethodGet, "/admin/groups/catalog?probed=only", "")
	if code != http.StatusOK {
		t.Fatalf("应 200，实际 %d", code)
	}
	models, _ := out["models"].([]any)
	if len(models) != 2 {
		t.Fatalf("未接线回调时必须不过滤（宁全量不空列表），实际 %v", out["models"])
	}
	if out["probed_excluded"] != float64(0) {
		t.Errorf("未接线时 excluded 应为 0，实际 %v", out["probed_excluded"])
	}
}
