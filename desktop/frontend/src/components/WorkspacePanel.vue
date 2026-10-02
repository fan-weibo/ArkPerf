<script setup lang="ts">
import { ref, computed, onMounted } from "vue";
import hljs from "highlight.js/lib/core";
import "highlight.js/styles/github-dark.css";
// 只注册我们真会打开的语言：全量包一秒几百 KB，这份按需注册小得多。
// 漏了的语言不会报错——走"不高亮"的纯文本分支，只是没颜色。
import langBash from "highlight.js/lib/languages/bash";
import langCpp from "highlight.js/lib/languages/cpp";
import langCss from "highlight.js/lib/languages/css";
import langGo from "highlight.js/lib/languages/go";
import langIni from "highlight.js/lib/languages/ini";
import langJava from "highlight.js/lib/languages/java";
import langJavascript from "highlight.js/lib/languages/javascript";
import langJson from "highlight.js/lib/languages/json";
import langMarkdown from "highlight.js/lib/languages/markdown";
import langPython from "highlight.js/lib/languages/python";
import langTypescript from "highlight.js/lib/languages/typescript";
import langXml from "highlight.js/lib/languages/xml";
import langYaml from "highlight.js/lib/languages/yaml";
import {
  Service,
  type WorkspaceInfo,
  type WsEntry,
} from "../../bindings/github.com/fan-weibo/ArkPerf/desktop";

hljs.registerLanguage("bash", langBash);
hljs.registerLanguage("cpp", langCpp);
hljs.registerLanguage("css", langCss);
hljs.registerLanguage("go", langGo);
hljs.registerLanguage("ini", langIni);
hljs.registerLanguage("java", langJava);
hljs.registerLanguage("javascript", langJavascript);
hljs.registerLanguage("json", langJson);
hljs.registerLanguage("markdown", langMarkdown);
hljs.registerLanguage("python", langPython);
hljs.registerLanguage("typescript", langTypescript);
hljs.registerLanguage("xml", langXml);
hljs.registerLanguage("yaml", langYaml);

// 扩展名 → 语言。鸿蒙工程里的 .ets 就是 ArkTS，按 TypeScript 高亮最接近。
const LANG_BY_EXT: Record<string, string> = {
  go: "go",
  py: "python",
  ts: "typescript",
  tsx: "typescript",
  ets: "typescript",
  js: "javascript",
  mjs: "javascript",
  cjs: "javascript",
  json: "json",
  json5: "json",
  html: "xml",
  htm: "xml",
  xml: "xml",
  svg: "xml",
  css: "css",
  scss: "css",
  less: "css",
  yaml: "yaml",
  yml: "yaml",
  toml: "ini",
  ini: "ini",
  conf: "ini",
  properties: "ini",
  sh: "bash",
  bash: "bash",
  ps1: "bash",
  bat: "bash",
  md: "markdown",
  markdown: "markdown",
  c: "cpp",
  h: "cpp",
  cpp: "cpp",
  cc: "cpp",
  hpp: "cpp",
  java: "java",
};

function langFor(name: string): string {
  const lower = name.toLowerCase();
  // 无扩展名的常见文件按名字认
  if (lower === "dockerfile") return "bash";
  if (lower === "makefile") return "bash";
  const dot = lower.lastIndexOf(".");
  if (dot < 0) return "";
  return LANG_BY_EXT[lower.slice(dot + 1)] ?? "";
}

defineProps<{ cwd: string }>();
const emit = defineEmits<{ (e: "open-file"): void }>();

// 面板有两种内容，用标签页切换（学 WorkBuddy 的右侧文件查看器）：
//   · 「文件」标签是文件列表，常驻、不可关闭；
//   · 打开的文件各占一个标签，可关闭，可同时开多个。
// 早先的版本把文件内容挤在列表下方的一小段 <pre> 里：折行、无行号、
// 只有 240px 宽——代码基本没法看。
type Tab = {
  path: string;
  name: string;
  content: string;
  err: string;
};

const LIST = -1; // 「文件」标签的下标约定

