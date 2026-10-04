<script setup lang="ts">
import { ref, computed, onMounted, onUnmounted, nextTick } from "vue";
import { marked } from "marked";
import DOMPurify from "dompurify";
import { Events } from "@wailsio/runtime";
import {
  Service,
  type BootstrapInfo,
  type MsgLine,
  type ToolInfo,
  type WorkspaceChoice,
} from "../bindings/github.com/fan-weibo/ArkPerf/desktop";
import SideBar from "./components/SideBar.vue";
import WorkspacePanel from "./components/WorkspacePanel.vue";
import WorkspaceBar from "./components/WorkspaceBar.vue";

// 转录行模型。assistant 的 text 可能是 markdown，渲染时走 md()。
// tool/result 的 text 分别是"参数串"与"输出全文"：
// summary 显示首行，展开显示全文（模板和类型必须对齐，否则 TS 报错）。
type Line =
  | { kind: "user"; text: string }
  | { kind: "assistant"; text: string; head?: boolean }
  | { kind: "tool"; text: string; name: string }
  | { kind: "result"; text: string; name: string; isErr: boolean }
  | { kind: "note"; text: string }
  | { kind: "error"; text: string }
  | { kind: "toolgroup"; key: number; items: ToolEntry[] };

// 一次工具调用 = 参数行 +（可能有）结果行。
// 单行仍各自可折叠；整块再包一层组折叠头（学 Reasonix）。
type ToolEntry = {
  name: string;
  args: string;
  result?: { content: string; isErr: boolean };
};

// 运行中手动干预过的组（key = 组首行在 lines 里的下标，追加式增长时稳定）。
// 存的是**显式开/关**而不是"是否展开"：默认开合随运行状态走，
// 用户点了就以用户为准——否则运行中点收起会被"最后一组默认展开"顶回去。
const groupOverride = ref<Record<number, boolean>>({});

function toggleGroup(key: number) {
  groupOverride.value = { ...groupOverride.value, [key]: !groupOpen(key) };
}

// 「方舟智诊」标签在**渲染时**推导，不靠数据里传标志位。
//
// 规则：每条 user 消息（= 一次任务）之后的第一段回答挂标签。
// 之前试过让后端在数据里带 head 标志——实时与回放两条路径都要各自维护，
// 结果回放路径把它弄丢了（实测：实时有一个标签，切会话回来一个都没有）。
// 推导只需要行的顺序，谁产生这些行都无所谓，天然对两条路径一致。
const renderedLines = computed<Line[]>(() => {
  let needHead = true;
  const out: Line[] = [];
  // 连续的 tool/result 行聚成一个可折叠组。组首行下标作 key：
  // lines 是追加式的，中途不会变动前面的行，key 因此稳定。
  let buf: ToolEntry[] = [];
  let bufKey = -1;
  const flush = () => {
    if (!buf.length) return;
    out.push({ kind: "toolgroup", key: bufKey, items: buf });
    buf = [];
    bufKey = -1;
  };

  lines.value.forEach((l, idx) => {
    if (l.kind === "tool") {
      if (bufKey < 0) bufKey = idx;
      buf.push({ name: l.name, args: l.text });
      return;
    }
    if (l.kind === "result") {
      if (bufKey < 0) bufKey = idx;
      const last = buf[buf.length - 1];
      if (last && !last.result) {
        last.result = { content: l.text, isErr: l.isErr };
      } else {
        // 结果比调用多（理论不该发生）：如实显示，不吞掉
        buf.push({ name: l.name, args: "", result: { content: l.text, isErr: l.isErr } });
      }
      return;
    }
    flush();
    if (l.kind === "user") {
      needHead = true; // 新的一次任务
      out.push(l);
      return;
    }
    if (l.kind === "assistant") {
      if (needHead) {
        needHead = false;
        out.push({ ...l, head: true } as Line);
      } else {
        out.push(l);
      }
      return;
    }
    out.push(l); // note / error 不影响分组与标签
  });
  flush();
  return out;
});

// 组的默认开合：**运行中**最后一组展开（要看得到实时进度），
// 任务结束自动收起（转录恢复干净）；用户点开过的保持展开。
const lastGroupKey = computed(() => {
  for (let i = renderedLines.value.length - 1; i >= 0; i--) {
    const l = renderedLines.value[i];
    if (l.kind === "toolgroup") return l.key;
  }
  return -1;
});

function groupOpen(key: number): boolean {
  const o = groupOverride.value[key];
  if (o !== undefined) return o; // 用户点过：以用户为准
  return running.value && key === lastGroupKey.value;
}

function failedCount(items: ToolEntry[]): number {
  return items.filter((x) => x.result?.isErr).length;
}

marked.setOptions({ gfm: true, breaks: true });

// 模型输出可能带任意内容（网页抓取、代码），innerHTML 前必须消毒，
// 否则 XSS 能摸到 Wails 的绑定桥——桌面应用里这不是小事。
function md(src: string): string {
  return DOMPurify.sanitize(marked.parse(src) as string);
}

const boot = ref<BootstrapInfo | null>(null);
const bootErr = ref("");
const lines = ref<Line[]>([]);
const tools = ref<ToolInfo[]>([]);
const toolsLoaded = ref(false);
const task = ref("");
const running = ref(false);
const toolCalls = ref(0);
const approval = ref<{ id: string; name: string; args: string; scope: string } | null>(null);
// 流式输出：活块在 lines 里的下标，-1 表示当前没有正在流的块。
// 与 TUI 同一做法——整块更新而不是每段追加一行：追加会被 markdown 渲染
// 切得七零八落，定稿时还得回头把碎片收拾干净。
const streamIdx = ref(-1);
// 思考过程**不进转录**（往往比回答长一个数量级，全留下来会把结论冲得看不见），
// 只在运行条上给个字数，让"它在想"可见。
const thinking = ref(0);
// 工具链与设备**各自**的报告。
//
// 不能共用一个 ref：两个页面都靠起子进程拿数据（探测要跑 5 个工具，
// 设备要跑 hdc list targets），都是秒级。共用的话，先发的请求后返回
// 就会把另一个页面正在显示的内容覆盖掉——用户看到"设备列表里出现
// 工具链报告"，而且时机不定，几乎没法靠观察定位（实测撞过）。
const tcReport = ref("");
const dvReport = ref("");
const streamEl = ref<HTMLElement | null>(null);
const ta = ref<HTMLTextAreaElement | null>(null);

