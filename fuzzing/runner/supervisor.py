"""Container-owned time budget, LibAFL invocation, and versioned event stream."""
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import signal
import subprocess
import time

ROOT = Path('/job')
sequence = 0
seen_samples = set()
latest_metrics = {}
last_coverage = None


def tail():
    with (ROOT / 'run.log').open('rb') as log:
        log.seek(max(0, log.seek(0, 2) - 32000))
        return log.read().decode(errors='replace')


def progress(cfg, start):
    global last_coverage
    detail = tail()
    for key, pattern in [('executions', r'executions: (\d+)'), ('execs_per_sec', r'exec/sec: ([\d.]+[kKmM]?)'),
                         ('coverage', r'edges: (\d+)')]:
        matches = re.findall(pattern, detail)
        if matches:
            text = matches[-1]
            scale = 1000 if text[-1:] in ('k', 'K') else 1000000 if text[-1:] in ('m', 'M') else 1
            latest_metrics[key] = float(text.rstrip('kKmM')) * scale
    if latest_metrics.get('coverage') != last_coverage:
        last_coverage = latest_metrics.get('coverage')
        latest_metrics['last_coverage_at'] = time.time()
    latest_metrics.update(corpus_count=sum(1 for p in (ROOT / 'corpus').iterdir() if p.is_file() and not p.name.startswith('.')),
                          elapsed_seconds=round(time.monotonic() - start, 2), coverage_kind='libafl_edges')
    for p in (ROOT / 'raw').iterdir():
        if not p.is_file() or p.is_symlink() or p.stat().st_size > cfg['max_input']:
            continue
        data = p.read_bytes()
        sha = hashlib.sha256(data).hexdigest()
        if sha in seen_samples:
            continue
        seen_samples.add(sha)
        (ROOT / 'samples' / sha).write_bytes(data)
        kind = 'timeout' if 'timeout' in p.name else 'crash_candidate'
        event('sample', kind=kind, signature=kind + ':' + sha, sample=sha, detail=detail[-16000:])
    latest_metrics['sample_count'] = len(seen_samples)
    event('metrics', metrics=latest_metrics)


def event(event_kind, **data):
    global sequence
    sequence += 1
    with (ROOT / 'events.jsonl').open('a') as out:
        out.write(json.dumps(dict(version=1, seq=sequence, type=event_kind, **data)) + '\n')
        out.flush()


def size():
    return sum(p.stat().st_size for p in ROOT.rglob('*') if p.is_file() and not p.is_symlink())


