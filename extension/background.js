// Polls Tideline for the number of due links and paints it on the toolbar badge.
const api = typeof browser !== "undefined" ? browser : chrome;

const POLL_ALARM = "tideline-poll";
const POLL_MINUTES = 5;

async function config() {
  const { serverUrl, token } = await api.storage.local.get(["serverUrl", "token"]);
  return { serverUrl: (serverUrl || "").replace(/\/+$/, ""), token: token || "" };
}

async function refreshBadge() {
  const { serverUrl, token } = await config();
  if (!serverUrl || !token) {
    await api.action.setBadgeText({ text: "" });
    return;
  }
  try {
    const res = await fetch(serverUrl + "/api/count", {
      headers: { Authorization: "Bearer " + token },
    });
    if (!res.ok) throw new Error("status " + res.status);
    const { count } = await res.json();
    await api.action.setBadgeText({ text: count > 0 ? String(count) : "" });
    await api.action.setBadgeBackgroundColor({ color: "#f97316" });
  } catch (e) {
    await api.action.setBadgeText({ text: "!" });
    await api.action.setBadgeBackgroundColor({ color: "#ef4444" });
  }
}

api.runtime.onInstalled.addListener(() => {
  api.alarms.create(POLL_ALARM, { periodInMinutes: POLL_MINUTES });
  refreshBadge();
});
api.runtime.onStartup && api.runtime.onStartup.addListener(refreshBadge);
api.alarms.onAlarm.addListener((a) => {
  if (a.name === POLL_ALARM) refreshBadge();
});
// Re-poll promptly when settings change or the popup asks us to.
api.storage.onChanged.addListener(refreshBadge);
api.runtime.onMessage.addListener((msg) => {
  if (msg === "refresh") refreshBadge();
});
