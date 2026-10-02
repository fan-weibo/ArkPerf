<script setup lang="ts">
import { ref, computed, onMounted, onUnmounted } from "vue";
import { Dialogs } from "@wailsio/runtime";
import {
  Service,
  type WorkspaceChoice,
} from "../../bindings/github.com/fan-weibo/ArkPerf/desktop";

// 工作区选择器（学 Reasonix 的 ComposerWorkspaceContextBar，代码用自己的 Vue 写）。
//
// 放在输入卡的第一行：点开是"搜索 + 最近工作区列表 + 手动输入路径"。
// 最近工作区不另存一份状态，直接来自会话历史里出现过的目录——
// 在哪个目录里聊过话，那个目录就是一个工作区。
const props = defineProps<{
  current: string;
  running: boolean;
}>();

const emit = defineEmits<{
  (e: "switch", path: string): void;
}>();

const open = ref(false);
const items = ref<WorkspaceChoice[]>([]);
const filter = ref("");
const loading = ref(false);
// 两个错误状态必须分开：列表区与动作区都在渲染消息，混用会同一句话显示两遍
const err = ref(""); // 列表加载失败
const dialogErr = ref(""); // 系统对话框失败
// Web 版（server 模式）不支持系统对话框——这是 Wails 的既定限制，不是故障。
// 撞到一次就记住，把入口标成不可用，免得用户反复点同一个坑。
const folderPickUnavailable = ref(false);
const root = ref<HTMLElement | null>(null);

function baseName(p: string): string {
  const clean = (p || "").replace(/[\\/]+$/, "");
  const parts = clean.split(/[\\/]/).filter(Boolean);
  return parts.length ? parts[parts.length - 1] : clean;
}

function fmtErr(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}

// 标题以 props.current 为准（它是权威值）：列表可能是切换之前拉的，
// 拿它当标题会显示成上一个目录。
const title = computed(
  () => baseName(props.current) || items.value.find((w) => w.Current)?.Name || "工作区",
);

const filtered = computed(() => {
  const q = filter.value.trim().toLowerCase();
  if (!q) return items.value;
  return items.value.filter(
    (w) => w.Name.toLowerCase().includes(q) || w.Path.toLowerCase().includes(q),
  );
});

async function toggle() {
  open.value = !open.value;
  if (open.value) {
    filter.value = "";
    dialogErr.value = "";
    await load();
  }
}

async function load() {
  loading.value = true;
  err.value = "";
  try {
    items.value = (await Service.ListWorkspaces()) ?? [];
  } catch (e) {
    err.value = fmtErr(e);
  } finally {
    loading.value = false;
  }
}

function pick(w: WorkspaceChoice) {
  if (w.Current) {
    open.value = false;
    return;
  }
  open.value = false;
  emit("switch", w.Path);
}

// 打开系统文件夹选择器（Wails v3 的 Dialogs.OpenFile 带 CanChooseDirectories 开关，
// 就是 Reasonix 那个「打开文件夹」——他们 v2 用的是 runtime.OpenDirectoryDialog）。
//
// 两个细节：
//  1. 以当前工作区作为起始目录，省得用户从"我的电脑"一层层点进去；
//  2. 用户取消时返回的是**空字符串**，不是错误——不能当成失败弹提示。
async function pickFolder() {
  open.value = false;
  dialogErr.value = "";
  try {
    const picked = await Dialogs.OpenFile({
      Title: "选择工作区目录",
      CanChooseDirectories: true,
      CanChooseFiles: false,
      Directory: props.current || undefined,
    });
    const path = typeof picked === "string" ? picked : (picked ?? [])[0] ?? "";
    if (path) emit("switch", path);
  } catch (e) {
    const msg = fmtErr(e);
    // server 模式（Web 版）明确不支持：翻成人话，并把入口关掉
    if (/server mode/i.test(msg)) {
      folderPickUnavailable.value = true;
      dialogErr.value = "Web 版不支持系统对话框（桌面版可以）。请改用桌面版，或从上面的最近工作区里选。";
    } else {
      dialogErr.value = `系统对话框打不开：${msg}`;
    }
    open.value = true;
  }
}

function onDocMouseDown(e: MouseEvent) {
  if (!open.value) return;
  if (root.value && e.target instanceof Node && !root.value.contains(e.target)) {
    open.value = false;
  }
}

function onDocKey(e: KeyboardEvent) {
  if (e.key === "Escape" && open.value) open.value = false;
}

onMounted(() => {
  document.addEventListener("mousedown", onDocMouseDown);
  document.addEventListener("keydown", onDocKey);
  void load(); // 首屏也拉一次：标题要能显示目录名
});

onUnmounted(() => {
  document.removeEventListener("mousedown", onDocMouseDown);
  document.removeEventListener("keydown", onDocKey);
});
</script>

