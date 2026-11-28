# rename

Rewrites an imported upstream gateway tree to the owner's brand (PLAN.md Step 2.1, ADR 0008). Nothing in the committed tree has been renamed yet. The owner chose the name **Lemru** and the prefix `lm` on 2026-11-25; the rename waits on a domain, which the tool requires to resolve in DNS (see Acceptance). `brand.example.json` holds placeholder values used only to test the tool.

```sh
cd apps/gateway/scripts/rename
GOWORK=off go test ./...
GOWORK=off go run . -brand brand.json -root <gateway dir>            # rewrite in place
GOWORK=off go run . -brand brand.json -root <gateway dir> -dry-run   # count what would change
GOWORK=off go run . -scan -root <gateway dir>                        # list upstream names still present
```

## Brand values

| Field | Replaces | Constraint |
| --- | --- | --- |
| `name` | `Bifrost`, `bifrost`, `BIFROST` | PascalCase letters and digits; its lowercase form becomes a Go package name |
| `prefix` | `bf` in `x-bf-*`, `sk-bf-`, `bf_*`, `BfMode`, `bfCtx` | lowercase letters and digits |
| `module` | `github.com/maximhq/bifrost` | `host/org/repo`; the part after the host also replaces `maximhq/bifrost` in image and npm names |
| `domain` | `getbifrost.ai` and its `docs.` and `www.` subdomains, and the company site `getmaxim.ai` | a bare hostname that **resolves in DNS** (see below) |

## What it does, in order

1. Contents of every text file, in one pass, most specific mapping first (`mappings.go` lists them).
2. In `.go` files only, identifiers that carry the short prefix, by token (`go/scanner`), so string literals and comments are not touched by this step.
3. `go.sum`: drops the checksums of the upstream modules.
4. `.go` files are formatted the way `gofmt` does, because renamed identifiers shift column alignment.
5. File and directory names, deepest first.
6. `go.work`: writes a `replace` block that pins every version at which one workspace module requires another to its directory.

It never touches `LICENSE`, `NOTICE`, `THIRD_PARTY_NOTICES.md`, `UPSTREAM`, `MODIFICATIONS.md`, `scripts/rename/`, `scripts/verify.sh`, binary files, or a line that carries a copyright notice.

## Acceptance

`pnpm gateway:verify [brand.json]` runs the whole acceptance on a scratch copy of the committed tree (`scripts/verify.sh`) and changes nothing in the repo. It first checks that the brand's domain resolves and whether it serves the pricing datasheet.

Last full run: tree at `0c0c338`, placeholder brand with a domain that resolves, 20 minutes, exit 0.

| Check | Result |
| --- | --- |
| Two renames of fresh copies | identical; 2628 files, 16 paths, `go.work` pinned |
| Second run on a renamed tree | 0 files, 0 paths, nothing pinned |
| Scan | 0 lines left; 47 lines allowed (the `maximhq` forks of `mcp-go` and `fasthttp`) |
| `make build LOCAL=1` and `go vet` in 14 modules | pass; the binary is `tmp/acme-http` |
| Boot | `/health` 200, UI title is the new name; `/v1/models` 200, `/api/config` 403 and a 400 chat response carry no upstream name, nor does the boot log |
| Unit tests against the renamed baseline | no regressions |

That run left 6 governance tests out of the comparison, because the placeholder domain does not serve the datasheet they download (`needs-datasheet-host.txt`). With a domain that does not resolve at all the script stops at the start: the catalog loader resolves the default pricing host first, one fixture test in `transports/.../lib` fails, and its package runs into the 20 minute test timeout.

So the brand's domain has to exist in DNS before the rename is accepted, and to get those 6 tests back something has to serve the datasheet there.

## Still open

- `-scan` now passes on a renamed tree: the only names left are the `maximhq` forks of `mcp-go` and `fasthttp`, which are external Go dependencies and are on the allowed list in `main.go` with that reason. A fully white-labeled module graph would need our own forks of both.
- The JSON error body has a field `is_bifrost_error`, which becomes `is_<name>_error`. It is on the wire, so it is worth deciding before launch whether a customer should see the brand there.
- Logo and icon files are renamed but still hold the upstream artwork.
- `docker-compose.yml` at the repo root is outside the tool's root. It names no upstream variable: the shipped config reads `GATEWAY_*` variables through `env.` references.
- The acceptance does not yet send a chat request through a virtual key against a mock upstream (Step 2.4, check 4).
- A renamed tree has not been run under Docker or on Postgres.