// 侧栏数据：工作区树（每个工作区带着它自己的会话）
const workspaces = ref<WorkspaceChoice[]>([]);

// 视图路由：chat = 聊天；tools/toolchain/devices = 侧栏功能各自的页面。
// 侧栏点功能是"跳转到页面"，不是弹框（用户明确的交互要求）——
// 切回会话（点侧栏会话或新会话）就回到 chat 视图。
const view = ref<"chat" | "tools" | "toolchain" | "devices">("chat");
const toolFilter = ref("");
const tcLoading = ref(false);
const dvLoading = ref(false);

const filteredTools = computed(() => {
  const q = toolFilter.value.trim().toLowerCase();
  if (!q) return tools.value;
  return tools.value.filter(
    (t) =>
      t.Name.toLowerCase().includes(q) ||
      t.Description.toLowerCase().includes(q) ||
      t.Approval.toLowerCase().includes(q),
  );
});

function openView(v: "tools" | "toolchain" | "devices") {
  view.value = v;
  // 进页面时自动拉数据：工具清单只拉一次，工具链/设备每次进入都刷新
  if (v === "tools" && !toolsLoaded.value) loadTools();
  if (v === "toolchain") checkToolchain();
  if (v === "devices") listDevices();
}

function backToChat() {
  view.value = "chat";
}

// 三栏宽度（可拖拽调整）
const sideW = ref(210);
const wsW = ref(240);
const resizing = ref("");

// 右栏工作区面板的句柄：切了工作区要让它重新列目录（面板自己不监听 prop）
const wsPanel = ref<InstanceType<typeof WorkspacePanel> | null>(null);

// 点开文件时把右栏放宽——代码在 240px 里没法看。
// 只在用户还没手动调过宽度（仍是默认窄栏）时才替他放宽：他拖过分隔条就尊重他的选择。
function onOpenFile() {
  if (wsW.value <= 260) wsW.value = 460;
}

const unsubs: (() => void)[] = [];
// 用户往上翻历史时不许拖拽跟底；滚回底部才恢复跟随
const stick = ref(true);

// 空态（金色问候 + 居中输入）的判据：出现过真正的对话内容才算开始。
// note（"新会话…""已恢复…"）不算——否则落地页永远出不来。
const hasConversation = computed(() =>
  lines.value.some((l) => l.kind === "user" || l.kind === "assistant" || l.kind === "error"),
);

const curTitle = computed(() => {
  for (const w of workspaces.value) {
    const cur = (w.Sessions ?? []).find((s) => s.Current);
    if (cur) return cur.Title;
  }
  return "新的会话";
});

function push(kind: Line["kind"], text: string, extra?: Partial<Line>) {
  lines.value.push({ kind, text, ...extra } as Line);
  toBottom();
}

function toBottom() {
  if (!stick.value) return;
  nextTick(() => {
    const el = streamEl.value;
    if (el) el.scrollTop = el.scrollHeight;
  });
}

function onScroll() {
  const el = streamEl.value;
  if (!el) return;
  stick.value = el.scrollHeight - el.scrollTop - el.clientHeight < 80;
}

function fmtErr(e: unknown): string {
  if (e instanceof Error) return e.message;
  if (typeof e === "string") return e;
  try {
    return JSON.stringify(e);
  } catch {
    return String(e);
  }
}

function firstLine(s: string): string {
  const l = s.split("\n", 1)[0] ?? "";
  return l.length > 120 ? l.slice(0, 120) + "…" : l;
}

// 审批标注在 Go 侧是一整句（如 "按需 · 按需分级: 工作区内免审批，状态根与工作区外需审批"）。
// 整句塞进 pill 会把行撑爆：pill 是 flex:none 且不换行，描述被挤成 0 宽后
// 文字溢出到 pill 上，看起来就是"内容重叠"（实测被用户当场指出）。
// → pill 只放短标签，整句留在悬停提示里。
function shortApproval(s: string): string {
  return (s || "").split(" · ")[0] || s;
}

function autoGrow() {
  const el = ta.value;
  if (!el) return;
  el.style.height = "auto";
  el.style.height = Math.min(el.scrollHeight, 200) + "px";
}

