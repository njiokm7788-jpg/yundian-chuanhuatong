# -*- coding: utf-8 -*-
"""打包教室端 exe（PyInstaller onefile + 无控制台）

含 WebView2( pywebview ) 界面：device/ui/index.html 作为数据文件打进 exe（_MEIPASS/ui/index.html）。
"""
import os, sys, shutil
import PyInstaller.__main__

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), '..'))
DEV = os.path.join(ROOT, 'device', 'yundianchuanhuatong.py')
UI = os.path.join(ROOT, 'device', 'ui')

for d in ('build', 'dist'):
    p = os.path.join(ROOT, d)
    shutil.rmtree(p, ignore_errors=True) if d == 'build' else None
    os.makedirs(p, exist_ok=True)

args = [
    DEV, '--noconfirm', '--clean', '--onefile', '--noconsole',
    '--name', 'yundianchuanhuatong',
    '--distpath', os.path.join(ROOT, 'dist'),
    '--workpath', os.path.join(ROOT, 'build'),
    '--specpath', os.path.join(ROOT, 'build'),
    '--paths', os.path.join(ROOT, 'device'),
    # 界面资源：解包到 _MEIPASS/ui/
    '--add-data', UI + os.pathsep + 'ui',
    '--hidden-import', 'local_tts',
    '--hidden-import', 'webui',
    # pywebview / WebView2
    '--hidden-import', 'webview',
    '--hidden-import', 'webview.platforms.edgechromium',
    '--hidden-import', 'clr_loader',
    '--hidden-import', 'pythonnet',
    '--collect-all', 'webview',
    # SAPI 语音
    '--hidden-import', 'win32com.client',
    '--hidden-import', 'pythoncom',
    '--hidden-import', 'pywintypes',
    '--hidden-import', 'win32gui',
    '--hidden-import', 'win32con',
    '--hidden-import', 'win32api',
]
print('PyInstaller', ' '.join(args))
sys.stdout.flush()
PyInstaller.__main__.run(args)
exe = os.path.join(ROOT, 'dist', 'yundianchuanhuatong.exe')
print('BUILD OK', exe, os.path.getsize(exe))
