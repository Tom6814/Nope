# 控制台完善 S2/S3/S4 设计

> 二期第三阶段子项目 2/3/4。S1（定时消息管理页+统计）已完成。

---

## S2：记忆可视化查看 / 修改 / 删除

**现状**：store 层已有 `MemoryStore`（AddMemory/ListMemories/UpdateMemory/DeleteMemory，per (bot_id,sender)），但 API 层与控制台均未暴露。

**后端**（`internal/api` 新增 `memory_handler.go`，router 挂 protected）：

- `GET /api/bots/{id}/memories?sender=wxid_x`：
  - 无 sender：返回该 bot 全部记忆，按 sender 分组 `{groups: [{sender, memories: [...]}]}`。实现：`ListContacts(botID)` 拿全部联系人 → 逐个 `ListMemories(botID, sender)` 合并（联系人无记忆时跳过）；若联系人接口不齐，可给 store 增加 `ListMemoriesByBot(botID)`（sqlite/postgres/memstore 三端 + storetest），实现更简单、可测。
  - 有 sender：直接 `ListMemories(botID, sender)` 返回数组。
  - 带 bot 归属校验（与 S1 一致，`GetBot` + `UserIDFromContext`）。
- `PUT /api/bots/{id}/memories/{memID}`：body `{content, importance?, expires_at?}`，先 `ListMemoriesByBot` 找到该记忆（归属），`UpdateMemory` 更新。200 返回更新后记忆。
- `DELETE /api/bots/{id}/memories/{memID}`：同上定位 + `DeleteMemory`。200 `{ok:true}`。

**前端**（`web/src/pages/persona.tsx` 新增「记忆」Tab）：

- Tab 内 bot 下拉选择器（`api.listBots`，与 bot 详情页选择风格一致；默认第一个）。
- 选中 bot 后 `api.botMemories(botId)` 加载；按 sender 分组展示卡片列表：内容（可编辑 textarea）、importance、创建/更新时间。
- 编辑：行内编辑 + 保存（PUT）；删除：`useConfirm` + DELETE；删除/保存后刷新。
- 空态、Skeleton、错误重试，遵循 persona.tsx 现有风格。

**测试**：后端 handler（归属 404、无 sender 分组、有 sender 过滤、PUT 更新、DELETE 删除）+ store 层若新增 `ListMemoriesByBot` 则补 storetest 往返。前端 `pnpm build` 通过。

**范围排除**：不做记忆按时间线可视化/搜索；不做 AI 整理触发。

---

## S3：底层规则增强（绑定唯一 + R18 限制，提示注入 + 代码强制）

**现状**：`DefaultPersonaRulesText`（store/persona.go:104-111）已有 NTR/专一/诚实等铁律，可经 rules API 编辑并按最高优先级注入；`SetOwner` 已保证 owner 唯一（clears other owners）；agent 工具（gen_image 等）无关系感知。

**改动**：

1. **默认规则文本扩充**（`DefaultPersonaRulesText` 增加，仍可编辑）：
   - 绑定关系唯一：明确「喜欢的人/主人（owner）只能有一个；对这个人你是专一的；不允许同时绑定/暧昧多人」。
   - R18 边界：R18/亲密内容只能对唯一的 owner 发生；对其他人一律不产生、不响应 R18 内容；被要求时温柔拒绝。
   - 保持现有 NTR 禁令与诚实原则。
2. **关系上下文注入**：`BuildMessages` 系列（internal/ai/chat.go）在 persona 段注入「当前联系人角色」：是否 owner、关系摘要（从 ContactStore 取 owner + 当前 sender 的关系边）。让 AI 在回复与工具调用时知晓当前对象是谁。实现：新增一个 helper（如 `persona.RelationshipContext(store, botID, sender) string`），`BuildMessagesForSender`/`BuildMessagesPersona` 在 system 段拼接；缺 store 能力时降级为「未知」。
3. **工具门控（gen_image R18）**：`handleGenImage`（internal/persona/image.go）在生成前做 R18 关键词预检：`prompt`（+negative）命中敏感词（常量列表 `r18Keywords`）时，若当前 sender 不是该 bot 的 owner（经 `SetOwner`/`GetOwner` 查询；store 需暴露 `GetOwner(botID)` 或复用 `ListContacts` 找 owner 字段）→ 返回拒绝结果（自然话术 + Diagnostics，ErrorStage="policy"）。owner 放行。非 owner 非 R18 照常。
   - store 需确认 `Contact` 有 owner 标记字段（`SetOwner` 已存在，读侧需 `GetOwner(botID) (*Contact, error)` 或类似；缺则新增 + 三端 + storetest）。
4. 前端规则页无需改动（rules API 已暴露；默认文本更新即可）。

**测试**：默认规则文本包含新条款（字符串断言）；关系上下文 helper 单测（owner/非 owner/无数据）；gen_image 门控单测（owner 放行、非 owner R18 拒绝且 ErrorStage=policy、非 owner 普通 prompt 放行）；store GetOwner 往返。

---

## S4：自动优化提示词生成角色卡

**现状**：角色卡导入（文件/JSON）已有；后端有 `POST /api/characters/import`（SillyTavern V2）。无「从一段描述生成角色卡」能力。

**后端**（`internal/api` 新增或并入 character_handler.go）：

- `POST /api/persona/generate-character` body `{description}`（requireAdmin）：
  - 调 AI：复用 persona/sink 的 `Completer`/`resolveConfig` 能力，构建一条生成指令：把 `description` 优化扩展成完整 SillyTavern V2 角色卡 JSON（字段：name/description/personality/mes_example/scenario/first_mes，风格与既有 V2 导入兼容）。
  - 解析返回 JSON（容错：剥离 markdown 围栏）；校验必填 name/description；失败 422 带错误。
  - 成功后直接 `SaveCharacter` 入库（映射到 store.Character 字段，参考 import handler 的映射逻辑），返回新角色。
- 复用 AI 配置：AI 未配置时 400「请先配置 AI」。

**前端**（`web/src/pages/persona.tsx` 角色卡 Tab）：

- 卡片区加「用提示词生成」按钮 → Dialog/内联面板输入描述 → 调 `api.generateCharacter(description)` → 成功 toast + 刷新角色列表（自动出现在列表）。

**测试**：handler 单测（mock Completer 返回合法 V2 JSON → 角色入库；非法 JSON → 422；未配置 → 400）。前端 `pnpm build` 通过。

---

## 依赖与验收

- S2/S3/S4 相互独立，按 S2→S3→S4 顺序落地。
- 每个子项目：`go test ./...`、`go build ./...`、`cd web && pnpm build` 通过；完成后提交并推送一次。
- 涉及 store 新方法（`ListMemoriesByBot`、`GetOwner`）需 sqlite/postgres/memstore 三端 + storetest。
