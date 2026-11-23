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
are visible in the parent snapshot; its controls are intentionally outside the
parent document's element references.

`/snapshot.html` is the focused snapshot-behavior suite. It adds same-node text
changes, recurring DOM replacement, a 100 ms live collection, staged success and
failure requests, dialog visibility, open Shadow DOM, a same-origin iframe, and
160 stable controls for offset-pagination checks. Its fidelity section adds
checked/pressed state, ARIA roles, contenteditable, inert and covered controls,
large and multiple selects, a closed Shadow DOM, and an application-owned ref
attribute. The page is deliberately human-readable so the same target can be
evaluated through an agent's visual or accessibility browser interface, not
only BrowserKit.

Run `make smoke` from the repository root with Chrome or Chromium installed. The
test invokes every enabled `Manager.Functions()` handler using JSON requests and
checks the resulting browser state. The test server and browser profile are
removed when the test finishes.

Run `make lab` to keep the same target available at `http://127.0.0.1:8879/`
for manual inspection. In another terminal, run a local BrowserKit client with
`browserkit --allow-private-network --url http://127.0.0.1:8879/`. Stop the lab
server with Ctrl+C.

Use `make snapshot-lab` as the explicit entry point for this suite, then open
`http://127.0.0.1:8879/snapshot.html`.
