import io
import os
import subprocess
import sys
import time

# 部署脚本：在 server/ 下执行 `wrangler deploy`。
# 凭据：环境变量 CLOUDFLARE_API_TOKEN / CLOUDFLARE_ACCOUNT_ID，
#       或仓库根 .secrets/cf_token.txt（该目录已被 .gitignore 忽略）。
BASE = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
SRV = os.path.join(BASE, 'server')
LOG = os.path.join(BASE, 'deploy_log.txt')


def _token():
    t = (os.environ.get('CLOUDFLARE_API_TOKEN') or '').strip()
    if t:
        return t
    p = os.path.join(BASE, '.secrets', 'cf_token.txt')
    if os.path.exists(p):
        return io.open(p, encoding='utf-8').read().strip()
    raise SystemExit('缺少 CLOUDFLARE_API_TOKEN（或仓库根 .secrets/cf_token.txt）')


env = {**os.environ, 'CLOUDFLARE_API_TOKEN': _token(),
       'CLOUDFLARE_ACCOUNT_ID': (os.environ.get('CLOUDFLARE_ACCOUNT_ID') or '').strip(),
       'WRANGLER_SEND_METRICS': 'false', 'CI': '1'}


def w(cmd, timeout=900):
    t0 = time.time()
    r = subprocess.run(cmd, shell=True, capture_output=True, encoding='utf-8',
                       errors='replace', timeout=timeout, env=env, cwd=SRV)
    return (r.returncode, (r.stdout or '')[-4000:], (r.stderr or '')[-3000:], time.time() - t0)


steps = [
    ('DEPLOY', 'npx -y wrangler@4 deploy'),
]
with io.open(LOG, 'w', encoding='utf-8') as f:
    f.write('START %s  SRV=%s\n' % (time.strftime('%H:%M:%S'), SRV))
    f.flush()
    for name, cmd in steps:
        try:
            rc, o, e, dt = w(cmd)
            f.write('\n===== %s rc=%s %.1fs =====\n%s\n--- ERR ---\n%s\n' % (name, rc, dt, o, e))
        except Exception as ex:
            f.write('\n===== %s EXCEPTION %r =====\n' % (name, ex))
        f.flush()
    f.write('\nDONE %s\n' % time.strftime('%H:%M:%S'))
print('launched: ' + SRV)
