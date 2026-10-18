# -*- coding: utf-8 -*-
"""生成/重置后台 admin 口令的 SQL（PBKDF2-HMAC-SHA256, 60000 次, salt 为明文串）

用法:
  python tools/mkadmin.py                  # 随机新口令
  python tools/mkadmin.py "MyPass123"      # 指定口令
  python tools/mkadmin.py --apply          # 生成后直接执行（写线上 D1）
"""
import hashlib
import os
import secrets
import string
import subprocess
import sys

BASE = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))


def gen(password=None):
    password = password or ''.join(secrets.choice(string.ascii_letters + string.digits) for _ in range(18))
    salt = secrets.token_hex(16)
    h = hashlib.pbkdf2_hmac('sha256', password.encode(), salt.encode(), 60000, 32).hex()
    sql = ("UPDATE users SET salt='%s', pass_hash='%s' WHERE username='admin';" % (salt, h))
    return password, sql


if __name__ == '__main__':
    arg = sys.argv[1] if len(sys.argv) > 1 else None
    apply_ = arg in ('--apply',)
    pw, sql = gen(None if (arg is None or apply_) else arg)
    print('NEW ADMIN PASSWORD:', pw)
    print(sql)
    if apply_:
        r = subprocess.run([sys.executable, os.path.join(BASE, 'tools', 'd1.py'), sql],
                           capture_output=True, encoding='utf-8', errors='replace')
        print(r.stdout[-800:], r.stderr[-400:])