onMounted(async () => {
  // 先订事件再 Bootstrap：Bootstrap 要读配置、连 MCP，可能耗时几秒，
  // 顺序反了这期间的事件会丢。
  unsubs.push(
    Events.On("arkperf:delta", (ev) => {
      // 只渲染可见回答；思考过程只在运行条上给个字数。
      if (ev.data.Kind === "reasoning") {
        thinking.value += [...ev.data.Text].length;
        return;
      }
      thinking.value = 0;
      if (streamIdx.value < 0) {
        lines.value.push({ kind: "assistant", text: ev.data.Text } as Line);
        streamIdx.value = lines.value.length - 1;
      } else {
        const cur = lines.value[streamIdx.value];
        if (cur && cur.kind === "assistant") cur.text += ev.data.Text;
      }
      toBottom();
    }),
    Events.On("arkperf:assistant", (ev) => {
      // 流式已经把它显示出来了：这里以**完整文本**为准定稿，不再追加一行。
      // 增量与完整文本有出入时以完整文本为准，否则屏幕上留下的
      // 会和下一轮上下文里的不一样。
      const idx = streamIdx.value;
      streamIdx.value = -1;
      if (idx >= 0) {
        const cur = lines.value[idx];
        if (cur && cur.kind === "assistant") {
          if (!ev.data.trim()) {
            lines.value.splice(idx, 1); // 流出来又变空：整块撤掉，别留一行空白
          } else {
            cur.text = ev.data;
          }
          toBottom();
          return;
        }
      }
      push("assistant", ev.data);
    }),
    Events.On("arkperf:toolcall", (ev) => {
      toolCalls.value++;
      push("tool", ev.data.Args, { name: ev.data.Name });
    }),
    Events.On("arkperf:toolresult", (ev) =>
      push("result", ev.data.Output, {
        name: ev.data.Name,
        isErr: ev.data.IsError,
      }),
    ),
    Events.On("arkperf:approval:request", (ev) => {
      approval.value = {
        id: ev.data.ID,
        name: ev.data.Name,
        args: ev.data.Args,
        scope: ev.data.Scope ?? "",
      };
    }),
    // 三种情况都要说出来——尤其是"命中规则"：用户看到工具直接跑了却没被问，
    // 唯一能解释它的就是这一条。
    Events.On("arkperf:approval:rule", (ev) => {
      const d = ev.data;
      if (d.Err) {
        push("note", `规则没能保存（${d.Err}）——这次已放行，但下次还会问你`);
        return;
      }
      if (d.Saved) {
        push("note", `已记住：${d.Name} 的「${d.Scope}」以后不再询问`);
        return;
      }
      if (d.Hit) {
        push("note", `按已保存的规则放行 ${d.Name}（${d.Scope}）`);
      }
    }),
    Events.On("arkperf:approval:resolved", (ev) =>
      push("note", `${ev.data.Granted ? "已批准" : "已拒绝"}：${ev.data.Name}`),
    ),
    Events.On("arkperf:done", (ev) => {
      running.value = false;
      approval.value = null;
      streamIdx.value = -1; // 中断时不会有定稿事件，活块就地收掉（已流出的内容保留）
      thinking.value = 0;
      if (ev.data.Err) {
        push("error", `任务失败：${ev.data.Err}`);
      } else {
        push("note", `结束：${ev.data.Reason} · ${ev.data.Turns} 轮 · ${ev.data.ToolUses} 次工具调用`);
      }
      // 任务跑完必须整块刷新：这一轮结束时后端已经把会话落盘了——
      // 在新工作区里的第一次任务会让那个工作区**第一次**出现在侧栏里。
      // 不刷新的话，侧栏停在任务开始前的那份，新工作区"凭空不见了"
      // （实测：在新目录发第一条消息，树里没有它，看起来像没保存）。
      // 模型也可能写过文件，右栏文件树一并刷新。
      void refreshAll();
    }),
    // 侧栏的结构变了（重命名/移除会话）：后端推一份新的树过来，前端不用自己再拉
    Events.On("arkperf:workspaces", (ev) => {
      workspaces.value = ev.data ?? [];
    }),
  );

  try {
    boot.value = await Service.Bootstrap();
    if (boot.value.Restored) {
      push("note", `已恢复上次会话：${boot.value.Turns} 轮 · ${boot.value.Updated}`);
      // 恢复出来的历史要**立刻上屏**：只提示"已恢复"而屏幕空着，
      // 用户会以为数据丢了（TUI 端实测被问到过同样的问题）。
      // 顺序是"提示在上、历史在下"，与 TUI 的启动横幅一致。
      lines.value = [...lines.value, ...replayLines((await Service.CurrentTranscript()) ?? [])];
      stick.value = true;
    }
    await loadWorkspaces();
  } catch (e) {
    bootErr.value = fmtErr(e);
  }
});

onUnmounted(() => unsubs.forEach((f) => f()));

async function loadWorkspaces() {
  try {
    workspaces.value = (await Service.ListWorkspaces()) ?? [];
  } catch {
    workspaces.value = []; // 侧栏失败不打断主流程
  }
}

// refreshAll 是**所有**"工作区/会话变了"之后必须做的那一组刷新。
//
// 抽成一个函数是因为它被几个入口共用（点会话、点工作区、切工作区的下拉、新会话），
// 而"每个入口各自列一遍"已经漏过一次：点别的工作区里的会话同样会切工作区，
// 但那条路径当时没刷新右栏文件树——于是界面变成"当前工作区是 fwb、
// 右侧却还在列 E:\ArkPerf 的文件"（实测被用户当场抓到）。
async function refreshAll() {
  await loadWorkspaces();
  boot.value = await Service.Bootstrap();
  wsPanel.value?.load();
}

async function run() {
  const text = task.value.trim();
  if (!text || running.value) return;
  task.value = "";
  autoGrow();
  push("user", text);
  running.value = true;
  toolCalls.value = 0;
  streamIdx.value = -1; // 新任务不能接着上一轮的活块写
  thinking.value = 0;
  try {
    await Service.RunTask(text);
  } catch (e) {
    running.value = false;
    push("error", fmtErr(e));
  }
}

function onKey(e: KeyboardEvent) {
  // isComposing：中文输入法的候选确认也是 Enter，绝不能当成"发送"
  if (e.key === "Enter" && !e.shiftKey && !e.isComposing) {
    e.preventDefault();
    run();
  }
}

async function interrupt() {
  try {
    await Service.Interrupt();
    push("note", "已请求中断…");
  } catch (e) {
    push("error", fmtErr(e));
  }
}

async function answer(decision: "once" | "always" | "deny") {
  const cur = approval.value;
  if (!cur) return;
  approval.value = null;
  try {
    await Service.AnswerApproval(cur.id, decision);
  } catch (e) {
    push("error", fmtErr(e));
  }
}

async function loadTools() {
  try {
    tools.value = (await Service.ListTools()) ?? [];
    toolsLoaded.value = true;
  } catch (e) {
    push("error", fmtErr(e));
  }
}

async function checkToolchain() {
  tcLoading.value = true;
  try {
    tcReport.value = await Service.CheckToolchain();
  } catch (e) {
    tcReport.value = fmtErr(e);
  } finally {
    tcLoading.value = false;
  }
}

async function listDevices() {
  dvLoading.value = true;
  try {
    dvReport.value = await Service.ListDevices();
  } catch (e) {
    dvReport.value = fmtErr(e);
  } finally {
    dvLoading.value = false;
  }
}

async function newSession() {
  view.value = "chat"; // 从功能页点「新会话」也要回到聊天视图
  try {
    await Service.ResetSession();
    lines.value = [];
    tcReport.value = "";
    dvReport.value = "";
    push("note", "已开始新会话：模型上下文已清空");
    await refreshAll();
  } catch (e) {
    push("error", fmtErr(e));
  }
}