<template>
  <div ref="root" class="wsbar">
    <button
      class="trigger"
      :class="{ open }"
      :disabled="running"
      :title="running ? '任务运行中不能切换工作区' : props.current"
      @click="toggle"
    >
      <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"
           stroke-linecap="round" stroke-linejoin="round">
        <path d="M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2Z" />
      </svg>
      <span class="name">{{ title }}</span>
      <svg class="caret" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"
           stroke-linecap="round" stroke-linejoin="round">
        <path d="M6 9l6 6 6-6" />
      </svg>
    </button>

    <div v-if="open" class="menu" role="menu">
      <label class="search">
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"
             stroke-linecap="round" stroke-linejoin="round">
          <circle cx="11" cy="11" r="7" /><path d="M21 21l-4.3-4.3" />
        </svg>
        <input v-model="filter" placeholder="搜索工作区…" autofocus />
      </label>

      <div class="list">
        <div v-if="loading && items.length === 0" class="note">加载中…</div>
        <div v-else-if="err" class="note err">{{ err }}</div>
        <div v-else-if="filtered.length === 0" class="note">没有匹配的工作区</div>
        <button
          v-for="w in filtered"
          :key="w.Path"
          class="item"
          :class="{ active: w.Current }"
          role="menuitem"
          :title="w.Path"
          @click="pick(w)"
        >
          <span class="tick">
            <svg v-if="w.Current" viewBox="0 0 24 24" fill="none" stroke="currentColor"
                 stroke-width="2.4" stroke-linecap="round" stroke-linejoin="round">
              <path d="M20 6L9 17l-5-5" />
            </svg>
          </span>
          <span class="meta">
            <span class="label">{{ w.Name }}</span>
            <span class="path">{{ w.Path }}</span>
          </span>
          <span class="info">{{ (w.Sessions ?? []).length > 0 ? `${(w.Sessions ?? []).length} 会话` : "" }}{{ w.Updated ? " · " + w.Updated : "" }}</span>
        </button>
      </div>

      <div class="actions">
        <div v-if="dialogErr" class="note err">{{ dialogErr }}</div>
        <button
          class="act"
          role="menuitem"
          :disabled="folderPickUnavailable"
          :title="folderPickUnavailable ? 'Web 版不支持系统对话框' : '打开系统文件夹选择器'"
          @click="pickFolder"
        >
          <span class="ico">📁</span>{{ folderPickUnavailable ? "打开文件夹（当前版本不支持）" : "打开文件夹…" }}
        </button>
      </div>
    </div>
  </div>
</template>

<style scoped>
.wsbar { position: relative; display: inline-block; }

.trigger {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  max-width: 100%;
  padding: 3px 8px;
  border: 1px solid transparent;
  border-radius: 7px;
  background: transparent;
  color: var(--fg-dim);
  cursor: pointer;
  font-size: var(--text-xs);
}
.trigger:hover:not(:disabled) { background: var(--bg-elev-2); color: var(--fg); }
.trigger.open { background: var(--bg-elev-2); color: var(--fg); border-color: var(--border-soft); }
.trigger:disabled { opacity: 0.5; cursor: default; }
.trigger svg { width: 13px; height: 13px; flex: none; opacity: 0.85; }
.trigger .caret { width: 12px; height: 12px; opacity: 0.6; }
.name { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; max-width: 200px; }

.menu {
  position: absolute;
  left: 0;
  bottom: calc(100% + 6px);
  z-index: 40;
  width: 380px;
  max-width: 70vw;
  border: 1px solid var(--border);
  border-radius: var(--radius-card);
  background: var(--bg-elev);
  box-shadow: var(--shadow-card);
  overflow: hidden;
}

.search {
  display: flex;
  align-items: center;
  gap: 7px;
  padding: 8px 10px;
  border-bottom: 1px solid var(--border-soft);
}
.search svg { width: 14px; height: 14px; color: var(--fg-faint); flex: none; }
.search input {
  flex: 1;
  min-width: 0;
  border: 0;
  outline: 0;
  background: transparent;
  color: var(--fg);
  font-size: var(--text-sm);
}
.search input::placeholder { color: var(--fg-faint); }

.list { max-height: 300px; overflow-y: auto; padding: 4px; }
.note { font-size: var(--text-xs); color: var(--fg-faint); padding: 8px 10px; }
.note.err { color: var(--err); }

.item {
  display: flex;
  align-items: center;
  gap: 8px;
  width: 100%;
  padding: 6px 8px;
  border: 0;
  border-radius: 7px;
  background: transparent;
  color: var(--fg-dim);
  cursor: pointer;
  text-align: left;
}
.item:hover { background: var(--bg-elev-2); }
.item.active { background: color-mix(in srgb, var(--accent) 13%, var(--bg-soft)); }
.tick { width: 14px; flex: none; color: var(--accent); }
.tick svg { width: 14px; height: 14px; display: block; }
.meta { display: flex; flex-direction: column; gap: 1px; min-width: 0; flex: 1; }
.label { font-size: var(--text-sm); color: var(--fg); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.path {
  font-family: var(--font-mono);
  font-size: 11px;
  color: var(--fg-faint);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.info { font-size: 11px; color: var(--fg-faint); flex: none; }

.actions { border-top: 1px solid var(--border-soft); padding: 5px; }
.act {
  display: block;
  width: 100%;
  padding: 6px 8px;
  border: 0;
  border-radius: 7px;
  background: transparent;
  color: var(--fg-dim);
  cursor: pointer;
  font-size: var(--text-sm);
  text-align: left;
}
.act:hover { background: var(--bg-elev-2); color: var(--fg); }
.act:disabled { opacity: 0.45; cursor: default; }
.act:disabled:hover { background: transparent; color: var(--fg-dim); }
.ico { display: inline-block; width: 18px; opacity: 0.8; }
</style>
