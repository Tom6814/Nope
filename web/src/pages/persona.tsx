import { useCallback, useEffect, useRef, useState } from "react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";
import { Switch } from "@/components/ui/switch";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Badge } from "@/components/ui/badge";
import { Skeleton } from "@/components/ui/skeleton";
import { useConfirm } from "@/components/ui/confirm-dialog";
import { useToast } from "@/hooks/use-toast";
import { api, botDisplayName, type Memory, type MemoryGroup } from "@/lib/api";
import { useUser } from "@/hooks/use-auth";
import {
  Loader2,
  Upload,
  Trash2,
  UserRound,
  BookOpenText,
  ShieldCheck,
  Sparkles,
  FileJson,
  Brain,
  RefreshCw,
} from "lucide-react";

export function PersonaPage() {
  const { data: user } = useUser();
  const isAdmin = user?.role === "admin" || user?.role === "superadmin";
  const [activeTab, setActiveTab] = useState("characters");

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-bold tracking-tight">角色与记忆</h1>
        <p className="text-sm text-muted-foreground mt-0.5">
          让 AI 像真人一样：有设定（角色卡 /
          世界书）、会记事（记忆）、会惦记你（主动消息）、能干活（Agent 工具）。
        </p>
      </div>

      <Tabs value={activeTab} onValueChange={setActiveTab} className="space-y-6">
        <TabsList className="bg-muted/50 p-1">
          <TabsTrigger value="characters" className="px-6">
            角色卡
          </TabsTrigger>
          <TabsTrigger value="memories" className="px-6">
            记忆
          </TabsTrigger>
          <TabsTrigger value="rules" className="px-6">
            人格铁律
          </TabsTrigger>
          {isAdmin && (
            <TabsTrigger value="behavior" className="px-6">
              行为设置
            </TabsTrigger>
          )}
        </TabsList>
        <TabsContent value="characters" className="m-0 space-y-6">
          <CharactersTab />
        </TabsContent>
        <TabsContent value="memories" className="m-0 space-y-6">
          <MemoriesTab />
        </TabsContent>
        <TabsContent value="rules" className="m-0 space-y-6">
          <RulesTab />
        </TabsContent>
        {isAdmin && (
          <TabsContent value="behavior" className="m-0 space-y-6">
            <BehaviorTab />
          </TabsContent>
        )}
      </Tabs>
    </div>
  );
}

// ---------------------------------------------------------------------------
// 角色卡
// ---------------------------------------------------------------------------

