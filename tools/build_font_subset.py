"""把 ZCOOL KuaiLe 圆体按页面实际用字裁剪成 woff2，供教师端 H5 自托管。

背景：参考稿用 Google Fonts 外链引入圆体，国内访问不通、且教室/手机端要求零外部依赖，
所以把字库落到仓库里自托管。完整 TTF 约 1.5 MB，按用字裁剪后约 60 KB。

依赖（仅构建时需要，不入仓库）：
    pip install fonttools brotli

字库来源（OFL-1.1，可商用，需保留授权）：
    https://github.com/google/fonts/tree/main/ofl/zcoolkuaile   -> ZCOOLKuaiLe-Regular.ttf
    首次使用请把该 TTF 放到 tools/_font_src/ZCOOLKuaiLe-Regular.ttf

用法（项目根目录执行）：
    python tools/build_font_subset.py
改完页面文案后重跑一次，新字才会进子集；未收录的字会自动回退到系统字体。
"""
import argparse
import io
import os
import sys

try:
    from fontTools import subset
except ImportError:
    sys.exit('缺少 fonttools：pip install fonttools brotli')

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('--html', default=os.path.join(ROOT, 'server', 'public', 'index.html'))
    ap.add_argument('--ttf', default=os.path.join(ROOT, 'tools', '_font_src', 'ZCOOLKuaiLe-Regular.ttf'))
    ap.add_argument('--out', default=os.path.join(ROOT, 'server', 'public', 'fonts', 'ZCOOLKuaiLe-sub.woff2'))
    a = ap.parse_args()

    if not os.path.exists(a.ttf):
        sys.exit('找不到源字库：%s\n请从 google/fonts 的 ofl/zcoolkuaile 下载后放到该路径。' % a.ttf)

    html = io.open(a.html, encoding='utf-8').read()
    text = ''.join(sorted(set(c for c in html if c not in '\r\n')))

    opts = subset.Options()
    opts.layout_features = []      # 展示型字库，不需要 GSUB/GPOS
    opts.hinting = False
    opts.desubroutinize = True
    opts.notdef_outline = True
    opts.name_IDs = ['*']

    font = subset.load_font(a.ttf, opts)
    sub = subset.Subsetter(options=opts)
    sub.populate(text=text)
    sub.subset(font)
    font.flavor = 'woff2'

    os.makedirs(os.path.dirname(a.out), exist_ok=True)
    font.save(a.out)          # TTFont.save 会按 flavor 走 brotli 压缩

    head = open(a.out, 'rb').read(4)
    if head != b'wOF2':
        sys.exit('输出容器不是 woff2（文件头 %r）：请确认已安装 brotli（pip install brotli）' % head)

    src = os.path.getsize(a.ttf)
    dst = os.path.getsize(a.out)
    print('用字 %d 个 | %s -> %s | %d -> %d 字节 (%.1f%%) | woff2 头校验 OK'
          % (len(text), os.path.basename(a.ttf), os.path.basename(a.out), src, dst, dst * 100.0 / src))


if __name__ == '__main__':
    main()
