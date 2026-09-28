package kernel

import (
	"os"
	"strings"
	"testing"
)

// TestLiveMultiTurn 是"活体测试"：只在显式开启时运行，因为它要真实调用模型
// （花钱、依赖网络、结果不确定），不适合放进普通测试套件。
//
//	ARKPERF_LIVE=1 go test ./internal/kernel/ -run TestLiveMultiTurn -v
//
// 保留它的理由：多轮会话的价值只有真实模型才能验证——单元测试可以证明
// "历史确实发进了请求"，但证明不了"模型真的用上了它"。这两件事必须分开验。
func TestLiveMultiTurn(t *testing.T) {
	if os.Getenv("ARKPERF_LIVE") != "1" {
		t.Skip("设置 ARKPERF_LIVE=1 才会运行（会真实调用模型）")
	}

	cfg, err := Load(ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	reg := NewRegistry()
	conv := NewConversation()
	runner := &Runner{Cfg: cfg, Registry: reg, Conversation: conv}

	// 第一轮：埋一个只有记住上下文才能回答的信息
	first, err := runner.Run(t.Context(), Task{
		Text: "请记住这个数字：7391。只回复两个字：记住。",
	})
	if err != nil {
		t.Fatalf("第一轮失败：%v", err)
	}
	t.Logf("第一轮回答：%s", first.Text)

	// 第二轮：问的正是上一轮埋的信息
	second, err := runner.Run(t.Context(), Task{
		Text: "我刚才让你记的数字是多少？只回复那个数字，不要解释。",
	})
	if err != nil {
		t.Fatalf("第二轮失败：%v", err)
	}
	t.Logf("第二轮回答：%s", second.Text)

	if !strings.Contains(second.Text, "7391") {
		t.Fatalf("第二轮没能回忆起第一轮的内容——多轮会话没有生效。回答：%q", second.Text)
	}
	if conv.Turns() != 2 {
		t.Fatalf("会话轮数应为 2，实际 %d", conv.Turns())
	}
}
