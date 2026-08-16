import "./polyfills";

function showStartupError(error: unknown): void {
  const root = document.getElementById("root");
  if (!root) return;
  const message = error instanceof Error ? error.message : String(error);
  root.replaceChildren();
  const panel = document.createElement("main");
  panel.className = "pi-mobile-fatal";
  const title = document.createElement("strong");
  title.textContent = "页面启动失败";
  const detail = document.createElement("p");
  detail.textContent = message;
  const reload = document.createElement("button");
  reload.type = "button";
  reload.textContent = "重新加载";
  reload.addEventListener("click", () => window.location.reload());
  panel.append(title, detail, reload);
  root.append(panel);
}

void import("./bootstrap").catch(showStartupError);
