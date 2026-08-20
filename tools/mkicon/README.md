# mkicon

重新生成 `icon.ico` 与检查用的预览图。

环境里没有 SVG/位图工具时也能用：图标只由圆头粗线段和圆构成，
这里用有向距离场直接求覆盖率做抗锯齿，不依赖任何第三方库。

```
cd tools/mkicon && go run .          # 产出 icon.ico 与 preview.png
cp icon.ico ../../icon.ico
cd ../.. && go run github.com/akavel/rsrc@latest -ico icon.ico -arch amd64 -o rsrc_windows_amd64.syso
```

`.syso` 才是 Windows 实际读取的资源。只改 `icon.ico` 而不重新生成它，
可执行文件上的图标不会变。

## 透明底带来的约束

图标是透明底的，落在什么颜色上由系统决定，而 `.ico` 是位图，无法跟随主题翻转。
因此主干色 `trunk` 取中性灰 `#6e6e73`，而不是界面里那个近黑色——
实测纯黑主干在 Windows 深色任务栏（`#202020`）上 16/24/32/48 四个尺寸全部消失，
整枚图标只剩三条悬空的支路。

`preview.png` 会把同一枚图标合成到浅色 `#f3f3f3` 与深色 `#202020` 两种底色上并排导出。
改动颜色后先看这张图：单看一张透明图检查不出这类问题。

## 与界面字形的关系

`ui/index.html` 标题栏那段 SVG 与本文件是同一套坐标，改动图形需要同步。

颜色则**刻意不同步**：界面里主干用 `var(--trunk)`，随主题在 `#1d1d1f` 与 `#f5f5f7`
之间翻转。那是 SVG 做得到而位图做不到的，没有理由放弃。
这里的中性灰是 `.ico` 格式受限下的折中，不是设计上更优的选择。
