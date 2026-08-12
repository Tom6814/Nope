# 提示词 Harness 优化设计（S6）

> 基于深度审计（2026-08-11 prompt-harness audit）选定的高价值/低风险改动。审计报告要点：H1 expires_at 未实现、H2 系统段无总量护栏、H3 组装内重复查询、H4 mes_example 未注入、M5 记忆预算全有或全无、M6/M7 关系信息重复且命名不一致、M8 死代码、M12 S4 不拆世界书。

**原则**：只做高价值/低风险；阈值保守（正常数据不触发）；铁律层（rules）永不被截断；保持既有行为/UX 兼容；每一处配测试。

---

## 1. expires_at 自动遗忘（H1，P0）

- store `ListMemories`（sqlite/postgres SQL）加 `AND (expires_at IS NULL OR expires_at > <now>)`；memstore 同步；`internal/ai/memory_inject.go` 的 `buildCurrentPerson` 与 `internal/persona/contacts.go` 的 `handleLookupContact` 再做防御性内存过滤（`ExpiresAt != nil && *ExpiresAt <= now` 跳过）。
- 三端 + storetest 增加「过期记忆不返回」用例；组装路径测试「过期记忆不注入」。

## 2. 系统段总量护栏 + 分层截断（H2，P0）

新增 `internal/ai/limits.go`（或并入 persona.go）一组常量与 helper：

- `truncateRunes(s string, maxRunes int) string`（按 rune，追加 `…`）。
- 角色卡三字段：Description/Personality/Scenario 各截断 `maxCardFieldRunes = 2000`。
- 世界书单条 Content：`maxWorldEntryRunes = 1000`；命中条数上限保持 8。
- 关系边：`maxRelationLines = 30`（超出省略并标注）；brief 单行 `maxBriefRunes = 200`。
- 记忆预算：改为按 rune 计 + 单条超预算时**截断保留**（`truncateRunes` 到剩余预算），不再整条丢弃。
- 系统段总量护栏：组装完成后统计各 system 层合计 rune 数，超过 `maxSystemRunes = 12000` 时**自下而上**（SystemPrompt → 角色层 → 关系骨架 → 世界书）收缩——实现方式：`truncateRunes` 已对各层内部限长，护栏作为兜底对超长角色层/世界书层整体截断；**铁律层豁免**（永不截断）。
- 全部常量集中定义；测试锁定：正常数据不变、超长数据被截断、铁律层完整。

## 3. 关系层单一来源（M6+M7，P1）

- `internal/store/persona.go` 的 `RelationshipContext` 改为接收一个可选的 `[]Contact` 结果（避免重复查询）：新签名 `RelationshipContextWith(contacts []Contact, sender string) string`，原 `RelationshipContext(store, botID, sender)` 保留为便捷包装（内部调用一次 `ListContacts`）。
- `internal/ai/memory_inject.go` 的 `buildRelationSkeleton`/`buildCurrentPerson` 改为接受外部传入的 `[]Contact`（`buildPersonaLayers` 一次性 `ListContacts`，在关系层、关系行、当前人层间共享）。
- 关系边输出：`formatRelation` 用 `displaySender`（名字/brief，缺省回落 sender）代替裸 wxid。
- owner 信息只从骨架一处注入（去掉当前人层重复的 owner 行，保留骨架 owner 标记）；RelationshipContext 行与骨架措辞统一（「唯一喜欢的人/主人」）。
- 更新受影响的断言测试；新增「同一组装仅一次 ListContacts」的测试（可用计数 store 或注入 fake）。

## 4. mes_example 注入（H4，P1）

- `internal/character/card.go` 的 `ComposePersonaText`（或角色层组合处）在角色层末尾追加「对话风格示例」块：`截断到 maxMesExampleRunes = 800`；仅在非空时注入，标注「（风格示例）」。

## 5. 组装内查询去重（H3，P1）

- `assembleMessages`/`buildPersonaLayers` 路径内 `GetCharacter` 只调一次（角色层 + `loadFirstMes` 共用同一结果——`loadFirstMes` 改为接收已取到的 Character 或复用缓存）。
- `ListContacts` 只调一次（骨架 + RelationshipContext 共享，见第 3 点）。
- 结果：正常回复每条消息约 7-8 次 DB 往返降为 5-6 次。

## 6. S4 生成路径拆分世界书（M12，P2）

- `internal/api/character_handler.go` 的 `handleGenerateCharacter` 在 `SaveCharacter` 后复用导入路径的 world-entry 拆分循环（抽公共 helper `saveWorldEntriesFromCard`，导入与生成共用）。

## 7. 死代码清理（M8，P2）

- 移除 `buildPersonaLayers` 的 channelID 参数与 `resolveBotID` 死路径；`layers, _` 的 error 返回值改为无 error（或保留但恒 nil 并标注）。涉及 `PersonaStore` 的 `channelLookup/GetChannel` 若仅为死路径服务则一并清理（需确认无其他调用）。

---

## 不做（记录）

- M9 PHI 位置调整、M10 constant 世界书、M14 rules 三级回退修复、L 系列——留作后续，避免本轮范围膨胀。

## 测试与验收

- 每项改动带 TDD（先失败后实现）。
- `go test ./...`、`go build ./...`、`cd web && pnpm build` 全绿（web 未改动时也可跳过 build，但要求 Go 全量绿）。
- 完成后提交并推送一次。