function CharactersTab() {
  const { toast } = useToast();
  const [characters, setCharacters] = useState<any[]>([]);
  const [loading, setLoading] = useState(true);
  const [aiConfig, setAiConfig] = useState<any>(null);
  const [importing, setImporting] = useState(false);
  const [jsonText, setJsonText] = useState("");
  const [genOpen, setGenOpen] = useState(false);
  const [genDesc, setGenDesc] = useState("");
  const [generating, setGenerating] = useState(false);
  const [selected, setSelected] = useState<any>(null);
  const fileRef = useRef<HTMLInputElement>(null);

  const refresh = useCallback(async () => {
    const [chars, cfg] = await Promise.all([
      api.listCharacters(),
      api.getAIConfig().catch(() => null),
    ]);
    setCharacters(chars || []);
    setAiConfig(cfg);
    setLoading(false);
  }, []);

  useEffect(() => {
    refresh();
  }, [refresh]);

  async function handleFile(file: File) {
    setImporting(true);
    try {
      const r = await api.importCharacterFile(file);
      toast({
        title: `已导入角色「${r.name}」`,
        description: `拆出 ${r.world_entries} 条世界书条目`,
      });
      setJsonText("");
      refresh();
    } catch (e: any) {
      toast({ variant: "destructive", title: "导入失败", description: e.message });
    }
    setImporting(false);
  }

  async function handleJsonImport() {
    if (!jsonText.trim()) return;
    setImporting(true);
    try {
      const r = await api.importCharacter(jsonText);
      toast({
        title: `已导入角色「${r.name}」`,
        description: `拆出 ${r.world_entries} 条世界书条目`,
      });
      setJsonText("");
      refresh();
    } catch (e: any) {
      toast({ variant: "destructive", title: "导入失败", description: e.message });
    }
    setImporting(false);
  }

  async function handleGenerate() {
    if (!genDesc.trim()) return;
    setGenerating(true);
    try {
      const r = await api.generateCharacter(genDesc);
      toast({ title: `已生成角色「${r.name}」`, description: "已保存到角色卡列表" });
      setGenDesc("");
      setGenOpen(false);
      refresh();
    } catch (e: any) {
      toast({ variant: "destructive", title: "生成失败", description: e.message });
    }
    setGenerating(false);
  }

  async function selectCharacter(id: string) {
    const cfg = aiConfig || {};
    await api.setAIConfig({ ...cfg, character_id: id });
    setAiConfig({ ...cfg, character_id: id });
    toast({ title: "已设为当前角色" });
  }

  async function clearCharacter() {
    const cfg = aiConfig || {};
    await api.setAIConfig({ ...cfg, character_id: "" });
    setAiConfig({ ...cfg, character_id: "" });
    toast({ title: "已关闭角色卡" });
  }

  async function removeCharacter(id: string) {
    await api.deleteCharacter(id);
    if (aiConfig?.character_id === id) {
      const cfg = { ...aiConfig, character_id: "" };
      await api.setAIConfig(cfg);
      setAiConfig(cfg);
    }
    if (selected?.id === id) setSelected(null);
    refresh();
    toast({ title: "已删除角色卡" });
  }

  return (
    <div className="grid gap-6 lg:grid-cols-3">
      {/* Import */}
      <Card className="border-border/50 bg-card/40 lg:col-span-3">
        <CardHeader>
          <CardTitle className="flex items-center gap-2">
            <Upload className="h-4 w-4" /> 导入角色卡
          </CardTitle>
          <CardDescription>
            支持 SillyTavern（酒馆）V2 角色卡：直接粘贴 JSON，或上传 .json /
            .png（内嵌卡）。角色卡里的世界书（character_book）会自动拆成世界书条目。
            也可以用一段描述让 AI 直接生成一张完整角色卡。
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-3">
          <div className="flex flex-wrap items-center gap-2">
            <input
              ref={fileRef}
              type="file"
              accept=".json,.png,application/json,image/png"
              className="hidden"
              onChange={(e) => {
                const f = e.target.files?.[0];
                if (f) handleFile(f);
                e.target.value = "";
              }}
            />
            <Button variant="outline" disabled={importing} onClick={() => fileRef.current?.click()}>
              {importing ? (
                <Loader2 className="h-4 w-4 animate-spin" />
              ) : (
                <FileJson className="h-4 w-4" />
              )}
              选择文件导入
            </Button>
            <Button variant="outline" onClick={() => setGenOpen((v) => !v)}>
              <Sparkles className="h-4 w-4" /> 用提示词生成
            </Button>
            <span className="text-xs text-muted-foreground">或粘贴 V2 JSON：</span>
          </div>
          <Textarea
            rows={4}
            value={jsonText}
            onChange={(e) => setJsonText(e.target.value)}
            placeholder='{"spec":"chara_card_v2","spec_version":"2.0","data":{"name":"...","description":"...","character_book":{...}}}'
            className="font-mono text-xs resize-y"
          />
          <div className="flex justify-end">
            <Button size="sm" disabled={importing || !jsonText.trim()} onClick={handleJsonImport}>
              {importing ? <Loader2 className="h-4 w-4 animate-spin" /> : "导入 JSON"}
            </Button>
          </div>
          {genOpen && (
            <div className="space-y-3 rounded-xl border border-border/50 bg-muted/10 p-3">
              <div className="flex items-center gap-2">
                <Sparkles className="h-4 w-4 text-primary" />
                <span className="text-sm font-bold">用一段描述生成角色卡</span>
                <span className="text-xs text-muted-foreground">
                  AI 会扩展成完整的 SillyTavern V2 角色卡并保存
                </span>
              </div>
              <Textarea
                rows={3}
                value={genDesc}
                onChange={(e) => setGenDesc(e.target.value)}
                placeholder="例：一个嘴硬心软的女主厨，25 岁，在巷子深处开了一家深夜食堂，收养了一只流浪猫……"
              />
              <div className="flex justify-end">
                <Button size="sm" disabled={generating || !genDesc.trim()} onClick={handleGenerate}>
                  {generating ? <Loader2 className="h-4 w-4 animate-spin" /> : "生成"}
                </Button>
              </div>
            </div>
          )}
        </CardContent>
      </Card>

      {/* Character list */}
      <Card className="border-border/50 bg-card/40 lg:col-span-2">
        <CardHeader>
          <CardTitle className="flex items-center gap-2">
            <UserRound className="h-4 w-4" /> 我的角色卡
          </CardTitle>
        </CardHeader>
        <CardContent>
          {loading ? (
            <div className="flex justify-center py-8">
              <Loader2 className="h-6 w-6 animate-spin text-muted-foreground" />
            </div>
          ) : characters.length === 0 ? (
            <div className="text-center py-8 text-sm text-muted-foreground border rounded-xl border-dashed">
              还没有角色卡，导入一张开始
            </div>
          ) : (
            <div className="space-y-2">
              {characters.map((c) => {
                const active = aiConfig?.character_id === c.id;
                return (
                  <div
                    key={c.id}
                    className={`flex items-center justify-between p-3 rounded-xl border transition-colors ${active ? "border-primary/40 bg-primary/[0.04]" : "border-border/50 bg-muted/10"}`}
                  >
                    <div className="flex items-center gap-3 min-w-0">
                      <div className="h-9 w-9 rounded-lg bg-primary/10 text-primary flex items-center justify-center shrink-0">
                        <UserRound className="h-4 w-4" />
                      </div>
                      <div className="min-w-0">
                        <p className="text-sm font-bold flex items-center gap-2">
                          {c.name}
                          {active && (
                            <Badge className="bg-primary/10 text-primary border-none text-[9px]">
                              使用中
                            </Badge>
                          )}
                        </p>
                        <p className="text-[10px] text-muted-foreground truncate">
                          {c.spec} · {new Date(c.created_at * 1000).toLocaleDateString()} 导入
                        </p>
                      </div>
                    </div>
                    <div className="flex items-center gap-1.5 shrink-0">
                      {!active ? (
                        <Button size="xs" variant="outline" onClick={() => selectCharacter(c.id)}>
                          <Sparkles className="h-3 w-3 mr-1" /> 设为当前
                        </Button>
                      ) : (
                        <Button size="xs" variant="ghost" onClick={clearCharacter}>
                          停用
                        </Button>
                      )}
                      <Button
                        size="icon"
                        variant="ghost"
                        className="h-7 w-7 text-destructive"
                        onClick={() => removeCharacter(c.id)}
                      >
                        <Trash2 className="h-3.5 w-3.5" />
                      </Button>
                    </div>
                  </div>
                );
              })}
            </div>
          )}
        </CardContent>
      </Card>

      {/* World entries */}
      <Card className="border-border/50 bg-card/40">
        <CardHeader>
          <CardTitle className="flex items-center gap-2">
            <BookOpenText className="h-4 w-4" /> 世界书条目
          </CardTitle>
          <CardDescription>
            触发式知识：聊到关键词时才注入。点击角色卡可查看其世界书。
          </CardDescription>
        </CardHeader>
        <CardContent>
          {!selected && (
            <div className="text-sm text-muted-foreground py-4">
              {characters.length > 0 ? "暂未选择查看对象" : "暂无角色卡"}
            </div>
          )}
        </CardContent>
      </Card>
    </div>
  );
}

