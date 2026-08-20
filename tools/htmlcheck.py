"""校验 HTML 标签配平。自闭合与 void 元素两侧都要跳过，否则满屏假阳性。"""
from html.parser import HTMLParser
import io, sys
VOID = {'meta','br','hr','img','input','link','source','area','base',
        'col','embed','param','track','wbr','path','circle','rect'}
class C(HTMLParser):
    def __init__(s): super().__init__(); s.st=[]; s.err=[]
    def handle_startendtag(s, t, a): pass          # <x/> 自闭合，两侧都不计
    def handle_starttag(s, t, a):
        if t not in VOID: s.st.append((t, s.getpos()[0]))
    def handle_endtag(s, t):
        if t in VOID: return
        if not s.st: s.err.append(f"第 {s.getpos()[0]} 行：多余的 </{t}>"); return
        if s.st[-1][0] != t:
            s.err.append(f"第 {s.getpos()[0]} 行：</{t}> 与第 {s.st[-1][1]} 行的 <{s.st[-1][0]}> 不配对")
        else: s.st.pop()
bad = 0
for f in sys.argv[1:]:
    c = C(); c.feed(io.open(f, encoding='utf-8').read())
    if c.st or c.err:
        bad = 1
        print(f"✗ {f}")
        for t, l in c.st: print(f"    未闭合 <{t}> 第 {l} 行")
        for e in c.err: print(f"    {e}")
    else:
        print(f"✓ {f} 标签配平")
sys.exit(bad)
