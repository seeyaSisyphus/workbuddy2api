package server

import "strings"

// realmPrefixAliases 前缀（**小写规范形**）→ 规范 realm（D6 前缀协议 + 同义前缀）。
//
//	"cn"     → cn
//	"global" → global
//	"g"      → global   ← 通用同义前缀：任何 global 模型都可写 g:<bare>，不止 deepseek
//
// 查找时对客户端前缀做小写折叠（见 resolveModel），故 "G:" / "Global:" / "GLOBAL:"
// 同样命中。前缀是网关自有的路由协议 token，**从不发给上游**，折叠无任何风险。
//
// 未命中（空前缀 / 未知前缀）→ 不剥离：realm=cn、bare=整串原样（见 resolveModel 注释）。
var realmPrefixAliases = map[string]string{
	"cn":     "cn",
	"global": "global",
	"g":      "global",
}

// modelAliases 模型名别名表（硬编码，无 config 项）。**键值均为小写规范形**。
//
// 作用时机：**前缀解析之后**，只作用于 bare，与 realm 无关 —— 同一别名在 cn/global
// 两域解析到同一 bare。例：
//
//	g:deepseek-flash / global:deepseek-flash → (global, deepseek-v4.1-flash)
//	deepseek-flash   / cn:deepseek-flash     → (cn,     deepseek-v4.1-flash)
//	deepseek-v4.1-flash（本身）               → 不变（幂等）
//
// 约束（由 TestModelAliasesInvariants 钉住）：
//   - 键值均小写（查找按小写折叠，大写键永远查不中）；
//   - 单跳：value 不得又是 key（解析只查一次，天然幂等；禁建链）；
//   - value 非空且不含 ":"（必须是真实上游 ID —— 出站 body 重写后的 bare 是 upstream
//     侧 effort/thinking 档位查表的键，指向不存在的 ID 会静默不透传档位）；
//   - key 不含 ":"（否则与 "deepseek:v3" 这类未知前缀形态的整串撞车）。
//
// ⚠ 别名会**遮蔽**同名真实模型：若上游日后真下发 id="deepseek-flash"，该拼写将永远
// 解析到别名目标。本表成员在 CN/global 两域均非真实 ID（2026-09-27 对 /v1/models 实测核对）。
var modelAliases = map[string]string{
	"deepseek-flash": "deepseek-v4.1-flash",
}

// resolveModel 解析模型名协议（PLAN D6 + 同义前缀 + 别名层，**全程大小写不敏感**）：
//
//	第 0 层 剥离：去掉客户端/代理层的归属标记与档位标记（wb/ · wb: · [1M] …），
//	             见 stripClientMarkers —— 它们从不是上游模型名的一部分。
//	第 1 层 前缀：取第一个 ":"，前段小写折叠命中 realmPrefixAliases 才剥离；否则视为
//	             裸名，realm=cn、bare=整串原样（不折叠，见下方"未知前缀"注释）。
//	第 2 层 归一：对 bare 走 canonicalModel（小写折叠 + 别名表，单跳、幂等、与 realm 无关）。
//
// 层序不可颠倒：先查别名再剥前缀会让 "g:deepseek-flash" 查不到（前缀层失效）。
// 空裸名（"g:" / "global:" / "cn:"）沿用旧协议：剥前缀、bare=""（不特例处理）。
//
// 未知前缀（冒号前段不在表内，如 "deepseek:v3"）：整串当裸名、**原样保留不做折叠**。
// 依据：上游模型 ID 不含 ":"，带冒号的串必然撞 11102；原样出站/回显便于按客户端原文
// 排错，也保住"未知前缀不解释"这条既有语义（resolve_model_test.go 的 deepseek:v3 用例）。
//
// bare 是出站 / 选号 / 粘性可用集合 / 11102·6004 冷却键 / 成本账本的**唯一**口径。
//
// 导出为 ResolveModel（cmd/server 粘性闭包需要），包内简写 resolveModel。
func resolveModel(model string) (realm, bare string) {
	model = stripClientMarkers(model)
	idx := strings.IndexByte(model, ':')
	if idx < 0 {
		return "cn", canonicalModel(model)
	}
	if r, ok := realmPrefixAliases[strings.ToLower(model[:idx])]; ok {
		return r, canonicalModel(model[idx+1:])
	}
	return "cn", model
}

