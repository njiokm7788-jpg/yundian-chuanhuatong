# -*- coding: utf-8 -*-
"""逐条执行 SQL 迁移文件（远程 D1），容忍“duplicate column”类错误
用法: python tools/migrate.py server/migrations_v2.sql

凭据：环境变量 CLOUDFLARE_API_TOKEN / CLOUDFLARE_ACCOUNT_ID，
      或仓库根 .secrets/cf_token.txt（该目录已被 .gitignore 忽略）。
库名：环境变量 D1_DATABASE，默认 yundian-chuanhuatong。
"""
import io
import os
import subprocess
import sys
import time

BASE = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
SRV = os.path.join(BASE, 'server')
DB = os.environ.get('D1_DATABASE', 'yundian-chuanhuatong')


def _token():
    t = (os.environ.get('CLOUDFLARE_API_TOKEN') or '').strip()
    if t:
        return t
    p = os.path.join(BASE, '.secrets', 'cf_token.txt')
    if os.path.exists(p):
        return io.open(p, encoding='utf-8').read().strip()
    raise SystemExit('缺少 CLOUDFLARE_API_TOKEN（或仓库根 .secrets/cf_token.txt）')


ENV = {**os.environ, 'CLOUDFLARE_API_TOKEN': _token(),
       'CLOUDFLARE_ACCOUNT_ID': (os.environ.get('CLOUDFLARE_ACCOUNT_ID') or '').strip(),
       'WRANGLER_SEND_METRICS': 'false', 'CI': '1', 'PYTHONIOENCODING': 'utf-8'}


def exec_sql(sql):
    cmd = ('npx -y wrangler@4 d1 execute %s --remote --command "%s" -y'
           % (DB, sql.replace('"', '\\"').replace('\n', ' ')))
    r = subprocess.run(cmd, shell=True, capture_output=True, encoding='utf-8',
                       errors='replace', timeout=600, env=ENV, cwd=SRV)
    return r.returncode, (r.stdout or '') + (r.stderr or '')


if __name__ == '__main__':
    path = sys.argv[1]
    if not os.path.isabs(path):
        path = os.path.join(BASE, path)
    raw = io.open(path, encoding='utf-8').read()
    clean = '\n'.join(l for l in raw.split('\n') if not l.strip().startswith('--'))
    stmts = [s.strip() for s in clean.split(';') if s.strip()]
    for i, st in enumerate(stmts, 1):
        rc, out = exec_sql(st)
        low = out.lower()
        if rc == 0 and 'error' not in low:
            print('[%d/%d] OK   %s' % (i, len(stmts), st[:70].replace('\n', ' ')))
        elif 'duplicate column' in low or 'already exists' in low:
            print('[%d/%d] SKIP %s' % (i, len(stmts), st[:70].replace('\n', ' ')))
        else:
            print('[%d/%d] FAIL %s\n%s' % (i, len(stmts), st[:70].replace('\n', ' '), out[-700:]))
        sys.stdout.flush()
        time.sleep(0.3)
    print('MIGRATE DONE')
