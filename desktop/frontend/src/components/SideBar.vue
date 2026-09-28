<script setup lang="ts">
import type { SessionInfo } from "../../bindings/github.com/fan-weibo/ArkPerf/desktop";

defineProps<{
  sessions: SessionInfo[];
  toolchain: string;
  running: boolean;
  // 当前激活的功能视图（chat / tools / toolchain / devices）——
  // 侧栏点功能是"跳转到页面"，当前页要在侧栏里高亮出来
  active: string;
}>();

defineEmits<{
  (e: "switch", id: string): void;
  (e: "new-session"): void;
  (e: "tools"): void;
  (e: "toolchain"): void;
  (e: "devices"): void;
}>();

// 会话名 = 工作目录最后一段（项目名）
function projName(cwd: string): string {
  const parts = cwd.replace(/[\\/]+$/, "").split(/[\\/]/);
  return parts[parts.length - 1] || cwd;
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

    <div class="sect">项目</div>
    <div class="sess-list">
      <button
        v-for="s in sessions"
        :key="s.ID"
        class="sess"
        :class="{ cur: s.Current, dim: !s.SameDir }"
        :disabled="running"
        :title="s.SameDir ? s.CWD : '其他工作目录：' + s.CWD"
        @click="$emit('switch', s.ID)"
      >
        <span class="stitle">{{ s.Title }}</span>
        <span class="smeta">
          <span class="stag" v-if="s.Current">会话</span>
          <span class="sdate">{{ s.Updated }}</span>
        </span>
      </button>
      <div v-if="sessions.length === 0" class="none">（还没有历史会话）</div>
    </div>

    <div class="foot" :title="toolchain">{{ toolchain }}</div>
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

.sess-list {
  display: flex;
  flex-direction: column;
  gap: 1px;
  flex: 1;
  min-height: 0;
  overflow-y: auto;
}
.sess {
  position: relative;
  display: flex;
  flex-direction: column;
  gap: 2px;
  padding: 6px 9px 6px 12px;
  border: 0;
  border-radius: var(--radius-row);
  background: transparent;
  color: var(--fg-dim);
  text-align: left;
  cursor: pointer;
}
.sess:hover { background: var(--bg-elev-2); }
.sess.cur {
  background: color-mix(in srgb, var(--accent) 13%, var(--bg-soft));
  box-shadow: inset 2px 0 0 var(--accent);
}
.sess.cur .stitle { color: var(--fg); }
.sess.dim { opacity: 0.45; }
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

.none { font-size: var(--text-xs); color: var(--fg-faint); padding: 4px 8px; }

.foot {
  margin-top: auto;
  padding: 8px 8px 2px;
  border-top: 1px solid var(--border-soft);
  font-size: 11px;
  color: var(--fg-faint);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
</style>
