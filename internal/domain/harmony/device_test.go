package harmony

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// writeFakeHdc 造一个假 hdc。用真实的脚本文件而不是接口打桩，
// 是为了顺带验证参数拼接、.bat 包装、输出读取这一整条链路。
func writeFakeHdc(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if runtime.GOOS == "windows" {
		p := filepath.Join(dir, "hdc.bat")
		if err := os.WriteFile(p, []byte("@echo off\r\n"+body+"\r\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	p := filepath.Join(dir, "hdc")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func echoArgsScript() string {
	if runtime.GOOS == "windows" {
		return "echo %*"
	}
	return `echo "$@"`
}

func toolchainWithHDC(path string) *Toolchain {
	return &Toolchain{Tools: []Tool{{Name: ToolHDC, Path: path}}}
}

// 实测 hdc list targets 无设备时输出恰好是 "[Empty]"。
// 把它当设备名会凭空造出一个假设备，后续所有操作都对着它失败。
func TestParseTargetsHandlesEmptyMarker(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"empty marker", "[Empty]", nil},
		{"empty marker with spaces", "  [Empty]  \n", nil},
		{"blank output", "\n\n", nil},
		{"one device", "127.0.0.1:5555", []string{"127.0.0.1:5555"}},
		{"two devices", "127.0.0.1:5555\nABC1234567\n", []string{"127.0.0.1:5555", "ABC1234567"}},
		{"noise dropped", "[Info] something\n127.0.0.1:5555\n", []string{"127.0.0.1:5555"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseTargets(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d devices %v, want %d", len(got), got, len(tc.want))
			}
			for i := range got {
				if got[i].Serial != tc.want[i] {
					t.Fatalf("device %d: %q, want %q", i, got[i].Serial, tc.want[i])
				}
			}
		})
	}
}

func TestListDevicesParsesOutput(t *testing.T) {
	tc := toolchainWithHDC(writeFakeHdc(t, "echo 127.0.0.1:5555"))
	devices, err := tc.ListDevices(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != 1 || devices[0].Serial != "127.0.0.1:5555" {
		t.Fatalf("devices: %+v", devices)
	}
}

// "没有设备"必须是合法结果（空列表 + nil error），
// 否则上层无法区分"模拟器没开"和"hdc 坏了"。
func TestListDevicesEmptyIsNotAnError(t *testing.T) {
	tc := toolchainWithHDC(writeFakeHdc(t, "echo [Empty]"))
	devices, err := tc.ListDevices(t.Context())
	if err != nil {
		t.Fatalf("empty device list must not be an error: %v", err)
	}
	if len(devices) != 0 {
		t.Fatalf("expected no devices, got %+v", devices)
	}
}

func TestListDevicesWithoutHDCFailsWithHint(t *testing.T) {
	tc := &Toolchain{Tools: []Tool{{Name: ToolHDC, Hint: "把 toolchains 加入 PATH"}}}
	_, err := tc.ListDevices(t.Context())
	if err == nil {
		t.Fatal("missing hdc must be an error")
	}
	if !strings.Contains(err.Error(), "hdc") || !strings.Contains(err.Error(), "PATH") {
		t.Fatalf("error must name the tool and the fix: %v", err)
	}
}

// -t 是全局参数，必须排在子命令之前，否则 hdc 会把它当成 shell 的参数。
func TestHDCArgsPutDeviceFlagFirst(t *testing.T) {
	tc := toolchainWithHDC(writeFakeHdc(t, echoArgsScript()))

	res, err := tc.Shell(t.Context(), "127.0.0.1:5555", "aa", "start", "-b", "com.x")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "-t 127.0.0.1:5555 shell aa start -b com.x") {
		t.Fatalf("unexpected argument order: %q", res.Output)
	}
}

// 不指定设备时不要硬塞 -t：单设备场景交给 hdc 自己决定，
// 多设备时它会报错，那比随机挑一个测错对象要好。
func TestHDCArgsOmitDeviceWhenEmpty(t *testing.T) {
	tc := toolchainWithHDC(writeFakeHdc(t, echoArgsScript()))

	res, err := tc.Shell(t.Context(), "  ", "ls")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Output, "-t") {
		t.Fatalf("-t must be omitted when no device is given: %q", res.Output)
	}
	if !strings.Contains(res.Output, "shell ls") {
		t.Fatalf("command mangled: %q", res.Output)
	}
}

// 复测要求同一个包名能装第二次，所以 -r 不能少。
func TestInstallUsesOverwriteFlag(t *testing.T) {
	tc := toolchainWithHDC(writeFakeHdc(t, echoArgsScript()))

	res, err := tc.Install(t.Context(), "dev1", `E:\proj\entry\build\default\entry.hap`)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"-t dev1", "install", "-r", "entry.hap"} {
		if !strings.Contains(res.Output, want) {
			t.Fatalf("install command missing %q: %q", want, res.Output)
		}
	}
}

