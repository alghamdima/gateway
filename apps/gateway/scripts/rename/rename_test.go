package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var acme = Brand{Name: "Acme", Prefix: "ac", Module: "github.com/acme-ai/acme", Domain: "acme.example"}

func TestRewrite(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		// module path and repository
		{"import", `import "github.com/maximhq/bifrost/core/schemas"`, `import "github.com/acme-ai/acme/core/schemas"`},
		{"versioned module", `module github.com/maximhq/bifrost/transports/v2`, `module github.com/acme-ai/acme/transports/v2`},
		{"repository url", `https://github.com/maximhq/bifrost/issues/12`, `https://github.com/acme-ai/acme/issues/12`},
		{"image name", `docker.io/maximhq/bifrost:latest`, `docker.io/acme-ai/acme:latest`},
		{"npm name", `"name": "@maximhq/bifrost-ui"`, `"name": "@acme-ai/acme-ui"`},
		// external dependencies under the same organisation stay
		{"mcp-go fork", `github.com/maximhq/mcp-go v0.4.0`, `github.com/maximhq/mcp-go v0.4.0`},
		{"fasthttp fork", `replace github.com/valyala/fasthttp => github.com/maximhq/fasthttp v1.0.0`, `replace github.com/valyala/fasthttp => github.com/maximhq/fasthttp v1.0.0`},
		{"maxim sdk", `github.com/maximhq/maxim-go v0.1.0`, `github.com/maximhq/maxim-go v0.1.0`},
		// vendor hosts
		{"docs host", `see https://docs.getbifrost.ai/plugins/x`, `see https://docs.acme.example/plugins/x`},
		{"www host", `"$schema": "https://www.getbifrost.ai/schema"`, `"$schema": "https://www.acme.example/schema"`},
		{"bare host", `https://getbifrost.ai/datasheet`, `https://acme.example/datasheet`},
		// the name in its three cases
		{"go identifier", `func (r *BifrostRequest) x(ctx *BifrostContext) *BifrostError`, `func (r *AcmeRequest) x(ctx *AcmeContext) *AcmeError`},
		{"package", `package bifrost`, `package acme`},
		{"metric", `bifrost_upstream_latency_seconds`, `acme_upstream_latency_seconds`},
		{"env var", `os.Getenv("BIFROST_ENCRYPTION_KEY")`, `os.Getenv("ACME_ENCRYPTION_KEY")`},
		{"ui text", `<title>Bifrost</title>`, `<title>Acme</title>`},
		{"inside a word", `isBifrostError, bifrostmcp.go`, `isAcmeError, acmemcp.go`},
		// wire prefixes
		{"header", `req.Header.Get("x-bf-vk")`, `req.Header.Get("x-ac-vk")`},
		{"header canonical", `X-Bf-Vk and X-BF-VK`, `X-Ac-Vk and X-AC-VK`},
		{"key prefix", `const prefix = "sk-bf-"`, `const prefix = "sk-ac-"`},
		// the short prefix as its own token
		{"tool prefix", `"bf_so_my_schema"`, `"ac_so_my_schema"`},
		{"query param", `?bf_mode=1&bf_sub=2`, `?ac_mode=1&ac_sub=2`},
		{"upper", `BF_TEST_HEADER_TOKEN`, `AC_TEST_HEADER_TOKEN`},
		{"dashed fixture", `"bf-sess-123"`, `"ac-sess-123"`},
		{"line start", "bf_call_1", "ac_call_1"},
		{"adjacent tokens", `bf_bf_x`, `ac_ac_x`},
		{"not a token: suffix of a word", `abf_x and subf-1`, `abf_x and subf-1`},
		{"not a token: hex", `0xbf-1 and a1bf_2`, `0xbf-1 and a1bf_2`},
		{"not a token: no separator", `bfs, buf, bf.x`, `bfs, buf, bf.x`},
		// the company name on its own is left alone, so none of these can break
		{"maximum", `maximum, Maximum, MAXIMUM, exclusiveMaximum, maximize, maximal`, `maximum, Maximum, MAXIMUM, exclusiveMaximum, maximize, maximal`},
		{"max + word", `maxImages, maxImageIndex, maxImpactPhrases`, `maxImages, maxImageIndex, maxImpactPhrases`},
		{"minimax", `MinimaxImageGenerationRequest`, `MinimaxImageGenerationRequest`},
		{"maxim product", `Maxim, maxim_config, www.getmaxim.ai`, `Maxim, maxim_config, www.getmaxim.ai`},
		// copyright lines are never edited
		{"copyright", "// Copyright 2025 Bifrost Authors\nvar x = \"bifrost\"\n", "// Copyright 2025 Bifrost Authors\nvar x = \"acme\"\n"},
		// line structure is preserved
		{"crlf and no final newline", "bifrost\r\nBifrost", "acme\r\nAcme"},
	}
	rewrite := NewRewriter(acme)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := rewrite(c.in)
			if got != c.want {
				t.Fatalf("rewrite(%q)\n got %q\nwant %q", c.in, got, c.want)
			}
			if again := rewrite(got); again != got {
				t.Fatalf("not idempotent: second pass turned %q into %q", got, again)
			}
		})
	}
}

