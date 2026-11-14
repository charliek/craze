# Orchestrator fixtures, v1

What `craze models --json` (`models.json`, `model-*.json`) and `craze new --wait --json` (`new-wait-*.json`,
`wait-result-warnings.json`, and `wait-result-web-search.json`: a native result with web search offered and used) print,
generated from a small synthetic catalog by `internal/cli`'s
`TestOrchestratorFixtures`, which regenerates and compares them (`-update` rewrites them). `crazeVersion` is a fixed
`0.0.0-fixture`. The models document's `schema` is `1`, an integer that only increases. A consumer must ignore keys
it does not know, in every object here.
