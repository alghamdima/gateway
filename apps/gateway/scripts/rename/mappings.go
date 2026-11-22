package main

import (
	"errors"
	"fmt"
	"go/scanner"
	"go/token"
	"regexp"
	"strings"
)

// Brand is the set of owner-chosen values every upstream name is rewritten to.
type Brand struct {
	// Name is the product name in PascalCase, used for Go identifiers and UI text: "Acme".
	Name string `json:"name"`
	// Prefix is the short wire prefix that replaces "bf": headers x-<prefix>-*, keys sk-<prefix>-*.
	Prefix string `json:"prefix"`
	// Module is the Go module root that replaces github.com/maximhq/bifrost.
	Module string `json:"module"`
	// Domain replaces getbifrost.ai; docs.<domain> and www.<domain> replace its subdomains.
	Domain string `json:"domain"`
}

var (
	nameRe   = regexp.MustCompile(`^[A-Z][A-Za-z0-9]*$`)
	prefixRe = regexp.MustCompile(`^[a-z][a-z0-9]*$`)
	moduleRe = regexp.MustCompile(`^[a-z0-9.-]+(/[A-Za-z0-9._-]+)+$`)
	domainRe = regexp.MustCompile(`^[a-z0-9-]+(\.[a-z0-9-]+)+$`)
	// upstreamRe matches every upstream name the rewrite acts on. A brand value that contains one
	// would be rewritten again on the next run, so the rewrite would stop being idempotent.
	upstreamRe = regexp.MustCompile(`(?i)bifrost|maximhq|getmaxim`)
	// shortRe matches the short prefix as its own token: bf_so_x, BF_TEST, bf-sess-1, but not abf_x or 0xbf-1.
	shortRe = regexp.MustCompile(`(^|[^A-Za-z0-9])(bf|BF)([_-])`)
	// orgRe matches the upstream GitHub organisation wherever the module mappings did not already
	// consume it (owner:"maximhq", maximhq.github.io), and captures the repositories that stay.
	orgRe = regexp.MustCompile(`maximhq(/(?:mcp-go|fasthttp|maxim-go)\b)?`)
)

var goKeywords = map[string]bool{
	"break": true, "case": true, "chan": true, "const": true, "continue": true, "default": true,
	"defer": true, "else": true, "fallthrough": true, "for": true, "func": true, "go": true,
	"goto": true, "if": true, "import": true, "interface": true, "map": true, "package": true,
	"range": true, "return": true, "select": true, "struct": true, "switch": true, "type": true,
	"var": true,
}

// Validate rejects brand values that would produce invalid Go or a rewrite that is not idempotent.
func (b Brand) Validate() error {
	var errs []error
	if !nameRe.MatchString(b.Name) {
		errs = append(errs, fmt.Errorf("name %q must be PascalCase letters and digits", b.Name))
	}
	if goKeywords[strings.ToLower(b.Name)] {
		errs = append(errs, fmt.Errorf("name %q lowercases to a Go keyword and cannot be a package name", b.Name))
	}
	if !prefixRe.MatchString(b.Prefix) || b.Prefix == "bf" {
		errs = append(errs, fmt.Errorf("prefix %q must be lowercase letters and digits, and not \"bf\"", b.Prefix))
	}
	if !moduleRe.MatchString(b.Module) {
		errs = append(errs, fmt.Errorf("module %q must look like host/org/repo", b.Module))
	}
	if !domainRe.MatchString(b.Domain) {
		errs = append(errs, fmt.Errorf("domain %q must be a bare hostname", b.Domain))
	}
	for field, v := range map[string]string{"name": b.Name, "module": b.Module, "domain": b.Domain} {
		if upstreamRe.MatchString(v) {
			errs = append(errs, fmt.Errorf("%s %q contains an upstream name", field, v))
		}
	}
	return errors.Join(errs...)
}

// Rewriter turns upstream names in a string into the brand's.
type Rewriter func(string) string