// ---------------------------------------------------------------------------
// 记忆（S2）
// ---------------------------------------------------------------------------

function formatRelativeTime(ts: number) {
  if (!ts) return "—";
  const diff = Math.floor((Date.now() - ts * 1000) / 1000);
  if (diff < 0) return "刚刚";
  if (diff < 60) return `${diff}秒前`;
  if (diff < 3600) return `${Math.floor(diff / 60)}分钟前`;
  if (diff < 86400) return `${Math.floor(diff / 3600)}小时前`;
  return `${Math.floor(diff / 86400)}天前`;
}

function MemoriesTab() {
  const { toast } = useToast();
  const { confirm, ConfirmDialog } = useConfirm();
  const [bots, setBots] = useState<any[]>([]);
  const [botId, setBotId] = useState("");
  const [groups, setGroups] = useState<MemoryGroup[] | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const [saving, setSaving] = useState<string | null>(null);
  const [deleting, setDeleting] = useState<string | null>(null);

  useEffect(() => {
    api
      .listBots()
      .then((l) => {
        const items = l || [];
        setBots(items);
        if (items.length) setBotId(items[0].id);
      })
      .catch((e: any) => setError(e.message || "加载失败"));
  }, []);

  const load = useCallback(() => {
    if (!botId) return;
    setLoading(true);
    setError("");
    api
      .botMemories(botId)
      .then((d) => setGroups(d.groups || []))
      .catch((e: any) => setError(e.message || "加载失败"))
      .finally(() => setLoading(false));
  }, [botId]);

  useEffect(() => {
    load();
  }, [load]);

  function patchMemory(memId: string, patch: Partial<Memory>) {
    setGroups((prev) =>
      (prev || []).map((g) => ({
        ...g,
        memories: g.memories.map((m) => (m.id === memId ? { ...m, ...patch } : m)),
      })),
    );
  }

  async function saveMemory(m: Memory) {
    setSaving(m.id);
    try {
      await api.updateBotMemory(botId, m.id, { content: m.content, importance: m.importance });
      toast({ title: "记忆已保存" });
      load();
    } catch (e: any) {
      toast({ variant: "destructive", title: "保存失败", description: e.message });
    } finally {
      setSaving(null);
    }
  }

  async function deleteMemory(m: Memory) {
    const ok = await confirm({
      title: "删除记忆",
      description: "确定删除这条记忆？AI 将不再记得这件事。",
      confirmText: "删除",
      variant: "destructive",
    });
    if (!ok) return;
    setDeleting(m.id);
    try {
      await api.deleteBotMemory(botId, m.id);
      toast({ title: "已删除" });
      load();
    } catch (e: any) {
      toast({ variant: "destructive", title: "删除失败", description: e.message });
    } finally {
      setDeleting(null);
    }
  }

  const total = groups?.reduce((n, g) => n + g.memories.length, 0) ?? 0;

  return (
    <Card className="border-border/50 bg-card/40">
      {ConfirmDialog}
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <Brain className="h-4 w-4" /> 记忆
        </CardTitle>
        <CardDescription>
          AI 记住的关于每个人的事实（按 人 分组，同一人在私聊和群里共享一份记忆）。
          可直接修改内容与重要程度，或删除不再需要的记忆。
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        {bots.length === 0 ? (
          <div className="text-center py-6 text-sm text-muted-foreground border rounded-xl border-dashed">
            还没有绑定角色实例（Bot）
          </div>
        ) : (
          <>
            <div className="flex items-center gap-3 flex-wrap">
              <Label className="text-xs font-bold uppercase text-muted-foreground">
                选择角色实例
              </Label>
              <Select value={botId} onValueChange={setBotId}>
                <SelectTrigger className="w-64">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {bots.map((b) => (
                    <SelectItem key={b.id} value={b.id}>
                      {botDisplayName(b)}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
              {!loading && !error ? (
                <Badge variant="secondary" className="text-[10px]">
                  共 {total} 条记忆
                </Badge>
              ) : null}
            </div>

            {loading ? (
              <div className="space-y-3">
                <Skeleton className="h-6 w-40" />
                <Skeleton className="h-24 w-full" />
                <Skeleton className="h-24 w-full" />
              </div>
            ) : error ? (
              <div className="text-center py-6 space-y-3">
                <p className="text-sm text-destructive">{error}</p>
                <Button variant="outline" size="sm" onClick={load}>
                  <RefreshCw className="h-3.5 w-3.5" /> 重试
                </Button>
              </div>
            ) : total === 0 ? (
              <div className="text-center py-8 text-sm text-muted-foreground border rounded-xl border-dashed">
                还没有记忆
              </div>
            ) : (
              <div className="space-y-4">
                {(groups || []).map((g) => (
                  <div key={g.sender} className="space-y-2">
                    <h3 className="text-xs font-bold uppercase text-muted-foreground flex items-center gap-2">
                      <UserRound className="h-3.5 w-3.5" /> {g.sender}
                      <span className="font-normal normal-case text-muted-foreground/60">
                        {g.memories.length} 条
                      </span>
                    </h3>
                    {g.memories.map((m) => (
                      <div
                        key={m.id}
                        className="rounded-xl border border-border/50 bg-muted/10 p-3 space-y-2"
                      >
                        <div className="flex items-center justify-between gap-2 flex-wrap">
                          <div className="flex items-center gap-2">
                            {m.category && <Badge variant="secondary">{m.category}</Badge>}
                            <span className="text-[10px] text-muted-foreground">
                              {formatRelativeTime(m.created_at)} 记 · 更新于{" "}
                              {formatRelativeTime(m.updated_at)}
                            </span>
                          </div>
                          <div className="flex items-center gap-1.5">
                            <div className="flex items-center gap-1.5">
                              <span className="text-[10px] text-muted-foreground">重要度</span>
                              <Select
                                value={String(m.importance)}
                                onValueChange={(v) => patchMemory(m.id, { importance: Number(v) })}
                              >
                                <SelectTrigger className="h-7 w-16 text-xs">
                                  <SelectValue />
                                </SelectTrigger>
                                <SelectContent>
                                  {[1, 2, 3, 4, 5].map((n) => (
                                    <SelectItem key={n} value={String(n)}>
                                      {n}
                                    </SelectItem>
                                  ))}
                                </SelectContent>
                              </Select>
                            </div>
                            <Button
                              size="xs"
                              variant="outline"
                              disabled={saving === m.id}
                              onClick={() => saveMemory(m)}
                            >
                              {saving === m.id ? (
                                <Loader2 className="h-3 w-3 animate-spin" />
                              ) : (
                                "保存"
                              )}
                            </Button>
                            <Button
                              size="icon"
                              variant="ghost"
                              className="h-7 w-7 text-destructive"
                              disabled={deleting === m.id}
                              onClick={() => deleteMemory(m)}
                            >
                              {deleting === m.id ? (
                                <Loader2 className="h-3.5 w-3.5 animate-spin" />
                              ) : (
                                <Trash2 className="h-3.5 w-3.5" />
                              )}
                            </Button>
                          </div>
                        </div>
                        <Textarea
                          rows={2}
                          value={m.content}
                          onChange={(e) => patchMemory(m.id, { content: e.target.value })}
                          className="text-sm resize-y bg-background/60"
                        />
                      </div>
                    ))}
                  </div>
                ))}
              </div>
            )}
          </>
        )}
      </CardContent>
    </Card>
  );
}

// ---------------------------------------------------------------------------
// 人格铁律
// ---------------------------------------------------------------------------

function RulesTab() {
  const { toast } = useToast();
  const [rules, setRules] = useState<any>(null);
  const [text, setText] = useState("");
  const [enabled, setEnabled] = useState(true);
  const [saving, setSaving] = useState(false);

  useEffect(() => {
    api.getPersonaRules().then((r) => {
      setRules(r);
      setText(r.rules_text);
      setEnabled(r.enabled);
    });
  }, []);

  async function save() {
    setSaving(true);
    try {
      await api.savePersonaRules({ enabled, rules_text: text });
      toast({ title: "人格铁律已保存" });
    } catch (e: any) {
      toast({ variant: "destructive", title: "保存失败", description: e.message });
    }
    setSaving(false);
  }

  return (
    <Card className="border-border/50 bg-card/40 max-w-3xl">
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <ShieldCheck className="h-4 w-4" /> 人格铁律（总开关，默认开启）
        </CardTitle>
        <CardDescription>
          这段规则注入在提示词最高优先级，是"约束"不是"台词"，不受口语化改写影响。
          可自行增删改；例如"严格禁止 NTR / 只专一 / 心里有喜欢的人"。
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        <div className="flex items-center justify-between">
          <Label className="text-sm font-medium">启用铁律层</Label>
          <Switch checked={enabled} onCheckedChange={setEnabled} />
        </div>
        <Textarea
          rows={16}
          value={text}
          onChange={(e) => setText(e.target.value)}
          className="font-mono text-xs resize-y"
        />
        <div className="flex justify-end gap-2">
          {text !== (rules?.rules_text || "") && (
            <Button variant="ghost" size="sm" onClick={() => setText(rules?.rules_text || "")}>
              还原默认
            </Button>
          )}
          <Button size="sm" disabled={saving} onClick={save}>
            {saving ? <Loader2 className="h-4 w-4 animate-spin" /> : "保存"}
          </Button>
        </div>
      </CardContent>
    </Card>
  );
}

// ---------------------------------------------------------------------------
// 行为设置（管理员）
// ---------------------------------------------------------------------------

function BehaviorTab() {
  const { toast } = useToast();
  const [cfg, setCfg] = useState<any>(null);
  const [saving, setSaving] = useState(false);

  useEffect(() => {
    api.getAIConfig().then(setCfg);
  }, []);

  function patch(p: any) {
    setCfg((prev: any) => ({ ...prev, ...p }));
  }

  async function save() {
    setSaving(true);
    try {
      await api.setAIConfig(cfg);
      toast({ title: "行为设置已保存" });
    } catch (e: any) {
      toast({ variant: "destructive", title: "保存失败", description: e.message });
    }
    setSaving(false);
  }

  if (!cfg) {
    return (
      <div className="flex justify-center py-10">
        <Loader2 className="h-6 w-6 animate-spin text-muted-foreground" />
      </div>
    );
  }

  return (
    <div className="space-y-6 max-w-3xl">
      <Card className="border-border/50 bg-card/40">
        <CardHeader>
          <CardTitle>拟人化表达</CardTitle>
          <CardDescription>让回复像真人发微信：碎句、不加句号、多条短消息连发。</CardDescription>
        </CardHeader>
        <CardContent className="space-y-4">
          <div className="flex items-center justify-between">
            <Label className="text-sm font-medium">拟人化拆条发送</Label>
            <Switch
              checked={cfg.humanize_wechat !== "false"}
              onCheckedChange={(v) => patch({ humanize_wechat: String(v) })}
            />
          </div>
          <div className="space-y-1.5">
            <Label className="text-xs font-bold uppercase text-muted-foreground">
              记忆可见模式
            </Label>
            <Select
              value={cfg.memory_scope || "global"}
              onValueChange={(v) => patch({ memory_scope: v })}
            >
              <SelectTrigger className="w-full">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="global">全局可见（默认）— 认识的人都心里有数</SelectItem>
                <SelectItem value="current">只见当前人 — 隐私更硬，其余靠工具查</SelectItem>
              </SelectContent>
            </Select>
            <p className="text-xs text-muted-foreground">
              记忆按 (bot, 人) 打通：同一个人私聊和群里是同一份记忆，不同人互不串。
            </p>
          </div>
          <div className="space-y-1.5">
            <Label className="text-xs font-bold uppercase text-muted-foreground">
              记忆整理阈值
            </Label>
            <Input
              type="number"
              min={10}
              value={cfg.memory_consolidate_threshold || 40}
              onChange={(e) => patch({ memory_consolidate_threshold: e.target.value })}
            />
            <p className="text-xs text-muted-foreground">
              某人记忆超过该条数时，后台异步整理一次（过期删除 / 陈旧压缩 / 近期保留）。
            </p>
          </div>
        </CardContent>
      </Card>

      <Card className="border-border/50 bg-card/40">
        <CardHeader>
          <CardTitle>主动发消息（AI 自主惦记）</CardTitle>
          <CardDescription>
            AI 依角色性格主动找你。防打扰兜底只拦 AI 自主发起的（用户约定的"叫早"不受限制）。
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-4">
          <div className="flex items-center justify-between">
            <Label className="text-sm font-medium">启用 AI 主动惦记</Label>
            <Switch
              checked={cfg.proactive_enabled !== "false"}
              onCheckedChange={(v) => patch({ proactive_enabled: String(v) })}
            />
          </div>
          <div className="space-y-1.5">
            <Label className="text-xs font-bold uppercase text-muted-foreground">静默时段</Label>
            <Input
              value={cfg.proactive_quiet_hours || "23:00-08:00"}
              onChange={(e) => patch({ proactive_quiet_hours: e.target.value })}
              placeholder="23:00-08:00"
            />
            <p className="text-xs text-muted-foreground">
              HH:MM-HH:MM，可跨午夜。期间 AI 自发消息被拦下。
            </p>
          </div>
          <div className="grid gap-4 sm:grid-cols-2">
            <div className="space-y-1.5">
              <Label className="text-xs font-bold uppercase text-muted-foreground">
                每日条数上限（/人）
              </Label>
              <Input
                type="number"
                min={0}
                value={cfg.proactive_daily_limit || 3}
                onChange={(e) => patch({ proactive_daily_limit: e.target.value })}
              />
            </div>
            <div className="space-y-1.5">
              <Label className="text-xs font-bold uppercase text-muted-foreground">
                最小间隔（秒）
              </Label>
              <Input
                type="number"
                min={60}
                value={cfg.proactive_min_interval || 7200}
                onChange={(e) => patch({ proactive_min_interval: e.target.value })}
              />
              <p className="text-xs text-muted-foreground">默认 7200 秒（2 小时）。</p>
            </div>
          </div>
        </CardContent>
      </Card>

      <div className="flex justify-end">
        <Button disabled={saving} onClick={save}>
          {saving ? <Loader2 className="h-4 w-4 animate-spin" /> : "保存行为设置"}
        </Button>
      </div>
    </div>
  );
}
