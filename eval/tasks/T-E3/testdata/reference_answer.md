The figure is computed locally: token usage from the transcript times the per-model rates you configure. Nothing comes from the provider's billing.

**Rates.** A model is priced by a `[models."<alias>".cost]` table in `~/.craze/native/models.toml` — `input`, `output`, `cache_read`, `cache_write`, in dollars per 1M tokens (`internal/harness/modeltable/modeltable.go:162-181`). `Table.Price(provider, wireModel)` looks the rates up by identity and converts them to integer picodollars per token.

**The walk.** `Session.spend` (`internal/harness/spend.go:98-126`) walks the transcript path from the root to the current leaf and prices every usage record by the model that produced it:

    CostPicoUSD += Input*in + Output*out + CacheRead*cacheRead + CacheCreation*cacheWrite   (spend.go:197)

Reasoning tokens are inside output and are not priced again. It runs again for every `Spent` event and when a session is loaded, so nothing is stored.

**Sub-agents: included.** A child's usage is summed and attached to the parent's tool entry as `SubagentUsage` rows, one per model (`turn.go:1113`), and each row is priced at the child's own model (`spend.go:144-146`). A background child's usage arrives with its results entry. Children emit no `Spent` of their own, so nothing is counted twice.

**Compaction: included.** Every compaction entry carries its usage — successful or failed, with retries summed — and the walk counts it (`spend.go:136-139`). Usage from a step or compaction whose save failed is added from `s.unsaved` while the session is live.

**Unpriced models.** The status row (`internal/tui/status.go:138-152`) reads `34% ctx · <turn> / <session>`:

- When the session's computed cost is zero and some of its usage had no price (`CostPicoUSD == 0 && Unpriced`), it shows billed token counts instead of dollars: `34% ctx · 12.3k / 1.21M tok`. That covers a session on an unpriced model, and one whose priced usage only has zero rates.
- Otherwise it shows dollars, and an amount that includes unpriced usage (a model switch, or a sub-agent on an unpriced model) gets a `+` suffix — "at least this much" — e.g. `$0.04 / $1.20+` (`spendMoney`, `status.go:172-177`). The turn's part can read `$0.00+`.

Amounts are rounded to cents; a tiny non-zero amount shows as `<$0.01`.
