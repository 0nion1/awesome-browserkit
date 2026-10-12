# Cross-origin frame tool design

The real-browser lab shows the boundary: same-origin iframe controls and open
shadow roots work with the existing page snapshot and ref tools. A cross-origin
iframe appears as a `frame` element with `frame_url`, while its controls are not
in the parent page's DOM. Closed shadow roots have the same visibility limit.

The next API addition should keep a frame's references distinct from the Tab's
main document:

- `browserFrameSnapshot(session_id, page_id, frame_ref, revision, max_chars,
  max_elements, offset)` returns `frame_id`, `frame_revision`, URL, text, and
  interactive elements. `frame_ref` must come from the parent snapshot.
- Existing element operations can accept optional `frame_id` and
  `frame_revision`. Omitting them retains current main-document behavior.
  The manager validates the frame's ownership of each ref.
- A frame navigation invalidates its own revision and refs, even when the Tab's
  main document does not navigate. Detached frames return `stale_reference`.
- Frame lookup must use Chrome's frame or target identity rather than a DOM
  selector or page-provided URL; a cross-origin URL cannot identify one of
  several matching frame instances.

Implement this only with a real Chrome test that opens two cross-origin frames,
changes one frame's document, and verifies that clicks and typing stay in the
correct frame. The browser's network policy must continue to apply to frame
navigation and subresources.