// 切换会话：会连带切换工作区（后端会自动跟随），所以刷新要整块做。
// 回放的转录行 → 界面行。工具调用也要回放：之前只回放 user/assistant，
// 结果"切个会话再回来，工具调用全没了"，用户看到的是残缺的对话。
function replayLines(hist: MsgLine[]): Line[] {
  return (hist ?? []).map((m) => {
    if (m.Role === "user") return { kind: "user", text: m.Content } as Line;
    if (m.Role === "tool") return { kind: "tool", text: m.Content, name: m.Name ?? "" } as Line;
    if (m.Role === "result")
      return { kind: "result", text: m.Content, name: m.Name ?? "", isErr: false } as Line;
    return { kind: "assistant", text: m.Content } as Line;
  });
}

async function onSwitch(id: string) {
  // 先切视图再等数据：点了会话就该立刻回到聊天页，
  // 历史转录加载完再填充（漏了这行就会"切了会话却还停在功能页"，实测撞过）
  view.value = "chat";
  try {
    // Go 的 nil slice 在 TS 侧是 null（绑定类型是 MsgLine[] | null），要兜 ?? []
    lines.value = replayLines((await Service.SwitchSession(id)) ?? []);
    tcReport.value = "";
    dvReport.value = "";
    stick.value = true;
    await refreshAll();
    toBottom();
  } catch (e) {
    push("error", fmtErr(e));
  }
}

// 切换工作区：换目录 → 回放该目录自己的会话 → 整块刷新。
async function onSwitchWorkspace(path: string) {
  view.value = "chat";
  try {
    lines.value = replayLines((await Service.SwitchWorkspace(path)) ?? []);
    tcReport.value = "";
    dvReport.value = "";
    stick.value = true;
    await refreshAll();
    toBottom();
  } catch (e) {
    push("error", fmtErr(e));
  }
}

// ---- 三栏拖拽 ----
function down(e: PointerEvent, which: "left" | "right") {
  resizing.value = which;
  const startX = e.clientX;
  const w0 = which === "left" ? sideW.value : wsW.value;
  const move = (ev: PointerEvent) => {
    const d = ev.clientX - startX;
    if (which === "left") {
      sideW.value = Math.min(340, Math.max(190, w0 + d));
    } else {
      wsW.value = Math.min(480, Math.max(220, w0 - d));
    }
  };
  const up = () => {
    resizing.value = "";
    window.removeEventListener("pointermove", move);
  };
  window.addEventListener("pointermove", move);
  window.addEventListener("pointerup", up, { once: true });
}

const examples = [
  "分析 com.example.app 的冷启动耗时，并给出优化建议",
  "检查工程里有没有内存泄漏的风险点",
  "跑一次构建，报告产物体积与耗时",
];

function useExample(t: string) {
  task.value = t;
  autoGrow();
  ta.value?.focus();
}
</script>

