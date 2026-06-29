# Tideline browser extension

Save the current page to your Tideline inbox with one click, and see how many
links are **due** on the toolbar badge.

## Load it in Firefox (temporary, for development)

1. Open `about:debugging#/runtime/this-firefox`
2. Click **Load Temporary Add-on…**
3. Select `extension/manifest.json`

(Temporary add-ons are removed when Firefox restarts. For a permanent install,
the add-on needs to be signed by Mozilla.)

## Configure

1. In Tideline, go to **Settings → API tokens** and create a **capture** token.
2. Click the Tideline toolbar icon → **Options**.
3. Enter your **Server URL** (e.g. `https://tideline.my-pi.local`) and paste the
   capture token. Save.

## Use

- **Add this page:** click the toolbar icon → **Add this page**.
- **Badge:** the icon shows the count of due links, refreshed every few minutes.
  `!` means it couldn't reach the server — check the URL/token in Options.

## Chrome / Chromium

This manifest targets Firefox (MV3 with a background script). Chrome requires a
background **service worker** instead — change `"background": { "scripts": [...] }`
to `"background": { "service_worker": "background.js" }` to load it there.