// NewRewriter builds the rewrite for a brand. Mappings run most specific first, in one pass, so the
// output of one mapping is never fed to another:
//
//  1. module path and repository: github.com/maximhq/bifrost, maximhq/bifrost; then the organisation
//     on its own (owner:"maximhq", maximhq.github.io), except in front of a repository we depend on
//  2. vendor hosts: docs.getbifrost.ai, www.getbifrost.ai, getbifrost.ai, and the company site
//     getmaxim.ai, whose /bifrost pages are the product's own
//  3. wire prefixes: x-bf-, sk-bf-
//  4. the name in its three cases: Bifrost, bifrost, BIFROST
//  5. the short prefix as its own token: bf_, BF_, bf-
//
// NewGoRewriter adds one mapping for Go source: the short prefix at the start of an identifier
// (BfMode, bfCtx). It has to move together with mapping 5, because the ORM derives the column name
// bf_mode from the field BfMode while raw SQL in the same package spells the column out.
//
// Deliberately not rewritten:
//   - the company name "Maxim" on its own (maximum, maximize, maxImages and Minimax would be hit by
//     any loose rule; the plugin that carried most of it is removed by patches/0003)
//   - other maximhq repositories (mcp-go, fasthttp, maxim-go): they are external dependencies
//   - any line that carries a copyright notice
func NewRewriter(b Brand) Rewriter {
	lower, upper := strings.ToLower(b.Name), strings.ToUpper(b.Name)
	repo := b.Module[strings.Index(b.Module, "/")+1:]
	org := repo[:strings.Index(repo, "/")]
	title := strings.ToUpper(b.Prefix[:1]) + b.Prefix[1:]
	replacer := strings.NewReplacer(
		"github.com/maximhq/bifrost", b.Module,
		"maximhq/bifrost", repo,
		"docs.getbifrost.ai", "docs."+b.Domain,
		"docs.getbifrost.io", "docs."+b.Domain,
		"www.getbifrost.ai", "www."+b.Domain,
		"getbifrost.ai", b.Domain,
		"www.getmaxim.ai/bifrost", "www."+b.Domain,
		"getmaxim.ai/bifrost", b.Domain,
		"www.getmaxim.ai", "www."+b.Domain,
		"getmaxim.ai", b.Domain,
		"x-bf-", "x-"+b.Prefix+"-",
		"X-Bf-", "X-"+title+"-",
		"X-BF-", "X-"+strings.ToUpper(b.Prefix)+"-",
		"sk-bf-", "sk-"+b.Prefix+"-",
		"Bifrost", b.Name,
		"bifrost", lower,
		"BIFROST", upper,
	)
	short := func(s string) string {
		return shortRe.ReplaceAllStringFunc(s, func(m string) string {
			i := strings.Index(m, "bf")
			if i < 0 {
				i = strings.Index(m, "BF")
				return m[:i] + strings.ToUpper(b.Prefix) + m[i+2:]
			}
			return m[:i] + b.Prefix + m[i+2:]
		})
	}
	line := func(s string) string {
		if strings.Contains(s, "Copyright") || strings.Contains(s, "copyright") {
			return s
		}
		s = replacer.Replace(s)
		s = orgRe.ReplaceAllStringFunc(s, func(m string) string {
			if m != "maximhq" {
				return m // a dependency we keep
			}
			return org
		})
		// shortRe consumes the character before the token, so two tokens that share it ("bf_bf_")
		// need a second look
		for {
			next := short(s)
			if next == s {
				return s
			}
			s = next
		}
	}
	return func(s string) string {
		lines := strings.SplitAfter(s, "\n")
		for i, l := range lines {
			lines[i] = line(l)
		}
		return strings.Join(lines, "")
	}
}

// NewGoRewriter is NewRewriter plus the identifier mapping that only Go source needs. It works on
// identifier tokens, not on text, so string literals (test fixtures hold base64 and hashes such as
// "wacedBfSgf") and comments are never touched by it, and an identifier is renamed the same way at
// every use. Inside an identifier it renames "Bf" at a word start (BfMode, streamBfCtx) and a leading
// "bf" before a capital (bfCtx). Source that does not scan is returned with only the general rewrite.
func NewGoRewriter(b Brand) Rewriter {
	base := NewRewriter(b)
	title := strings.ToUpper(b.Prefix[:1]) + b.Prefix[1:]
	ident := func(name string) string {
		var out strings.Builder
		for i := 0; i < len(name); i++ {
			rest := name[i:]
			wordEnd := len(rest) == 2 || (len(rest) > 2 && isUpper(rest[2]))
			switch {
			case strings.HasPrefix(rest, "Bf") && wordEnd && (i == 0 || !isUpper(name[i-1])):
				out.WriteString(title)
				i++
			case i == 0 && strings.HasPrefix(rest, "bf") && len(rest) > 2 && isUpper(rest[2]):
				out.WriteString(b.Prefix)
				i++
			default:
				out.WriteByte(name[i])
			}
		}
		return out.String()
	}
	return func(s string) string {
		src := []byte(base(s))
		fset := token.NewFileSet()
		file := fset.AddFile("", fset.Base(), len(src))
		var sc scanner.Scanner
		failed := false
		sc.Init(file, src, func(token.Position, string) { failed = true }, 0)
		var out strings.Builder
		last := 0
		for {
			pos, tok, lit := sc.Scan()
			if tok == token.EOF {
				break
			}
			if tok != token.IDENT {
				continue
			}
			renamed := ident(lit)
			if renamed == lit {
				continue
			}
			off := file.Offset(pos)
			out.Write(src[last:off])
			out.WriteString(renamed)
			last = off + len(lit)
		}
		if failed {
			return string(src)
		}
		out.Write(src[last:])
		return out.String()
	}
}

func isUpper(c byte) bool { return c >= 'A' && c <= 'Z' }