<template>
  <div class="layout" :class="{ resizing: resizing !== '' }">
    <!-- 左：功能选择栏 -->
    <aside class="side" :style="{ width: sideW + 'px' }">
      <SideBar
        :workspaces="workspaces"
        :toolchain="boot?.Toolchain ?? ''"
        :tc-found="boot?.ToolchainFound ?? 0"
        :tc-total="boot?.ToolchainTotal ?? 0"
        :running="running"
        :active="view"
        @open-session="onSwitch"
        @new-session="newSession"
        @tools="openView('tools')"
        @toolchain="openView('toolchain')"
        @devices="openView('devices')"
        @error="push('error', $event)"
      />
    </aside>
    <div class="resizer" @pointerdown="(e) => down(e, 'left')"></div>

    <!-- 中：聊天 / 功能页面（视图路由） -->
    <main class="center">
      <div v-if="bootErr" class="booterr">
        启动失败：{{ bootErr }}（检查 ~/.arkperf/config.json）
      </div>

      <!-- 空态：金色问候 + 居中输入（学 Reasonix 的落地页） -->
      <div v-if="view === 'chat' && !hasConversation" class="landing">
        <div class="greet">ArkPerf，开始今天的分析吧！</div>

        <div class="card landing-card" :class="{ running }">
          <div class="cardtop">
            <WorkspaceBar :current="boot?.CWD ?? ''" :running="running" @switch="onSwitchWorkspace" />
          </div>
          <div v-if="running" class="glowring" aria-hidden="true"><i></i></div>
          <div v-if="running" class="runstrip">
            <span class="rdot"></span>
            <span>运行中 · 已调用 {{ toolCalls }} 次工具</span>
          </div>
          <textarea
            ref="ta"
            v-model="task"
            rows="1"
            placeholder="描述任务…（Enter 发送 · Shift+Enter 换行）"
            @keydown="onKey"
            @input="autoGrow"
          ></textarea>
          <div class="crow">
            <span class="hint">Enter 发送 · Shift+Enter 换行</span>
            <button class="send" :disabled="running || !task.trim()" @click="run" title="发送">
              <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2"
                   stroke-linecap="round" stroke-linejoin="round">
                <path d="M12 19V5" /><path d="M5 12l7-7 7 7" />
              </svg>
            </button>
          </div>
        </div>
        <div v-if="boot" class="model-line">
          {{ boot.Model }} · {{ boot.ToolCount }} 工具 · {{ boot.MCP }}
        </div>
      </div>

      <!-- 会话态：转录 -->
      <template v-else-if="view === 'chat'">
        <div class="topstrip">
          <span class="sess-title" :title="boot?.CWD">{{ curTitle }}</span>
          <span class="model-tag" v-if="boot">{{ boot.Model }}</span>
        </div>

        <main ref="streamEl" class="stream" @scroll="onScroll">
          <div class="column">
            <template v-for="(l, i) in renderedLines" :key="i">
              <div v-if="l.kind === 'user'" class="node user">
                <div class="bubble">{{ l.text }}</div>
              </div>

              <div v-else-if="l.kind === 'assistant'" class="node assistant">
                <!-- head：一次任务只在第 1 段回答上挂标签 -->
                <div class="meta" v-if="l.head"><span class="adot"></span>方舟智诊</div>
                <!-- 模型输出是 markdown；v-html 的内容已经过 DOMPurify 消毒 -->
                <div class="md" v-html="md(l.text)"></div>
              </div>

              <!-- 工具块：整组一个折叠头（学 Reasonix）。运行中最后一组自动展开看进度，
                   结束后收起；单行仍各自可折叠，两层互不干扰。 -->
              <details
                v-else-if="l.kind === 'toolgroup'"
                class="toolgroup"
                :open="groupOpen(l.key)"
              >
                <summary @click.prevent="toggleGroup(l.key)">
                  <span class="tgcount">{{ l.items.length }} 次工具调用</span>
                  <span class="tgfail" v-if="failedCount(l.items)">
                    {{ failedCount(l.items) }} 次失败
                  </span>
                </summary>

                <div class="tgbody">
                  <template v-for="(it, k) in l.items" :key="k">
                    <!-- 单行折叠：与原来的行为完全一致，只是被组包起来了 -->
                    <details class="toolrow">
                      <summary>
                        <span class="tname">{{ it.name }}</span>
                        <span class="targs">{{ firstLine(it.args) }}</span>
                      </summary>
                      <pre class="tbody">{{ it.args }}</pre>
                    </details>

                    <details
                      v-if="it.result"
                      class="toolres"
                      :class="{ iserr: it.result.isErr }"
                      :open="it.result.content.length <= 240"
                    >
                      <summary>
                        <span class="mark">{{ it.result.isErr ? "✗" : "✓" }}</span>
                        <span class="tname">{{ it.name }}</span>
                        <span class="targs">{{ firstLine(it.result.content) }}</span>
                      </summary>
                      <pre class="tbody">{{ it.result.content }}</pre>
                    </details>
                  </template>
                </div>
              </details>

              <div v-else-if="l.kind === 'note'" class="note">{{ l.text }}</div>
              <div v-else-if="l.kind === 'error'" class="errline">{{ l.text }}</div>
            </template>
          </div>
        </main>

        <section v-if="approval" class="approval">
          <div class="approval-title">需要批准 · {{ approval.name }}</div>
          <div class="approval-args">{{ approval.args }}</div>
          <!-- 类别必须原样显示：只给工具名的话，用户会把"以后都不问"
               理解成"永远允许这个工具"，而实际范围可能只是某个目录。 -->
          <div v-if="approval.scope" class="approval-scope">类别：{{ approval.scope }}</div>
          <div class="approval-actions">
            <button class="primary" @click="answer('once')">允许一次</button>
            <button v-if="approval.scope" class="ghost" @click="answer('always')">这一类以后都不问</button>
            <button class="ghost" @click="answer('deny')">拒绝</button>
          </div>
        </section>

        <footer class="composer">
          <div class="card" :class="{ running }">
            <div class="cardtop">
              <WorkspaceBar :current="boot?.CWD ?? ''" :running="running" @switch="onSwitchWorkspace" />
            </div>
            <div v-if="running" class="glowring" aria-hidden="true"><i></i></div>
            <div v-if="running" class="runstrip">
              <span class="rdot"></span>
              <span>运行中 · 已调用 {{ toolCalls }} 次工具</span>
              <span v-if="thinking > 0" class="thinkdim">思考中 {{ thinking }} 字</span>
              <button class="linkbtn" @click="interrupt">中断</button>
            </div>
            <textarea
              ref="ta"
              v-model="task"
              rows="1"
              placeholder="描述任务…（Enter 发送 · Shift+Enter 换行）"
              @keydown="onKey"
              @input="autoGrow"
            ></textarea>
            <div class="crow">
              <span class="hint">Enter 发送 · Shift+Enter 换行</span>
              <button class="send" :disabled="running || !task.trim()" @click="run" title="发送">
                <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2"
                     stroke-linecap="round" stroke-linejoin="round">
                  <path d="M12 19V5" /><path d="M5 12l7-7 7 7" />
                </svg>
              </button>
            </div>
          </div>
        </footer>
      </template>

      <!-- 功能页：工具清单 -->
      <div v-else-if="view === 'tools'" class="page">
        <div class="page-inner">
          <div class="page-head">
            <div>
              <div class="page-title">工具清单</div>
              <div class="page-sub">{{ filteredTools.length }} / {{ tools.length }} 个已注册工具 · 审批标注与命令行 arkperf tools 一致</div>
            </div>
            <input v-model="toolFilter" class="search" placeholder="搜索工具…" />
          </div>
          <div class="tool-list">
            <div
              v-for="t in filteredTools"
              :key="t.Name"
              class="tool-line"
              :title="t.Description + '\n\n审批：' + t.Approval"
            >
              <span class="tname">{{ t.Name }}</span>
              <span class="pill" :class="{ 'pill-accent': t.Approval === '需审批' }">
                {{ shortApproval(t.Approval) }}
              </span>
              <span class="tdesc">{{ t.Description }}</span>
            </div>
            <div v-if="filteredTools.length === 0" class="empty-sub">没有匹配的工具</div>
          </div>
        </div>
      </div>

      <!-- 功能页：工具链探测 -->
      <div v-else-if="view === 'toolchain'" class="page">
        <div class="page-inner">
          <div class="page-head">
            <div>
              <div class="page-title">工具链探测</div>
              <div class="page-sub">hdc / hvigorw / ohpm / node / java 与 DevEco 安装位置</div>
            </div>
            <button class="tbtn" :disabled="tcLoading" @click="checkToolchain">
              {{ tcLoading ? "探测中…" : "重新探测" }}
            </button>
          </div>
            <pre class="page-pre">{{ tcLoading ? "探测中…" : tcReport }}</pre>
        </div>
      </div>

      <!-- 功能页：设备列表 -->
      <div v-else-if="view === 'devices'" class="page">
        <div class="page-inner">
          <div class="page-head">
            <div>
              <div class="page-title">设备列表</div>
              <div class="page-sub">通过 hdc list targets 查询，只读操作</div>
            </div>
            <button class="tbtn" :disabled="dvLoading" @click="listDevices">
              {{ dvLoading ? "查询中…" : "刷新" }}
            </button>
          </div>
            <pre class="page-pre">{{ dvLoading ? "查询中…" : dvReport }}</pre>
        </div>
      </div>
    </main>

    <div class="resizer" @pointerdown="(e) => down(e, 'right')"></div>

    <!-- 右：工作区文件 -->
    <aside class="workspace" :style="{ width: wsW + 'px' }">
      <WorkspacePanel ref="wsPanel" :cwd="boot?.CWD ?? ''" @open-file="onOpenFile" />
    </aside>
  </div>
