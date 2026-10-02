<script setup lang="ts">
import { ref } from "vue";
import type { WorkspaceChoice } from "../../bindings/github.com/fan-weibo/ArkPerf/desktop";

defineProps<{
  // 工作区树：每个工作区带着它自己的会话（不是只列当前工作区——
  // 那样切走之后别的项目就看不见、也点不回去了）
  workspaces: WorkspaceChoice[];
  // toolchain 是完整摘要（悬停看全文）；tcFound/tcTotal 供底部的状态点判断"齐不齐"
  toolchain: string;
  tcFound: number;
  tcTotal: number;
  running: boolean;
  // 当前激活的功能视图（chat / tools / toolchain / devices）——
  // 侧栏点功能是"跳转到页面"，当前页要在侧栏里高亮出来
  active: string;
}>();

defineEmits<{
  (e: "open-session", id: string): void;
  (e: "new-session"): void;
  (e: "tools"): void;
  (e: "toolchain"): void;
  (e: "devices"): void;
}>();

// 展开状态：**默认全部展开**，各工作区互不影响。
//
// 两个刻意的决定：
//  1. 不跟着"当前工作区"自动展开/收起——那样切一次就会把上一个收起来，
//     用户明确要求"每个工作区应该可以同时展开"；
//  2. 展开/收起与"切到哪个工作区"解耦：点工作区只是收放列表，
//     **不会**把正在看的会话换掉（换会话是点具体某条会话的事）。
//     把这两件事绑在一起的版本，用户点一下标题就丢了当前上下文（实测被指出）。
const collapsed = ref<Record<string, boolean>>({});

function isOpen(w: WorkspaceChoice): boolean {
  return !collapsed.value[w.Path];
}

function toggle(w: WorkspaceChoice) {
  collapsed.value = { ...collapsed.value, [w.Path]: isOpen(w) };
}

function sessionCount(w: WorkspaceChoice): number {
  return (w.Sessions ?? []).length;
}
</script>

<template>
  <div class="side-root">
    <div class="brand">ArkPerf</div>

    <!-- 主操作：整个侧栏唯一的强调项 -->
    <button class="new-chat" @click="$emit('new-session')">
      <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"
           stroke-linecap="round" stroke-linejoin="round">
        <path d="M12 20h9" /><path d="M16.5 3.5a2.12 2.12 0 0 1 3 3L7 19l-4 1 1-4Z" />
      </svg>
      新会话
    </button>

    <div class="sect">功能</div>
    <button class="item" :class="{ active: active === 'tools' }" @click="$emit('tools')">
      <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"
           stroke-linecap="round" stroke-linejoin="round">
        <path d="M8 6h13" /><path d="M8 12h13" /><path d="M8 18h13" />
        <path d="M3 6h.01" /><path d="M3 12h.01" /><path d="M3 18h.01" />
      </svg>
      工具清单
    </button>
    <button class="item" :class="{ active: active === 'toolchain' }" @click="$emit('toolchain')">
      <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"
           stroke-linecap="round" stroke-linejoin="round">
        <path d="M4 17l6-6-6-6" /><path d="M12 19h8" />
      </svg>
      工具链探测
    </button>
    <button class="item" :class="{ active: active === 'devices' }" @click="$emit('devices')">
      <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"
           stroke-linecap="round" stroke-linejoin="round">
        <rect x="2" y="3" width="20" height="14" rx="2" /><path d="M8 21h8" /><path d="M12 17v4" />
      </svg>
      设备列表
    </button>

    <!-- 工具链状态：紧跟功能区，不钉在窗口最底边。
         钉底会把"状态"和"窗口边框"混在一起看（用户直接指出"太靠下"），
         放在功能区下方既稳定（不会被下面的列表挤走）又在语义上贴着它的邻居。 -->
    <button
      class="status-row"
      :title="toolchain ? toolchain + '\n\n（启动时探测的快照）点击查看完整探测结果' : '点击查看工具链探测'"
      @click="$emit('toolchain')"
    >
      <span
        class="dot"
        :class="{ warn: tcTotal > 0 && tcFound < tcTotal, unknown: tcTotal === 0 }"
      ></span>
      <span class="status-text">
        {{ tcTotal > 0 ? `工具链 ${tcFound}/${tcTotal}` : "工具链未探测" }}
      </span>
      <span v-if="tcTotal > 0 && tcFound < tcTotal" class="status-hint">有缺失</span>
    </button>

    <div class="sect">工作区（{{ workspaces.length }}）</div>
    <div class="tree">
      <template v-for="w in workspaces" :key="w.Path">
        <!-- 整行只做展开/收起，不切工作区 -->
        <button
          class="ws-head"
          :class="{ cur: w.Current }"
          :title="w.Path"
          @click="toggle(w)"
        >
          <span class="chev" :class="{ open: isOpen(w) }">
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4"
                 stroke-linecap="round" stroke-linejoin="round">
              <path d="M9 6l6 6-6 6" />
            </svg>
          </span>
          <span class="ws-name">{{ w.Name }}</span>
          <span class="ws-count">{{ sessionCount(w) }}</span>
        </button>

        <div v-if="isOpen(w)" class="ws-sessions">
          <button
            v-for="s in w.Sessions ?? []"
            :key="s.ID"
            class="sess"
            :class="{ cur: s.Current }"
            :disabled="running"
            :title="s.CWD"
            @click="$emit('open-session', s.ID)"
          >
            <span class="stitle">{{ s.Title }}</span>
            <span class="smeta">
              <span class="stag" v-if="s.Current">会话</span>
              <span class="sdate">{{ s.Updated }}</span>
            </span>
          </button>
          <div v-if="sessionCount(w) === 0" class="none">（还没有会话）</div>
        </div>
      </template>
    </div>
  </div>
