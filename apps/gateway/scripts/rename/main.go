// Command rename rewrites an imported upstream gateway tree to the owner's brand: file contents,
// then file and directory names. It is deterministic and idempotent: the same tree and brand always
// give the same result, and a second run changes nothing (ADR 0008).
//
// Usage:
//
//	go run . -brand brand.json -root <gateway dir>          rewrite in place
//	go run . -brand brand.json -root <gateway dir> -dry-run  report what would change
//	go run . -scan -root <gateway dir>                       list upstream names still present
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// skipNames are never opened or renamed: licence texts must stay as upstream wrote them, and the
// rest is ours or generated.
var skipNames = map[string]bool{
	"LICENSE": true, "NOTICE": true, "THIRD_PARTY_NOTICES.md": true, "UPSTREAM": true,
	"MODIFICATIONS.md": true, ".git": true, "node_modules": true,
}

// skipPaths are relative to the root: our own tooling, which names upstream on purpose.
var skipPaths = map[string]bool{"patches": true, "scripts": true}

const upstreamModule = "github.com/maximhq/bifrost"

// scanRe is the acceptance scan from PLAN.md Step 2.4, check 5.
var scanRe = regexp.MustCompile(`(?i)bifrost|maximhq|getmaxim|x-bf-|sk-bf-`)

// Result counts what a run changed.
type Result struct {
	FilesRewritten int
	PathsRenamed   int
}

func main() {
	brandPath := flag.String("brand", "", "brand JSON file (see brand.example.json)")
	root := flag.String("root", "", "gateway directory to rewrite")
	dryRun := flag.Bool("dry-run", false, "report changes without writing them")
	scan := flag.Bool("scan", false, "list upstream names still present under -root and exit 1 if any")
	flag.Parse()
	if *root == "" {
		fatal("-root is required")
	}
	if *scan {
		hits, err := Scan(*root)
		if err != nil {
			fatal(err.Error())
		}
		for _, h := range hits {
			fmt.Println(h)
		}
		fmt.Fprintf(os.Stderr, "%d lines or paths still carry an upstream name\n", len(hits))
		if len(hits) > 0 {
			os.Exit(1)
		}
		return
	}
	raw, err := os.ReadFile(*brandPath)
	if err != nil {
		fatal(err.Error())
	}
	var brand Brand
	if err := json.Unmarshal(raw, &brand); err != nil {
		fatal(err.Error())
	}
	if err := brand.Validate(); err != nil {
		fatal(err.Error())
	}
	res, err := Run(*root, NewRewriter(brand), NewGoRewriter(brand), *dryRun)
	if err != nil {
		fatal(err.Error())
	}
	pinned, err := PinWorkspace(*root, *dryRun)
	if err != nil {
		fatal(err.Error())
	}
	fmt.Printf("files rewritten: %d, paths renamed: %d, go.work pinned: %t\n", res.FilesRewritten, res.PathsRenamed, pinned)
}

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, "rename:", msg)
	os.Exit(1)
}

// walk calls fn for every file and directory under root that the rewrite may touch, in lexical order.
func walk(root string, fn func(path string, d fs.DirEntry) error) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if skipNames[d.Name()] || skipPaths[filepath.ToSlash(rel)] {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		return fn(path, d)
	})
}

func isBinary(data []byte) bool {
	return bytes.IndexByte(data[:min(len(data), 8000)], 0) >= 0
}

// Run rewrites contents first and names second, so a failure leaves paths that still match upstream.
// rewriteGo is used for the contents of .go files, rewrite for everything else and for names.
func Run(root string, rewrite, rewriteGo Rewriter, dryRun bool) (Result, error) {
	var res Result
	var paths []string
	err := walk(root, func(path string, d fs.DirEntry) error {
		paths = append(paths, path)
		if !d.Type().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if isBinary(data) {
			return nil
		}
		text := string(data)
		if d.Name() == "go.sum" {
			text = dropUpstreamSums(text)
		}
		if filepath.Ext(path) == ".go" {
			text = rewriteGo(text)
		} else {
			text = rewrite(text)
		}
		if text == string(data) {
			return nil
		}
		res.FilesRewritten++
		if dryRun {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		return os.WriteFile(path, []byte(text), info.Mode().Perm())
	})
	if err != nil {
		return res, err
	}
	// deepest first, so a directory is renamed after everything inside it
	sort.Sort(sort.Reverse(sort.StringSlice(paths)))
	for _, path := range paths {
		dir, name := filepath.Split(path)
		renamed := rewrite(name)
		if renamed == name {
			continue
		}
		target := filepath.Join(dir, renamed)
		if _, err := os.Lstat(target); err == nil {
			return res, fmt.Errorf("cannot rename %s: %s already exists", path, target)
		}
		res.PathsRenamed++
		if dryRun {
			continue
		}
		if err := os.Rename(path, target); err != nil {
			return res, err
		}
	}
	return res, nil
}

// dropUpstreamSums removes the checksums of the upstream modules from a go.sum. After the rewrite
// those modules exist only in this workspace, and a renamed line would name a module no proxy serves.
func dropUpstreamSums(sum string) string {
	lines := strings.SplitAfter(sum, "\n")
	kept := lines[:0]
	for _, l := range lines {
		if !strings.HasPrefix(l, upstreamModule+"/") && !strings.HasPrefix(l, upstreamModule+" ") {
			kept = append(kept, l)
		}
	}
	return strings.Join(kept, "")
}

// Scan returns "path:line: text" for every line, and "path" for every name, that still carries an
// upstream name.
func Scan(root string) ([]string, error) {
	var hits []string
	err := walk(root, func(path string, d fs.DirEntry) error {
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if scanRe.MatchString(d.Name()) {
			hits = append(hits, rel)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if isBinary(data) {
			return nil
		}
		for i, l := range strings.Split(string(data), "\n") {
			if scanRe.MatchString(l) {
				hits = append(hits, fmt.Sprintf("%s:%d: %s", rel, i+1, strings.TrimSpace(l)))
			}
		}
		return nil
	})
	return hits, err
}