</template>

<style scoped>
.layout {
  display: flex;
  height: 100vh;
  min-width: 0;
  background: var(--bg);
}
.layout.resizing,
.layout.resizing * {
  user-select: none !important;
  cursor: col-resize !important;
}

/* ---- 左：功能选择栏 ---- */
.side {
  flex: none;
  width: 210px;
  min-width: 0;
  overflow: hidden;
  background: var(--sidebar-bg);
  border-right: 1px solid var(--border-soft);
}

/* ---- 分隔条 ---- */
.resizer {
  flex: none;
  width: 5px;
  cursor: col-resize;
  background: transparent;
  transition: background-color 0.12s;
}
.resizer:hover,
.resizing .resizer {
  background: var(--accent-soft);
}

/* ---- 中 ---- */
.center {
  flex: 1;
  min-width: 0;
  display: flex;
  flex-direction: column;
}

.topstrip {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 9px 18px;
  border-bottom: 1px solid var(--border-soft);
}
.sess-title {
  font-size: var(--text-md);
  color: var(--fg-dim);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.model-tag { font-size: var(--text-xs); color: var(--fg-faint); flex: none; }

.booterr {
  margin: 10px 16px 0;
  padding: 10px 12px;
  border: 1px solid color-mix(in srgb, var(--err) 45%, var(--border));
  border-radius: var(--radius-row);
  color: var(--err);
  background: color-mix(in srgb, var(--err) 8%, var(--bg-elev));
}

/* ---- 空态（落地页） ---- */
.landing {
  flex: 1;
  min-height: 0;
  overflow-y: auto;
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: 26px;
  padding: 24px;
}
.greet {
  font-size: 30px;
  font-weight: 700;
  background: var(--grad-greet);
  -webkit-background-clip: text;
  background-clip: text;
  color: transparent;
  text-align: center;
  letter-spacing: 0.01em;
}
.model-line { font-size: var(--text-xs); color: var(--fg-faint); margin-top: -14px; }

/* ---- 转录 ---- */
.stream {
  flex: 1;
  min-height: 0;
  overflow-y: auto;
  overflow-x: hidden;
  padding: 24px;
}
.column {
  max-width: 800px;
  margin: 0 auto;
  display: flex;
  flex-direction: column;
  gap: 16px;
}

.node.user { display: flex; justify-content: flex-end; }
.bubble {
  max-width: 78%;
  padding: 10px 16px;
  border-radius: var(--radius-bubble);
  background: var(--bubble-user-bg);
  border: 1px solid var(--bubble-user-border);
  white-space: pre-wrap;
  word-break: break-word;
}

.node.assistant .meta {
  display: flex;
  align-items: center;
  gap: 6px;
  font-size: var(--text-xs);
  color: var(--fg-faint);
  margin-bottom: 4px;
}
.adot { width: 7px; height: 7px; border-radius: 50%; background: var(--accent); }

/* markdown（v-html 的内容，scoped 下必须 :deep()） */
.md { color: var(--fg); min-width: 0; }
.md :deep(p) { margin: 0 0 10px; }
.md :deep(p:last-child) { margin-bottom: 0; }
.md :deep(ul), .md :deep(ol) { margin: 0 0 10px; padding-left: 22px; }
.md :deep(li) { margin-bottom: 2px; }
.md :deep(h1), .md :deep(h2), .md :deep(h3), .md :deep(h4) { font-weight: 600; margin: 14px 0 8px; }
.md :deep(h1) { font-size: var(--text-lg); }
.md :deep(h2) { font-size: var(--text-base); }
.md :deep(h3) { font-size: var(--text-md); }
.md :deep(code) {
  font-family: var(--font-mono);
  font-size: 0.875em;
  background: var(--bg-soft);
  border: 1px solid var(--border-soft);
  border-radius: 4px;
  padding: 1px 5px;
}
.md :deep(pre) {
  background: var(--bg-soft);
  border: 1px solid var(--border-soft);
  border-radius: var(--radius-row);
  padding: 10px 12px;
  overflow-x: auto;
  margin: 0 0 10px;
}
.md :deep(pre code) { background: none; border: 0; padding: 0; font-size: var(--text-sm); }
.md :deep(a) { color: var(--link); }
.md :deep(blockquote) {
  margin: 0 0 10px;
  padding: 2px 12px;
  border-left: 3px solid var(--border);
  color: var(--fg-dim);
}
.md :deep(table) { border-collapse: collapse; margin: 0 0 10px; font-size: var(--text-sm); }
.md :deep(th), .md :deep(td) { border: 1px solid var(--border-soft); padding: 4px 10px; text-align: left; }
.md :deep(th) { color: var(--fg-dim); background: var(--bg-soft); }
.md :deep(hr) { border: 0; border-top: 1px solid var(--border-soft); margin: 12px 0; }

/* ---- 工具行（可折叠） ---- */
/* ---- 工具组折叠头 ---- */
.toolgroup {
  border: 1px solid var(--border-soft);
  border-radius: var(--radius-row);
  background: var(--bg-elev-2);
  overflow: hidden;
  min-width: 0;
}
.toolgroup > summary {
  cursor: pointer;
  list-style: none;
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 6px 10px;
  font-size: var(--text-sm);
  color: var(--fg-dim);
}
.toolgroup > summary::-webkit-details-marker { display: none; }
.toolgroup > summary::before {
  content: "▸";
  color: var(--fg-faint);
  transition: transform 0.12s;
  flex: none;
}
details[open].toolgroup > summary::before { transform: rotate(90deg); }
.toolgroup > summary:hover { color: var(--fg); }
.tgcount { font-variant-numeric: tabular-nums; }
.tgfail { color: var(--err); font-size: var(--text-xs); }
/* 组体：比组头缩进一点，视觉上"属于这个组" */
.tgbody { display: flex; flex-direction: column; gap: 4px; padding: 2px 8px 8px 14px; }

/* ---- 单行折叠（保持原样，只是住进了组里） ---- */
.toolrow, .toolres {
  border: 1px solid var(--border-soft);
  border-radius: var(--radius-row);
  background: var(--bg-soft);
  overflow: hidden;
  min-width: 0;
}
.toolrow summary, .toolres summary {
  cursor: pointer;
  list-style: none;
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 7px 10px;
  font-size: var(--text-sm);
  color: var(--fg-dim);
  min-width: 0;
}
.toolrow summary::-webkit-details-marker,
.toolres summary::-webkit-details-marker { display: none; }
.toolrow summary::before, .toolres summary::before {
  content: "▸";
  color: var(--fg-faint);
  transition: transform 0.12s;
  flex: none;
}
details[open] > summary::before { transform: rotate(90deg); }
.toolrow summary:hover, .toolres summary:hover { background: var(--bg-elev); }
.tname { color: var(--fg); flex: none; }
.targs {
  font-family: var(--font-mono);
  font-size: var(--text-xs);
  color: var(--fg-faint);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  min-width: 0;
}
.tbody {
  margin: 0;
  padding: 8px 12px;
  border-top: 1px solid var(--border-soft);
  white-space: pre-wrap;
  overflow-wrap: anywhere;
  font-family: var(--font-mono);
  font-size: var(--text-xs);
  color: var(--fg-dim);
  max-height: 320px;
  overflow-y: auto;
}
.toolres .mark { flex: none; font-weight: 600; }
.toolres:not(.iserr) .mark { color: var(--ok); }
.toolres.iserr .mark, .toolres.iserr .tname { color: var(--err); }
.toolres.iserr { border-color: color-mix(in srgb, var(--err) 35%, var(--border-soft)); }

.note { font-size: var(--text-sm); color: var(--fg-faint); }
.errline { font-size: var(--text-sm); color: var(--err); }

/* ---- 空态里的示例 ---- */
.empty { padding: 8px 0 0; text-align: center; }
.empty-sub { opacity: 0.5; font-size: var(--text-sm); }
.chips { display: flex; gap: 8px; justify-content: center; margin-top: 12px; flex-wrap: wrap; }
.chip {
  border: 1px solid var(--border-soft);
  background: var(--bg-elev);
  border-radius: 999px;
  padding: 6px 14px;
  font-size: var(--text-sm);
  color: var(--fg-dim);
  cursor: pointer;
  transition: border-color 80ms ease, color 80ms ease;
}
.chip:hover { border-color: var(--accent); color: var(--fg); }

/* ---- 审批卡 ---- */
.approval {
  margin: 0 auto 10px;
  width: min(800px, calc(100% - 48px));
  border: 1px solid color-mix(in srgb, var(--warn) 45%, var(--border));
  background: color-mix(in srgb, var(--warn) 8%, var(--bg-elev));
  border-radius: var(--radius-card);
  padding: 12px 14px;
}
.approval-title { font-weight: 500; color: var(--warn); }
.approval-args {
  font-family: var(--font-mono);
  font-size: var(--text-xs);
  margin: 8px 0 10px;
  word-break: break-all;
  opacity: 0.9;
}
/* "这一类"要一眼能和参数区分开：它是用户决定要不要按第三个按钮的依据 */
.approval-scope {
  font-size: var(--text-xs);
  color: var(--fg-dim);
  margin: -4px 0 10px;
  word-break: break-all;
}
.approval-actions { display: flex; gap: 8px; flex-wrap: wrap; }
.approval-actions .primary {
  background: var(--accent);
  color: var(--accent-fg);
  border: 0;
  border-radius: var(--radius-row);
  height: 30px;
  padding: 0 16px;
  font-weight: 500;
  cursor: pointer;
}
.approval-actions .ghost {
  background: transparent;
  color: var(--fg-dim);
  border: 1px solid var(--border);
  border-radius: var(--radius-row);
  height: 30px;
  padding: 0 16px;
  cursor: pointer;
}

/* ---- 面板（工具清单 / 报告） ---- */
.panel, .report {
  margin: 0 auto 10px;
  width: min(800px, calc(100% - 48px));
  border: 1px solid var(--border);
  border-radius: var(--radius-card);
  background: var(--bg-elev);
  overflow: hidden;
}
.panel-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 8px 12px;
  border-bottom: 1px solid var(--border-soft);
  font-size: var(--text-sm);
  color: var(--fg-dim);
}
.x {
  border: 0;
  background: transparent;
  color: var(--fg-faint);
  cursor: pointer;
  font-size: var(--text-sm);
}
.x:hover { color: var(--fg); }
.panel-body { max-height: 40vh; overflow: auto; padding: 6px 10px; }
.trow { display: flex; align-items: baseline; gap: 8px; padding: 5px 2px; border-bottom: 1px solid var(--border-soft); }
.trow:last-child { border-bottom: 0; }
.tdesc { opacity: 0.72; font-size: var(--text-xs); min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.report {
  margin: 0;
  padding: 12px 14px 14px;
  white-space: pre-wrap;
  overflow-wrap: anywhere;
  font-family: var(--font-mono);
  font-size: var(--text-sm);
  max-height: 45vh;
  overflow-y: auto;
  position: relative;
}
.x-report { position: absolute; right: 10px; top: 8px; }

/* ---- 输入区 ---- */
.composer { padding: 0 24px 14px; }
.card {
  position: relative;
  max-width: 800px;
  margin: 0 auto;
  border: 1px solid var(--border);
  border-radius: var(--radius-card);
  background: var(--bg-elev);
  box-shadow: var(--shadow-card);
  transition: border-color 0.12s;
}
.card:focus-within { border-color: var(--fg-faint); }
.card.running { border-color: color-mix(in srgb, var(--accent) 45%, var(--border)); }

/* 输入卡的第一行：工作区选择器（学 Reasonix 把工作区放在 composer 顶部） */
.cardtop {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 7px 10px 0;
  min-width: 0;
}

/* 空态：输入框变紧凑居中（学 Reasonix 落地页的输入卡）。
   注意 .card.landing-card 是 .landing（flex 列 + align-items:center）的直接子元素，
   **必须给显式宽度**：flex 会把这类子元素收缩到内容宽，而里面的 textarea
   又是 width:100%（相对卡片），互相依赖会塌缩成 ~130px（实测踩过）。 */
.card.landing-card {
  width: min(480px, 100%);
}

textarea {
  display: block;
  width: 100%;
  box-sizing: border-box;
  background: transparent;
  border: 0;
  outline: 0;
  resize: none;
  color: var(--fg);
  font: var(--text-base)/1.6 var(--font-ui);
  padding: 12px 14px 4px;
  min-height: 44px;
}
textarea::placeholder { color: var(--fg-faint); }

.crow {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 6px 12px 10px;
}
.hint { font-size: var(--text-xs); color: var(--fg-faint); }
.send {
  flex: none;
  width: 32px;
  height: 32px;
  border-radius: 50%;
  border: 0;
  background: var(--accent);
  color: var(--accent-fg);
  cursor: pointer;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  transition: background-color 80ms ease;
}
.send svg { width: 16px; height: 16px; }
.send:hover:not(:disabled) { background: var(--accent-strong); }
.send:disabled { opacity: 0.4; cursor: default; }

/* 运行条 + 呼吸点 */
.runstrip {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 9px 14px 0;
  font-size: var(--text-sm);
  color: var(--fg-dim);
}
.rdot {
  width: 8px;
  height: 8px;
  border-radius: 50%;
  background: var(--accent);
  animation: rdot-pulse 1.2s ease-in-out infinite;
  flex: none;
}
@keyframes rdot-pulse {
  0%, 100% { opacity: 0.35; }
  50% { opacity: 1; }
}
/* 思考字数：流式刚开始的那几秒屏幕没有别的变化，得让人知道"它在想" */
.thinkdim {
  color: var(--fg-dim);
  opacity: 0.75;
  font-variant-numeric: tabular-nums;
}
.linkbtn {
  margin-left: auto;
  border: 0;
  background: transparent;
  color: var(--err);
  cursor: pointer;
  font-size: var(--text-sm);
  padding: 2px 6px;
}
.linkbtn:hover { text-decoration: underline; }

/* 辉光环（学自 Reasonix 的 composer-glowring：任务运行时沿边框走一圈光） */
.glowring {
  position: absolute;
  inset: -1px;
  border-radius: inherit;
  padding: 1.5px;
  pointer-events: none;
  overflow: hidden;
  -webkit-mask: linear-gradient(#000 0 0) content-box, linear-gradient(#000 0 0);
  -webkit-mask-composite: xor;
  mask: linear-gradient(#000 0 0) content-box, linear-gradient(#000 0 0);
  mask-composite: exclude;
}
.glowring i {
  position: absolute;
  width: 150px;
  height: 150px;
  background: radial-gradient(
    circle closest-side,
    var(--accent) 0%,
    color-mix(in srgb, var(--accent) 42%, transparent) 38%,
    transparent 72%
  );
  offset-path: border-box;
  animation: glowtrace 3.4s linear infinite;
}
@keyframes glowtrace {
  to { offset-distance: 100%; }
}
@media (prefers-reduced-motion: reduce) {
  .glowring { background: color-mix(in srgb, var(--accent) 55%, transparent); }
  .glowring i { display: none; animation: none; }
  .rdot { animation: none; opacity: 0.8; }
}

/* ---- 右：工作区 ---- */
.workspace {
  flex: none;
  width: 250px;
  min-width: 0;
  overflow: hidden;
  background: var(--sidebar-bg);
  border-left: 1px solid var(--border-soft);
}

/* ---- 功能页（工具清单 / 工具链 / 设备） ---- */
.page {
  flex: 1;
  min-height: 0;
  overflow-y: auto;
  padding: 28px 32px;
}
.page-inner { max-width: 760px; margin: 0 auto; }
.page-head {
  display: flex;
  align-items: flex-end;
  justify-content: space-between;
  gap: 16px;
  margin-bottom: 18px;
}
.page-title { font-size: 17px; font-weight: 600; }
.page-sub { font-size: var(--text-sm); color: var(--fg-faint); margin-top: 4px; }
.search {
  width: 240px;
  padding: 6px 10px;
  border: 1px solid var(--border-soft);
  border-radius: var(--radius-row);
  background: var(--bg);
  color: var(--fg);
  font-size: var(--text-sm);
  outline: none;
}
.search:focus { border-color: var(--fg-faint); }
.search::placeholder { color: var(--fg-faint); }
.tool-list { border-top: 1px solid var(--border-soft); }
.tool-line {
  display: flex;
  align-items: baseline;
  gap: 12px;
  padding: 10px 2px;
  border-bottom: 1px solid var(--border-soft);
}
.tool-line:last-child { border-bottom: 0; }
.tool-line .tname {
  font-family: var(--font-mono);
  font-size: var(--text-sm);
  flex: none;
  width: 210px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
/* pill 限宽且不换行：它承载的是短标签，整句在 title 里 */
.tool-line .pill {
  flex: none;
  max-width: 110px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
/* 描述吃掉剩余宽度并**允许换行**：工具说明是这一页的主要信息，
  截成一行反而难读；换行后行高变高，也不会再溢出到 pill 上 */
.tool-line .tdesc {
  flex: 1;
  min-width: 0;
  opacity: 0.72;
  font-size: var(--text-xs);
  line-height: 1.5;
  overflow-wrap: anywhere;
}
.page-pre {
  margin: 0;
  padding: 14px 16px;
  white-space: pre-wrap;
  overflow-wrap: anywhere;
  font-family: var(--font-mono);
  font-size: var(--text-sm);
  color: var(--fg-dim);
  background: var(--bg-soft);
  border: 1px solid var(--border-soft);
  border-radius: var(--radius-row);
}
.page .tbtn {
  height: 30px;
  padding: 0 14px;
  border: 1px solid var(--border);
  border-radius: var(--radius-row);
  background: var(--bg-elev-2);
  color: var(--fg-dim);
  cursor: pointer;
  font-size: var(--text-sm);
  flex: none;
}
.page .tbtn:hover:not(:disabled) {
  color: var(--fg);
  border-color: color-mix(in srgb, var(--fg-faint) 48%, var(--border));
}
.page .tbtn:disabled { opacity: 0.5; cursor: default; }
</style>