const ws = ref<WorkspaceInfo | null>(null);
const path = ref("");
const entries = ref<WsEntry[]>([]);
const filter = ref("");
const err = ref("");

const tabs = ref<Tab[]>([]);
const active = ref<number>(LIST);

const shown = computed(() => {
  const q = filter.value.trim().toLowerCase();
  if (!q) return entries.value;
  return entries.value.filter((e) => e.Name.toLowerCase().includes(q));
});

const curTab = computed<Tab | null>(() =>
  active.value >= 0 ? (tabs.value[active.value] ?? null) : null,
);
const lines = computed(() => (curTab.value ? curTab.value.content.split("\n") : []));
// 行号用"同样行高的一列数字"，与代码各自成列——比逐行 flex 简单，对齐也可靠
const gutter = computed(() => lines.value.map((_, i) => i + 1).join("\n"));

// 行数上限：再大就不高亮（高亮是同步的，几万行会把界面卡住）。
// 宁可没颜色，也不能打开一个文件就卡住——大文件本来就是翻着看，不是读代码。
const MAX_HIGHLIGHT_LINES = 3000;

const highlighted = computed(() => {
  const t = curTab.value;
  if (!t || t.err || !t.content) return "";
  if (lines.value.length > MAX_HIGHLIGHT_LINES) return "";
  const lang = langFor(t.name);
  if (!lang || !hljs.getLanguage(lang)) return "";
  try {
    // ignoreIllegals：语法有残缺的配置文件也照常高亮，不抛错
    return hljs.highlight(t.content, { language: lang, ignoreIllegals: true }).value;
  } catch {
    return ""; // 高亮失败就退回纯文本，不影响读文件
  }
});

function fmtSize(n: number): string {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  return `${(n / 1024 / 1024).toFixed(1)} MB`;
}

function join(base: string, name: string): string {
  return base.replace(/[\\/]+$/, "") + "\\" + name;
}
function parent(p: string): string {
  const cut = p.replace(/[\\/]+$/, "");
  const i = Math.max(cut.lastIndexOf("\\"), cut.lastIndexOf("/"));
  return i <= 2 ? cut : cut.slice(0, i);
}
function baseName(p: string): string {
  const cut = p.replace(/[\\/]+$/, "");
  const i = Math.max(cut.lastIndexOf("\\"), cut.lastIndexOf("/"));
  return i === -1 ? cut : cut.slice(i + 1);
}

onMounted(load);

async function load() {
  err.value = "";
  tabs.value = [];
  active.value = LIST;
  try {
    ws.value = await Service.Workspace();
    path.value = ws.value.Root;
    await loadDir();
  } catch (e) {
    err.value = String(e);
  }
}

async function loadDir() {
  err.value = "";
  try {
    entries.value = (await Service.ListDir(path.value)) ?? [];
  } catch (e) {
    err.value = String(e);
  }
}

async function open(e: WsEntry) {
  err.value = "";
  const p = join(path.value, e.Name);
  if (e.Dir) {
    path.value = p;
    filter.value = "";
    await loadDir();
    active.value = LIST;
    return;
  }

  // 已经开过就直接切过去，不重复读盘
  const idx = tabs.value.findIndex((t) => t.path === p);
  if (idx >= 0) {
    active.value = idx;
    return;
  }

  const tab: Tab = { path: p, name: e.Name, content: "", err: "" };
  tabs.value = [...tabs.value, tab];
  const idxNew = tabs.value.length - 1;
  active.value = idxNew;
  emit("open-file"); // 让外层把右栏放宽一点：代码在 240px 里没法看

  // **必须写回 tabs.value[idxNew]，不能写局部变量 tab**：
  // 对象进数组后，视图读的是 Vue 代理；改原始对象数据会变但**不会触发重渲染**，
  // 表现就是"标签开了、路径对、代码区一片空白"（实测被用户当场抓到）。
  try {
    tabs.value[idxNew].content = await Service.ReadTextFile(p);
  } catch (ex) {
    tabs.value[idxNew].err = String(ex);
  }
}

function closeTab(i: number) {
  tabs.value = tabs.value.filter((_, idx) => idx !== i);
  if (active.value === i) active.value = LIST;
  else if (active.value > i) active.value -= 1;
}

