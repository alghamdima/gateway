# rename

Rewrites an imported upstream gateway tree to the owner's brand (PLAN.md Step 2.1, ADR 0008). Nothing in the committed tree has been renamed yet: no brand is chosen. `brand.example.json` holds placeholder values used only to test the tool.

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

It never touches `LICENSE`, `NOTICE`, `THIRD_PARTY_NOTICES.md`, `UPSTREAM`, `MODIFICATIONS.md`, `patches/`, `scripts/import.sh`, `scripts/rename/`, `scripts/verify.sh`, binary files, or a line that carries a copyright notice.

## Checked on a scratch copy (placeholder brand, tree at `bb19342`)

| Check | Result |
| --- | --- |
| Rewrite | 2620 files, 16 paths, `go.work` pinned |
| Second run | 0 files, 0 paths, nothing pinned |
| `make build LOCAL=1` | exit 0; binary is `tmp/acme-http` |
| Boot | `/health` 200; UI title is the new name; 401, 400 and 403 bodies and the boot log carry no upstream name |
| `go vet`, 15 modules | clean |
| Unit tests against `docs/platform/gateway-baseline/results.tsv`, test names mapped back | identical except the two items below |

The two differences both come from the placeholder domain `acme.example`, which resolves nowhere:

- `transports/.../lib`: `TestLoadConfigServesCatalogFeedsFromFixture` fails and the package then runs into its 20 minute timeout. The loader resolves the default pricing host before the test fixture can answer for it. With a domain that resolves, the package passes 820 of 820, as in the baseline.
- `plugins/governance`: 6 tests skip instead of pass. They download the real datasheet from the default pricing host.

So the brand's domain has to exist in DNS before the rename is accepted, and something has to serve the datasheet there (or the default has to become a file; Step 2.2).

## Acceptance

`pnpm gateway:verify [brand.json]` runs the whole acceptance on a scratch copy (`scripts/verify.sh`): determinism, the scan, build and vet, boot and response checks, and the unit tests against a renamed copy of the baseline. It first checks that the brand's domain resolves.

## Still open

- `-scan` now passes on a renamed tree: the only names left are the `maximhq` forks of `mcp-go` and `fasthttp`, which are external Go dependencies and are on the allowed list in `main.go` with that reason. A fully white-labeled module graph would need our own forks of both.
- The JSON error body has a field `is_bifrost_error`, which becomes `is_<name>_error`. It is on the wire, so it is worth deciding before launch whether a customer should see the brand there.
- Logo and icon files are renamed but still hold the upstream artwork.
- `docker-compose.yml` at the repo root sets `BIFROST_SETUP_TOKEN` and is outside the tool's root; it has to change in the same commit as the rename.
- The acceptance does not yet send a chat request through a virtual key against a mock upstream (Step 2.4, check 4).
- A renamed tree has not been run under Docker or on Postgres.
