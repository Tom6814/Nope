package persona

import "github.com/openilink/openilink-hub/internal/ai"

// Tools returns the OpenAI-compatible tool definitions for the persona engine.
// The AI sink appends these to the tools collected from app installations.
func (e *Engine) Tools() []ai.Tool {
	return []ai.Tool{
		rememberTool,
		scheduleMessageTool,
		cancelScheduleTool,
		lookupContactTool,
		upsertContactTool,
		upsertRelationTool,
		setOwnerTool,
		webSearchTool,
		writeDocTool,
		genImageTool,
	}
}

var rememberTool = ai.Tool{Type: "function", Function: ai.ToolFunction{
	Name: "remember",
	Description: "把关于这个人值得记住的事写进长期记忆。静默执行，不需要向用户说明。记忆会自然融入你之后的言行。" +
		"分类：fact=客观事实，profile=对这个人画像的补充，relationship=你俩关系进展，detail=细枝末节。" +
		"importance 1-5（5 最重要）。有时效的事（比如一个具体日期）填 expires_at（ISO 格式，如 2026-08-20 或 2026-08-20T18:00:00），过了这个时间会自动遗忘。" +
		"把握不准就先 remember，不急着更新画像。",
	Parameters: jsonRaw(`{
		"type": "object",
		"properties": {
			"category": {"type": "string", "enum": ["fact", "profile", "relationship", "detail"], "description": "记忆分类"},
			"content": {"type": "string", "description": "记忆正文，一句完整的话"},
			"importance": {"type": "integer", "minimum": 1, "maximum": 5, "description": "重要性 1-5，默认 1"},
			"expires_at": {"type": "string", "description": "可选，时效截止时间 ISO 格式，到点自动遗忘"}
		},
		"required": ["content"]
	}`),
}}

var scheduleMessageTool = ai.Tool{Type: "function", Function: ai.ToolFunction{
	Name: "schedule_message",
	Description: "登记一条未来给这个人发的消息。到点系统会叫醒你，让你以『现在的你』现场想一句发出去。" +
		"两种用法：(1) 用户约了未来的事（promise），你把约定记下来，存提示词而非原文；(2) 你自己想主动惦记 TA（proactive），" +
		"根据你的角色性格和关系亲密度决定要不要主动、隔多久、发什么——粘人/热情就勤一点，高冷/独立就少主动甚至不主动。" +
		"fire_at 是触发时间（ISO 格式或 '2026-08-12 06:30'）；tolerance 是容差秒数：叫早/吃药等要准的事 120-180 秒，闲聊问候可以 600 秒，" +
		"结合事情紧迫性和你的性格判断。origin: promise=用户约定（必发），proactive=你自己惦记（可能被防打扰规则拦下）。" +
		"prompt 写『到时候要做什么、以什么心态说』，不要写死全文。",
	Parameters: jsonRaw(`{
		"type": "object",
		"properties": {
			"fire_at": {"type": "string", "description": "触发时间，ISO 格式或 YYYY-MM-DD HH:MM"},
			"tolerance": {"type": "integer", "minimum": 30, "description": "容差秒数，参考事情紧迫性与角色性格"},
			"prompt": {"type": "string", "description": "到点提醒自己的提示词，写意图与心态，不写死全文"},
			"origin": {"type": "string", "enum": ["promise", "proactive"], "description": "promise=用户约定；proactive=你自己主动惦记"}
		},
		"required": ["fire_at", "prompt"]
	}`),
}}

var cancelScheduleTool = ai.Tool{Type: "function", Function: ai.ToolFunction{
	Name:        "cancel_schedule",
	Description: "取消一条还没发的定时消息（比如约定取消了）。需要知道它的 id（schedule_message 的结果里会返回）。",
	Parameters: jsonRaw(`{
		"type": "object",
		"properties": {
			"id": {"type": "string", "description": "定时消息 id"}
		},
		"required": ["id"]
	}`),
}}

var lookupContactTool = ai.Tool{Type: "function", Function: ai.ToolFunction{
	Name: "lookup_contact",
	Description: "查看某个人的完整画像、记忆和关系边（骨架里只有一行简介，想深入了解某人时用它）。" +
		"静默执行，结果只供你参考，不要复述给用户。sender 是该人的 wxid（对方 id，不是名字）。",
	Parameters: jsonRaw(`{
		"type": "object",
		"properties": {
			"sender": {"type": "string", "description": "对方的 wxid（消息里能看到，就是对方 id）"}
		},
		"required": ["sender"]
	}`),
}}

