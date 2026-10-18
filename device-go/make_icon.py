# -*- coding: utf-8 -*-
"""换图标：一张方图 → ui/favicon.ico（多尺寸）+ rsrc_windows_amd64.syso（exe 图标资源）

用法:
    python make_icon.py 新图标.png

做了什么:
  · 生成 16/24/32/48/64/128/256 多尺寸 ico，覆盖 ui/favicon.ico
    （托盘图标与界面 favicon 都读它；exe 图标资源也指向它）；
  · 16/24/32 用「主体正方形裁切」重采样，48 以上用整块圆角瓦片——
    细密插画在 16×16 下会糊成一团，小尺寸单独处理才看得清；
  · 圆角外的纯白自动抠成透明（从四角洪泛，不会伤到图内被包围的白色）；
  · 用 windres 把 app.rc + ui/favicon.ico 编成 rsrc_windows_amd64.syso，
    Go 编译时会自动链进 exe（资源管理器/快捷方式/任务栏的图标就是它）。

注意: 改完图标必须重新打包 —— python pack.py
"""
import os
import subprocess
import sys
from collections import deque

from PIL import Image

HERE = os.path.dirname(os.path.abspath(__file__))
ICO = os.path.join(HERE, 'ui', 'favicon.ico')
SYSO = os.path.join(HERE, 'rsrc_windows_amd64.syso')
RC = os.path.join(HERE, 'app.rc')
SIZES = [16, 24, 32, 48, 64, 128, 256]
SMALL_MAX = 32          # 这个尺寸及以下用主体裁切
WINDRES = os.environ.get(
    'WINDRES',
    r'I:\gcc\x86_64-8.1.0-release-posix-seh-rt_v6-rev0\mingw64\bin\windres.exe')


def cut_white_corners(im, tol=6):
    """从四角洪泛，把圆角外的纯白变透明（图内被包围的白色不受影响）。"""
    im = im.convert('RGBA')
    px = im.load()
    w, h = im.size

    def white(p):
        r, g, b, a = p
        return a > 0 and r >= 255 - tol and g >= 255 - tol and b >= 255 - tol

    seen, q = set(), deque()
    for c in ((0, 0), (w - 1, 0), (0, h - 1), (w - 1, h - 1)):
        if white(px[c]):
            seen.add(c)
            q.append(c)
    while q:
        x, y = q.popleft()
        px[x, y] = (255, 255, 255, 0)
        for dx, dy in ((1, 0), (-1, 0), (0, 1), (0, -1)):
            nx, ny = x + dx, y + dy
            if 0 <= nx < w and 0 <= ny < h and (nx, ny) not in seen and white(px[nx, ny]):
                seen.add((nx, ny))
                q.append((nx, ny))
    return im


def subject_crop(im):
    """主体所在的居中正方形（略偏上，避开底部桌面）。"""
    x0, y0, x1, y1 = im.getbbox()
    w, h = x1 - x0, y1 - y0
    cx, cy, half = x0 + 0.50 * w, y0 + 0.44 * h, 0.33 * w
    return im.crop((int(cx - half), int(cy - half), int(cx + half), int(cy + half)))


def main():
    if len(sys.argv) < 2:
        sys.exit('用法: python make_icon.py 新图标.png')
    src = sys.argv[1]
    if not os.path.exists(src):
        sys.exit('找不到图片: %s' % src)

    tile = cut_white_corners(Image.open(src))
    small = subject_crop(tile)
    frames = {s: (small if s <= SMALL_MAX else tile).resize((s, s), Image.LANCZOS)
              for s in SIZES}
    frames[256].save(ICO, sizes=[(s, s) for s in SIZES],
                     append_images=[frames[s] for s in SIZES])
    print('已更新 %s (%d 字节)' % (ICO, os.path.getsize(ICO)))

    with open(RC, 'w', encoding='ascii') as fp:
        fp.write('1 ICON "ui/favicon.ico"\n')
    if not os.path.exists(WINDRES):
        print('未找到 windres，跳过 syso：请手动执行\n  windres -O coff -i app.rc -o rsrc_windows_amd64.syso')
        return
    subprocess.run([WINDRES, '-O', 'coff', '-i', 'app.rc', '-o', 'rsrc_windows_amd64.syso'],
                   cwd=HERE, check=True)
    print('已更新 %s (%d 字节)' % (SYSO, os.path.getsize(SYSO)))
    print('记得重新打包: python pack.py')


if __name__ == '__main__':
    main()
