const api = typeof browser !== "undefined" ? browser : chrome;

async function getConfig() {
  const { serverUrl, token } = await api.storage.local.get(["serverUrl", "token"]);
  return { serverUrl: (serverUrl || "").replace(/\/+$/, ""), token: token || "" };
}

function setStatus(msg, cls) {
  const el = document.getElementById("status");
  el.textContent = msg;
  el.className = "status " + (cls || "");
}

async function init() {
  const { serverUrl, token } = await getConfig();
  const configured = serverUrl && token;
  document.getElementById("ready").hidden = !configured;
  document.getElementById("needs-config").hidden = !!configured;
}

document.getElementById("opts").addEventListener("click", (e) => {
  e.preventDefault();
  api.runtime.openOptionsPage();
});

document.getElementById("add").addEventListener("click", async () => {
  const { serverUrl, token } = await getConfig();
  const [tab] = await api.tabs.query({ active: true, currentWindow: true });
  if (!tab || !tab.url) {
    setStatus("No active tab.", "err");
    return;
  }
  setStatus("Saving…");
  try {
    const res = await fetch(serverUrl + "/api/links", {
      method: "POST",
      headers: { "Content-Type": "application/json", Authorization: "Bearer " + token },
      body: JSON.stringify({ url: tab.url }),
    });
    if (res.status === 201) {
      setStatus("✓ Added to your inbox", "ok");
      api.runtime.sendMessage("refresh");
    } else if (res.status === 403) {
      setStatus("Token lacks capture scope.", "err");
    } else {
      setStatus("Failed (" + res.status + ")", "err");
    }
  } catch (e) {
    setStatus("Could not reach Tideline.", "err");
  }
});

init();
