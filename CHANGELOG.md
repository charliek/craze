# Changelog

Each release gets its own section headed `## vX.Y.Z — YYYY-MM-DD`, newest first.

## v0.5.0 — 2026-11-24

craze can drive Google's Antigravity CLI, `agy`, on your own Antigravity plan
(the `antigravity` provider): the Gemini 4 models (Neon, Argon, Hydrogen) and
the plan's bundled Claude and GPT-OSS models, in the TUI, `craze new --wait`
and the Claude Code plugin. Nothing in `~/.craze` needs migrating: [Upgrading,
since v0.4.1](docs/getting-started/upgrading.md#since-v041) lists every change
in behaviour; restart a running hub after upgrading.

### Behaviour changes

- **Every provider listing has a fifth row, `antigravity`**, between claude and
  native (`craze providers`, the picker, `/provider`, the hub's create
  options); the unknown-provider and no-default errors name it too.
- **`[agents].antigravity`** in `config.toml` is read: `agy`'s absolute path.
- **`craze new` refuses the calling agent's own provider** on antigravity:
  `ANTIGRAVITY_AGENT` is a third caller marker beside `CLAUDECODE` and
  `CURSOR_AGENT`. Exactly one marker names the caller; with two or more set
  (and no `CRAZE_SESSION_PROVIDER`) nothing is refused.

### Antigravity on your plan

- `agy acp` with cursor's, grok's and claude's chrome: streaming, permission
  and question cards, `run_command` rows, edits and `files`, sub-agent rows,
  the default, accept-edits, auto and plan modes (leaving plan offers
  "Implement the plan above."), `/model` and effort, resume and detached hosts.
  craze never signs in for `agy`; a logged-out `agy` is an `auth` error naming
  `agy acp --login`.
- `--continue` re-applies the session's model and effort, which `agy` resets on
  load; a value it refuses is noted and the session starts anyway.
- `--ask` is refused before the spawn: `agy` has no read-only mode.
- Known limits: a sub-agent row's `completed` means its parent turn ended, and
  a session id `agy` no longer has loads as an empty session.

### The Claude Code plugin

- **`craze:neon`**, a preset on antigravity's `gemini-4-neon` at high effort;
  `craze:delegate` takes `antigravity` targets. Neither takes `mode=ask`.
- The plugin's install in Cursor, Grok and `agy` is documented, and
  `--plugin-dir` is documented as ignored (with a diagnostic) on every
  provider but cursor.

### Tests

- The agent package's fake agents are written once per test binary, fixing an
  `ETXTBSY` flake.
- Two claude-provider tests no longer race their event collector (a macOS
  `-race` flake).

## v0.4.1 — 2026-11-19

Gemini 4 Neon joins the catalog on OpenRouter, and Gemini Pro Latest is
deprecated in its favour. Nothing in `~/.craze` needs migrating: [Upgrading,
since v0.4.0](docs/getting-started/upgrading.md#since-v040) lists the change.

### Behaviour changes

- **Deprecated** (still runs by name, no longer offered):
  `openrouter/gemini-pro-latest` (use `openrouter/gemini-4-neon`). The router
  still answers as Gemini 4 Carbon at $2 / $10; Neon costs half as much again,
  so craze never switches you to it. `openrouter/gemini-4-argon`, already
  deprecated, now names Neon as its replacement.

### The native provider

- **OpenRouter**: Gemini 4 Neon (`openrouter/gemini-4-neon`), Google's fast,
  mid-priced Gemini 4 model for coding and agent work: images, effort `low` to
  `xhigh` (default `medium`), a 65,536 output ceiling, $3 / $15 per million
  tokens ($0.15 cached input). Gemini 4 Hydrogen stays OpenRouter's start model.

### Tests

- `TestALogComesAfterTheSignal` no longer flakes under `-race` on a loaded
  runner: its commands survive SIGTERM, it runs the reap rounds on a test's
  bound, and it checks the whole log order.

## v0.4.0 — 2026-11-17

craze can drive your own Claude Code on your Claude Pro or Max plan (the
`claude` provider, through `claude-agent-acp`), and the gx provider is gone.
Native web search retries Exa's rate limits, takes a key stored mid-session,
asks with a three-way card and gains `/websearch`, `domains` and
`num_results`. The model catalog adds GPT-6.2 Sol and Astra (on the ChatGPT
plan and an OpenAI key), Gemini 4 Hydrogen and Qwen4 Max, and deprecates three
models. [Upgrading, since
v0.3.0](docs/getting-started/upgrading.md#since-v030) lists every change in
behaviour. Nothing in `~/.craze` needs migrating; restart a running hub after
upgrading.

### Behaviour changes

- **gx is gone**: `--provider gx` is refused (exit 2), and `provider = "gx"` or
  `CRAZE_PROVIDER=gx` warns and falls back; sessions that ran on gx stay in the
  index but are not offered.
- **Every provider listing has a fourth row, `claude`**, between grok and
  native (`craze providers`, the picker, `/provider`, the hub's create
  options).
- **`craze new` refuses the calling agent's own provider** (exit 2, kind
  `usage`; `--allow-same-provider` overrides), read from the new
  `CRAZE_SESSION_PROVIDER` every ACP agent's environment carries.
- **Deprecated** (still run by name, no longer offered): `openai/gpt-6.1-sol`
  (use `openai/gpt-6.2-sol`), `openai/gpt-6-astra` (use
  `openai/gpt-6.2-astra`) and `openrouter/gemini-flash-latest` (use
  `openrouter/gemini-4-hydrogen`).
- **Start models**: the ChatGPT plan starts on `gpt-6.2-sol` where the account
  lists it, an OpenAI key on `openai/gpt-6.2-sol`, and OpenRouter on
  `openrouter/gemini-4-hydrogen`.
- **The web search consent card has three options** (Always allow, Allow for
  this session, Disable web search): a program answering it by index moves a
  disable from the second option to the third.

### Claude on your plan

- A `claude` provider with cursor's and grok's chrome: streaming, permission,
  question and plan cards, todos, sub-agents, modes, `/model` and effort,
  resume, detached hosts and `craze new --wait`. craze holds no credential and
  reads nothing under `~/.claude`; fast mode, which bills extra usage, is kept
  off.
- An overage guard: on any extra-usage signal the turn ends as `quota`, the
  adapter is stopped and a machine-wide record refuses claude starts until the
  reset. The account's own extra-usage setting stays the only hard guarantee.
- The usage section reports a claude session's plan, window and utilization; a
  question takes free text, and can be cancelled.
- The Claude Code plugin and its skill are provider-neutral.

### The native provider

- **Models**: GPT-6.2 Sol and Astra on the ChatGPT plan (its model list is now
  asked for as client 0.166.0) and on an OpenAI key (`openai/gpt-6.2-sol`,
  `openai/gpt-6.2-astra`, $8 / $40 for Astra); `openrouter/gemini-4-hydrogen`
  and `openrouter/qwen4-max`; `openrouter/gpt-astra-latest` repriced for
  GPT-6.2 Astra. The plugin's `craze:sol` and `craze:astra` run the GPT-6.2
  models.
- A `--model chatgpt/…` the cached plan list lacks fetches a stale list before
  the start answers, so the first start after an upgrade finds a new plan
  model.
- **Web search**: rate-limited calls are retried (up to three attempts, with
  `Retry-After`), at most four calls per session are at Exa at once, and the
  model reads failures in plain words; a key stored mid-session is used from the
  next turn; `/websearch` shows and sets it; `websearch` takes `domains` and
  `num_results`.

### Fixes

- A cancelled or closed ChatGPT sign-in's callback port stops taking
  connections at once (it could linger until the server's goroutine started).
- A tool still running when a turn fails is shown failed.
- `/websearch`'s note no longer says "host elsewhere".

## v0.3.0 — 2026-11-15

Native sessions can search the web and fetch pages (`websearch`, `webfetch`,
through Exa, asked once before first use), OpenAI API keys join the native
providers, each model maker's models come from one source you can order, and
Fireworks starts on DeepSeek V4.1 Flash 1017. The waiting commands become a
contract a program can drive unattended: typed failures it can branch on and
retry, a session that ends with its turn, a session file that names the session
before the result, `stop` and `cancel` safe to send twice, and prompts up to 512
KiB. A program that reads `craze new --wait --json`, `craze send --wait --json`
or `craze wait --json` needs changes: [Upgrading, since
v0.2.0](docs/getting-started/upgrading.md#since-v020) lists each one, with the
other behaviour changes. Nothing in `~/.craze` needs migrating; restart a running
hub after upgrading.

### Behaviour changes

- The waiting commands' JSON: `error` is one object, `{message, kind, retryable,
  detail?}`, on a result and on an exit-1 failure alike (it was a string on a
  result); a `failed` result always has one, and the plain footer's `stopped:`
  line is folded into `error:`. `stopReason` is on every terminal status,
  `model` is left out until it is resolved, and `planEnded` is new.
- A turn a stop cuts short (`craze stop`, a `SIGTERM` to the host) is a
  `cancelled` result, exit 3, with what it did and spent so far, `stopReason:
  stopped` and `usageComplete: false` when a step was in flight. It was a
  failure, exit 1. A killed host is told from a stop and from a lost connection.
- `craze stop` on a session that has ended is exit 0 (`already_ended`); it was
  exit 1.
- `files` lists only successful edits inside the workspace, relative to it;
  edits elsewhere move to `outsideFiles`, the session's plan file to `planFile`.
- Unattended hosts (`craze new --wait`, and `craze send` reloading one) idle out
  after `host_idle_exit_unattended`, ten minutes by default, and an open
  permission ask no longer holds them up. They stayed for `host_idle_exit`, an
  hour by default.
- A usage error under `--json` prints an `{"error":{…}}` object on stdout, and
  plain `craze new --json` gains `kind`.
- A native host killed outright has its shell commands ended (`SIGTERM`, then
  `SIGKILL`) by the waiter that sees it go or by the next craze command; every
  native command has `CRAZE_HOST_ID` in its environment.
- A native session treats an exported `EXA_API_KEY` as a key in every
  configuration, `catalog = false` included: the commands it runs are started
  without it, and its value is redacted from tool output.

### Sessions and scripting

- `--exit-after-turn` on `craze new --wait` and `craze send --wait` ends the
  session with its turn: no other prompt is admitted, and the host is gone by
  the command's exit.
- `--session-file <path>` writes the session's identity (`session`, `short`,
  `hostPid`, `hostId`) before the prompt is sent, and the round's `handle` once
  it is taken.
- `craze stop --json` and `craze cancel --json`, and `craze cancel --turn
  <turn>`, which cancels that turn only while it is current, so a cancel sent
  again never reaches the next queued turn.
- `craze ps --json --brief` leaves out each session's model and mode
  catalogs.
- Failure kinds: each `error` names one of a closed set of kinds and whether it
  may be retried (`provider`, `stopped`, `host_exited`, `connection_lost`,
  `unobserved`, `prompt`, `config` and the rest); cursor's and grok's refusal of
  `--model` reads `the agent refused model "x": <the agent's words>`, kind
  `model_not_found`.
- A native step that fails mid-stream (an HTTP/2 reset, an unexpected EOF, a
  server error event) is sent again from the same input, once by default
  (`provider_retries`, 0 to 3). The result gains `retries`; `craze prompt
  --json` gains a `step_retry` line, and the control protocol a `step_retry`
  event and `retracted` entries.
- `craze new --wait` and `craze send` take a prompt of up to 512 KiB, from
  words, `--prompt-file` or stdin (was 32 KiB, #111). The bound fits the
  socket's 4 MiB request line with any JSON escaping; over it is still exit 2,
  kind `prompt`.
- A `craze send` over 32 KiB to a session that is running a turn would have to
  wait in the session's queue, which still holds 32 KiB a row: the session
  refuses it, exit 1, kind `prompt`, nothing queued, in words of its own (`it
  is over 32 KiB, the most a prompt can wait in the session's queue, and the
  session was busy; send it once the turn has ended`). On v0.2.0 any prompt
  over 32 KiB was refused before anything was sent, a usage error, exit 2.
- The CLI reference has a new section, "A waiter run detached", for a program
  that drives craze from elsewhere: run each round's waiter in a session of its
  own with its result and exit code in files (the exit file marking the round's
  end, and the round's pid telling a running round from a lost waiter; macOS,
  which has no `setsid` binary, calls `setsid(2)` from the driving program),
  `craze cancel` and `craze stop` to end a round or the session, no
  `--timeout`, plain `craze wait` to read a
  lost round, `CRAZE_RUNTIME_DIR` on a machine people log in to, and the last
  round's `usage.session` as the session's total (a floor when a step was cut
  short, sent again or unpriced). End-to-end tests run it that
  way: detached, no terminal, across a host that idled out and was loaded
  again, and a later `craze send --wait` round that a plain `craze wait` reads
  without owning and `craze cancel` ends.
- `craze auth login` from a pipe takes a key with no trailing newline (it
  always did; now tested and documented).

### The native provider

- Web search: `websearch` and `webfetch` search the web and fetch pages through
  Exa's hosted endpoint, with no key needed (`EXA_API_KEY` raises its rate
  limit). Decided when a session opens: `websearch = false` in `config.toml`
  turns it off and a `[websearch]` table with `provider = "exa"` on; otherwise
  the answer remembered in `native/websearch.json`; otherwise a session whose
  questions reach a person asks once, on the first call (**Allow via Exa**,
  **Disable web search**, both remembered; a skip declines for the session).
  A session that answers its own questions (`craze prompt`, `craze new
  --wait`, an unattended host) never asks and is offered no web tools until
  someone has decided, so its requests are unchanged. A sub-agent gets them
  only when its parent opened on. The file tools refuse `websearch.json`.
- `--web-search` and `--no-web-search` on `craze new --wait`, `craze prompt`
  and `craze serve` decide web search for one session: `--no-web-search`
  withholds the tools whatever the home says (never refused, a no-op off
  native); `--web-search` offers them over a remembered Disable with no
  question and nothing remembered, and is refused — exit 1, kind `config` —
  by an explicit off in `config.toml` or a provider other than native. Under
  `craze new --wait` the refusal comes before the prompt is read, with
  `detail.reason` `web_search_not_offered` (no `detail.model`), and wins over
  a model refusal; `craze prompt` reports it as its startup `error` event.
  Both together, or either on plain `craze new` or with `--load`, are usage
  errors. The choice is recorded on the session's index row (`webSearch`,
  outside `unattended`) and applied on every load: a `craze send` reload, a
  hub reopen (through a hub whose `hello` says `reopenWebSearch`: an older
  hub still running is reported, not reopened through), `--continue`, the
  resume picker and the session list. A
  recorded `--web-search` that `config.toml` has since turned off refuses a
  reload nobody attends, and opens a person's resume without web tools, with
  a line saying why.
- A stored Exa key: `craze auth login websearch/exa` (bare `exa` too, unless a
  model provider has that name) keeps one in `native/credentials.toml`
  (`0600`, its own lock, unknown tables and keys kept, read leniently with
  value-free diagnostics); `craze auth logout websearch/exa` removes it, and
  `craze auth list` shows it under a new "Web search" section, even when
  `providers.toml` cannot be read. The search sends `EXA_API_KEY`, else the
  stored key, else none (the keyless endpoint), resolved once when a session
  opens; every native session redacts the stored key, learns one stored while
  it runs at its next turn, and the file tools refuse `credentials.toml`.
- Web search counts: a native session's waiting-command result carries
  `webSearch` — `offered` (always written, `true` or `false`) and the target
  turn's `searches`, `fetches`, `failed`, `urls` (successful results' URLs,
  key scrubbed and redacted, at most 100 of at most 500 bytes) and
  `urlsTruncated` — on every status, never on an exit-1 envelope, and the plain
  footer gains `web: 1 search, 2 fetches (1 failed)`. The counts are activity
  accounting for the turn's window, not a statement of which sources an answer
  used: a background sub-agent's calls count in the turn they finished in, and a
  close is the cutoff. On the wire, a `webSearch` state section (cumulative, a
  replacement, stamped by the client with the running turn), the snapshot's
  `turn.webSearch` with its `turnId`, and the info document's
  `webSearch.offered` (once ready, native only); no codec version moves.
  `craze models --json` and `--shipped --json` gain `capabilities:
  {"webSearch": true}`.
- A shipped model can be deprecated (`deprecated` and `replaced_by` in the
  catalog): it runs when named, is never offered or chosen for you, and each
  `models.toml` reference to it warns. A `[[retired]]` row can name its
  replacement, which a `--model` naming it leads with (`<alias> was retired;
  use <replacement>`) and a dropped reference warns about. A model is retired
  no earlier than the next minor release and 30 days after a release shipped
  it deprecated (RELEASING.md: the person cutting a release checks both;
  `TestDeprecationHistory` checks that an earlier release shipped it
  deprecated).
- `default_context`: a context smaller than the window that a session works
  within — compaction's threshold and the usage report's `contextWindow` are
  measured against it — set by the catalog and per model in `models.toml`
  (`0` turns it off).
- `--model <provider id>/<alias>`: `zai-coding-plan/glm-5.3` is `glm-5.3`.
- The agent tool's model list and its "Unknown model" answer hold to the
  models `/model` offers plus the ones you configured.
- An OpenAI API key: the `openai` provider (`OPENAI_API_KEY`, or `craze auth
  login openai`), last in the start order, with `openai/gpt-6.1-sol` (its
  start), `openai/gpt-6-astra`, `openai/gpt-6.1-terra` and `openai/gpt-6-luna`
  on the Responses API, each compacting at 85% of a 272,000-token context
  default (a trigger, not a cap) and priced above 272,000 at OpenAI's
  long-context rates. An exported `OPENAI_API_KEY` runs its models by name
  (`--model`, `default_model`, the model memory, a sub-agent's model), but
  while it is the provider's only key craze never chooses one of them for you:
  a new session with nothing else funded is refused, kind `config`, naming the
  variable and `craze auth login openai`, which stores the key and lifts that
  (the catalog's `env_key_by_name_only`, on `openai` alone). `env_keys = []`
  opts out of the variable altogether.
- Cache writes on the Responses API routes are counted as cache writes and
  priced at `cache_write`, or the input rate when a model has none.
- On the `openai-responses` driver a 429 naming spent credit or a spend limit
  is final, kind `quota`, and so is the same code or type inside the stream;
  an in-stream overload is retried; a message's phase is kept and replayed for
  the shipped `openai/*` models alone (their transcripts are not readable by an
  older craze; a model of your own on a provider named `openai` keeps none).
- A key provider's 403 says the provider's own message before the key's
  advice, and a 400 or 403 with code `model_not_found` is kind
  `model_not_found` (Anthropic's and the ChatGPT plan's are unchanged).
- A `models.toml` entry that lowers a shipped window below its long-context
  threshold keeps the entry with the tier off, and warns.
- `craze models` lists the native models — provider, status and replacement,
  efforts, context, and whether each is funded and offered here — as a table or
  one JSON object (`schema` 1); `craze models <alias>` reads one as `--model`
  does, `--shipped` answers from the compiled-in catalog alone. Each release
  ships that as `models.json`, listed in `checksums.txt`.
- Catalog refresh: `fireworks/deepseek-v4p1-flash-1017` (Fireworks' start
  now, chosen from the November 2026 start-model evaluation; set
  `default_model = "fireworks/ember-1"` to keep the old one) and
  `fireworks/kimi-k3p1-pro`, `openrouter/gemini-pro-latest`,
  `openrouter/gpt-terra-latest`, `openrouter/minimax-m3.1-flash`,
  `muse-ember-1.0` (a 200,000 context default and Meta's long-context tier) and
  `anthropic/claude-fable-5-2` (priced) are new. Deprecated, so out of the
  pickers but still running by name: `fireworks/kimi-k3`,
  `fireworks/deepseek-v4p1-flash`, `openrouter/gemini-4-argon` and
  `openrouter/minimax-m3`, each naming its replacement. OpenRouter's four GPT
  routers gain OpenAI's 272,000 context default and long-context price; its
  DeepSeek Flash, GLM Flash and GLM routers are priced at the maker's own
  endpoint rate (the listing is the cheapest endpoint's, and requests were
  billed 3 to 9 times it). Retired OpenRouter aliases that the catalog
  replaced name their replacement. A Nexus Auto recipe (`wire_model = "auto"`)
  is in the configuration reference.
- `craze new --wait --provider native` checks `--model`, an explicit `--effort`
  and `--fast` before any host is spawned: an unknown or retired model, an effort
  the model does not list, and `--fast` (no native model has one) fail with exit
  1, kind `config` and a `detail.reason` (`model_unknown`, `model_retired`,
  `effort_not_offered`, `fast_not_offered`). Other commands stay lenient.
- A native result or failure on a deprecated model prints a stderr line and,
  with `--json`, `warnings: [{kind: "model_deprecated", model, replacedBy,
  message}]`; the TUI notes it once when the session opens.
- One source per maker: when OpenAI's, Meta's, GLM's or DeepSeek's models are
  reachable through several providers, the pickers, a new session's start and
  the agent tool's menu offer them from the first funded source in the source
  order (`zai-coding-plan`, `chatgpt`, `meta`, `openai`, `anthropic`,
  `fireworks`, `openrouter`); the others still run by name. `source_order` in
  `models.toml` goes before the catalog's (`[]` turns the rule off). A
  remembered model on a hidden source no longer starts new sessions: the next
  remembered model that is offered does, then the pin, then the provider
  order (`source_order` or `--model` keeps a hidden model as the start).
  While the ChatGPT plan is signed in but offers none of its models (no usable
  model list, another account's list or plan usage off), craze never starts a
  session for you on OpenAI's models from another source, nor on any model
  billed per token (on a provider whose models are priced) of a provider
  after `chatgpt` in the start order or left out of it (Fireworks,
  OpenRouter, Meta, Anthropic, OpenAI in the catalog's): with such a sign-in
  and only those keys the start is refused, kind `config`, naming the sign-in
  and `--model`, and `craze providers` says `needs setup` with the same
  reason. A Z.AI key, a provider of your own none of whose models has a
  price, the model memory, `default_model` and `--model` still start. A sub-agent call's model is held to the
  menu plus what you configured. In the TUI a quota or plan-limit failure is
  followed by a note naming the maker's other funded sources and how to
  continue on one (never part of the failure a waiting command reads); `craze auth
  list`, `craze models` and `/connect` say where each maker's models come from.
  `craze models` rows carry their `maker`, and its table notes the model
  table's warnings. An unknown `--model` under `craze new --wait`, and a miss
  of `craze models <alias>`, say `craze models lists the models` instead of
  listing every one. New: `openrouter/muse-spark-1.3` and
  `openrouter/muse-wonder`, Meta's models on OpenRouter.

### Fixes

- A native sub-agent started after a `providers.toml` key was rotated before
  the session's first turn redacts the key the session opened with too; it
  knew only the new one, so a copy of the old key it read reached its model.

### Maintenance

- Go 1.27.3 (was 1.27.1; `go.mod`'s floor is now `go 1.27.3`) and golangci-lint
  2.15.0 (was 2.13.2, which cannot read Go 1.27.2+'s export data). Its errcheck
  now reports an error received from a channel and dropped; the three places
  that did so wait without discarding a received error.
- Fantasy v0.45.0 (was v0.43.2). Fantasy builds on Charm's fork of the OpenAI
  SDK (`github.com/charmbracelet/openai-go`) from v0.45.0, so
  `github.com/openai/openai-go/v3` leaves the build; the dependency-boundary
  tests forbid the fork as they forbid the SDK. Through it: an
  OpenAI-compatible provider that reports reasoning tokens apart from output
  tokens has them counted in output (and spend), tool messages stay together
  when tool results carry images (craze's own regrouping is unchanged), and
  JSON repair fixes. `bin/craze` is 44,139,178 bytes (was 42,484,176).
- **Held:** Fantasy v0.45.1 to v0.48.0. From v0.45.1 Fantasy no longer retries
  a step because a tool returned an error, and four of the native harness's
  cancel and callback tests fail against it; that is more than a version bump.
- golang.org/x/sys v0.50.0, golang.org/x/image v0.48.0, spf13/pflag v1.1.0,
  kaptinlin/jsonschema v0.9.10 and charmbracelet/x/term v0.2.2; no TUI golden
  or wire fixture moved.
- **Held:** charmbracelet/x/ansi v0.11 (stays v0.10.1), and with it
  charmbracelet/bubbles v1.0.0 (needs it; stays v0.21.0) and
  mattn/go-runewidth (stays v0.0.16). x/ansi v0.11.9 measures some graphemes
  (Devanagari with combining marks, keycap emoji) differently from the
  grapheme walk the TUI copies and highlights with: copying a whole row like
  `नमस्ते hello` dropped its last character, highlighting next to `1️⃣` erased
  the character after it, and a clamped title could overrun its width by a
  cell. A test now holds the TUI's width measures to one another.
- GitHub Actions: actions/checkout v7, actions/setup-go v7, astral-sh/setup-uv
  v10.3.3.
- Python locks: `eval/` takes aiohttp 3.14.5 (three advisories fixed in 3.14.4)
  and pytest 9.2.0; the docs site takes zensical 0.1.1.

## v0.2.0 — 2026-10-14

Effort becomes a control of its own on native models (`/effort`, and a switch
lands on the model's own effort), Meta's models move to its Responses API at
about 0.6 times the spend, saved sessions reopen through the hub, and Gemini 4
Argon joins OpenRouter. This is the release the repository's Claude Code plugin
needs (craze 0.2.0 or newer). Nothing in `~/.craze` needs migrating, but several
existing behaviours changed: [Upgrading, since
v0.1.1](docs/getting-started/upgrading.md#since-v011) lists each one and how to
keep the old behaviour.

### Behaviour changes

- A native `/model` switch lands on the effort you last used on that model, else
  its `default_effort`, instead of carrying the current effort over. Name a
  level to keep one: `/model anthropic/claude-opus-5-5 max`.
- Native Meta models use Meta's Responses API (the new `openai-responses`
  driver): craze replays each step's encrypted reasoning and Meta caches the
  session's prompt. On `muse-spark-1.3-contributor` it passed as many tasks as
  Chat Completions at about 0.6 times the median spend per run. A
  `[providers.meta]` with `driver = "openai-compat"` goes back; an entry of your
  own with a `base_url` and no `driver` now sends Responses requests.
- Haiku 5.5 requests whose prompt is over 100,000 tokens are priced at
  Anthropic's long-context rates, so their spend no longer reads low.
- Plain `craze prompt` puts a blank line between text runs; `--json` is
  unchanged.
- A stopped session leaves the roster, the session list and `craze ps` at once,
  with no `Closing…` row (for hosts started after the upgrade).
- `/model <id> <effort>` matches the effort word as `--effort` does, and refuses
  a word that names two levels. A `models.toml` model with more than 8 efforts,
  or an effort that is not one short printable line, no longer loads.
- The session list offers a saved session by its newest row only.

### The native provider

- **OpenRouter**: Gemini 4 Argon (`openrouter/gemini-4-argon`), with efforts,
  vision and its price. Gemini Flash Latest stays the start model.
- **Meta**: `muse-spark-1.3` offers `max`.
- The `openai-responses` driver serves any Responses API endpoint with an API
  key, and a `models.toml` cost can carry a long-context tier
  (`long_context_above`, `[models."<alias>".cost.long_context]`).

### The terminal UI

- `/effort` opens a picker of the current model's levels; `/effort <level>` sets
  one. The model dialog's effort row follows the highlighted model, and the
  session list's `/effort` offers the chosen model's real levels.
- Shift+Enter starts a new line in Ghostty and Roost.
- A `/model`, `/plan`, `/ask` or `/agent` typed mid-turn says why it did
  nothing, and the draft stays.
- The session list shows each running session's model at 100 columns and wider.
- Reasons, fixes and notes wrap instead of being cut short, and a long path
  breaks after a `/`.

### Sessions and scripting

- `craze ps --saved` lists saved sessions no host runs, and
  `craze new --load <id>` reopens one in a host of its own. A hub client does the
  same with `sessions.saved` and `session.reopen`.
- `craze bridge --hub --no-start` attaches to a running hub and never starts one
  (exit 3 when none runs).
- When an upgrade replaced its binary, a hub from before says how to stop it
  (`kill <pid>`; no session ends), and `craze new` explains an older hub's
  refusal.
- The Claude Code plugin gains a `craze:wonder` preset (`muse-wonder-1.0`).
- The control protocol gains effort facts on native catalog models
  (`capabilities.effortFacts`), the slash catalog's builtin `effort` row,
  `stopping` on a registry entry, and the two saved-session methods. All
  additive: the protocol and codec versions are unchanged.

### Fixes

- A closing native session cancels its own turn before its sub-agents, so it no
  longer asks the model once more with their aborted results.
- A model-dialog switch sends the effort the dialog showed, even when another
  craze process changed the remembered effort since.
- A failed ChatGPT-plan model list fetch backs off only that account.
- The session list keeps ` · here` whole and shortens the directory's name
  instead.

## v0.1.1 — 2026-10-12

Anthropic joins the native providers, `/btw` asks a side question without
touching the session, and new commands drive sessions from a script. Nothing in
`~/.craze` needs migrating, but a few existing behaviours changed: [Upgrading,
since v0.1.0](docs/getting-started/upgrading.md#since-v010) lists each one and
how to keep the old behaviour.

### Behaviour changes

- An exported `ANTHROPIC_API_KEY` now funds craze's Anthropic provider, and is
  kept out of the commands a native session runs. `env_keys = []` under
  `[providers.anthropic]` in `providers.toml` opts out. A
  `[providers.anthropic]` of your own now overlays craze's: rename it to keep a
  gateway.
- `craze -c` and `--continue` skip sub-agent sessions (those `craze new --wait`
  starts). `--resume` lists them after your own, tagged `sub-agent`.
- OpenRouter aliases send OpenRouter's `~family-latest` routers where one
  exists (`openrouter/gpt-sol-latest`), so an alias moves when OpenRouter moves
  its router. The numbered aliases they replace are retired; their wire ids
  still work from `models.toml`.
- A skill saved mid-session reaches the `/` menu at the next turn end, not when
  you type `/`.

### The native provider

- **Anthropic**: Claude models on Anthropic's API with an API key
  (`craze auth login anthropic`, `/connect`, or `ANTHROPIC_API_KEY`). The
  account's model list is fetched at most once a day, so new Claude models
  appear without a craze release. It is last in the start order; a session with
  only an Anthropic key starts on the newest Opus at medium effort.
- Under an Anthropic parent, a sub-agent's `opus`, `sonnet`, `haiku` or `fable`
  picks that family's newest Claude model unless `[subagents.tiers]` maps it.
- **Meta**: Muse Wonder 1.0 (`muse-wonder-1.0`), chosen with `/model`,
  `--model`, `default_model` or a sub-agent's model. Muse Spark stays Meta's
  start model.
- **OpenRouter**: MiMo V2.6 Pro and the `glm-latest` and
  `deepseek-flash-latest` routers. The start model is
  `openrouter/gemini-flash-latest`.
- A turn the model refuses ends with a note naming the refusal category. The
  note is shown but never saved in the session or sent to a model.

### The terminal UI

- `/btw <question>` asks a side question in a full-window view, mid-turn or
  idle, without adding to the session; Esc throws it away. Native streams the
  answer; Grok and gx answer in one piece. Cursor does not offer it.

### Scripting

- `craze new --wait`, `craze send`, `craze wait`, `craze cancel` and
  `craze stop` start, continue, wait on and stop sessions, with an exit code per
  outcome. The Claude Code plugin in this repository is built on them; it is
  supported from v0.2.0.
- `craze new --title` names a session as it starts. A hub still running from
  before the upgrade refuses until it exits idle.
- `craze prompt --effort` and `--fast`/`--no-fast`.
- The control protocol gains `session.aside` (side questions), the `slash`
  catalog section and a `title` on `session.create`. All additive: the protocol
  and codec versions are unchanged.

### Fixes

- `craze wait` no longer reports `completed` when an interrupt or `--timeout`
  ended the wait before it read a returned plan.
- craze no longer mistakes a dead host for a live one while another scan is
  cleaning it up.
- A title is normalized the same way in the session and in the session index
  (zero-width spaces, bidi controls and carriage returns are dropped).

### Docs

- The README is now a short overview that links to the docs site. The site
  gains a [Claude Code sub-agents](docs/getting-started/claude-code.md) page and
  an Upgrading section for changes since v0.1.0, and tests in `make test` fail
  when the reference pages fall behind the code.

## v0.1.0 — 2026-10-05

Two big changes since v0.0.1. craze now has its own agent, the **native**
provider, which runs against a model provider's API or a ChatGPT plan with no
other CLI to install. And sessions now run in a detached host that outlives the
terminal. Cursor, Grok and gx work as before. Nothing in `~/.craze` needs
migrating, but several defaults changed: [Upgrading from
v0.0.1](docs/getting-started/upgrading.md) lists each one and how to turn it
off.

### Breaking changes

- `CRAZE_CONFIG` is removed. Set `CRAZE_HOME` to the **directory** that holds
  `config.toml`. While `CRAZE_CONFIG` is set, every command but `--help` and
  `--version` refuses to run and says so.
- Sessions run in a detached host by default. `detach = false` in
  `config.toml`, or `CRAZE_DETACH=0`, keeps the session in the TUI's own
  process, as v0.0.1 did.
- `--agent-bin` and `CRAZE_AGENT_BIN` now apply only to the launch's own
  provider. To set a provider's binary for good, use `[agents]` in
  `config.toml`.

### The native provider

- `--provider native` runs the agent inside craze. It connects to Fireworks,
  Meta, OpenRouter or the Z.AI Coding Plan with an API key, or to a ChatGPT
  plan (`craze auth login chatgpt`, or `/connect` in the TUI).
  `craze auth login`, `logout` and `list` manage the keys.
- The model catalog ships in the binary, so upgrading craze brings new models;
  `models.toml` changes or adds models. `/model` switches model and effort.
- Where a new session starts: on your last model, else your `default_model`,
  else the start model of the first connected provider in this order:
  - Z.AI Coding Plan: `glm-5.3`
  - ChatGPT plan: its preferred model
  - Fireworks: `fireworks/ember-1`
  - OpenRouter: `openrouter/gemini-3.8-flash`
  - Meta: `muse-spark-1.3-contributor`
- Tools for reading, searching, editing and running commands. A command still
  running at its timeout becomes a background job instead of being killed.
- Agent, plan and ask modes, and foreground and background sub-agents, each
  sub-agent on a model you can choose.
- Your project and user instruction files and your skills and commands are in
  its prompt.
- Resume, `/compact` and automatic compaction, and usage and cost per session.
- Vision models see pasted images and images read from disk.

### Sessions outlive the terminal

- Each session runs in a detached host (`craze serve`). Closing the terminal or
  losing an ssh connection closes only the view.
- `craze -c` or `craze attach` rejoins a session and `craze ps` lists them.
  `craze new` starts one in the background through a per-machine hub, which
  starts and exits on its own.
- A host with no client and nothing to do exits after an hour
  (`host_idle_exit`).
- Every session writes a journal (`journal = false` turns it off), and every
  host has a control socket (`control_socket = false`).

### The terminal UI

- A session list (`←` on an empty composer, or `/sessions`) to switch to,
  start and stop sessions. A new session's directory can be chosen with `@`,
  along with its provider, model, effort and fast mode.
- Images: paste or drag a screenshot, or Ctrl+V an image, and it becomes an
  `[Image #N]` chip. Cursor and Grok receive the image itself.
- `@` file mentions, and `!` shell mode, which runs a command from the
  composer.
- `/model` sets effort and fast mode on each provider that offers them.
- Inside herdr or roost, craze reports idle, working or blocked, and never
  conversation content (`host_status = false` turns it off).

### Scripting

- `craze prompt --json` events now carry a `seq`.
- `craze bridge` relays an SSH client to a session's control socket; the
  socket's protocol is documented in `docs/reference/protocol.md`.
- `craze providers` says which providers can start a session here, and why the
  others cannot.

### Known limits

- Still a proof of concept.
- On macOS, dragging a screenshot into Ghostty inserts its path rather than an
  image chip (#5). Pasting from the clipboard works, and so does dragging in
  roost.
- Intel macOS is untested at runtime.

## v0.0.1 — 2026-09-16

First release. craze is a Linux and macOS terminal UI that speaks ACP to an
agent you already have: **Cursor** (`cursor-agent acp`), **Grok**
(`grok agent stdio`), or **gx**, a third-party fork of the Grok CLI. craze owns
the chrome; the provider still runs the agent. It is a proof of concept —
usable, but the version number is honest.

### Install

Homebrew (`brew install charliek/tap/craze`) and apt
(`https://apt.stridelabs.ai`), both from this release. Apple Silicon and Linux
amd64/arm64 are tested; Intel macOS is cross-compiled and shipped best-effort.

### The terminal UI

- A transcript of rendered entries — markdown, tool rows rewritten in place,
  diffs, todos and streamed assistant text — rather than a debug log.
- A boxed composer that keeps the whole draft on screen, with slash commands
  that open on a token anywhere in the draft.
- Questions, plans and permission requests arrive as cards you answer inline.
- Sub-agent rows for both providers, in spawn order, with a read-only view of
  any child's own transcript.
- Status rows and a clickable mode chip; mouse selection and copy from the
  transcript; wheel scroll separate from PgUp/PgDn.
- Seven themes with a picker that repaints live as you move through it, and a
  themed terminal background (OSC 10/11) that is handed back on exit —
  `background = false` or `--no-background` turns it off.

### Sessions

- `--continue` resumes the last session; `--resume` opens a picker; `/rename`
  names one. The terminal's tab title follows the session.
- A message queue: type while a turn runs, send now, or interject mid-turn.
- Plan mode offers to implement the plan when a plan-mode turn ends.

### Providers

- Cursor, Grok and gx, chosen with `--provider` or a startup picker that only
  offers the ones that actually resolve on your machine.
- Cursor's plugin commands are found and expanded client-side, because its ACP
  server never advertises them.
- Skills are scanned by each agent's own rules rather than one guess at both.

### Headless

- `craze prompt` drives an ACP turn without a TUI, for scripting and tests.
- `craze frame` renders a deterministic frame, which is what the golden suite
  compares.

### Known limits

- Proof of concept: multi-project harness integration is later work.
- Intel macOS is untested at runtime.
- A terminal that honours OSC 11 but not OSC 10 (or that ignores the reset)
  can be left recoloured after an abnormal exit; `background = false` avoids
  the whole mechanism.
