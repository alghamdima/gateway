package main

import (
	"errors"
	"fmt"
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
	upstreamRe = regexp.MustCompile(`(?i)bifrost|maximhq`)
	// shortRe matches the short prefix as its own token: bf_so_x, BF_TEST, bf-sess-1, but not abf_x or 0xbf-1.
	shortRe = regexp.MustCompile(`(^|[^A-Za-z0-9])(bf|BF)([_-])`)
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
//  1. module path and repository: github.com/maximhq/bifrost, maximhq/bifrost
//  2. vendor hosts: docs.getbifrost.ai, www.getbifrost.ai, getbifrost.ai
//  3. wire prefixes: x-bf-, sk-bf-
//  4. the name in its three cases: Bifrost, bifrost, BIFROST
//  5. the short prefix as its own token: bf_, BF_, bf-
//
// Deliberately not rewritten:
//   - the company name "Maxim" on its own (maximum, maximize, maxImages and Minimax would be hit by
//     any loose rule; what is left of it goes with the maxim plugin, which a patch removes)
//   - other maximhq repositories (mcp-go, fasthttp, maxim-go): they are external dependencies
//   - any line that carries a copyright notice
func NewRewriter(b Brand) Rewriter {
	lower, upper := strings.ToLower(b.Name), strings.ToUpper(b.Name)
	repo := b.Module[strings.Index(b.Module, "/")+1:]
	title := strings.ToUpper(b.Prefix[:1]) + b.Prefix[1:]
	replacer := strings.NewReplacer(
		"github.com/maximhq/bifrost", b.Module,
		"maximhq/bifrost", repo,
		"docs.getbifrost.ai", "docs."+b.Domain,
		"docs.getbifrost.io", "docs."+b.Domain,
		"www.getbifrost.ai", "www."+b.Domain,
		"getbifrost.ai", b.Domain,
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
