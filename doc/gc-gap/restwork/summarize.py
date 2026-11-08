#!/usr/bin/env python3
"""Summarize restwork/run.sh output (JSON records) into a table and CSV.

usage: summarize.py results/restwork.jsonl [results/restwork-summary.csv]
"""
import collections, json, re, sys

path = sys.argv[1]
csv = sys.argv[2] if len(sys.argv) > 2 else None
recs, buf = [], ''
for line in open(path):
    buf += line
    if line.startswith('}'):
        recs.append(json.loads(buf)); buf = ''
groups = collections.OrderedDict()
for r in recs:
    groups.setdefault(re.sub(r'-rep\d+$', '', r['label']), []).append(r)
mean = lambda xs: sum(xs) / len(xs)
hdr = f"{'config':24s} {'req/s':>7s} {'CPU ms/req':>10s} {'GC CPU%':>8s} {'GCs':>5s} {'alloc KB/req':>12s} {'live MiB':>8s} {'peak heap MiB':>13s} {'freed%':>7s} {'faded%':>7s} {'aband%':>7s} n"
print(hdr)
rows = []
for k, rs in groups.items():
    g = lambda f: mean([r[f] for r in rs])
    freed, faded, regions = g('region_freed_bytes'), g('region_faded_bytes'), g('region_regions')
    tot = freed + faded
    row = dict(config=k, rps=g('req_per_s'), cpums=g('cpu_ms_per_req'), gc=100 * g('gc_cpu_frac'), gcs=g('gc_cycles'),
               alloc=g('alloc_bytes_per_req') / 1024, live=g('heap_live_bytes') / 2**20, peak=g('peak_heap_bytes') / 2**20,
               freedp=(100 * freed / tot if tot else 0), fadedp=(100 * faded / tot if tot else 0),
               aband=(100 * g('region_abandoned') / regions if regions else 0), n=len(rs))
    rows.append(row)
    print(f"{k:24s} {row['rps']:7.0f} {row['cpums']:10.3f} {row['gc']:8.1f} {row['gcs']:5.0f} {row['alloc']:12.1f} {row['live']:8.1f} {row['peak']:13.1f} {row['freedp']:7.1f} {row['fadedp']:7.1f} {row['aband']:7.1f} {row['n']}")
if csv:
    with open(csv, 'w') as f:
        f.write("config,req_per_s,cpu_ms_per_req,gc_cpu_pct,gc_cycles,alloc_kb_per_req,live_mib,peak_heap_mib,freed_pct,faded_pct,abandoned_pct,runs\n")
        for r in rows:
            f.write(f"{r['config']},{r['rps']:.0f},{r['cpums']:.4f},{r['gc']:.2f},{r['gcs']:.0f},{r['alloc']:.2f},{r['live']:.1f},{r['peak']:.1f},{r['freedp']:.1f},{r['fadedp']:.1f},{r['aband']:.1f},{r['n']}\n")
