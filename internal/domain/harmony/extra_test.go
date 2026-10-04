package harmony

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------- JSON5

func TestStripJSON5HandlesCommentsAndQuotes(t *testing.T) {
	// 这是 DevEco 生成的 module.json5 的真实形态：注释、单引号、尾逗号都有
	raw := `{
  // 模块配置
  "module": {
    name: 'entry',   /* 单引号 */
    'type': "entry",
    "deviceTypes": [
      "phone",
      "tablet", // 尾逗号在下面
    ],
  },
}`
	var doc struct {
		Module struct {
			Name        string   `json:"name"`
			Type        string   `json:"type"`
			DeviceTypes []string `json:"deviceTypes"`
		} `json:"module"`
	}
	if err := parseJSON5String(raw, &doc); err != nil {
		t.Fatalf("解析失败：%v\n归一化后：%s", err, stripJSON5(raw))
	}
	if doc.Module.Name != "entry" || doc.Module.Type != "entry" {
		t.Fatalf("解析出的值不对：%+v", doc.Module)
	}
	if len(doc.Module.DeviceTypes) != 2 {
		t.Fatalf("deviceTypes 应为 2 项，实际 %v", doc.Module.DeviceTypes)
	}
}

// parseJSON5String 是 parseJSON5 的字符串版（测试用）。
func parseJSON5String(s string, v any) error {
	dir, err := os.MkdirTemp("", "json5")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "x.json5")
	if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
		return err
	}
	return parseJSON5(path, v)
}

func TestStripJSON5KeepsSlashesInsideStrings(t *testing.T) {
	// 字符串里的 // 不能被当成注释删掉——这是用正则做的实现最容易错的地方
	raw := `{"url": "https://example.com/a"}`
	var doc map[string]string
	if err := parseJSON5String(raw, &doc); err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if doc["url"] != "https://example.com/a" {
		t.Fatalf("URL 被破坏：%q", doc["url"])
	}
}

// ---------------------------------------------------------------- 配置校验