// stripClientMarkers 剥离**客户端/代理层标记**，它们从不是上游模型名的一部分：
//
//  1. provider 归属前缀 "wb/" / "wb:"（可重复，大小写不敏感）。ai-proxy 的 provider key
//     是 `wb`（config.js：WorkBuddy 的模型自带 `global:`/`cn:` 前缀，故 provider key 必须
//     另起名字），客户端持有的是 `<provider>/<model>` 复合名，由代理剥一层后转发。一旦
//     客户端**直连网关**（绕过代理）或复合名被叠加两层，残留标记就会原样进来。
//  2. 尾部上下文档位标记 "[1M]" / "[300K]"（数字 + K/M，大小写不敏感）。客户端用它声明
//     上下文档位，ai-proxy 对这类后缀"无条件剥离"（config.js 注释），上游不接受。
//
// 为什么必须剥：标记若原样出站，上游按"模型不存在"处理 → 11102 → 每个账号各拒一次
// （11102 是 (账号,模型) 负缓存，换号必然同结果，白烧轮换）→ 连败触发闸门 429。
// 2026-10-02 实测：CyberSlots 的 dsh(sidechat) 会话模型为 "wb/wb/g:deepseek-flash"，代理
// 剥掉一层后网关收到 "wb/g:deepseek-flash" —— 不剥标记时 realm 落 cn、bare 整串出站，
// 上游返 `model [wb/g:deepseek-flash] service info not found`，客户端连吃 429/503。
//
// 碰撞风险为零：2026-10-02 对 /v1/models 两域 75 个 ID 实测核对，无一含 "/"、"[" 或以
// "wb" 开头；"wb" 也不是任何 realm 前缀（realmPrefixAliases 只有 cn/global/g）。
// 只剥**带分隔符**的 "wb/"/"wb:"（裸名 "wb" 不受影响），且剥空则保留原串。
func stripClientMarkers(model string) string {
	// 尾部档位标记：[<数字><K|M>]（如 [1M] / [300K] / [64k]）。
	if i := strings.LastIndexByte(model, '['); i > 0 && strings.HasSuffix(model, "]") {
		if isCtxTierMarker(model[i+1 : len(model)-1]) {
			model = model[:i]
		}
	}
	// provider 归属前缀：可重复（"wb/wb/…"），剥到只剩模型名为止；剥空则回退原样。
	for {
		rest := ""
		switch lower := strings.ToLower(model); {
		case strings.HasPrefix(lower, "wb/"):
			rest = model[3:]
		case strings.HasPrefix(lower, "wb:"):
			rest = model[3:]
		default:
			return model
		}
		if rest == "" {
			return model
		}
		model = rest
	}
}

// isCtxTierMarker 判定括号内是否为上下文档位标记（数字 + K/M 单位，如 "1M" / "300K"）。
// 非该形态（如 "[test]"）一律不剥：宁可原样出站按上游 11102 排错，也不猜。
func isCtxTierMarker(inner string) bool {
	if len(inner) < 2 {
		return false
	}
	switch inner[len(inner)-1] {
	case 'K', 'k', 'M', 'm':
	default:
		return false
	}
	for i := 0; i < len(inner)-1; i++ {
		if inner[i] < '0' || inner[i] > '9' {
			return false
		}
	}
	return true
}

// canonicalModel 把客户端裸模型名归一到上游规范形（大小写不敏感 + 别名表）：
//
//  1. 别名表命中（小写折叠后）→ 返回表内规范值（权威上游 ID）；
//  2. 未命中 → 返回小写折叠后的输入本身。
//
// 为什么可以无条件小写折叠：WorkBuddy 上游模型 ID 目录实测**全小写**——2026-09-27 对
// /v1/models 两域 71 个 ID 逐一核对（cn:auto / cn:glm-5.3 / global:gpt-5.3-codex / …），
// 无一大写；历史静态名单（GlobalModelNames）同样全小写。而**上游对模型名严格大小写敏感**
// （实测 GLM-5.3 → 11102「模型不存在」，logs 里 5 条此类 503 即由此而来），
// 所以大写拼写必须折叠成小写才能命中——这正是本函数存在的理由。
//
// 附带修复"同模型两个键"的账本分裂：折叠前 GLM-5.3 与 glm-5.3 在 pool.modelCooldowns
// （11102 避让）与 pool.modelCost（成本账本）里是两个独立键——大写拼写的失败避让永远
// 等不到成功清命（BlockModelClear 用的是另一个键），成本观测也被拆成两份 EMA。
//
// ⚠ 唯一假设：上游不存在含大写的模型 ID。若日后下发（如 MiniMax-M3），本折叠会把它
// 错折成小写，该模型将对所有人 11102。届时须把折叠改为"按上游目录归一"（目录为权威）。
// 当前目录全小写，此风险为零；一旦上游改口径，表现为整模型不可用、立刻可见。
func canonicalModel(name string) string {
	lower := strings.ToLower(name) // 已全小写时 Go 直接返回原串，零分配
	if canonical, ok := modelAliases[lower]; ok {
		return canonical
	}
	return lower
}

// ResolveModel 是 resolveModel 的导出面（跨包调用）。
func ResolveModel(model string) (realm, bare string) { return resolveModel(model) }
