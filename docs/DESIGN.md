# Mammoth Mail UI design reference

The conventions the human UI follows. The stylesheet is
`internal/httpapp/assets/app.css` (embedded, served at `/assets/app.css` with a
content-hash query, linked from `pageTemplate`). Keep this file and that
stylesheet in step.

## Product context

- **Who:** self-hosters and small teams running email for AI agents. They are
  technical but not necessarily email-infrastructure experts.
- **Mode:** an operating tool. Scanability, consistency and safety outrank
  expression; the brand lives in precise details, not decoration.
- **Home is a single dense pane.** The dashboard deliberately shows Inboxes,
  Clients, Domains and Recent messages together. Keep it that way; do not split
  it into separate pages or add a large alerts band. Health on the home page is
  compact (an icon with a tooltip); full error detail belongs on the inbox,
  domain or log page.

## Rules

1. **Colour means something.** Neutral greys and ink for structure. Colour is
   reserved for state:
   - amber (`--mm-warn*`): needs a decision or setup — awaiting approval,
     pending send, provider not configured, rotate a secret;
   - red (`--mm-danger*`): a failure or a destructive action — disconnected,
     failed, delete;
   - green (`--mm-ok*`): healthy or done — approve, sent, copied;
   - blue (`--mm-accent*`): unread counts, links, selection, focus.
   The mammoth orange stays in the logo only.
2. **Buttons look like buttons.** Every action is a filled (primary) or
   bordered (secondary) button. Bare text links are for navigation and inline
   references only. Never "quiet" an action into a text link.
3. **One primary per surface.** Primary = ink fill (`button`, `.btn`).
   Secondary = `.secondary`. Destructive = `.secondary.danger` (bordered red)
   or a red-filled `button.danger` for the confirming step. Icon-only row
   actions are `.secondary.icon-btn` with `title` and `aria-label`; destructive
   icon buttons stay neutral until hover so a table is not a column of red.
4. **Approvals state exactly what is authorised.** Show who requested it,
   from, to and subject before the decision, and name them in the confirmation
   (`data-confirm`), without promising timing (sending may be paused and queue). The review page reaches Approve only after the full
   message, never via a sticky bar.
5. **Labels are sentence case** ("Add client", "Log out", "Save draft").
6. **No inline colours.** Use tokens. A new rule goes in `app.css`, not in a
   `style=""` attribute, a template `<style>` block, or a JS-injected style.

## Tokens (`:root` in app.css)

| Group | Tokens |
|---|---|
| Type | `--mm-font` (system stack), `--mm-mono`; sizes `xs 12 · sm 13 · md 14 · base 15 · lg 17 · xl 20 · 2xl 24` px |
| Surfaces | `--mm-bg` page, `--mm-surface` cards/dialogs, `--mm-subtle` hover/inset, `--mm-selected` active nav |
| Lines | `--mm-divider` row rules, `--mm-border` card edges, `--mm-border-strong` button edges, `--mm-control` input edges (≥3:1) |
| Text | `--mm-text`, `--mm-text-2` secondary, `--mm-text-3` muted (≥4.5:1), `--mm-text-4` icons/placeholder only |
| Action | `--mm-ink`, `--mm-ink-hover`, `--mm-on-ink`, `--mm-focus` |
| State | `--mm-{accent,ok,warn,danger}` plus `-bg`, `-border`; `--mm-warn-solid` for amber icons |
| Shape | radius `--mm-r-sm 6 · --mm-r 8 · --mm-r-lg 12`; control heights `--mm-ctl-h 34 · --mm-ctl-h-sm 28`; `--mm-shadow-sm`, `--mm-shadow-pop` |

## Components

- **Page header:** logo, account name, live status, storage, then Settings /
  Feedback / Log out as small secondary buttons on one line.
- **Page head row** (`.pagehead`): title + address on the left, toolbar icon
  buttons on the right.
- **Cards** (`.card`, `.card-head`): white, 12 px radius, 1 px border, faint
  shadow; the heading is 17 px semibold with actions on the right. Never nest
  cards.
- **Tables:** 12 px semibold grey headers in sentence case, 1 px row rules,
  tabular numerals. Dense dashboard tables use `table.dense`. At ≤900 px the
  inbox and activity tables reflow into stacked rows; other wide tables scroll
  inside `.table-wrap`.
- **Mail list** (`.mailrow`): unread rows are white and bold with a blue dot;
  read rows sit on a faint grey. On phones a row shows the sender + date, then a
  two-line subject.
- **Pills:** `.pill` neutral, `.pill.amber`, `.pill.danger`, `.pill.ok`;
  `.unread-pill` blue, `.pending-pill` amber (links to the drafts queue);
  `.draft-status` for draft state.
- **Health flags** (`.issue-dot`): `data-tone="warn"` amber triangle = setup
  missing; `data-tone="error"` red cross = disconnected. Detail goes in
  `title`/`aria-label`.
- **Banners** (`.banner.warn`, `.banner.danger`), inline `.error` / `.ok`, and
  toast `.notice`. Errors name the problem and the fix.
- **Dialogs:** native `<dialog>`, 12 px radius, pop shadow, actions bottom
  right (primary last), destructive bottom left. Settings-style dialogs use the
  left nav (`.inbox-settings-nav`) with the active item on `--mm-selected`.
- **Forms:** label above the control; `.setting-row` for label/help + control
  pairs in settings; `.check-row` for a checkbox with a sentence;
  `.choice-list` / `.choice-option` for a radio choice with descriptions.
- **Secrets:** shown once in `.secret` (amber), monospace, with Copy where the
  browser allows the clipboard (secure contexts only).

## States and accessibility

- Focus: a 2 px `--mm-focus` outline on `:focus-visible` everywhere; inputs
  also get a soft focus ring.
- Every control has hover, focus, disabled and (where relevant) loading
  states; disabled buttons drop to 45% opacity.
- Empty states say what is missing and where to start.
- Motion: 120–160 ms state transitions only; every animation has a
  `prefers-reduced-motion` fallback.
- Check each screen at 1440, 834 and 390 px wide with long addresses and
  subjects; nothing may overflow the viewport horizontally.