async function up() {
  path.value = parent(path.value);
  filter.value = "";
  await loadDir();
}

defineExpose({ load });
</script>

<template>
  <div class="ws-root">
    <!-- 标签栏：「文件」常驻，打开的文件各一个标签 -->
    <div class="tabs">
      <button class="tab list-tab" :class="{ on: active === LIST }" @click="active = LIST">
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"
             stroke-linecap="round" stroke-linejoin="round">
          <path d="M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2Z" />
        </svg>
        文件
      </button>
      <button
        v-for="(t, i) in tabs"
        :key="t.path"
        class="tab"
        :class="{ on: active === i }"
        :title="t.path"
        @click="active = i"
      >
        <span class="tname">{{ t.name }}</span>
        <span class="x" title="关闭" @click.stop="closeTab(i)">✕</span>
      </button>
    </div>

    <!-- 文件列表 -->
    <template v-if="active === LIST">
      <input v-model="filter" class="search" placeholder="搜索文件…" />

      <div v-if="ws" class="meta">
        <div v-if="ws.IsProject" class="proj">
          <span class="proj-badge">OpenHarmony 工程</span>
          <span v-if="(ws.Modules ?? []).length" class="proj-modules">
            模块：{{ (ws.Modules ?? []).join(" / ") }}
          </span>
        </div>
        <div v-else class="proj">
          <span class="proj-badge dim">当前目录不是 OpenHarmony 工程</span>
        </div>
        <div class="path" :title="path">{{ path }}</div>
      </div>

      <div class="nav">
        <button class="up" :disabled="path === (ws?.Root ?? '')" @click="up">← 上一级</button>
        <span class="here">{{ baseName(path) }}</span>
      </div>

      <div class="list">
        <button v-for="e in shown" :key="e.Name" class="frow" @click="open(e)">
          <span class="icon">{{ e.Dir ? "▸" : "·" }}</span>
          <span class="fname">{{ e.Name }}</span>
          <span class="fsize">{{ e.Dir ? "" : fmtSize(e.Size) }}</span>
        </button>
        <div v-if="shown.length === 0 && !err" class="empty">（没有匹配的文件）</div>
        <div v-if="err" class="werr">{{ err }}</div>
      </div>
    </template>

    <!-- 文件内容：行号 + 不折行的代码区 -->
    <template v-else-if="curTab">
      <div class="filepath" :title="curTab.path">{{ curTab.path }}</div>
      <div v-if="curTab.err" class="werr">{{ curTab.err }}</div>
      <div v-else class="code">
        <pre class="gutter">{{ gutter }}</pre>
        <!-- v-html 的内容由 highlight.js 生成：它自己会转义源码，不引入 XSS -->
        <pre v-if="highlighted" class="src hljs" v-html="highlighted"></pre>
        <pre v-else class="src">{{ curTab.content }}</pre>
      </div>
    </template>
  </div>
</template>

<style scoped>
.ws-root {
  display: flex;
  flex-direction: column;
  height: 100%;
  min-height: 0;
  background: var(--sidebar-bg);
}

