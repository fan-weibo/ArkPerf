package tui

import (
	"image/color"
	"strings"

	"charm.land/lipgloss/v2"
)

// 主题系统对齐 opencode（packages/theme）：
//
//   - 颜色**真彩色**（#rrggbb），不再钉死在 256 色的索引上。
//     lipgloss 会按终端实际能力降级，所以写了真彩色在 8 色终端上也只是变难看，
//     不会出乱码。
//   - 颜色**从几个根色派生**（背景 + accent），而不是十几个硬编码值：
//     换一套主题只需要换根色，派生出来的正文/边框/表面自动跟着变。
//     这正是 opencode 的 `increase()/decrease()/source()` 在做的事。
//   - 栏背景是一个**抬起来的表面**（surface），与终端背景只差一点亮度。
//     不做成固定灰：固定灰在浅色终端上会变成一块突兀的深色。
//
// 全局的 styleXxx 变量由 applyTheme 重新赋值，调用点不用改——
// 这样"换主题"不需要碰任何渲染代码。

// Theme 是一套解析完成的配色。
//
// 前四个字段是"根色"，其余由 resolve 派生；不要手工填派生字段。
type Theme struct {
	Name   string
	IsDark bool

	// Accent / OK / Warn / Err 是语义色（根色）。
	Accent color.Color
	OK     color.Color
	Warn   color.Color
	Err    color.Color

	// 以下是派生色。
	Background color.Color // 终端背景（探测值；未探到时用主题的假定值）
	Text       color.Color // 正文
	TextDim    color.Color // 次要信息
	Border     color.Color // 分隔线 / 边框
	Scrollbar  color.Color // 滚动条
	Surface    color.Color // 抬起的表面（栏背景）
	SurfaceAlt color.Color // 再高一层（用于选中项、徽标底）
	ModeAsk    color.Color // "ask" 徽标底
	ModeAuto   color.Color // "auto" 徽标底
}

// ---------------------------------------------------------------- 颜色运算

// rgba 把任意 color.Color 归一到 RGBA，便于做分量运算。
func rgba(c color.Color) color.RGBA {
	if c == nil {
		return color.RGBA{A: 255}
	}
	return color.RGBAModel.Convert(c).(color.RGBA)
}

// mix 按比例插值：t=0 取 a，t=1 取 b。
func mix(a, b color.Color, t float64) color.Color {
	x, y := rgba(a), rgba(b)
	if t < 0 {
		t = 0
	}
	if t > 1 {
		t = 1
	}
	f := func(p, q uint8) uint8 {
		return uint8(float64(p) + (float64(q)-float64(p))*t + 0.5)
	}
	return color.RGBA{R: f(x.R, y.R), G: f(x.G, y.G), B: f(x.B, y.B), A: 255}
}

// lighten / darken 是 mix 的两个方向，对应 opencode 的 increase / decrease。
func lighten(c color.Color, amount float64) color.Color {
	return mix(c, color.RGBA{R: 255, G: 255, B: 255, A: 255}, amount)
}

func darken(c color.Color, amount float64) color.Color {
	return mix(c, color.RGBA{A: 255}, amount)
}

// increase / decrease 按主题深浅自动选方向：
// 深色主题上"抬起一个表面"是变亮，浅色主题上是变暗。
// 调用处不用操心方向，这是 surface 能同时适配深浅主题的关键。
func (t Theme) increase(c color.Color, amount float64) color.Color {
	if t.IsDark {
		return lighten(c, amount)
	}
	return darken(c, amount)
}

// ---------------------------------------------------------------- 主题定义

// themeSpec 是一套主题的**根色**（其余派生）。
type themeSpec struct {
	Name   string
	IsDark bool
	Accent string
	OK     string
	Warn   string
	Err    string
	// Base 是主题假定的背景色，仅在终端背景探测失败时使用。
	Base string
}

// builtinThemes 是内置主题表。第一项是默认主题。
//
// 都用真彩色而不是 256 色索引：索引色在不同终端上调色板差别很大，
// 同一个 "39" 在有些终端上是亮蓝、有些是暗蓝，主题就没法保证观感一致。
var builtinThemes = []themeSpec{
	{
		Name: "arkperf", IsDark: true,
		Accent: "#5aa9ff", OK: "#5dd39e", Warn: "#f0b34a", Err: "#ff6b81",
		Base: "#101418",
	},
	{
		Name: "nord", IsDark: true,
		Accent: "#88c0d0", OK: "#a3be8c", Warn: "#ebcb8b", Err: "#bf616a",
		Base: "#2e3440",
	},
	{
		Name: "gruvbox", IsDark: true,
		Accent: "#83a598", OK: "#b8bb26", Warn: "#fabd2f", Err: "#fb4934",
		Base: "#282828",
	},
	{
		Name: "dracula", IsDark: true,
		Accent: "#bd93f9", OK: "#50fa7b", Warn: "#f1fa8c", Err: "#ff5555",
		Base: "#282a36",
	},
	{
		Name: "daylight", IsDark: false,
		Accent: "#1d6fd0", OK: "#2f8f4e", Warn: "#a86a00", Err: "#c0392b",
		Base: "#fbfbfd",
	},
}

