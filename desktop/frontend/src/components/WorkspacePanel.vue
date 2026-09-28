<script setup lang="ts">
import { ref, computed, onMounted } from "vue";
import {
  Service,
  type WorkspaceInfo,
  type WsEntry,
} from "../../bindings/github.com/fan-weibo/ArkPerf/desktop";

const props = defineProps<{ cwd: string }>();

const ws = ref<WorkspaceInfo | null>(null);
const path = ref("");
const entries = ref<WsEntry[]>([]);
const filter = ref("");
const preview = ref("");
const previewPath = ref("");
const err = ref("");

function fmtSize(n: number): string {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  return `${(n / 1024 / 1024).toFixed(1)} MB`;
}

// Windows 项目里路径分隔符是 \；命令行里也可能混着 /
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

// 搜索框：按名字过滤当前目录（纯前端过滤，不打扰后端）
const shown = computed(() => {
  const q = filter.value.trim().toLowerCase();
  if (!q) return entries.value;
  return entries.value.filter((e) => e.Name.toLowerCase().includes(q));
});

onMounted(load);

async function load() {
  err.value = "";
  preview.value = "";
  previewPath.value = "";
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
  preview.value = "";
  previewPath.value = "";
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
    return;
  }
  try {
    previewPath.value = p;
    preview.value = await Service.ReadTextFile(p);
  } catch (ex) {
    preview.value = "";
    previewPath.value = p;
    err.value = String(ex);
  }
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
    <div class="head">
      <span class="head-title">文件</span>
      <span v-if="ws?.IsProject" class="badge">当前</span>
      <button class="reload" title="刷新" @click="load">⟳</button>
    </div>

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

    <div v-if="previewPath" class="preview">
      <div class="preview-head">
        <span class="preview-path" :title="previewPath">{{ baseName(previewPath) }}</span>
        <button class="x" @click="preview = ''; previewPath = ''">✕</button>
      </div>
      <pre class="preview-body">{{ preview }}</pre>
    </div>
  </div>
</template>

<style scoped>
.ws-root {
  display: flex;
  flex-direction: column;
  height: 100%;
  min-height: 0;
  padding: 10px;
  gap: 8px;
}
.head { display: flex; align-items: center; gap: 8px; }
.head-title { font-size: var(--text-md); color: var(--fg-dim); font-weight: 500; }
.badge {
  font-size: 10px;
  color: var(--fg-faint);
  border: 1px solid var(--border-soft);
  border-radius: 4px;
  padding: 0 4px;
}
.reload {
  margin-left: auto;
  border: 0;
  background: transparent;
  color: var(--fg-faint);
  cursor: pointer;
  font-size: var(--text-md);
}
.reload:hover { color: var(--fg); }

.search {
  width: 100%;
  box-sizing: border-box;
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

.meta { display: flex; flex-direction: column; gap: 4px; }
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

.nav { display: flex; align-items: center; gap: 8px; }
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
.werr { font-size: var(--text-xs); color: var(--err); padding: 8px; }

.preview {
  flex: none;
  max-height: 42%;
  display: flex;
  flex-direction: column;
  border: 1px solid var(--border);
  border-radius: var(--radius-row);
  background: var(--bg);
  overflow: hidden;
}
.preview-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 5px 9px;
  border-bottom: 1px solid var(--border-soft);
  font-size: var(--text-xs);
  color: var(--fg-dim);
}
.preview-path { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.x { border: 0; background: transparent; color: var(--fg-faint); cursor: pointer; }
.x:hover { color: var(--fg); }
.preview-body {
  margin: 0;
  padding: 8px 10px;
  overflow: auto;
  white-space: pre-wrap;
  overflow-wrap: anywhere;
  font-family: var(--font-mono);
  font-size: 11px;
  color: var(--fg-dim);
}
</style>