</template>

<style scoped>
.side-root {
  display: flex;
  flex-direction: column;
  height: 100%;
  min-height: 0;
  padding: 12px 8px 8px;
}
.brand {
  font-weight: 500;
  font-size: 15px;
  color: var(--fg);
  padding: 2px 8px 14px;
}

/* 主操作：微主色描边的盒子，侧栏唯一的强调项 */
.new-chat {
  display: flex;
  align-items: center;
  gap: 9px;
  width: 100%;
  padding: 7px 10px;
  border: 1px solid color-mix(in srgb, var(--accent) 26%, var(--border-soft));
  border-radius: var(--radius-row);
  background: color-mix(in srgb, var(--accent) 7%, transparent);
  color: var(--fg);
  cursor: pointer;
  font-size: var(--text-md);
  transition: border-color 80ms ease, background-color 80ms ease;
}
.new-chat:hover {
  border-color: color-mix(in srgb, var(--accent) 45%, var(--border-soft));
  background: var(--accent-soft);
}
.new-chat svg { width: 15px; height: 15px; color: var(--accent); flex: none; }

.sect {
  font-size: 11px;
  color: var(--fg-faint);
  padding: 14px 8px 5px;
  letter-spacing: 0.04em;
}

.item {
  display: flex;
  align-items: center;
  gap: 9px;
  width: 100%;
  padding: 6px 10px;
  border: 0;
  border-radius: var(--radius-row);
  background: transparent;
  color: var(--fg-dim);
  cursor: pointer;
  font-size: var(--text-md);
  transition: background-color 80ms ease, color 80ms ease;
}
.item:hover { background: var(--bg-elev-2); color: var(--fg); }
.item.active {
  background: color-mix(in srgb, var(--accent) 13%, var(--bg-soft));
  box-shadow: inset 2px 0 0 var(--accent);
  color: var(--fg);
}
.item svg { width: 15px; height: 15px; flex: none; opacity: 0.85; }

/* ---- 工作区树 ---- */
.tree { flex: 1; min-height: 0; overflow-y: auto; }

.ws-head {
  display: flex;
  align-items: center;
  gap: 6px;
  width: 100%;
  padding: 6px 8px 6px 6px;
  border: 0;
  border-radius: var(--radius-row);
  background: transparent;
  color: var(--fg-dim);
  cursor: pointer;
  text-align: left;
  font-size: var(--text-md);
}
.ws-head:hover { background: var(--bg-elev-2); color: var(--fg); }
/* 当前工作区只做视觉标记，不是点击目标 */
.ws-head.cur {
  color: var(--fg);
  background: color-mix(in srgb, var(--accent) 10%, var(--bg-soft));
  box-shadow: inset 2px 0 0 var(--accent);
}
.ws-name { flex: 1; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.ws-count {
  font-size: 10px;
  color: var(--fg-faint);
  border: 1px solid var(--border-soft);
  border-radius: 999px;
  padding: 0 5px;
  flex: none;
}

.chev {
  flex: none;
  width: 14px;
  height: 14px;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  color: var(--fg-faint);
}
.chev svg { width: 11px; height: 11px; transition: transform 0.12s ease; }
.chev.open svg { transform: rotate(90deg); }

.ws-sessions { padding-left: 16px; }
.sess {
  position: relative;
  display: flex;
  flex-direction: column;
  gap: 2px;
  width: 100%;
  padding: 5px 8px 5px 12px;
  border: 0;
  border-radius: var(--radius-row);
  background: transparent;
  color: var(--fg-dim);
  text-align: left;
  cursor: pointer;
}
.sess:hover { background: var(--bg-elev-2); }
.sess.cur { background: color-mix(in srgb, var(--accent) 13%, var(--bg-soft)); }
.sess.cur .stitle { color: var(--fg); }
.sess:disabled { cursor: default; }
.stitle {
  font-size: var(--text-sm);
  color: var(--fg-dim);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.smeta {
  display: flex;
  align-items: center;
  gap: 6px;
  font-size: 11px;
  color: var(--fg-faint);
}
.stag {
  font-size: 10px;
  color: var(--accent);
  border: 1px solid color-mix(in srgb, var(--accent) 30%, transparent);
  border-radius: 4px;
  padding: 0 4px;
}
.sdate { flex: none; }
.none { font-size: var(--text-xs); color: var(--fg-faint); padding: 3px 8px 6px 12px; }

/* 工具链状态行：跟着功能区走，不钉底（钉底会跟窗口边框混在一起看） */
.status-row {
  display: flex;
  align-items: center;
  gap: 6px;
  width: 100%;
  margin-top: 1px;
  padding: 6px 10px;
  border: 0;
  border-radius: var(--radius-row);
  background: transparent;
  color: var(--fg-faint);
  cursor: pointer;
  font-size: 11px;
  text-align: left;
}
.status-row:hover { background: var(--bg-elev-2); color: var(--fg-dim); }
.dot {
  flex: none;
  width: 7px;
  height: 7px;
  border-radius: 50%;
  background: var(--ok);
}
.dot.warn { background: var(--warn); }
.dot.unknown { background: var(--fg-faint); }
.status-text { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.status-hint { flex: none; color: var(--warn); }
</style>
