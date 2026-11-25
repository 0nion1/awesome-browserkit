# Frame and Shadow DOM support

The existing `Snapshot` and element operations cover nested same-origin and cross-origin iframes/OOPIFs and their open shadow roots. No separate frame tool or host-side coordinate conversion is required. `page_id` identifies the top-level tab, and its `revision` follows top-level document changes. Child references include frame and loader identity; navigation, removal, or node replacement cannot silently redirect an old reference to another element. Cross-origin password fields have actionable references but do not expose their values.

`page_frame.go` discovers in-process documents through `Page.getFrameTree`, then verifies OOPIF ownership with `DOM.getFrameOwner` in each parent session. Frame count, attachment count, and visibility probes have no fixed count limit. Child sessions retain the browser session lifetime rather than an individual operation's context, and their bindings are released when frames or tabs close.

Every document uses the collector in `page_snapshot.go`, including open shadow roots. Text, element, and value-preview budgets apply to the whole tab. One global element offset supports `next_offset` pagination across documents.

## Snapshot coverage

`frame_coverage` reports the Chrome frame identity and URL of each discovered document:

| Status | Meaning |
| --- | --- |
| `captured` | The document was collected within the current budget. |
| `partial` with `snapshot_budget` | Text or elements were truncated; elements can be paged. |
| `partial` with `metadata_only` | Same-origin controls remain discoverable, but the frame's presentation suppresses text, values, and options. |
| `partial` with `text_not_visible` | A same-origin frame's text is outside the visible area; ordinary field values remain available. |
| `hidden` | The frame is excluded by the existing host-visibility rules. |
| `unavailable` | The document or session could not be read, or the browser could not verify the visibility or origin of the frame or one of its ancestors. The result includes a reason. |

A `frame_id="*"` entry with status `unavailable` means frame discovery failed, so the result cannot assert that every descendant was listed. Each entry declares `shadow_dom=open_only`; `captured` is not a claim to return the complete DOM.

Control discovery and value visibility are separate. A same-origin frame that is positioned far outside the page, has low opacity, or has no content area can still expose control references and names while withholding its values, select options, and text. Normal page scrolling can hide the frame's text without hiding its field values. Excluded cross-origin frames are not made visible by either rule. `metadata_only` and `text_not_visible` remain the frame's coverage reasons on paginated results. Use `next_offset` to retrieve any additional controls.

## Transforms and hit testing

Click, hover, and drag operations map an element's center through `DOM.getBoxModel` content quads and the OOPIF owner chain into the top-level viewport. Nondegenerate affine scaling, rotation, mirroring, and skew are supported. `DOM.getNodeForLocation` checks the hit in each parent viewport; clipping or an overlay that prevents the intended hit returns `frame_unavailable` before input is dispatched to the wrong element. The implementation does not guess another click point when the center is unavailable.

Perspective quads and degenerate geometry return `frame_transform_unsupported`. Transforms changing between sampling and input dispatch, and arbitrary combinations of 3D compositing, have not been fully verified.

Minimal perspective fixture:

```html
<iframe src="https://other-origin.test/form"
  style="width:400px;height:240px;
         transform:perspective(300px) rotateY(30deg)"></iframe>
```

The frame can still appear in the snapshot, but a click returns an unsupported-transform error before dispatch. `TestRealBrowserCrossFramePerspectiveRejected` covers this boundary. `TestRealBrowserAffineFrameClicks` covers scale, 20/180-degree rotation, `scaleX(-1)`, and skew. `TestRealBrowserClosedShadowScopeAndClipping` verifies explicit rejection when `clip-path` removes the intended click point.

## Closed shadow roots

```html
<div id="host"></div>
<script>
  document.getElementById('host').attachShadow({mode:'closed'})
    .innerHTML = '<button>Closed secret</button>';
</script>
```

The collector and reference resolver do not enter this closed root. The SDK does not issue a reference for its button, and coverage remains explicitly `open_only`. Chromium's debugging protocol can inspect some closed-root nodes, but those identities have not been integrated consistently with actions, document invalidation, and cross-process frames. This is not advertised as complete closed-root support. `TestRealBrowserClosedShadowScopeAndClipping` verifies the actual omission.

## Validation

Docker tests use Go 1.25.14 and Chromium 156 on linux/arm64. Targeted fixtures cover nested OOPIFs with open shadow roots, password input and clicks, ten business frames, global pagination and coverage, cloned-node and navigation invalidation, hidden hosts, scrolling, and overlays. `TestRealBrowserSnapshotFormValues` checks that same-origin control metadata remains present while masked values stay empty. Commands and results are recorded in [SDK validation](sdk-baseline-review.md). Real Chromium execution on linux/amd64 and extended stress runs remain unverified.
