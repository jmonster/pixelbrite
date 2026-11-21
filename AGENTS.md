# Building an adapter

- `pkg/adapter/README.md` is the spec. Follow it over anything else.
- Start from `adapters/tones`. In Go, implement `adapter.Adapter` and the interface for each capability you serve, then call `adapter.Main`.
- Build into the adapter folder, run `pixelbrite-adapter check -setup STEP=VALUE -query TEXT PATH`, and fix what it reports until there are no failures and no skips you didn't mean. A skip usually means a missing `-setup` value or a query with no results. Install the check with `go -C pkg/adapter install ./cmd/pixelbrite-adapter`.
- The check can't catch everything. Also: one adapter per source, named after it. No UI. Don't convert media. Never call the server; it calls you. Keep each member's data separate by the `Member` header, and delete it on `DELETE /member`.
