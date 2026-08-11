# OpeniLink Hub 拟真角色 / 记忆 / 主动消息 / Agent 能力 设计文档

- 日期：2026-08-11
- 基线项目：[openilink/openilink-hub](https://github.com/openilink/openilink-hub)（Go 后端 + React 前端，微信 ClawBot / iLink 协议消息管理平台）
- 目标：在 Hub 现有 AI 回复管线之上，叠加「像真人一样有设定、有记忆、会惦记你、能干活」的能力，且核心回复循环尽量零侵入。

## 1. 背景与目标

Hub 已内置「AI 自动回复」通道（接 OpenAI 兼容 API），并已有 `system_prompt` / `max_history` 配置。本设计在此基础上新增四类能力：

1. **角色卡**：兼容 SillyTavern（酒馆）V2 角色卡 + 世界书（Lorebook），让 AI 更符合设定、更像真人。
2. **记忆**：AI 主动记录（用户画像 / 关系进展 / 关键事实 / 细节），并由 AI 驱动整理（过期删除、陈旧压缩、近期保留）；记忆作为背景注入，AI 自然带出而非生硬复读。
3. **主动发消息**：AI 在对话中登记「未来的约定」（存提示词 + 具体时间 + 容差），系统到点现场调用 AI 生成消息并推送。
4. **Agent 工具**：联网搜索、写文档、生图（perchance，底层 Stable Diffusion，含基础负向词，支持 R18）。

### 设计总原则

- **两条线解耦**：所有「人格」（角色卡 / 世界书 / 记忆）在 `BuildMessages` 的 system 段注入（读路径）；所有「能力 / 写入」（记忆写入、主动消息登记、agent 工具）走 `builtin.Register` 挂工具（写路径）+ 一条后台 ticker（主动触发）。核心回复循环不改。
- **不臆造接口**：所有挂靠点均基于对基线源码的查证（见附录 A）。perchance 无稳定契约，仅承诺「先研究再实现」并预置降级。
- **代码兜底校验**：不完全信任 AI 输出（如整理记忆时对「近期不删」做代码强约束）。

## 2. 已查证的基线事实（设计依据）

| 事实 | 位置 | 对设计的意义 |
|---|---|---|
| 上下文顺序 = `system_prompt → 历史 → 当前消息` | `internal/ai/chat.go` `BuildMessages` | 角色卡 + 记忆都在 system 段注入，不动核心循环 |
| 会话隔离维度 = `(channelID, sender)` | `internal/sink/ai.go` `reply()` 调 `BuildMessages(ctx,cfg,store,d.Channel.ID,sender,...)` | 引擎按会话传参，但**记忆 key 设计取 `(bot, sender)`**（二轮修订，去 channel）——同一人跨会话打通；`sender`=wxid 稳定 |
| 工具循环支持「工具回图片直接发用户」 | `internal/sink/ai.go` `reply()` tool-call 循环（最多 5 轮），`ReplyType=="image"` 自动发送 | 生图 / 搜索 / 写文档 = 内置 App，零侵入 |
| 内置 App 注册 = `builtin.Register(manifest, handler)` | `internal/builtin/registry.go` | 有现成模板（bridge），照抄结构 |
| `AIConfig` 扁平结构体，从 `ai.*` KV 组装 | `internal/store/channel.go` + `internal/sink/ai.go` `resolveConfig` | 角色卡 / 记忆开关加字段，走既有配置通道 |
| 迁移 = goose 编号 SQL（`0001`..`0009`），外键 `ON DELETE CASCADE`，`unixepoch()` | `internal/store/sqlite/migrations/` | 新表加 `0010`，风格照抄 |
| Store = 多子接口聚合 | `internal/store/store.go` | 新增 `CharacterStore` / `MemoryStore` 等聚合进 `Store` |
| 已有主动推送范本 = `reminderLoop` 10 分钟 ticker 主动 `inst.Send` | `internal/bot/manager.go` `reminderLoop` / `checkReminders` | 主动消息复用同套发送逻辑 |
| 主动发送接口 = `inst.Send(ctx, provider.OutboundMessage{Text/Data/FileName, ContextToken})` | `internal/provider/provider.go` | 拿 `GetLatestContextToken(bot)` 即可发文本 / 文件 / 图片 |
| 约束：微信 iLink 24 小时会话窗口 | README / reminder 逻辑 | 用户超 24h 未发消息则主动消息会失败，需优雅标记 |

## 3. 分期范围

```
一期（人格地基）：角色卡 + 世界书 + 记忆(AI主动记 + AI整理) + 主动发消息
二期（agent 能力）：web_search + write_doc + gen_image(perchance, 降级 OpenAI)
```

一期结束即可独立验证「是否像一个有设定、会记事、会惦记你、且不生硬的真人」。

## 4. 数据模型

新增迁移 `internal/store/sqlite/migrations/0010_persona_memory.sql`（goose 格式，postgres 侧同步一份），共 7 张表：`characters`、`world_entries`、`contacts`、`contact_relations`、`memories`、`scheduled_messages`、`persona_rules`。

> **核心修订（2026-08-11 二轮头脑风暴）**：记忆从「按会话隔离」改为「**bot 级全局打通 + 按人区分表达**」。隔离维度从 `(bot, channel, sender)` 收敛为 `(bot, sender)`——同一个人（wxid 稳定标识，见附录 A）不论私聊还是任何群，都是同一份记忆。`sender` 语义已查证：`provider.InboundMessage.Sender` 私聊为对方 wxid、群聊为群成员 wxid，跨会话稳定。注意 `store.User` 是**平台后台登录账户**（username/password/role），与「聊天联系人」是两个无关概念，本设计的 contacts 指后者。
>
> 「认识每个人 / 知道名字 / 每人一份画像 / 用户间关系链」四诉求落为**一棵关系树**：`contacts` = 节点（人），`contact_relations` = 有向带标签的边（谁→谁，什么关系），`memories` = 挂在节点上的零碎事。注入采用 **skill 式两层懒加载**（详见 §5）：常驻只放「每人一行极短索引 + 当前对话人全量」，其余靠工具按需拉取——因此「全局可见」不等于「一锅端全塞」，bot 认识很多人也不会撑爆上下文。

### 4.1 `characters` —— 角色卡（一张卡 = 一份酒馆 V2 JSON）

```
characters
├── id          TEXT PRIMARY KEY
├── name        TEXT NOT NULL              -- 角色名
├── spec        TEXT NOT NULL DEFAULT 'chara_card_v2'
├── card_json   TEXT NOT NULL              -- 完整 SillyTavern V2 JSON 原样存
├── avatar_key  TEXT                       -- PNG 内嵌卡提取的头像，存 MinIO
├── created_at  INTEGER NOT NULL DEFAULT (unixepoch())
└── updated_at  INTEGER NOT NULL DEFAULT (unixepoch())
```

整卡塞 `card_json` 而非逐列拆分：酒馆 V2 字段多（`description/personality/scenario/first_mes/mes_example/system_prompt/post_history_instructions/character_book…`），原样存、用时解析，导入导出零损耗、最大化兼容社区卡。

### 4.2 `world_entries` —— 世界书（Lorebook，触发式知识）

```
world_entries
├── id           TEXT PRIMARY KEY
├── character_id TEXT REFERENCES characters(id) ON DELETE CASCADE  -- NULL = 全局世界书
├── keys         TEXT NOT NULL              -- 触发关键词，JSON 数组
├── content      TEXT NOT NULL              -- 命中后注入的知识
├── enabled      INTEGER NOT NULL DEFAULT 1
├── priority     INTEGER NOT NULL DEFAULT 0 -- 多条命中时排序 / 预算裁剪
└── created_at   INTEGER NOT NULL DEFAULT (unixepoch())
```

酒馆卡内嵌的 `character_book` 导入时拆进本表，便于单独开关与调优；也允许手建全局世界书。

### 4.3 `contacts` —— 联系人（关系树的节点：每个认识的人 = 一行）

```
contacts
├── id          TEXT PRIMARY KEY
├── bot_id      TEXT NOT NULL REFERENCES bots(id) ON DELETE CASCADE
├── sender      TEXT NOT NULL              -- wxid，跨会话稳定；(bot_id, sender) 唯一
├── name        TEXT NOT NULL DEFAULT ''   -- AI 认得的名字/称呼
├── brief       TEXT NOT NULL DEFAULT ''   -- ★极短索引：常驻上下文的那一行，如「主人。程序员，最近换工作」
├── profile     TEXT NOT NULL DEFAULT ''   -- 完整画像，按需(查某人)才加载；会随交往演变
├── is_owner    INTEGER NOT NULL DEFAULT 0 -- Q2-A：标记「喜欢的人/主人」，后台可指定；专一铁律朝向此人
├── created_at  INTEGER NOT NULL DEFAULT (unixepoch())
└── updated_at  INTEGER NOT NULL DEFAULT (unixepoch())

UNIQUE INDEX idx_contacts_scope ON contacts(bot_id, sender)
```

`brief` 与 `profile` 分列是懒加载的关键：`brief` 便宜、常驻、撑起关系树骨架；`profile` 贵、按需拉。`is_owner` 全局至多一人（应用层保证）。「自由+ 模式」下开局全表为空、`is_owner` 全 0，由 AI 在交往中自己写。

### 4.4 `contact_relations` —— 关系链（关系树的边：有向带标签）

```
contact_relations
├── id           TEXT PRIMARY KEY
├── bot_id       TEXT NOT NULL REFERENCES bots(id) ON DELETE CASCADE
├── from_sender  TEXT NOT NULL             -- 起点 wxid（对应某 contact.sender）
├── to_sender    TEXT NOT NULL             -- 终点 wxid
├── relation     TEXT NOT NULL             -- 关系标签，如 情侣 | 兄弟 | 同事 | 朋友
├── note         TEXT NOT NULL DEFAULT ''  -- 补充，如「2026 在一起」
├── created_at   INTEGER NOT NULL DEFAULT (unixepoch())
└── updated_at   INTEGER NOT NULL DEFAULT (unixepoch())

INDEX idx_relations_bot ON contact_relations(bot_id)
```

有向边：`张三 --情侣--> 李四`。需要双向时 AI 建两条或查询时双向匹配（应用层，不在 DB 约束）。边只引用 `sender` 字符串、不加 FK，避免「关系先于联系人被提及」时的插入顺序问题（自由+ 模式常见）。

### 4.5 `memories` —— 记忆（AI 主动记，挂在人身上）

```
memories
├── id          TEXT PRIMARY KEY
├── bot_id      TEXT NOT NULL REFERENCES bots(id) ON DELETE CASCADE
├── sender      TEXT NOT NULL              -- ★去掉 channel_id：隔离维度 = (bot, sender)，同一个人跨会话打通
├── category    TEXT NOT NULL DEFAULT 'fact'  -- fact | profile | relationship | detail
├── content     TEXT NOT NULL              -- 记忆正文
├── importance  INTEGER NOT NULL DEFAULT 1 -- 1-5，注入预算超了先砍低分
├── expires_at  INTEGER                    -- 可空；AI 记有时效的事时标保质期，整理时据此删
├── created_at  INTEGER NOT NULL DEFAULT (unixepoch())
└── updated_at  INTEGER NOT NULL DEFAULT (unixepoch())

INDEX idx_memories_scope ON memories(bot_id, sender)
```

`category` 四类：`profile`=画像补充、`relationship`=关系进展、`fact/detail`=事实与细节。画像的「定稿」沉淀进 `contacts.profile`，`memories` 承接零碎与演变过程。

### 4.6 `scheduled_messages` —— 主动发消息（存提示词，非原文）

```
scheduled_messages
├── id          TEXT PRIMARY KEY
├── bot_id      TEXT NOT NULL REFERENCES bots(id) ON DELETE CASCADE
├── sender      TEXT NOT NULL             -- ★同步去 channel：对齐 (bot, sender)
├── fire_at     INTEGER NOT NULL          -- 目标触发时间戳（具体时间点）
├── tolerance   INTEGER NOT NULL DEFAULT 600  -- AI 登记时给的容差(秒)，参考事情紧迫性 + 角色性格
├── prompt      TEXT NOT NULL             -- 存提示词而非原文，如「叫主人起床，温柔点」
├── origin      TEXT NOT NULL DEFAULT 'promise'  -- promise=用户约定 | proactive=AI自主惦记；仅用于统计/限流(§8.4)，不影响发送路径
├── status      TEXT NOT NULL DEFAULT 'pending'  -- pending|sent|failed|cancelled
├── fail_reason TEXT                       -- 失败原因（如「会话窗口已过期」）
├── created_at  INTEGER NOT NULL DEFAULT (unixepoch())
└── sent_at     INTEGER

INDEX idx_sched_due ON scheduled_messages(status, fire_at)
```

### 4.7 `persona_rules` —— 底层铁律（Q1-A：单文本框 + 总开关）

```
persona_rules
├── id          TEXT PRIMARY KEY            -- 单行配置，固定 id 'default' 即可（bot 级如需多套再扩 bot_id）
├── bot_id      TEXT NOT NULL DEFAULT '' REFERENCES bots(id) ON DELETE CASCADE  -- ''=全局默认
├── enabled     INTEGER NOT NULL DEFAULT 1  -- 总开关，默认开
├── rules_text  TEXT NOT NULL DEFAULT ''    -- 可编辑铁律，内置默认已填（禁NTR/只专一/有喜欢的人…）
├── created_at  INTEGER NOT NULL DEFAULT (unixepoch())
└── updated_at  INTEGER NOT NULL DEFAULT (unixepoch())
```

Q1 选 A：一个可编辑文本框 + 总开关（默认开），内置默认填好几条铁律，用户能加/减/改（对应「等等」）。铁律层在 §5 注入时置于 system 段**最高优先级**，且不受 §10 口语化改写影响（铁律是约束不是台词）。
### 4.8 Store 接口（聚合进 `internal/store/store.go` 的 `Store`）

```go
type CharacterStore interface {
    SaveCharacter(c *Character) error
    GetCharacter(id string) (*Character, error)
    ListCharacters() ([]Character, error)
    DeleteCharacter(id string) error
    ListWorldEntries(characterID string) ([]WorldEntry, error) // 含全局(character_id IS NULL)
}

type ContactStore interface {
    UpsertContact(c *Contact) error                       // 按 (bot_id, sender) 唯一键 upsert
    GetContact(botID, sender string) (*Contact, error)    // 未命中 ErrNotFound
    ListContacts(botID string) ([]Contact, error)         // 用于「每人一行 brief 索引」常驻注入
    SetOwner(botID, sender string) error                  // 置 is_owner，同 bot 其余清 0（应用层保证唯一）
    DeleteContact(botID, sender string) error
    UpsertRelation(r *ContactRelation) error
    ListRelations(botID string) ([]ContactRelation, error) // 用于拼「关系树」
    DeleteRelation(id string) error
}

type MemoryStore interface {
    AddMemory(m *Memory) error
    ListMemories(botID, sender string) ([]Memory, error)   // ★去 channelID
    UpdateMemory(m *Memory) error
    DeleteMemory(id string) error
    CountMemories(botID, sender string) (int, error)        // ★去 channelID
}

type ScheduledMessageStore interface {
    AddScheduledMessage(m *ScheduledMessage) error
    ListDueScheduledMessages(now int64) ([]ScheduledMessage, error) // status='pending' AND fire_at <= now
    MarkScheduledMessageSent(id string, sentAt int64) error
    MarkScheduledMessageFailed(id, reason string) error
    CancelScheduledMessage(id string) error
}

type PersonaRuleStore interface {
    GetPersonaRules(botID string) (*PersonaRules, error)   // 未命中回退全局('')；再未命中返回内置默认
    SavePersonaRules(r *PersonaRules) error
}
```

各 backend（sqlite / postgres / memstore / storetest）均需实现，保持与现有子接口一致。

## 5. Prompt 组装与「不生硬」策略

### 5.1 system 段分层（改造 `BuildMessages`）—— skill 式两层懒加载

原本 system 段仅一句 `cfg.SystemPrompt`。改造后按固定顺序拼装（每层可缺省），**记忆采用两层懒加载**：常驻只放便宜的骨架，贵的完整画像靠工具按需拉。

```
[铁律] persona_rules 层  ← enabled 时置顶，最高优先级；如「严格禁止NTR/只专一/心里有喜欢的人」
                          默认开，不受 §10 口语化改写影响（约束≠台词）
[A] 角色卡人格层  ← characters.card_json 解析 description/personality/scenario → 「你是<name>，…」
[B] 世界书层      ← 扫「当前消息 + 最近 10 条历史」关键词，命中 world_entries.keys 才注入 content
[C] 关系树骨架层  ← ListContacts(bot) 每人一行 brief + ListRelations(bot) 连成边（常驻，便宜）
                    「你认识这些人：wxid_张三=主人(程序员,换工作中)；wxid_李四=张三女友…
                     关系：张三—情侣—李四。你心里有数，但是否提及/提多少，凭你的性格与分寸」
[D] 当前对话人全量层 ← GetContact(bot,sender).profile + ListMemories(bot,sender)（永远全量）
                    「以下是你对正在聊天这位的了解，自然体现在态度用词里，别罗列复述」
[E] 原 system_prompt（全局兜底，保留）
  ↓ 之后接 first_mes 开场白（仅新会话）→ 历史 → 当前消息
```

**三种记忆可见模式**（配置 `ai.memory_scope`，见 6.2）共用这套结构，差别只在 [C]：

- **A 默认（全局可见）**：[C] 注入全员 brief + 完整关系树。AI「心里有数」全局；说不说、说多少靠性格拿捏（呼应「不能随便暴露别人的秘密」）。要深入某人细节 → §9 `lookup_contact` 工具按需拉 profile+memories。
- **B（只见当前人）**：[C] 只注入关系树骨架的「边」+ 与当前人直接相连者的 brief，不注入无关人 brief；其余全靠 `lookup_contact` 主动查。隐私更硬、更省 token。
- **自由+（后期，默认关）**：开局 contacts/relations 全空、无 is_owner。[C] 初始为空，AI 通过 §9 `upsert_contact`/`upsert_relation`/`set_owner` 工具在交往中自己长出这棵树、自己认「喜欢的人」。机制复用同一套，仅初始为空 + 放开写权限。

`BuildMessages` 新增依赖 `CharacterStore` / `ContactStore` / `MemoryStore` / `PersonaRuleStore` 与角色卡选择（Bot 级全局，见 6.1）。为避免破坏现有签名，采用「组装器」封装：新增 `internal/ai/persona.go`（角色卡→人格文本 + 铁律层）、`internal/ai/lorebook.go`（世界书命中）、`internal/ai/memory_inject.go`（关系树骨架 + 当前人全量 → 背景文本），由 `BuildMessages` 调用；旧逻辑（历史 + 当前消息）保持不变。

### 5.2 「不生硬」三机制

1. **措辞框定**：记忆层 [C] 的包裹指令明确「这是你心里知道的事，不是要说出口的事」。
2. **软性触发**：世界书 [B] 只在相关时注入，避免每轮把全部记忆怼进去逼 AI 提及。
3. **负向指令**：禁止「我记得你说过…」「根据我的记录…」式复读，要求像老朋友自然带出。

对比（同样记了「怕坐飞机」）：

- ❌ 生硬：「根据我的记忆，你怕坐飞机。」
- ✅ 自然：「出差顺利呀～飞机上要是紧张就听点歌，落地跟我说一声。」

## 6. 关键作用域决策

### 6.1 角色卡作用域 = Bot 级全局

一个 Bot 对所有联系人扮演「同一个角色」（符合「AI 是一个有个性的真人」）。以 `ai.character_id` 存入配置（走既有 `ai.*` KV 通道）。

### 6.2 记忆隔离 = (bot, sender)，bot 级全局打通

★二轮修订：去掉 channel 维度。同一个人（`sender`=wxid，跨会话稳定）不论私聊、任何群，都是同一份记忆——bot 走到哪都记得这个人。张三李四仍互不串（sender 不同），但「张三在私聊说的」和「张三在群里说的」归并为同一份。

记忆可见模式配置 `ai.memory_scope`（走既有 `ai.*` KV 通道）：
- `global`（默认）= 模式 A：关系树全员 brief 常驻，AI 拿捏是否提及。
- `current` = 模式 B：只见当前人 + 关系树边，其余靠工具查。
- `freeform` = 自由+（后期）：开局空树，AI 自建。首版落 `global`/`current` 两档，`freeform` 预留配置值与工具写权限，逻辑后续接。

## 7. 记忆写入与整理

### 7.1 AI 主动记 = `remember` 内置工具

`remember` 是内置 App 工具（走 `builtin.Register`）。AI 聊天时自行判断该不该记，静默写入，不打断对话、不说「我记住了」。

```
用户：「我下周要去日本出差，有点怕坐飞机」
  AI 判断值得记 → 调用 remember：
     { category:"profile", content:"怕坐飞机", importance:3 }
     { category:"fact", content:"下周去日本出差", importance:2, expires_at:<下周末> }
  静默写入 memories，AI 正常回复用户
```

### 7.2 AI 驱动整理（consolidation）

- **触发**：懒触发。写记忆后若该 `(bot,sender)` 记忆数超阈值（默认 40）则异步跑一次整理，不阻塞聊天（项目 cron 已移除，不引入新调度依赖）。
- **流程**：把该 scope 全部记忆 + 当前时间喂给 AI，输出整理指令：
  - `DELETE`：有时效 && 已过期（`expires_at` 已过）&& 小事件
  - `COMPRESS`：把若干条陈旧旧记忆提炼合并为一条（如多次「工作忙」→「长期高压工作状态」）
  - `KEEP`：近期记忆一律保留
- **近期不删的硬保证**：整理时按 `created_at` 分「近期窗口（最近 15 条 / 7 天内）」与「陈旧区」，`DELETE`/`COMPRESS` 只作用于陈旧区。**代码层强制**：即便 AI 指令命中近期条目也忽略（不完全信任 AI 输出）。

## 8. 主动发消息

主动消息有**两个来源**，共用同一张 `scheduled_messages` 表、同一条触发 ticker、同一套发送逻辑，仅「谁登记的、为什么登记」不同：

| 来源 | 触发场景 | 例子 |
|---|---|---|
| **A. 用户约定**（显式） | 用户话里含未来时间点 / 约定 | 「明早 6 点半叫我起床」 |
| **B. AI 自主惦记**（隐式，本轮新增） | AI 依角色性格 + 关系亲密度，自己决定要不要在稍后主动找人 | 粘人角色在对话冷场后隔几分钟发「在吗？」 |

两者都**只是往 `scheduled_messages` 写一行 `pending`**（存提示词 + 触发时间 + 容差），到点由 ticker 现场生成消息。区别仅在「由谁、依据什么登记」，落库结构完全一致——因此 B 不需要新表、新循环，只是把 `schedule_message` 工具的调用权也开放给「AI 的主观意愿」。

### 8.1 登记 = `schedule_message` / `cancel_schedule` 内置工具

**来源 A（用户约定）**：AI 从对话里识别未来约定，存提示词而非原文：

```
用户：「我明天早上 6 点半得起，好困啊」
  AI → schedule_message：
     { fire_at:"2026-08-12 06:42", tolerance:120,
       prompt:"叫主人起床，温柔地，提醒他昨晚说今天早起", origin:"promise" }
  静默写入 scheduled_messages(status=pending)
  AI 正常回复：「那早点睡呀，明早我叫你～」（不剧透具体文案）
```

**来源 B（AI 自主惦记，本轮新增）**：没有用户指令，AI 依**角色性格 + 关系亲密度 + 记忆**自行决定「要不要过一会儿主动找 TA」。工具同一个，AI 自主调用：

```
场景：一段热聊后，用户 20 分钟没回，角色设定为「粘人 / 缠人」
  AI 主动 → schedule_message：
     { fire_at:"<当前+8分钟>", tolerance:300,
       prompt:"假装刚忙完，随口问一句在不在，别显得太刻意", origin:"proactive" }
  （无需任何用户约定；下一轮该用户仍没回 → 到点发「在吗？」/「刚忙完，你在干嘛呀」）
```

- **是否发、隔多久、发什么，全由角色性格决定**：粘人 / 热情 → 会主动惦记、间隔短；高冷 / 独立 → 几乎不主动，或很久才来一句。判断依据（角色卡 + 记忆 + 当前关系）已在 system 段上下文中，AI 登记时即「就是这个角色」，无需新字段。
- **防打扰的代码兜底**（不完全信任 AI）：见 §8.4。
- `origin` 字段（`promise` | `proactive`）仅用于统计与限流区分，不影响发送路径。

### 8.2 触发 = 后台 ticker（复用 reminderLoop 同一循环体系）

在 `internal/bot/manager.go` 紧挨 `checkReminders()` 增加 `checkScheduledMessages()`：

- **基座粒度改为 1 分钟**（原 reminder 的 10 分钟 ticker 拆分或新增 1 分钟 ticker；对 reminder 逻辑无害）。
- 每分钟扫：`ListDueScheduledMessages(now)` = `status='pending' AND fire_at <= now`（容差在业务层判断，见 §8.3）。
- 命中一条 → 现场调用 AI：把 `prompt` + 该用户记忆 + 最近对话喂进去 → 生成「当下这一刻」的消息（每次不同，活的）→ 经 §10 `sendHumanized` 拆条发出。
- 成功：`status=sent, sent_at=now`；失败（窗口过期）：`status=failed, fail_reason="会话窗口已过期"`（不静默丢）。
- **发送前二次确认**（针对来源 B）：若在 `fire_at` 之前用户其实已经回过消息 / 已重新热聊，则该条「惦记」可能已无意义 → ticker 命中时把「距上次用户消息的间隔、这条的 `origin` 与 `prompt`」一并交给 AI，由 AI 决定「还发不发、发什么」，AI 可选择放弃（标 `cancelled`）。避免「刚聊完又机械地问在吗」。

### 8.3 触发精度 = AI 自定容差（参考事情 + 角色设定）

`tolerance` 由 AI 登记时给出，是软值而非全局硬编码：

- **基座必须细（1 分钟）+ 容差放宽**：若基座是 10 分钟，则「叫早收窄到 1-3 分钟」无法成立；因此基座细、用 `tolerance` 控制波动范围。
- **参考事情紧迫性**：叫早 / 吃药宜准（`tolerance` 120-180s）；闲聊问候宜松（600s）。
- **参考角色设定**（本次强调）：`schedule_message` 工具说明中指示 AI「结合你的角色性格与事情紧迫性综合判断容差」。守时严谨的人设 → 重要事容差更小；随性洒脱的人设 → 更宽松；傲娇 / 高冷人设甚至可能选择不主动发或语气冷淡。**无需新字段**：角色卡已在 system 段上下文中，AI 登记时即「就是这个角色」。

效果示例：

```
叫早  tolerance=120  → 仅 06:40~06:42 被扫到 → 准
喝咖啡 tolerance=600  → 14:50~15:00 任一分钟扫到即发 → 松
```

### 8.4 防打扰的代码兜底（不完全信任 AI）

**动机**：来源 B（`origin=proactive`，AI 自主惦记）由 AI 自行决定何时发、发什么，完全放任可能过度打扰（半夜发、一天刷太多、刚聊完又发）。因此在 `checkScheduledMessages()` 命中后、真正调用 AI 生成内容**之前**，加一道纯代码闸门 `gateProactive`，**只对 `origin=proactive` 生效**；`origin=promise`（用户亲口约定）默认直通——用户自己要的不拦。

呼应 §8.1 结论：`origin` 不改变底层「ticker → AI → `sendHumanized`」技术管线，但决定这条待发消息**是否进入本节闸门**（promise 直通，proactive 过闸）。

| 兜底规则 | 作用对象 | 默认值 | 命中后动作 | 配置项 |
| --- | --- | --- | --- | --- |
| 总开关 | 全部 proactive | true | 关则根本不产生 proactive（promise 照常） | `ai.proactive_enabled` |
| 静默时段 | proactive | 23:00–08:00 | 标 `cancelled`（fail_reason=「静默时段」） | `ai.proactive_quiet_hours` |
| 每日条数上限 | proactive / (bot,sender) | 3 条/天 | 超限后当天余下一律 `cancelled` | `ai.proactive_daily_limit` |
| 最小间隔 | proactive / (bot,sender) | 7200s(2h) | 距上条已发不足即 `cancelled` | `ai.proactive_min_interval` |
| 热聊抑制 | proactive | —— | 交 AI 二次确认是否放弃（见 §8.2） | —（AI 协同） |
| pending 去重 | proactive | 同人仅留 1 条 | 登记新 proactive 时替换旧 pending | —（登记逻辑） |

- **静默时段**：叫早类是 `promise` 不受此限；只有 AI 自发的深夜「在吗」被拦。
- **每日上限 / 最小间隔**：只按 `origin=proactive` 计数，`promise` 不占额度、不参与间隔计算——避免「用户约定的 3 个提醒把 AI 主动惦记的额度吃光」。
- **热聊抑制**：代码先算「距上次用户消息的间隔」，连同 `prompt`、`origin` 交 AI 复核，由 AI 决定还发不发（§8.2 已述）；此为 gate 与 AI 协同的一环。
- **pending 去重**：`schedule_message` 登记 proactive 时，若同 `(bot, sender)` 已有 pending 的 proactive，则替换而非叠加，防止堆积刷屏。

**实现落点**：`internal/bot/manager.go` 的 `checkScheduledMessages()`，在 `ListDueScheduledMessages(now)` 之后、调用 AI 之前插入 `gateProactive(msg, lastUserMsgAt, now, cfg)`。gate 抽成**纯函数**（输入 `(msg, 上次用户消息时间, now, cfg)`，输出 `放行 | cancel(reason)`），便于单测（见 §13）。所有 gate 判定与 `origin` 一并落日志/计数，便于回看「AI 今天想主动发几次、被拦几次」。

## 9. Agent 工具（二期）

均为内置 App 工具，走同一套路。

### 9.1 生图 gen_image（perchance，底层 Stable Diffusion）

**已查证事实**（[perchance text-to-image-plugin](https://perchance.org/text-to-image-plugin)）：出图靠公共插件 `text-to-image-plugin`，跑在 perchance 服务器 GPU 上，底层 Stable Diffusion；原生支持 `negativePrompt`/`resolution`/`seed`/`style`；**无官方 API**，有 `too many requests` 限流与反滥用。

**设计：provider 接口 + 可插拔实现**（不把 perchance 焊死）：

```go
type ImageProvider interface {
    Generate(ctx context.Context, prompt, negativePrompt string, opts ImageOptions) ([]byte, error)
}
// perchanceProvider  ← 逆向实现（需先做抓包研究，见 9.1 研究子任务）
// openaiImageProvider ← OpenAI 兼容出图 API，作为降级 / 兜底
```

- **perchance 研究子任务（实现前必做）**：用浏览器实际点一次生成，抓完整请求链路（拿 token → 提交 prompt → 轮询结果 → 取图 URL）。研究清楚再实现；若反爬一时难破，降级 OpenAI 兼容出图 API，功能不阻塞。
- **内置基础负向词**：默认拼入 `negativePrompt`：`bad hands, extra fingers, missing fingers, fused fingers, mutated hands, deformed`。
- **R18 说明**：功能做成通用第三方出图接入（prompt 进 → 图出 → 发微信，provider 可换），出图内容由所配置服务与 prompt 决定；不专门为色情内容做 prompt 调优，不绕过上游站点自身限制。

### 9.2 web_search / write_doc

| 工具 | 实现 |
|---|---|
| `web_search` | 接一个搜索 API，AI 调用 → 返回摘要融进回复 |
| `write_doc` | 生成 Markdown / 文本 → 存 MinIO → 经 `OutboundMessage{Data, FileName}` 作为文件消息发送 |

### 9.3 联系人 / 关系树工具（懒加载底座，一期就要）

§5.1 的两层懒加载靠这组内置工具落地——它们不是二期附属，而是「全局记忆不撑爆上下文」的核心机制，随一期数据地基一起做。均走 `builtin.Register`，静默执行、不打断对话。

| 工具 | 作用 | 对应 Store |
|---|---|---|
| `lookup_contact` | 按需拉某人完整画像 + 全部记忆 + 关系边（[C] 骨架里只有 brief，要深入才调） | `GetContact` + `ListMemories` + `ListRelations` |
| `upsert_contact` | 认识新朋友 / 更新某人 `name`·`brief`·`profile`；AI 交往中演进画像 | `UpsertContact` |
| `upsert_relation` | 登记 / 更新「谁—关系—谁」的一条边（张三—情侣—李四） | `UpsertRelation` |
| `set_owner` | 认定「喜欢的人 / 主人」（Q2-A）；自由+ 模式下由 AI 自己认 | `SetOwner` |

- **写权限随模式收放**：`global`/`current` 下 `upsert_*`/`set_owner` 可用但克制（画像演进）；`freeform`（后期）放开，靠这组工具从空树长出全部联系人与关系。
- **与 `remember` 分工**：`remember` 记零碎事件（挂 memories）；`upsert_contact` 沉淀「这个人是谁」的定稿画像（挂 contacts.profile）。AI 判断把握不准时优先 `remember`，整理阶段（§7.2）再择要提炼进 profile。

## 10. 拟人化微信表达

真人发微信有强烈的口语体特征：句尾基本不加句号、一段话拆成多条短消息连发、偶尔单独发一个字或一个标点。内容对了但格式像念稿会立刻穿帮。分两层实现。

### 10.1 内容风格层（prompt）

在角色卡人格层 [A] 追加「微信口语体」指令，让 AI 生成时即为碎句：

```
你在用微信聊天，像真人一样：
- 句尾不加句号（问号 / 感叹号在表达情绪时才用）
- 一次想说的话拆成几条短句，用换行分隔，不要堆成一大段
- 偶尔可以单独一条只发一个字或一个标点（「？」「哈哈哈」「嗯…」）
- 不用书面语和 markdown，不列 1. 2. 3.
```

### 10.2 拆条连发层（发送）

现状：`reply()` 把整段一次性 `Send`（`internal/sink/ai.go` line 387-390）。需将该 Send 抽成 `sendHumanized(reply)`：按换行拆分 → 逐条 `Send` → 条间加拟人停顿（可配合 `SendTyping`）。

```
AI 输出： "在呢\n刚看到消息\n怎么啦？"
   → Send("在呢")        停顿 ~1s
   → Send("刚看到消息")   停顿 ~1.2s
   → Send("怎么啦？")
```

主动发消息（§8）复用同一个 `sendHumanized`，保证「叫早」等也一条条自然冒出。

### 10.3 两个工程约束（代码兜底）

1. **停顿 / 拆条上限**：单条回复最多拆 N 条、条间停顿总和封顶（如各 ≤1.5s、总 ≤6s、拆条 ≤6）。超限则合并或秒发，避免 AI 输出很多行时真等很久。
2. **历史存整段**：拆条只影响「发送呈现」，写入 `SaveMessage` 的历史仍按「一次完整回复」存整段（保持 `internal/sink/ai.go` line 404-412 现状语义），避免多轮后上下文碎裂。

### 10.4 配置开关

新增 `ai.humanize_wechat`（默认开，走既有 `ai.*` KV）。关闭则回退为原「整段一次性发」，便于对比 / 降级。

## 11. 操作与回复分离（输出管线纪律）

核心原则：**用户只应看到「最终那句人话」+「操作的产物」，绝不该看到 AI 的内心独白、工具调用中间过程、自言自语**。已查证现状（`internal/sink/ai.go` `reply()`）：tool-call 循环里工具结果只回喂 LLM（line 330 `ContinueWithToolResults`）、不经 `Send`，故**工具调用过程天然对用户不可见** ✅。但有两个漏口与一个行为缺口需在本设计闭合。

### 11.1 thinking 永不外发（铁规矩）

现状 line 357-366：当模型返回 reasoning/thinking 且 `cfg.HideThinking==false` 时，会把 `💭 + 思考` 拼进 reply 一起发（line 365）——这会漏出自言自语。

**决策（用户选 A）**：思考对用户**永远不可见**，做成铁规矩，不受任何配置影响。
- 发往 provider 的文本**永不包含** thinking。改造 line 359-367：删除「`!HideThinking` 时把 thinking 拼进 reply」的分支，或强制视作隐藏。
- thinking 仍可留在 **trace/span 属性**（line 362 `ai.thinking_length` 等）供后台调试，但**不进 `OutboundMessage.Text`、不进历史**。
- 与 §10 拆条发送兼容：拆条前的 reply 已不含 thinking。

### 11.2 操作产物配一句自然的话（看性格，用户选 C）

现状 line 300-315 `skipLLM`：某轮工具结果全为 `Async` 或含 `Images` 时，判定「工具已直接处理」→ 直接 `return`，**不再让 AI 说收尾的话**。后果：用户可能只收到一张图 / 一个文件，没有任何一句话，显得机械。

**决策（用户选 C —— 交给性格）**：
- **默认**：操作有产物后，AI **给点反应**（发图配「画好啦～」、发文档配「写完了，你看看」）。
- **性格可覆盖**：高冷 / 惜字如金的人设，可以只甩产物、不配话——由角色卡人格决定，不写死。
- 实现方向：**不再无条件 `skipLLM` 直接 return**。当存在有产物的工具结果时，仍给 AI 一轮机会生成「配套的一句话」；是否真说、说什么，由 AI 结合人格判断（可为空 → 空则只发产物，天然覆盖高冷情况）。保留「纯异步且无需收尾」的静默快路径，避免每次都强行多一轮 LLM。**（此改动触碰核心循环，须单独小步 + 回归测试，见 §14）**

### 11.3 操作执行权归 AI（可犹豫 / 拒绝 / 被软磨硬泡，看性格 + 记忆）

「操作」不是「用户一声令下就必执行」。AI 对是否调用工具（尤其 `gen_image` 这类）有**自主权**：
- 结合**角色性格 + 记忆 + 铁律层**，AI 可以这一轮**不调工具**，只回一句「不要啦这个」「现在不想画」，表达犹豫或拒绝。
- 用户**软磨硬泡**、多轮请求后，AI 可能在某一轮才决定执行——这是自然的人际拉扯，不是 bug。
- **已查证可直接支撑**：tool-call 循环里「调不调工具」本就是 AI 每轮自主决定（line 265+ 的 round 循环 + `collectTools`），**无需新机制**；只需在铁律层 / 工具 description 中**显式赋予 AI「可以拒绝、可以拖延」的权利**，并说明「结合人设与关系亲密度决定」。
- 与铁律层（§4.7）协同：如「只专一」，则涉及暧昧类操作对非 `is_owner` 用户可倾向拒绝——由 AI 拿捏，铁律划边界。

> 三条共同构成「输出纪律」：**11.1 藏思考（永远）、11.2 产物配话（看性格）、11.3 操作可拒（看性格+记忆）**。11.1 是硬编码铁规矩；11.2/11.3 把决定权交回角色人格，仅提供机制与权利、不写死行为。

## 12. 错误处理汇总

| 场景 | 处理 |
|---|---|
| 角色卡 JSON 损坏 / 非法 | 导入时校验，拒绝入库并报错，不污染运行时 |
| 记忆读取失败 | 降级为「无记忆」正常聊天，不中断 |
| AI 整理越界（欲删近期记忆） | 代码兜底：近期窗口硬保护，忽略越界指令 |
| 主动消息发送失败（24h 窗口过期） | 标 `failed` + `fail_reason`，不静默丢 |
| perchance 失效 / 限流 | 降级 OpenAI 出图；再失败回一句「画不出来」，不崩溃 |
| 世界书 / 记忆注入过长 | 按 `importance` / `priority` 预算裁剪，保护 context |
| 拆条发送中途失败 | 记录已发条数与错误，不重复发已成功的条 |

## 13. 测试策略

沿用项目现有表驱动测试风格（`internal/ai/chat_test.go`、`internal/store/storetest`）。外部依赖（AI API、perchance、provider）用 mock / fake，不打真实网络。

- **Store 层**：7 张表 CRUD + 记忆 `(bot,sender)` 隔离（张三李四不串、同人跨会话打通）+ `contacts` 按 `(bot,sender)` upsert 唯一 + `set_owner` 同 bot 唯一 + `ListDueScheduledMessages` 到点判定 + `ListWorldEntries` 含全局 + `GetPersonaRules` 三级回退。
- **组装层**：`BuildMessages` 分层注入顺序（铁律置顶）、世界书命中 / 未命中、关系树骨架 vs 当前人全量、`memory_scope` 三模式 [C] 差异、记忆预算裁剪、无角色卡 / 无记忆的降级路径。
- **整理层**：喂带 `expires_at` 的记忆，断言「近期保留 + 过期删除 + 陈旧压缩」，并断言代码兜底能拦住「删近期」的越界指令。
- **主动消息**：`fire_at + tolerance` 到点判定用模拟时钟（不真等）；窗口过期 → `failed`。
- **防打扰兜底（§8.4）**：`gateProactive` 纯函数表驱动——`origin=promise` 恒直通（不受静默/上限/间隔约束）；`origin=proactive` 命中静默时段 / 超每日上限 / 不足最小间隔时判 `cancelled` 且 `fail_reason` 正确；边界值（正好 08:00、正好第 3 条、间隔正好等于阈值）断言；总开关关闭时不产生 proactive。
- **拟人化**：`sendHumanized` 拆条数、条间停顿上限、历史仍存整段；`humanize_wechat=false` 时整段发。
- **输出纪律（§11）**：断言发往 provider 的文本**永不含 thinking**（即便模拟 `HideThinking=false`）；`skipLLM` 改造后「有产物工具结果」仍允许 AI 追加一句话、且 AI 返回空话术时只发产物不发空消息；工具自主权用 mock LLM 模拟「首轮拒绝、次轮执行」断言中间轮不外发。

## 14. 实施顺序（分步迭代，不批量乱改）

**一期（人格地基）**

1. 迁移 `0010_persona_memory.sql`（sqlite + postgres）+ model 结构体 + Store 接口与各 backend 实现 + storetest。
2. 角色卡：V2 JSON / PNG 导入解析、`character_book` 拆世界书、导入 API + 前端页、`ai.character_id` 配置。
3. `BuildMessages` 分层组装（`persona.go` / `lorebook.go` / `memory_inject.go`），含「不生硬」指令与微信口语体指令。
4. `remember` 内置工具 + AI 驱动整理（懒触发 + 近期硬保护）。
5. 主动发消息：`schedule_message` / `cancel_schedule` 工具（带 `origin` 双来源）+ `checkScheduledMessages()` + 1 分钟基座 ticker + AI 自定容差（参考事情 + 角色）+ `gateProactive` 防打扰兜底（§8.4，纯函数，只拦 proactive）。
6. 拟人化发送：`sendHumanized` 拆条连发 + `ai.humanize_wechat` 开关；主动消息复用。
7. 输出纪律（§11）：11.1 thinking 永不外发（改 `reply()` line 359-367，铁规矩）；11.2 `skipLLM` 改造让操作产物可配一句话（触碰核心循环，单独小步 + 回归）；11.3 铁律层 / 工具 description 赋予 AI「可犹豫 / 拒绝 / 被软磨硬泡」的权利（prompt 层，无新机制）。

**二期（agent 能力）**

8. `ImageProvider` 接口 + OpenAI 兜底实现 + `gen_image` 工具（内置负向词）。
9. perchance 抓包研究子任务 → `perchanceProvider` 实现（失败则维持降级）。
10. `web_search` + `write_doc` 工具。

每步独立可测、可回归，完成即验证，不一次性大改核心循环。

## 附录 A：关键源码锚点

- `internal/ai/chat.go` `BuildMessages` — system 段注入点
- `internal/sink/ai.go` `reply()` — 回复主流程；line 371-373 `StripMarkdown` 后处理钩子；line 387-390 文本发送；line 404-412 历史保存；tool-call 循环与 `ReplyType=="image"`
- `internal/builtin/registry.go` `Register` — 内置 App 工具注册
- `internal/bot/manager.go` `reminderLoop` / `checkReminders` — 后台主动推送范本（ticker）
- `internal/provider/provider.go` `OutboundMessage` / `Send` / `SendTyping` / `GetLatestContextToken` — 发送接口
- `internal/store/store.go` — Store 子接口聚合
- `internal/store/sqlite/migrations/0007_cron_jobs.sql` — 迁移风格范本
- `internal/store/channel.go` + `resolveConfig` — `ai.*` KV 配置通道

## 附录 B：待研究 / 已知风险

- perchance 无稳定契约，逆向请求随其前端改动可能失效；已用 `ImageProvider` 接口 + OpenAI 降级隔离风险。
- 微信 iLink 24 小时会话窗口为平台约束，主动消息在窗口外必然失败，仅能优雅标记，无法绕过。
- 拟人化条间停顿与 provider 的实际发送节流 / 限频需联调（避免被判定频繁发送）。