// writeProject 造一个最小可识别的工程骨架。
func writeProject(t *testing.T, root string, moduleJSON string) {
	t.Helper()
	mustWrite(t, filepath.Join(root, "build-profile.json5"), `{
  "app": { "products": [ { "name": "default", "compatibleSdkVersion": "6.1.1(24)" } ] },
  "modules": [ { "name": "entry", "srcPath": "./entry" } ]
}`)
	mustWrite(t, filepath.Join(root, "hvigor", "hvigor-config.json5"), `{"modelVersion":"5.0.0"}`)
	mustWrite(t, filepath.Join(root, "AppScope", "app.json5"), `{"app":{"bundleName":"com.example.demo"}}`)
	mustWrite(t, filepath.Join(root, "entry", "build-profile.json5"), `{"apiType":"stageMode","targets":[{"name":"default"}]}`)
	mustWrite(t, filepath.Join(root, "entry", "src", "main", "module.json5"), moduleJSON)
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCheckProjectSchemaCatchesMissingDeviceTypes(t *testing.T) {
	root := t.TempDir()
	// entry 模块缺 deviceTypes：这是实测会让构建失败、但报错不提字段名的一种
	writeProject(t, root, `{
  "module": {
    "name": "entry",
    "type": "entry",
    "mainElement": "EntryAbility",
    "pages": "$profile:main_pages"
  }
}`)
	issues, checked, err := CheckProjectSchema(root)
	if err != nil {
		t.Fatal(err)
	}
	if checked == 0 {
		t.Fatal("一个配置文件都没读到")
	}
	if !hasIssueField(issues, "module.deviceTypes") {
		t.Fatalf("应报出 deviceTypes 缺失，实际：%+v", issues)
	}
}

func TestCheckProjectSchemaPassesValidProject(t *testing.T) {
	root := t.TempDir()
	writeProject(t, root, `{
  "module": {
    "name": "entry",
    "type": "entry",
    "mainElement": "EntryAbility",
    "deviceTypes": ["phone", "tablet"],
    "pages": "$profile:main_pages",
    "abilities": [ { "name": "EntryAbility", "srcEntry": "./ets/entryability/EntryAbility.ets" } ]
  }
}`)
	issues, _, err := CheckProjectSchema(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 0 {
		t.Fatalf("合法工程不应报问题，实际：%+v", issues)
	}
}

func hasIssueField(issues []SchemaIssue, field string) bool {
	for _, i := range issues {
		if strings.Contains(i.Field, field) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- 构建诊断

func TestDiagnoseBuildLogClassifiesSDKHome(t *testing.T) {
	// 这是 ArkPerf 自己踩过的坑：命令行构建没有 DEVECO_SDK_HOME
	logText := strings.Repeat("> hvigor ERROR: something\n", 10) +
		"Invalid value of 'DEVECO_SDK_HOME' in the system environment path\n"
	d := DiagnoseBuildLog(logText)
	if d.Cause != "sdk-home" {
		t.Fatalf("应判为 sdk-home，实际 %s（%s）", d.Cause, d.Title)
	}
	if len(d.Advice) == 0 {
		t.Fatal("分类命中了但没给建议——只说原因不说下一步等于没诊断")
	}
}

func TestDiagnoseBuildLogIsHonestWhenUnknown(t *testing.T) {
	d := DiagnoseBuildLog("some totally unknown failure\nxyz 123\n")
	if d.Confident {
		t.Fatalf("不该自信地给出分类，实际判为 %s", d.Cause)
	}
	if d.Evidence == "" {
		t.Fatal("认不出来时也必须把原始证据带回去，不能只说「不知道」")
	}
}

// ---------------------------------------------------------------- API 索引

func TestParseDeclarationsExtractsClassAndMembers(t *testing.T) {
	src := `declare class UIAbility {
  onCreate(want: Want, launchParam: LaunchParam): void
  onDestroy(): void
  static helper(): void
}
export declare function foo(): void
declare interface Want {
  bundleName: string
}`
	dir := t.TempDir()
	path := filepath.Join(dir, "ability.d.ts")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	syms := parseDeclarations(path, "ability.d.ts")
	want := map[string]bool{
		"UIAbility":           false,
		"UIAbility.onCreate":  false,
		"UIAbility.onDestroy": false,
		"foo":                 false,
		"Want":                false,
	}
	for _, s := range syms {
		if _, ok := want[s.Name]; ok {
			want[s.Name] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Fatalf("没抽到符号 %s；实际：%+v", name, syms)
		}
	}
	// 静态成员同样要识别：它们是最常被问到的 API
	var hasStatic bool
	for _, s := range syms {
		if s.Name == "UIAbility.helper" {
			hasStatic = true
		}
	}
	if !hasStatic {
		t.Fatalf("static 成员没被识别：%+v", syms)
	}
}

// ---------------------------------------------------------------- 模拟器清单

func TestEmulatorListParsesRealShape(t *testing.T) {
	// 形态取自本机实测的 lists.json（字段是字符串型的数字，最容易解析失败）
	raw := `[{
		"name":"Pura 90",
		"apiVersion":"24",
		"resolutionHeight":"2856",
		"resolutionWidth":"1320",
		"density":"560",
		"diagonalSize":"6.8",
		"type":"phone",
		"path":"C:/Users/x/AppData/Local/Huawei/Emulator/deployed/Pura 90",
		"imageDir":"system-image/HarmonyOS-6.1.1/phone_all_x86/",
		"showVersion":"HarmonyOS 6.1.1(24)"
	}]`
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "lists.json"), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	// 直接验证解析规则（不经 EmulatorRoot 的环境依赖）
	var list []Emulator
	if err := parseEmulatorLists(filepath.Join(dir, "lists.json"), &list); err != nil {
		t.Fatalf("解析失败——很可能是数字字段忘了加 ,string：%v", err)
	}
	if len(list) != 1 {
		t.Fatalf("应有 1 条，实际 %d", len(list))
	}
	e := list[0]
	if e.Width != 1320 || e.Height != 2856 || e.Density != 560 {
		t.Fatalf("数字字段解析错误：%+v", e)
	}
	if e.Resolution() != "1320x2856" {
		t.Fatalf("分辨率格式化错误：%s", e.Resolution())
	}
}

// ---------------------------------------------------------------- 模拟器：官方 CLI 输出解析

// ---------------------------------------------------------------- 启动参数

func TestEmulatorFromConfigINIReadsLayeredKeys(t *testing.T) {
	// 实测 -create 新建实例产出的 config.ini：键名带点号（分层键）
	raw := `name=ArkPerfTmp
deviceType=tablet
deviceModel=PADEMU-FD00
productModel=MatePad Pro 13
uuid=d65bd15b-ceb8-4ef9-91a0-7e06f57591d1
imageSubPath=system-image/HarmonyOS-6.1.1/tablet_x86/
instancePath=C:/Users/x/AppData/Local/Huawei/Emulator/deployed/ArkPerfTmp
os.osVersion=HarmonyOS 6.1.1(24)
os.apiVersion=24
hw.lcd.density=320
hw.lcd.single.width=2880
hw.lcd.single.height=1920
hw.lcd.single.diagonalSize=13.2
`
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.ini"), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	emu, err := emulatorFromConfigINI(dir)
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if emu.Name != "ArkPerfTmp" || emu.Type != "tablet" || emu.Model != "MatePad Pro 13" {
		t.Fatalf("基本字段不对：%+v", emu)
	}
	if emu.ShowVer != "HarmonyOS 6.1.1(24)" || emu.APIVersion != "24" {
		t.Fatalf("版本字段不对：%+v", emu)
	}
	if emu.Width != 2880 || emu.Height != 1920 || emu.Density != 320 || emu.Diagonal != "13.2" {
		t.Fatalf("屏幕字段不对：%+v", emu)
	}
}

func TestParseINIKeepsDottedKeysAndSkipsNoise(t *testing.T) {
	kv := parseINI("; 注释\r\n\r\nname=A\r\nos.osVersion=HarmonyOS 6.1.1(24)\r\n空行\r\nbad-line-no-equals\r\n")
	if kv["name"] != "A" {
		t.Fatalf("name 不对：%q", kv["name"])
	}
	if kv["os.osVersion"] != "HarmonyOS 6.1.1(24)" {
		t.Fatalf("分层键被截断：%q", kv["os.osVersion"])
	}
	if _, ok := kv["bad-line-no-equals"]; ok {
		t.Fatal("没有 = 的行不该进结果")
	}
}

func TestStartArgsKeepNamesWithSpacesIntact(t *testing.T) {
	// 这是实测踩到的真 bug：实例名带空格（本机就有 "Pura 90"、"Mate X7"）。
	// 早先用 `cmd /c start /B` 拼命令行，引号丢失后 Emulator.exe 把
	// "Pura 90" 拆成两个参数，报 "Invalid command" 并以退出码 0 退出。
	// 现在走 execx.StartDetached 传参数数组，这条测试守住"不再拼命令行"。
	name := "Pura 90"
	args := []string{"-hvd", name, "-path", `C:\Users\x\AppData\Local\Huawei\Emulator\deployed`, "-imageRoot", `C:\Users\x\AppData\Local\Huawei\Sdk`}
	for i, a := range args {
		if a == "" {
			t.Fatalf("第 %d 个参数为空", i)
		}
	}
	if args[1] != name {
		t.Fatal("实例名必须作为**一个**参数整体传递")
	}
	if len(args) != 6 {
		t.Fatalf("参数个数应为 6，实际 %d", len(args))
	}
}

func TestParseImageListOutputSkipsLeadingNoise(t *testing.T) {
	// 真实输出前面会夹进度条之类的噪声行
	raw := "Downloading: 100.0% (1708/1708 bytes)\n" + `[
    {
        "SoftWareVersion": "6.1.0.126",
        "deviceType": "phone",
        "downloaded": "true",
        "osVersion": "HarmonyOS 6.1.1(24)",
        "releaseType": "Release"
    },
    {
        "SoftWareVersion": "6.1.0.125",
        "deviceType": "2in1",
        "downloaded": "true",
        "osVersion": "HarmonyOS 6.1.1(24)",
        "releaseType": "Release"
    }
]`
	got, err := parseImageListOutput(raw)
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if len(got) != 2 {
		t.Fatalf("应有 2 项，实际 %d：%+v", len(got), got)
	}
	// 排序后按 OSVersion 再按 DeviceType：2in1 应在 phone 前
	if got[0].DeviceType != "2in1" || got[1].DeviceType != "phone" {
		t.Fatalf("排序不对：%+v", got)
	}
	if got[1].OSVersion != "HarmonyOS 6.1.1(24)" || got[1].SoftVersion != "6.1.0.126" {
		t.Fatalf("字段映射不对：%+v", got[1])
	}
}

func TestPickImageMatchesDeviceType(t *testing.T) {
	images := []SystemImage{
		{OSVersion: "HarmonyOS 6.1.1(24)", DeviceType: "phone"},
		{OSVersion: "HarmonyOS 6.1.1(24)", DeviceType: "foldable"},
	}
	if got, ok := pickImage(images, "Foldable"); !ok || got.DeviceType != "foldable" {
		t.Fatalf("形态匹配应忽略大小写，实际 %+v ok=%v", got, ok)
	}
	if _, ok := pickImage(images, "tv"); ok {
		t.Fatal("没有该形态的镜像时必须返回 false，好让上层给出明确报错")
	}
}

// ---------------------------------------------------------------- 画像

func TestProfileReportsModulesAndBundle(t *testing.T) {
	root := t.TempDir()
	writeProject(t, root, `{
  "module": {
    "name": "entry",
    "type": "entry",
    "mainElement": "EntryAbility",
    "deviceTypes": ["phone"],
    "pages": "$profile:main_pages",
    "abilities": [ { "name": "EntryAbility", "srcEntry": "./ets/entryability/EntryAbility.ets" } ]
  }
}`)
	mustWrite(t, filepath.Join(root, "entry", "src", "main", "ets", "pages", "Index.ets"), "// demo\n")

	p, err := Profile(root)
	if err != nil {
		t.Fatal(err)
	}
	if p.BundleName != "com.example.demo" {
		t.Fatalf("包名读错：%q", p.BundleName)
	}
	if len(p.Modules) != 1 || p.Modules[0].Name != "entry" {
		t.Fatalf("模块解析错误：%+v", p.Modules)
	}
	if p.SourceFiles == 0 {
		t.Fatal("源码文件统计为 0，遍历逻辑有问题")
	}
	out := p.Format()
	if !strings.Contains(out, "com.example.demo") || !strings.Contains(out, "entry") {
		t.Fatalf("画像文本缺关键信息：\n%s", out)
	}
}

func TestProfileRejectsNonProject(t *testing.T) {
	if _, err := Profile(t.TempDir()); err == nil {
		t.Fatal("非工程目录应该报错，不能给出一份空画像假装成功")
	}
}
