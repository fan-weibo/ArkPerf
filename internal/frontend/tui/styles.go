package tui

import (
	"image/color"

	"charm.land/lipgloss/v2"
)

// 配色全部来自 theme.go 的主题系统：applyTheme 会把当前主题写进下面这些变量。
//
// 为什么用包级变量而不是把主题一路传下去：渲染代码有上百处直接引用
// styleAccent / styleDim 之类，改成传参要动每一个调用点，而换来的只是
// "同进程跑多个不同主题的实例"——本程序不存在这种情况。
var (
	styleAccent    lipgloss.Style
	styleDim       lipgloss.Style
	styleOK        lipgloss.Style
	styleWarn      lipgloss.Style
	styleErr       lipgloss.Style
	styleText      lipgloss.Style
	styleBorder    lipgloss.Style
	styleScrollbar lipgloss.Style

	// styleBold 与主题无关（只加粗不改色），初始化一次即可。
	styleBold = lipgloss.NewStyle().Bold(true)

	// 模式徽标的底色（由主题给）；字色在 modeTagStyle 里取深色以保证对比度。
	colModeAsk  color.Color
	colModeAuto color.Color

	// paneToggleStyle 渲染折叠态那一列图标（点它把栏重新拉出来）。
	paneToggleStyle lipgloss.Style
)

// inputBoxStyle / cardStyle 的**边框色**随主题走，所以每套主题要重设一次。
var (
	inputBoxStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			Padding(0, 1)
	cardStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			Padding(0, 1)
)

// modeTagStyle 渲染模式徽标（ask / auto）。
//
// 参数用 image/color.Color 而不是 lipgloss.Color：v2 里后者已经是一个
// 构造函数而不是类型。
func modeTagStyle(bg color.Color) lipgloss.Style {
	return lipgloss.NewStyle().
		Background(bg).
		Foreground(lipgloss.Color("232")).
		Padding(0, 1).
		Bold(true)
}