// 实测：只给 -b 会报 10103101 "Failed to find a matching application for
// implicit launch"，即使包确实装着。所以 -a 必须始终出现。
func TestStartAbilityAlwaysPassesAbilityFlag(t *testing.T) {
	tc := toolchainWithHDC(writeFakeHdc(t, echoArgsScript()))

	res, err := tc.StartAbility(t.Context(), "dev1", "com.demo", "EntryAbility", true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "-a EntryAbility -b com.demo -W") {
		t.Fatalf("launch command: %q", res.Output)
	}
}

// ability 留空时先从设备包信息解析，解析不到退回生态惯例的 EntryAbility。
func TestStartAbilityResolvesEmptyAbility(t *testing.T) {
	tc := toolchainWithHDC(writeFakeHdc(t, echoArgsScript()))

	res, err := tc.StartAbility(t.Context(), "", "com.demo", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "-a "+DefaultAbility) {
		t.Fatalf("解析失败时应退回 %s：%q", DefaultAbility, res.Output)
	}
	if strings.Contains(res.Output, "-W") {
		t.Fatalf("wait=false 时不该附加 -W：%q", res.Output)
	}
}

func TestStartAbilityRequiresBundle(t *testing.T) {
	tc := toolchainWithHDC(writeFakeHdc(t, echoArgsScript()))
	if _, err := tc.StartAbility(t.Context(), "", "  ", "EntryAbility", true); err == nil {
		t.Fatal("bundle 为空时必须报错")
	}
}

// 取自设备上真实的 bm dump -n 输出片段。
func TestParseMainAbilityFromRealDump(t *testing.T) {
	out := `
        "name": "com.example.perflab",
                        "EntryAbility"
                    "name": "EntryAbility",
                    "srcEntrance": "./ets/entryability/EntryAbility.ets",
            "mainAbility": "EntryAbility",
            "mainElementName": "EntryAbility",`
	if got := parseMainAbility(out); got != "EntryAbility" {
		t.Fatalf("parseMainAbility: %q", got)
	}
	if got := parseMainAbility("no such field here"); got != "" {
		t.Fatalf("解析不到时应返回空串：%q", got)
	}
}

// 取自设备上真实的 aa start -W 输出。
func TestParseStartTimingsFromRealOutput(t *testing.T) {
	out := `StartMode: Cold
BundleName: com.example.perflab
AbilityName: EntryAbility
TotalTime: 1163
WaitTime: 1175
start ability successfully.`

	got, ok := ParseStartTimings(out)
	if !ok {
		t.Fatalf("应当解析成功：%q", out)
	}
	if got.WaitMs != 1175 || got.TotalMs != 1163 || got.Mode != "Cold" {
		t.Fatalf("解析结果：%+v", got)
	}
	if _, ok := ParseStartTimings("start ability successfully."); ok {
		t.Fatal("没有耗时字段时不该报成功")
	}
}

// 包信息读不到时不要瞎猜 Ability 名：猜错只会换来一个不说原因的 10103101。
func TestResolveAbilityFallsBackToDefault(t *testing.T) {
	tc := toolchainWithHDC(writeFakeHdc(t, echoArgsScript()))
	if got := tc.ResolveAbility(t.Context(), "dev1", "com.demo"); got != DefaultAbility {
		t.Fatalf("got %q, want %q", got, DefaultAbility)
	}
	if got := tc.ResolveAbility(t.Context(), "dev1", ""); got != DefaultAbility {
		t.Fatalf("空包名也应返回默认值，got %q", got)
	}
}

// hilog 不加 -x 会一直挂着刷，命令永不返回，只能等超时被杀。
func TestLogsAlwaysUsesExitAfterRead(t *testing.T) {
	tc := toolchainWithHDC(writeFakeHdc(t, echoArgsScript()))

	res, err := tc.Logs(t.Context(), "dev1", "-T", "MyTag")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "shell hilog -x -T MyTag") {
		t.Fatalf("logs command must include -x and the extra filters: %q", res.Output)
	}
}

func TestHDCFailureSurfacesAsError(t *testing.T) {
	tc := toolchainWithHDC(writeFakeHdc(t, "echo device offline 1>&2 & exit 7"))

	_, err := tc.ListDevices(t.Context())
	if err == nil {
		t.Fatal("non-zero exit must be an error")
	}
	// 报错里要能看见 hdc 说了什么，否则"退出码 7"毫无信息量
	if !strings.Contains(err.Error(), "device offline") {
		t.Fatalf("error must carry the tool output: %v", err)
	}
}
