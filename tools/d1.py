# -*- coding: utf-8 -*-
"""D1 远程 SQL 执行小工具
用法:
  python tools/d1.py "PRAGMA table_info(classes)"
  python tools/d1.py --file=server/migrations.sql

凭据：环境变量 CLOUDFLARE_API_TOKEN / CLOUDFLARE_ACCOUNT_ID，
      或仓库根 .secrets/cf_token.txt（该目录已被 .gitignore 忽略）。
库名：环境变量 D1_DATABASE，默认 yundian-chuanhuatong。
"""
import io
import os
import subprocess
import sys

try:
    sys.stdout.reconfigure(encoding='utf-8', errors='replace')  # wrangler 输出含 emoji，GBK 控制台会崩
except Exception:
    pass

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
       'WRANGLER_SEND_METRICS': 'false', 'CI': '1'}


def run(arg):
    cmd = 'npx -y wrangler@4 d1 execute %s --remote %s -y' % (DB, arg)
    r = subprocess.run(cmd, shell=True, capture_output=True, encoding='utf-8',
                       errors='replace', timeout=600, env=ENV, cwd=SRV)
    return r.returncode, (r.stdout or ''), (r.stderr or '')


if __name__ == '__main__':
    a = sys.argv[1]
    arg = '--file=%s' % a.split('=', 1)[1] if a.startswith('--file=') else '--command "%s"' % a
    rc, out, err = run(arg)
    print('rc', rc)
    print(out[-4000:])
    if err.strip():
        print('--- ERR ---')
        print(err[-2000:])
