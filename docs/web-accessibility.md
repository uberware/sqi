# Web UI Accessibility Baseline

This document states the accessibility baseline the `sqi-server` web UI commits
to. It is intentionally modest — a floor, not a ceiling — so future contributors
know the target and do not regress it. Anything below counts as a bug; going
further is welcome.

The audience is operators using a keyboard-and-screen-reader workflow or working
in varied lighting on varied displays, not a formal WCAG 2.1 AA certification
effort. The UI now has a login flow (rendered in place of the app shell when the
server has `auth.enabled: true` and no session is present) and a small number of
confirmation dialogs, both of which are in scope for the keyboard and
text-alternative commitments below.

---

## The three commitments

### 1. Everything interactive is reachable and operable by keyboard

- Every control a user can click — nav links, filter pills, sort headers, row
  actions (cancel, retry, enable/disable), bulk-action buttons, pagination, copy
  buttons, the submit form, and the log-viewer controls — must be reachable with
  `Tab`/`Shift+Tab` and operable with `Enter`/`Space`.
- Use real semantic elements: `<button>` for actions, `<a>`/`<NavLink>` for
  navigation, native `<input>`/`<select>` for form controls. Do **not** attach
  click handlers to bare `<div>`/`<span>`. Native elements are focusable and key-
  operable for free; a `<div onClick>` is neither.
- Focus must be visible. Do not remove focus outlines without replacing them with
  an equally clear `:focus-visible` style.
- Interactive elements that toggle or select state expose that state to assistive
  tech (e.g. `aria-pressed` on toggle pills, `aria-current` on the active nav
  link, `aria-sort` on sortable column headers, `aria-checked` on selection
  checkboxes).
- No keyboard traps: opening the slide-in log panel or any popover must not strand
  focus; `Esc`/closing returns focus to a sensible place.

### 2. Color usage meets WCAG AA contrast

- Text and meaningful UI graphics meet the WCAG 2.1 AA contrast minimums against
  their background: **4.5:1** for normal text, **3:1** for large text (≥ 18.66 px
  bold or ≥ 24 px) and for the visual boundary of essential controls and status
  indicators.
- The design tokens in `web/src/styles/tokens.css` are the single source of
  foreground/background pairings. When adding or changing a color, verify the
  resulting pairing meets the ratio above before committing — do not hand-pick
  ad-hoc colors in a component.
- **Color is never the only signal.** Status is always carried by text as well as
  hue (see commitment 3); a red/green distinction must remain intelligible to a
  user who cannot distinguish red from green.

### 3. Status badges and icons have text alternatives

- Status is conveyed by a visible text label, not color alone. The `StatusBadge`
  component renders the status name (`running`, `failed`, `succeeded`, …) as text
  inside the colored pill, so the meaning survives grayscale rendering and screen
  readers.
- The WebSocket `ConnectionStatusBadge` (the colored connection dot) must carry a
  text equivalent — a visible label and/or an accessible name (`aria-label` /
  `title`) such as "Live updates: connected" — so its meaning is not purely the
  dot color.
- Icon-only controls (e.g. a copy button rendered as just an icon) have an
  accessible name via `aria-label` or visually-hidden text. Purely decorative
  icons are hidden from assistive tech with `aria-hidden="true"`.
- Non-text content that conveys information (progress bars, the task-progress
  fraction) also exposes its value as text or an appropriate ARIA attribute
  (e.g. `role="progressbar"` with `aria-valuenow`/`aria-valuemin`/`aria-valuemax`).

---

## What is explicitly out of scope for this baseline

These are not regressions to file against; they are deliberately deferred:

- Formal WCAG 2.1 AA conformance auditing and a published VPAT.
- Full screen-reader walkthroughs of every view with assistive-tech matrices.
- Reduced-motion, high-contrast-mode, and forced-colors theming.
- Localisation / RTL layout.
- Automated accessibility gating in CI (axe-core or similar). Encouraged later;
  not required now.

---

## How contributors uphold this

- **Tab through your change.** Before opening a PR, navigate the affected view
  with the keyboard only. Every control you added must be reachable, operable,
  and show a visible focus ring.
- **Prefer semantic HTML** over ARIA. A correct native element beats a `div`
  patched with `role` and `tabindex`. Reach for ARIA only to fill a genuine gap.
- **Pull colors from tokens** and confirm contrast for any new pairing.
- **Give every non-text indicator a text equivalent** — a label, visually-hidden
  text, or an ARIA value.
- **Spot-check with a screen reader** (VoiceOver on macOS, NVDA on Windows) when
  you add a new interactive pattern.

See [`web-development.md`](web-development.md) for the development workflow and the
`CONTRIBUTING.md` "Web UI contributions" section for the component and testing
conventions these checks fit into.

---

## Headings in rendered Markdown

Product and preset readmes render through `src/components/Markdown.tsx`, which
offsets heading levels rather than emitting them verbatim: `#` becomes `<h3>`,
`##` becomes `<h4>`, and so on, clamped at `<h6>`. Both detail pages already
nest `<h2>` (the product or preset title) under `<h1>` (PageHeader), so an
un-offset `#` would put a second `<h1>` mid-document and break the outline.

Real headings are used rather than visually-styled paragraphs: a bold paragraph
looks like a heading to sighted users and is invisible as structure to a screen
reader.