// resolve 把根色展开成完整配色。
//
// 派生规则只有两条，但它们是"看起来协调"的全部来源：
//   - 正文与背景的对比度固定（深色主题用亮字，浅色主题用暗字），
//     而不是写死某个灰；
//   - 表面、边框都从**背景**派生，所以它们天然贴着当前终端底色，
//     不会出现"深色块贴在浅色背景上"这种突兀。
func (s themeSpec) resolve(bg color.Color) Theme {
	assumed := lipgloss.Color(s.Base)
	if bg == nil {
		bg = assumed
	}
	t := Theme{
		Name:       s.Name,
		IsDark:     s.IsDark,
		Accent:     lipgloss.Color(s.Accent),
		OK:         lipgloss.Color(s.OK),
		Warn:       lipgloss.Color(s.Warn),
		Err:        lipgloss.Color(s.Err),
		Background: bg,
	}
	// 正文：深色主题上是接近白的灰，浅色主题上是接近黑的灰。
	// 不直接用纯黑纯白——纯色在长时间阅读下偏刺眼。
	if s.IsDark {
		t.Text = mix(bg, color.RGBA{R: 255, G: 255, B: 255, A: 255}, 0.88)
	} else {
		t.Text = mix(bg, color.RGBA{A: 255}, 0.86)
	}
	t.TextDim = mix(t.Text, bg, 0.45) // 次要信息比正文更靠近背景
	t.Surface = t.increase(bg, 0.06)  // 抬起的表面
	t.SurfaceAlt = t.increase(bg, 0.12)
	t.Border = t.increase(bg, 0.18)
	t.Scrollbar = t.increase(bg, 0.30)
	// 模式徽标：底色调成与主题协调的深浅，字色在渲染处取深色
	t.ModeAsk = t.increase(bg, 0.14)
	t.ModeAuto = t.Warn
	return t
}

// themeByName 按名字取主题；找不到就返回默认主题（不 panic：
// 一个写错的主题名不该让整个界面起不来）。
func themeByName(name string, bg color.Color) Theme {
	for _, s := range builtinThemes {
		if s.Name == name {
			return s.resolve(bg)
		}
	}
	return builtinThemes[0].resolve(bg)
}

// themeNames 列出所有主题名（供切换与展示）。
func themeNames() []string {
	out := make([]string, 0, len(builtinThemes))
	for _, s := range builtinThemes {
		out = append(out, s.Name)
	}
	return out
}

// shimmer 给文字加一道扫过的高光（对齐 opencode 的 shimmer-text）。
//
// **逐字符着色**，所以只适合短文本（状态行那几个字）：每字符一个转义序列，
// 拿去渲染整段正文会让输出体积翻几倍。
//
// 高光位置由 animFrame 推进，而 animFrame 是跟着 spinner 的 tick 走的——
// 复用现成的定时器，不为动画单起一个：常驻动画意味着"每帧全屏重绘"，
// 那点观感不值得一直烧 CPU。
func (m *Model) shimmer(s string) string {
	runes := []rune(s)
	n := len(runes)
	if n == 0 {
		return s
	}
	const period = 24
	phase := float64(m.animFrame%period) / period
	var b strings.Builder
	for i, r := range runes {
		pos := float64(i) / float64(n)
		d := pos - phase
		if d < 0 {
			d = -d
		}
		t := 1 - d*4
		if t < 0 {
			t = 0
		}
		b.WriteString(lipgloss.NewStyle().
			Foreground(mix(currentTheme.TextDim, currentTheme.Accent, t)).
			Render(string(r)))
	}
	return b.String()
}

// ---------------------------------------------------------------- 应用到全局样式

// applyTheme 把一套主题写进全局样式变量。
//
// 用全局变量而不是把主题贯穿到每个渲染函数：现有渲染代码有上百处引用
// styleAccent / styleDim 之类，改成传参要动每一个调用点。
// 代价是多实例（同一进程里两个 Model）会互相影响——本程序只会有一个。
func applyTheme(t Theme) {
	styleAccent = lipgloss.NewStyle().Foreground(t.Accent)
	styleDim = lipgloss.NewStyle().Foreground(t.TextDim)
	styleOK = lipgloss.NewStyle().Foreground(t.OK)
	styleWarn = lipgloss.NewStyle().Foreground(t.Warn)
	styleErr = lipgloss.NewStyle().Foreground(t.Err)
	styleText = lipgloss.NewStyle().Foreground(t.Text)
	styleBorder = lipgloss.NewStyle().Foreground(t.Border)
	styleScrollbar = lipgloss.NewStyle().Foreground(t.Scrollbar)

	colModeAsk = t.ModeAsk
	colModeAuto = t.ModeAuto
	paneToggleStyle = lipgloss.NewStyle().Foreground(t.TextDim)

	// 输入框与卡片的边框色随主题走
	inputBoxStyle = inputBoxStyle.BorderForeground(t.Border)
	cardStyle = cardStyle.BorderForeground(t.Warn)

	currentTheme = t
}

// setTheme 切换主题：按当前已知的终端背景重新解析，并应用到全局样式。
//
// 背景是整套配色的**唯一输入**（正文灰、边框、表面都从它派生），
// 所以换主题和"探到背景"走的是同一条路径。
func (m *Model) setTheme(name string) {
	if name == "" {
		name = builtinThemes[0].Name
	}
	m.themeName = name
	m.theme = themeByName(name, m.themeBg)
	applyTheme(m.theme)

	// 没探到真实背景就不铺色块：宁可少一层观感，也不拿"主题假定的背景"
	// 去猜——猜错就是浅色终端上糊一块深灰（见 panePrefix 的说明）。
	if m.themeBg == nil {
		m.panePrefix, m.paneReset = "", ""
		return
	}
	m.panePrefix, m.paneReset = bgSeq(m.theme.Surface)
}

// currentTheme 是当前生效的主题。渲染时需要读它的派生色
// （例如"抬起一个表面"要按深浅选方向），所以必须留一份。
var currentTheme Theme

func init() {
	// 起步用默认主题 + 假定背景；探测到终端背景后会重新应用一次。
	currentTheme = builtinThemes[0].resolve(nil)
	applyTheme(currentTheme)
}
