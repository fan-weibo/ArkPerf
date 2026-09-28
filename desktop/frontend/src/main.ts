import { createApp } from "vue";
import "./theme.css";
import App from "./App.vue";

// 这个外壳（原生窗口 / WebView2）里没有 DevTools，一旦 JS 或渲染出错，
// 用户看到的只是"一片空白"，完全无从判断。所以把错误显式画到页面上：
// 空白窗口 → 有信息的窗口。排查问题时这一行救命。
function showFatal(label: string, detail: unknown) {
  if (document.querySelector("[data-fatal]")) return; // 只报第一条，避免刷屏
  const text =
    detail instanceof Error ? `${detail.message}\n${detail.stack ?? ""}` : String(detail);
  const pre = document.createElement("pre");
  pre.dataset.fatal = "1";
  pre.style.cssText =
    "position:fixed;left:0;right:0;top:0;z-index:99999;margin:0;padding:10px 14px;" +
    "white-space:pre-wrap;font:12px/1.5 monospace;color:#ff9a9a;" +
    "background:rgba(48,12,12,.96);border-bottom:1px solid #ff8a8a";
  pre.textContent = `[ArkPerf] ${label}：${text}`;
  document.body.prepend(pre);
}

window.addEventListener("error", (e) => showFatal("运行时错误", e.error ?? e.message));
window.addEventListener("unhandledrejection", (e) =>
  showFatal("未处理的 Promise 拒绝", e.reason),
);

try {
  createApp(App).mount("#app");
} catch (e) {
  showFatal("挂载失败", e);
}