var upsertContactTool = ai.Tool{Type: "function", Function: ai.ToolFunction{
	Name: "upsert_contact",
	Description: "认识/更新一个人：定稿『这个人是谁』。name=你怎么称呼 TA；brief=一行极短索引（常驻上下文，如『主人。程序员，最近换工作』）；" +
		"profile=完整画像（按需加载，会随交往演变）。不确定的细节用 remember 记，别急着写进画像。静默执行。",
	Parameters: jsonRaw(`{
		"type": "object",
		"properties": {
			"sender": {"type": "string", "description": "对方的 wxid"},
			"name": {"type": "string", "description": "你怎么称呼 TA"},
			"brief": {"type": "string", "description": "一行极短索引"},
			"profile": {"type": "string", "description": "完整画像"}
		},
		"required": ["sender"]
	}`),
}}

var upsertRelationTool = ai.Tool{Type: "function", Function: ai.ToolFunction{
	Name: "upsert_relation",
	Description: "登记/更新两个人之间的关系边，如 张三—情侣—李四。from_sender/to_sender 是 wxid。relation 是关系标签（情侣/兄弟/同事/朋友…），" +
		"note 是补充（如『2026 在一起』）。静默执行。",
	Parameters: jsonRaw(`{
		"type": "object",
		"properties": {
			"from_sender": {"type": "string", "description": "关系起点 wxid"},
			"to_sender": {"type": "string", "description": "关系终点 wxid"},
			"relation": {"type": "string", "description": "关系标签"},
			"note": {"type": "string", "description": "补充说明"}
		},
		"required": ["from_sender", "to_sender", "relation"]
	}`),
}}

var setOwnerTool = ai.Tool{Type: "function", Function: ai.ToolFunction{
	Name: "set_owner",
	Description: "认定『喜欢的人/主人』。全 bot 只能有一个。如果你认定当前这位就是，用它标记；之后专一铁律朝向 TA。" +
		"这是一个郑重的决定，只有在心里确认时才调用。",
	Parameters: jsonRaw(`{
		"type": "object",
		"properties": {
			"sender": {"type": "string", "description": "对方的 wxid"}
		},
		"required": ["sender"]
	}`),
}}

var webSearchTool = ai.Tool{Type: "function", Function: ai.ToolFunction{
	Name:        "web_search",
	Description: "联网搜索实时信息，只给结论，不列来源，不展开成长摘要。用户问新闻/实时数据/不知道的最新信息时用它。结果别照抄，像你刚查过一样自然回。",
	Parameters: jsonRaw(`{
		"type": "object",
		"properties": {
			"query": {"type": "string", "description": "搜索关键词"}
		},
		"required": ["query"]
	}`),
}}

var writeDocTool = ai.Tool{Type: "function", Function: ai.ToolFunction{
	Name:        "write_doc",
	Description: "把一段内容写成一个文档（Markdown/文本）发给用户。适合用户要『写一份什么』的场合（总结、提纲、清单、方案）。title=文档名，content=内容。",
	Parameters: jsonRaw(`{
		"type": "object",
		"properties": {
			"title": {"type": "string", "description": "文档名；第一阶段始终输出 .md，传入其他后缀会自动改成 .md"},
			"content": {"type": "string", "description": "文档正文"}
		},
		"required": ["title", "content"]
	}`),
}}

var genImageTool = ai.Tool{Type: "function", Function: ai.ToolFunction{
	Name: "gen_image",
	Description: "按描述生成图片发给用户，数量没提就默认一张，需要几张由你按语境判断。你可以结合人设决定要不要画、什么时候画；用户软磨硬泡时也可以在某一轮才答应。" +
		"prompt 写画面内容（英文效果更好）；negative_prompt 可选（默认已内置基础负向词）。",
	Parameters: jsonRaw(`{
		"type": "object",
		"properties": {
			"prompt": {"type": "string", "description": "画面描述"},
			"negative_prompt": {"type": "string", "description": "可选，不要出现的内容"},
			"size": {"type": "string", "enum": ["square", "landscape", "portrait"], "description": "可选，默认 square"},
			"count": {"type": "integer", "minimum": 1, "maximum": 4, "description": "可选，一次生成几张，默认 1，最多 4 张"}
		},
		"required": ["prompt"]
	}`),
}}