func TestBrandValidate(t *testing.T) {
	if err := acme.Validate(); err != nil {
		t.Fatalf("valid brand rejected: %v", err)
	}
	bad := map[string]Brand{
		"lowercase name":         {Name: "acme", Prefix: "ac", Module: acme.Module, Domain: acme.Domain},
		"name with a dash":       {Name: "Ac-me", Prefix: "ac", Module: acme.Module, Domain: acme.Domain},
		"name is a go keyword":   {Name: "Func", Prefix: "ac", Module: acme.Module, Domain: acme.Domain},
		"name has upstream name": {Name: "BifrostTwo", Prefix: "ac", Module: acme.Module, Domain: acme.Domain},
		"prefix unchanged":       {Name: "Acme", Prefix: "bf", Module: acme.Module, Domain: acme.Domain},
		"prefix uppercase":       {Name: "Acme", Prefix: "AC", Module: acme.Module, Domain: acme.Domain},
		"module without org":     {Name: "Acme", Prefix: "ac", Module: "acme", Domain: acme.Domain},
		"module has upstream":    {Name: "Acme", Prefix: "ac", Module: "github.com/maximhq/acme", Domain: acme.Domain},
		"domain with scheme":     {Name: "Acme", Prefix: "ac", Module: acme.Module, Domain: "https://acme.example"},
		"domain has upstream":    {Name: "Acme", Prefix: "ac", Module: acme.Module, Domain: "getbifrost.ai"},
	}
	for name, b := range bad {
		if b.Validate() == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, root, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestRunOnTree(t *testing.T) {
	root := t.TempDir()
	license := "Apache License\nCopyright Maxim / Bifrost\n"
	binary := "PNG\x00bifrost\x00"
	write(t, root, "LICENSE", license)
	write(t, root, "THIRD_PARTY_NOTICES.md", "bifrost depends on ...\n")
	write(t, root, "UPSTREAM", "repo: https://github.com/maximhq/bifrost.git\n")
	write(t, root, "patches/0001-x.patch", "--- a/transports/bifrost-http/main.go\n")
	write(t, root, "scripts/import.sh", "# imports maximhq/bifrost\n")
	write(t, root, "ui/node_modules/x/index.js", "bifrost\n")
	write(t, root, "ui/public/bifrost-logo.png", binary)
	write(t, root, "transports/bifrost-http/main.go", "package main\n\nimport bifrost \"github.com/maximhq/bifrost/core\"\n")
	write(t, root, "core/mcp/bifrostmcp.go", "package mcp // BifrostMCP\n")
	write(t, root, "core/plain.go", "package core\n")
	write(t, root, "core/go.sum", "github.com/maximhq/bifrost/core v1.0.0 h1:aaa=\ngithub.com/maximhq/bifrost/core v1.0.0/go.mod h1:bbb=\ngithub.com/maximhq/mcp-go v0.4.0 h1:ccc=\ngithub.com/x/y v1.0.0 h1:ddd=\n")
	if err := os.Chmod(filepath.Join(root, "core/plain.go"), 0o755); err != nil {
		t.Fatal(err)
	}

	dry, err := Run(root, NewRewriter(acme), true)
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, root, "core/mcp/bifrostmcp.go"); got != "package mcp // BifrostMCP\n" {
		t.Fatalf("dry run wrote to disk: %q", got)
	}

	first, err := Run(root, NewRewriter(acme), false)
	if err != nil {
		t.Fatal(err)
	}
	if first != dry {
		t.Fatalf("dry run reported %+v, real run %+v", dry, first)
	}
	// 3 text files rewritten (main.go, bifrostmcp.go, go.sum); 3 names renamed (dir, go file, png)
	if first.FilesRewritten != 3 || first.PathsRenamed != 3 {
		t.Fatalf("unexpected counts: %+v", first)
	}

	want := map[string]string{
		"LICENSE":                      license,
		"THIRD_PARTY_NOTICES.md":       "bifrost depends on ...\n",
		"UPSTREAM":                     "repo: https://github.com/maximhq/bifrost.git\n",
		"patches/0001-x.patch":         "--- a/transports/bifrost-http/main.go\n",
		"scripts/import.sh":            "# imports maximhq/bifrost\n",
		"ui/node_modules/x/index.js":   "bifrost\n",
		"ui/public/acme-logo.png":      binary,
		"transports/acme-http/main.go": "package main\n\nimport acme \"github.com/acme-ai/acme/core\"\n",
		"core/mcp/acmemcp.go":          "package mcp // AcmeMCP\n",
		"core/go.sum":                  "github.com/maximhq/mcp-go v0.4.0 h1:ccc=\ngithub.com/x/y v1.0.0 h1:ddd=\n",
	}
	for rel, content := range want {
		if got := read(t, root, rel); got != content {
			t.Errorf("%s\n got %q\nwant %q", rel, got, content)
		}
	}
	for _, gone := range []string{"transports/bifrost-http", "core/mcp/bifrostmcp.go", "ui/public/bifrost-logo.png"} {
		if _, err := os.Lstat(filepath.Join(root, gone)); err == nil {
			t.Errorf("%s still exists", gone)
		}
	}
	if info, _ := os.Stat(filepath.Join(root, "core/plain.go")); info.Mode().Perm() != 0o755 {
		t.Errorf("file mode changed to %v", info.Mode().Perm())
	}

	second, err := Run(root, NewRewriter(acme), false)
	if err != nil {
		t.Fatal(err)
	}
	if second != (Result{}) {
		t.Fatalf("second run changed something: %+v", second)
	}

	hits, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	// only the dependency we keep is left; skipped files are not scanned
	if len(hits) != 1 || hits[0] != "core/go.sum:1: github.com/maximhq/mcp-go v0.4.0 h1:ccc=" {
		t.Fatalf("unexpected scan hits: %q", hits)
	}
}

func TestRunRefusesToOverwrite(t *testing.T) {
	root := t.TempDir()
	write(t, root, "bifrost.go", "package x\n")
	write(t, root, "acme.go", "package x\n")
	if _, err := Run(root, NewRewriter(acme), false); err == nil {
		t.Fatal("expected an error when the renamed path already exists")
	}
}

func TestPinWorkspace(t *testing.T) {
	root := t.TempDir()
	work := "go 1.27.4\n\nuse (\n\t./core\n\t./plugins/otel\n\t./transports\n)\n"
	write(t, root, "go.work", work)
	write(t, root, "core/go.mod", "module github.com/acme-ai/acme/core\n\ngo 1.27.4\n\nrequire github.com/x/y v1.0.0\n")
	write(t, root, "plugins/otel/go.mod", "module github.com/acme-ai/acme/plugins/otel\n\nrequire (\n\tgithub.com/acme-ai/acme/core v1.16.0\n\tgithub.com/x/y v1.0.0\n)\n")
	write(t, root, "transports/go.mod", "module github.com/acme-ai/acme/transports/v2\n\nrequire (\n\tgithub.com/acme-ai/acme/core v1.16.0\n\tgithub.com/acme-ai/acme/core v1.15.0 // indirect\n\tgithub.com/acme-ai/acme/plugins/otel v1.2.0\n)\n")

	if changed, err := PinWorkspace(root, true); err != nil || !changed {
		t.Fatalf("dry run: changed=%t err=%v", changed, err)
	}
	if got := read(t, root, "go.work"); got != work {
		t.Fatalf("dry run wrote to disk: %q", got)
	}
	if changed, err := PinWorkspace(root, false); err != nil || !changed {
		t.Fatalf("first run: changed=%t err=%v", changed, err)
	}
	want := work + "\n" + replaceMarker + "replace (\n" +
		"\tgithub.com/acme-ai/acme/core v1.15.0 => ./core\n" +
		"\tgithub.com/acme-ai/acme/core v1.16.0 => ./core\n" +
		"\tgithub.com/acme-ai/acme/plugins/otel v1.2.0 => ./plugins/otel\n" +
		")\n"
	if got := read(t, root, "go.work"); got != want {
		t.Fatalf("go.work\n got %q\nwant %q", got, want)
	}
	if changed, err := PinWorkspace(root, false); err != nil || changed {
		t.Fatalf("second run: changed=%t err=%v", changed, err)
	}

	// a bumped requirement replaces the old pin instead of piling up
	write(t, root, "plugins/otel/go.mod", "module github.com/acme-ai/acme/plugins/otel\n\nrequire github.com/acme-ai/acme/core v1.17.0\n")
	if _, err := PinWorkspace(root, false); err != nil {
		t.Fatal(err)
	}
	got := read(t, root, "go.work")
	if !strings.Contains(got, "core v1.17.0 => ./core") || strings.Count(got, "replace (") != 1 {
		t.Fatalf("pins not regenerated: %q", got)
	}
}

func TestPinWorkspaceWithoutGoWork(t *testing.T) {
	if changed, err := PinWorkspace(t.TempDir(), false); err != nil || changed {
		t.Fatalf("changed=%t err=%v", changed, err)
	}
}
