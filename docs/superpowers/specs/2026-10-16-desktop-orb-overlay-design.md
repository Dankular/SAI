# Desktop Orb Overlay Design

**Status:** For review

**Goal:** Replace the current desktop widget page with a borderless, draggable orb that displays only the existing spiral and sphere, follows the user's speech level, and disappears cleanly when disabled.

**User requirements:** The widget must be directly draggable; it must show no labels, status copy, frame, opaque background, or title bar; the sphere should react to the user's microphone level; disabling it must not leave a mark on the wallpaper. The design should allow both Chromium and a future custom renderer.

## Context

The current Windows widget starts Edge in app mode, then reparents its window into Explorer's WorkerW desktop layer. That makes it non-interactive and prevents direct dragging. Its page paints a dark panel, label, status footer, and HUD frame around the orb. The dashboard already runs the sphere renderer in web/orb.js and the speech listener already produces microphone RMS levels, but the widget currently receives state only.

DeskX separates behind-icons wallpaper content from widgets in a transparent overlay. It routes pointer input to the widget and passes input through elsewhere. We will use that interaction model for one orb rather than recreate DeskX's full multi-layer, multi-monitor system.

## Chosen approach

Use one independent, transparent, borderless overlay window for the orb. Keep it above desktop icons and below normal application windows. Hit testing should route input to the visible orb region for dragging and pass clicks through outside that region. This directly addresses movement while keeping the surrounding desktop usable.

The overlay remains a supervised service. Its Windows host owns window creation, z-order, hit testing, persisted position, and shutdown. It is no longer parented to WorkerW. On disable, the host hides and closes the window cleanly, then invalidates the desktop area it occupied. Force-kill is only a timed fallback. Startup cleanup removes any orphaned widget process/window from an earlier forced exit.

The current corner setting remains a placement preset. The actual position is saved as normalized coordinates within the primary monitor's work area, clamped so the orb stays on-screen. Dragging updates the saved coordinates; selecting a corner moves it back to that preset.

## Renderer boundary

The window host consumes an implementation-neutral orb frame containing assistant state and normalized audio level. Rendering is supplied behind a small renderer interface, so window management, drag behavior, state transport, and persistence do not depend on a browser engine.

The Chromium backend is the first implementation. It uses the existing Edge app-mode process and existing orb renderer, but the widget document becomes a transparent drawing surface containing only the orb. It removes the page-like decoration and reports drag changes through the local app API.

A custom renderer is a planned second backend behind the same interface. This work defines the boundary so a native or other custom renderer can be added without changing the overlay host or state protocol. It does not implement an unspecified custom rendering technology or expose a renderer selector in the dashboard yet. Chromium remains the default and only available backend until a concrete custom renderer is chosen.

## Voice data flow

The dashboard's existing HandsFree listener remains the only microphone capture. While speech is being listened for, it sends a smoothed, normalized RMS level to the local Go server at no more than 30 updates per second. The widget consumes the latest level and feeds it to the existing orb renderer. The server sends only the scalar level and assistant state; raw microphone samples are neither copied to the widget nor stored. When listening stops or the sample becomes stale, the level returns to zero.

The widget does not request microphone permission or open another capture stream. It reacts to mic level only while the existing dashboard listener is active.

## Scope and constraints

- Windows remains the only platform with a desktop widget; the existing non-Windows stub remains.
- Initial placement and saved coordinates target the primary monitor. Multi-monitor placement is outside this change.
- Keep the current service toggle and corner preset in the dashboard.
- Keep the orb's existing state colors and animation. The widget contains no visible page chrome or text.
- Do not add dependencies or a general widget framework.
- Do not implement the custom renderer until its technology and visual parity requirements are specified.

## Acceptance criteria

1. The widget appears without a title bar or opaque rectangle and displays only the existing spiral and sphere.
2. The orb can be dragged directly on the desktop; its position survives app restart and stays within the primary work area.
3. Clicking outside the orb reaches the desktop normally; clicking and dragging the orb moves it.
4. User speech level drives the orb while the existing microphone listener is active. No second microphone stream is opened and no raw audio is sent to the widget.
5. Disabling the widget closes its process/window and repaints the desktop without leaving a grey artifact.
6. The renderer interface allows a future custom backend while Chromium remains the only implementation in this change.

## Alternatives considered

- Keep the WorkerW child and remove page decorations: smallest change, but it remains non-interactive and cannot be directly dragged.
- Recreate DeskX's full three-layer, per-monitor overlay system: supports broader future use, but is unnecessary for the single orb and exceeds this request.
- Replace Chromium with a custom renderer now: no custom renderer technology has been selected, so this would require unrelated design choices and risks replacing the existing sphere implementation.