/* ---- 标签栏 ---- */
.tabs {
  display: flex;
  align-items: stretch;
  gap: 1px;
  border-bottom: 1px solid var(--border-soft);
  overflow-x: auto;
  flex: none;
  background: var(--bg-soft);
}
.tab {
  display: flex;
  align-items: center;
  gap: 6px;
  flex: none;
  max-width: 180px;
  padding: 6px 10px;
  border: 0;
  border-right: 1px solid var(--border-soft);
  background: transparent;
  color: var(--fg-faint);
  cursor: pointer;
  font-size: var(--text-xs);
}
.tab:hover { color: var(--fg); background: var(--bg-elev-2); }
.tab.on {
  color: var(--fg);
  background: var(--sidebar-bg);
  box-shadow: inset 0 -2px 0 var(--accent);
}
.tab svg { width: 13px; height: 13px; flex: none; }
.list-tab { color: var(--fg-dim); }
.tname { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.x {
  flex: none;
  width: 14px;
  height: 14px;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  border-radius: 3px;
  font-size: 10px;
  opacity: 0.6;
}
.x:hover { background: var(--bg-soft); opacity: 1; color: var(--err); }

/* ---- 列表 ---- */
.search {
  margin: 8px 8px 0;
  padding: 5px 9px;
  border: 1px solid var(--border-soft);
  border-radius: var(--radius-row);
  background: var(--bg);
  color: var(--fg);
  font-size: var(--text-sm);
  outline: none;
}
.search:focus { border-color: var(--fg-faint); }
.search::placeholder { color: var(--fg-faint); }

.meta { display: flex; flex-direction: column; gap: 4px; padding: 8px 10px 0; }
.proj { display: flex; flex-direction: column; gap: 3px; }
.proj-badge {
  align-self: flex-start;
  font-size: var(--text-xs);
  color: var(--accent);
  border: 1px solid color-mix(in srgb, var(--accent) 32%, var(--border));
  background: var(--accent-soft);
  border-radius: 999px;
  padding: 2px 9px;
}
.proj-badge.dim { color: var(--fg-faint); border-color: var(--border-soft); background: transparent; }
.proj-modules { font-size: var(--text-xs); color: var(--fg-dim); }
.path {
  font-family: var(--font-mono);
  font-size: 11px;
  color: var(--fg-faint);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.nav { display: flex; align-items: center; gap: 8px; padding: 8px 10px 6px; }
.up {
  border: 1px solid var(--border-soft);
  background: transparent;
  color: var(--fg-dim);
  border-radius: 6px;
  padding: 3px 9px;
  cursor: pointer;
  font-size: var(--text-xs);
}
.up:hover:not(:disabled) { color: var(--fg); border-color: var(--border); }
.up:disabled { opacity: 0.35; cursor: default; }
.here { font-size: var(--text-sm); color: var(--fg-dim); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }

.list {
  flex: 1;
  min-height: 0;
  overflow: auto;
  margin: 0 8px 8px;
  border: 1px solid var(--border-soft);
  border-radius: var(--radius-row);
  background: var(--bg);
  padding: 4px;
}
.frow {
  display: flex;
  align-items: center;
  gap: 7px;
  width: 100%;
  padding: 5px 8px;
  border: 0;
  border-radius: 6px;
  background: transparent;
  color: var(--fg-dim);
  cursor: pointer;
  text-align: left;
  font-size: var(--text-sm);
}
.frow:hover { background: var(--bg-elev-2); color: var(--fg); }
.icon { color: var(--accent); flex: none; width: 12px; }
.fname { flex: 1; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.fsize { font-size: 11px; color: var(--fg-faint); flex: none; }
.empty { font-size: var(--text-xs); color: var(--fg-faint); padding: 8px; }
.werr { font-size: var(--text-xs); color: var(--err); padding: 8px 10px; }

/* ---- 代码区 ---- */
.filepath {
  flex: none;
  padding: 6px 10px;
  border-bottom: 1px solid var(--border-soft);
  font-family: var(--font-mono);
  font-size: 11px;
  color: var(--fg-faint);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.code {
  flex: 1;
  min-height: 0;
  display: flex;
  overflow: auto;
  background: var(--bg);
}
.gutter, .src {
  margin: 0;
  padding: 8px 0;
  font-family: var(--font-mono);
  font-size: 11.5px;
  line-height: 1.55;
  white-space: pre; /* 不折行：代码被折行会骗人 */
  tab-size: 4;
}
.gutter {
  flex: none;
  padding-left: 10px;
  padding-right: 8px;
  color: var(--fg-faint);
  text-align: right;
  user-select: none;
  border-right: 1px solid var(--border-soft);
  position: sticky;
  left: 0;
  background: var(--bg);
}
.src { padding-left: 10px; padding-right: 12px; color: var(--fg-dim); }
/* 覆盖 highlight.js 主题自带的底色与内边距：底色由我们自己的面板决定 */
.src.hljs {
  background: transparent;
  padding: 8px 12px 8px 10px;
  color: var(--fg-dim);
}
</style>
