package tui

import (
	"image/color"

	"charm.land/lipgloss/v2"
)

// 配色刻意只用 256 色、并且不假设背景深浅：
// 终端主题千差万别，与其猜一个"好看"的配色，不如保证在所有主题下都可读。
var (
	colAccent = lipgloss.Color("39")  // 蓝
	colDim    = lipgloss.Color("244") // 次要信息
	colOK     = lipgloss.Color("42")  // 绿
	colWarn   = lipgloss.Color("214") // 橙
	colErr    = lipgloss.Color("203") // 红

	styleAccent = lipgloss.NewStyle().Foreground(colAccent)
	styleDim    = lipgloss.NewStyle().Foreground(colDim)
	styleOK     = lipgloss.NewStyle().Foreground(colOK)
	styleWarn   = lipgloss.NewStyle().Foreground(colWarn)
	styleErr    = lipgloss.NewStyle().Foreground(colErr)
	styleBold   = lipgloss.NewStyle().Bold(true)

	// 模式标签：底色随模式变，字色取深色以保证对比度
	colModeAsk  = lipgloss.Color("240")
	colModeAuto = lipgloss.Color("214")
)

// inputBoxStyle 是输入框外框。宽度由调用方按终端宽度算好。
var inputBoxStyle = lipgloss.NewStyle().
	Border(lipgloss.RoundedBorder()).
	BorderForeground(colDim).
	Padding(0, 1)

// cardStyle 用于需要用户决定的卡片（目前是审批卡）。
var cardStyle = lipgloss.NewStyle().
	Border(lipgloss.RoundedBorder()).
	BorderForeground(colWarn).
	Padding(0, 1)

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
