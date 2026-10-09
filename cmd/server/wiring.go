package main

import (
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/server"
)

// realmAwareAvailableForModel 构造会话粘性路由按模型可用口径的 realm 感知闭包。
//
// 粘性分配的模型名可能带 realm 前缀（"global:gpt-5.4" / "g:gpt-5.4" / "cn:glm-5.2"）：
// 必须按前缀剥出 realm + bareModel，再交给分池选号域过滤——否则裸名取池子全集，global 号
// 会被粘性分配给 CN 前缀请求（跨 realm 泄漏）。裸名/显式 cn → cn 集合；global:/g: → global 集合。
//
// 与 handler 共用 server.ResolveModel 单一来源，故前缀同义（g:）与模型名别名层
// （resolve_model.go 的 modelAliases）在此自动同口径——粘性可用集合与选号集合的模型键
// 必然一致，不会出现"选号用别名目标、粘性用客户端原串"的错配。
//
// realm 为空串时 pool.AvailableUIDsForModelRealm 退化为现状（AvailableUIDsForModel），
// 老调用（无前缀模型名）语义零改动。
func realmAwareAvailableForModel(p *pool.Pool) func(model string) []string {
	return func(model string) []string {
		realm, bare := server.ResolveModel(model)
		return p.AvailableUIDsForModelRealm(bare, realm)
	}
}