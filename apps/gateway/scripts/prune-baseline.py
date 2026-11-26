#!/usr/bin/env python3
"""Drop the rows of docs/platform/gateway-baseline/results.tsv whose test (or package) no longer exists.

Usage, from the repository root: python3 apps/gateway/scripts/prune-baseline.py [--write] [--show [N]]
Run it after deleting tests, so scripts/test.sh does not report them as missing. Without --write it
only counts; --show prints the rows it would drop. Check them before writing: a test that is gone by
mistake looks the same as one that was deleted on purpose.
"""
import os,re,sys,glob
root='apps/gateway'; tsv='docs/platform/gateway-baseline/results.tsv'
modpath={}
for mod in open(f'{root}/go.work').read().split():
    if mod.startswith('./') and os.path.exists(f'{root}/{mod}/go.mod'):
        m=re.search(r'^module\s+(\S+)',open(f'{root}/{mod}/go.mod').read(),re.M).group(1)
        modpath[mod[2:]]=m
cache={}
def tests_in(d):
    if d not in cache:
        names=set(); files=glob.glob(f'{d}/*_test.go')
        for f in files: names|=set(re.findall(r'^func (Test\w+)\(',open(f,errors='ignore').read(),re.M))
        cache[d]=(bool(files),names)
    return cache[d]
keep=[];drop=[]
for line in open(tsv):
    mod,pkg,test,res=line.rstrip('\n').split('\t')
    base=modpath.get(mod)
    if base is None: drop.append(line); continue
    rel=pkg[len(base):].lstrip('/') if pkg.startswith(base) else None
    d=f'{root}/{mod}/{rel}' if rel else f'{root}/{mod}'
    has,names=tests_in(d) if os.path.isdir(d) else (False,set())
    ok = (os.path.isdir(d) and bool(glob.glob(f'{d}/*.go'))) if test=='-' else test in names
    (keep if ok else drop).append(line)
print(f'keep {len(keep)} drop {len(drop)}')
if '--write' in sys.argv: open(tsv,'w').writelines(keep)
if '--show' in sys.argv:
    sys.stdout.writelines(drop[:int(sys.argv[-1])] if sys.argv[-1].isdigit() else drop)
