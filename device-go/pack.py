# -*- coding: utf-8 -*-
"""打包 Go 版教室端（单文件 exe，界面内嵌，无控制台）

用法: python pack.py            # 生成 device-go/dist/云点传话筒.exe
      python pack.py --keep-ui  # 额外把 ui/ 复制到 dist/（想热改界面时用）

产物: dist/云点传话筒.exe —— 双击即用，不再需要 Python/依赖。
与 Python 版（tools/pack.py → dist/yundianchuanhuatong.exe 24MB）的区别：
  界面用 go:embed 内嵌，单文件 ~7MB；外置 exeDir/ui/ 存在时优先用它（免编译改样式）。
"""
import os, shutil, subprocess, sys, time

HERE = os.path.dirname(os.path.abspath(__file__))
DIST = os.path.join(HERE, 'dist')
NAME = '云点传话筒.exe'   # 与 Python 版发布名保持一致
UI = ['ui/index.html', 'ui/bridge.js', 'ui/favicon.ico']


def run(cmd, **kw):
    print('>', cmd, flush=True)
    r = subprocess.run(cmd, shell=True, cwd=HERE, capture_output=True,
                       encoding='utf-8', errors='replace', timeout=600, **kw)
    if r.stdout.strip():
        print(r.stdout.strip()[-2000:], flush=True)
    if r.returncode != 0:
        print(r.stderr.strip()[-3000:], flush=True)
        sys.exit('命令失败(%d): %s' % (r.returncode, cmd))


def main():
    for f in UI:
        if not os.path.exists(os.path.join(HERE, f)):
            sys.exit('缺少界面文件: %s' % f)
    # 环境：不联网也能编译（依赖已进 module cache）；如需拉依赖可去掉 GOPROXY=off
    os.environ.setdefault('GOFLAGS', '-mod=mod')
    if '--online' not in sys.argv:
        os.environ.setdefault('GOPROXY', 'off')

    bad = subprocess.run('gofmt -l .', shell=True, cwd=HERE, capture_output=True,
                         encoding='utf-8', errors='replace').stdout.strip()
    if bad:
        print('提示: 以下文件未 gofmt（CRLF 行尾导致，可忽略）:', bad.replace('\n', ' '), flush=True)
    run('go vet ./...')

    exe = os.path.join(DIST, NAME)
    run('go build -trimpath -ldflags="-s -w -H=windowsgui" -o "%s" .' % exe)
    print('\nBUILD OK', exe, os.path.getsize(exe), 'bytes', flush=True)

    if '--keep-ui' in sys.argv:
        d = os.path.join(DIST, 'ui')
        shutil.rmtree(d, ignore_errors=True)
        os.makedirs(d)
        for f in UI:
            shutil.copyfile(os.path.join(HERE, f), os.path.join(d, os.path.basename(f)))
        print('已复制 ui/ 到 dist（该目录存在时优先于内嵌界面）', flush=True)
    else:
        shutil.rmtree(os.path.join(DIST, 'ui'), ignore_errors=True)

    # 版本戳方便对账
    with open(os.path.join(DIST, 'BUILD.txt'), 'w', encoding='utf-8') as fp:
        fp.write('云点传话筒 教室端 Go 版\n构建时间: %s\n' % time.strftime('%Y-%m-%d %H:%M:%S'))
        fp.write('exe: %s\n' % NAME)
        fp.write('大小: %d bytes\n' % os.path.getsize(exe))
        fp.write('界面: 内嵌（dist/ui/ 存在则优先）\n')


if __name__ == '__main__':
    main()