def run(args, seconds, cwd, env, budget, update=None):
    with (ROOT / 'run.log').open('ab', buffering=0) as log:
        process = subprocess.Popen(args, cwd=cwd, env=env, stdout=log, stderr=log, start_new_session=True)
        deadline = time.monotonic() + seconds
        reason = ''
        next_update = time.monotonic()
        while process.poll() is None:
            if time.monotonic() >= deadline:
                reason = 'budget_exhausted'
                break
            if size() > budget:
                reason = 'output_limit'
                break
            if update and time.monotonic() >= next_update:
                update()
                next_update = time.monotonic() + 3
            time.sleep(0.5)
        # Also clean children that outlive the target/build script.
        try:
            os.killpg(process.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        code = process.wait()
        return code, reason


def main():
    cfg = json.loads((ROOT / 'config.json').read_text())
    budget = cfg['output_mb'] * 1024 * 1024 + size()
    source = ROOT / 'source'
    work = ROOT / 'build'
    shutil.copytree(source, work)
    out = ROOT / 'out'
    out.mkdir()
    corpus = ROOT / 'corpus'
    corpus.mkdir()
    samples = ROOT / 'samples'
    samples.mkdir()
    artifacts = ROOT / 'raw'
    artifacts.mkdir()
    env = os.environ.copy()
    env.update(OUT=str(out), CFLAGS='-g -O1 -fsanitize=fuzzer-no-link,address,undefined',
               CXXFLAGS='-g -O1 -fsanitize=fuzzer-no-link,address,undefined',
               ASAN_OPTIONS='abort_on_error=1:detect_leaks=0:symbolize=1',
               UBSAN_OPTIONS='halt_on_error=1:print_stacktrace=1')
    build = (work / cfg['build']).resolve()
    build.relative_to(work)
    code, reason = run(['bash', str(build)], cfg['build_seconds'], work, env, budget)
    target = out / 'fuzz_target'
    if code or reason or not target.is_file():
        event('state', state='failed', reason='build_failed:' + (reason or str(code)))
        return
    # Symbols must show the LibAFL runtime and coverage instrumentation. A plain
    # executable or accidentally linked LLVM libFuzzer is not a valid campaign.
    symbols = subprocess.run(['nm', str(target)], capture_output=True, text=True, timeout=15).stdout
    if 'libafl' not in symbols.lower() or '__sanitizer_cov' not in symbols:
        event('state', state='failed', reason='missing_libafl_or_coverage_instrumentation')
        return
    for directory in [source / cfg['seeds'], ROOT / 'resume']:
        if directory.is_dir():
            for p in directory.rglob('*'):
                if p.is_file() and not p.is_symlink() and p.stat().st_size <= cfg['max_input']:
                    b = p.read_bytes()
                    (corpus / hashlib.sha256(b).hexdigest()).write_bytes(b)
    if not any(corpus.iterdir()):
        event('state', state='failed', reason='seed_corpus_empty')
        return
    # LibAFL's libFuzzer compatibility runtime does not implement every
    # upstream libFuzzer flag (notably -max_len); input size is enforced while
    # importing the corpus and collecting artifacts above.
    common = [str(target), '-timeout=' + str(cfg['timeout']),
              '-artifact_prefix=' + str(artifacts) + '/']
    if cfg.get('dictionary'):
        common.append('-dict=' + str(source / cfg['dictionary']))
    if cfg.get('replay'):
        args = common + [str(ROOT / 'replay')]
        seconds = cfg['timeout'] + 10
    else:
        args = common + ['-fork=1', '-ignore_crashes=1', '-ignore_timeouts=1', str(corpus)]
        seconds = cfg['seconds']
    event('state', state='running')
    start = time.monotonic()
    code, reason = run(args, seconds, work, env, budget, lambda: progress(cfg, start))
    progress(cfg, start)
    # Read a bounded tail for summary; full logs remain an artifact.
    with (ROOT / 'run.log').open('rb') as log:
        log.seek(max(0, log.seek(0, 2) - 32000))
        detail = log.read().decode(errors='replace')
    candidates = list(artifacts.iterdir())
    if cfg.get('replay') and (code or reason):
        candidates.append(ROOT / 'replay')
    for p in candidates:
        if not p.is_file() or p.is_symlink() or p.stat().st_size > cfg['max_input']:
            continue
        b = p.read_bytes()
        sha = hashlib.sha256(b).hexdigest()
        (samples / sha).write_bytes(b)
        kind = 'timeout' if 'timeout' in p.name or reason == 'budget_exhausted' else 'crash_candidate'
        # Without a per-sample verified stack, group by input hash. Never merge
        # unrelated samples using the final process's stack trace.
        if sha not in seen_samples:
            seen_samples.add(sha)
            event('sample', kind=kind, signature=kind + ':' + sha, sample=sha, detail=detail[-16000:])
    latest_metrics.update(exit_code=code, sample_count=len(seen_samples))
    event('metrics', metrics=latest_metrics)
    if reason == 'output_limit':
        event('state', state='completed', reason=reason)
    elif code and not candidates and not reason:
        event('state', state='failed', reason='runner_exit_' + str(code))
    else:
        event('state', state='completed', reason=reason or ('sample_found' if candidates else 'budget_exhausted'))


if __name__ == '__main__':
    try:
        main()
    except Exception as exc:
        event('state', state='failed', reason=str(exc)[:1000])
        raise
