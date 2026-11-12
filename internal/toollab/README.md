# Browser function tool lab

This package serves deterministic pages for real Chrome function tests. The pages
exercise forms, hover menus, DOM replacement, document scrolling, nested
scrolling, assets, asynchronous fetch, navigation, and popup tabs.
Form checks include associated labels, checkbox roles, and native select options.
Wheel checks cover both document and nested scrolling.
The embedded target covers a same-origin iframe, an open Shadow DOM, and a
native file input. The upload tool reads only regular files from the configured
`UploadDir` and rejects files larger than 10 MiB.
The lab also loads a second local origin into an iframe. Its frame title and URL
are visible in the parent snapshot; its text and controls are collected through
CDP with independent frame references. The test clicks a cross-origin button and
checks the child document result. It also checks batch actions, explicit dialogs,
checked state, and real HTML5 drag and drop through JSON function calls.

Run `make smoke` from the repository root with Chrome or Chromium installed. The
test invokes every enabled `Manager.Functions()` handler using JSON requests and
checks the resulting browser state. The test server and browser profile are
removed when the test finishes.

Run `make lab` to keep the same target available at `http://127.0.0.1:8879/`
for manual inspection. In another terminal, run a local BrowserKit client with
`browserkit --allow-private-network --url http://127.0.0.1:8879/`. Stop the lab
server with Ctrl+C.
