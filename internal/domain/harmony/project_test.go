package harmony

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// makeProject 造一个符合 OpenHarmony 真实结构的假工程。
//
// 结构照抄本机实测的工程（有 build-profile.json5 与 hvigor/hvigor-config.json5）：
// 根目录有 build-profile.json5 与 hvigor/hvigor-config.json5，
// 模块目录下有 src/main/module.json5，**但没有 hvigorw 脚本**。
func makeProject(t *testing.T, withLocalHvigorw bool) string {
	t.Helper()
	root := t.TempDir()

	files := []string{
		"build-profile.json5",
		"hvigorfile.ts",
		"oh-package.json5",
		filepath.Join("hvigor", "hvigor-config.json5"),
		filepath.Join("entry", "build-profile.json5"),
		filepath.Join("entry", "src", "main", "module.json5"),
		filepath.Join("librarySDK", "src", "main", "module.json5"),
		filepath.Join("entry", "src", "main", "ets", "Index.ets"),
	}
	if withLocalHvigorw {
		files = append(files, "hvigorw.bat")
	}
	for _, f := range files {
		p := filepath.Join(root, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("// stub\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestFindProjectWalksUpFromNestedDir(t *testing.T) {
	root := makeProject(t, false)
	nested := filepath.Join(root, "entry", "src", "main", "ets")

	proj, err := FindProject(nested)
	if err != nil {
		t.Fatal(err)
	}
	if proj.Root != root {
		t.Fatalf("root: %q, want %q", proj.Root, root)
	}
	// 模块判定用 src/main/module.json5，而不是"含 build-profile.json5"
	want := []string{"entry", "librarySDK"}
	if len(proj.Modules) != len(want) {
		t.Fatalf("modules: %v, want %v", proj.Modules, want)
	}
	for i := range want {
		if proj.Modules[i] != want[i] {
			t.Fatalf("modules: %v, want %v", proj.Modules, want)
		}
	}
}

// 本机三个真实工程根目录都没有 hvigorw 脚本——如果用它当判据，
// 一个工程都找不到。这里把这条事实钉在测试里。
func TestFindProjectWorksWithoutLocalHvigorw(t *testing.T) {
	root := makeProject(t, false)
	proj, err := FindProject(root)
	if err != nil {
		t.Fatal(err)
	}
	if proj.Hvigorw != "" {
		t.Fatalf("no local hvigorw expected, got %q", proj.Hvigorw)
	}
}

func TestLocalHvigorwWinsOverToolchain(t *testing.T) {
	proj, err := FindProject(makeProject(t, true))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(proj.Hvigorw, "hvigorw.bat") || proj.HvigorwSource != "工程自带" {
		t.Fatalf("local hvigorw not preferred: %q (%s)", proj.Hvigorw, proj.HvigorwSource)
	}
	// 即使工具链里有，也不该覆盖工程自带的
	tc := &Toolchain{Tools: []Tool{{Name: ToolHvigorw, Path: `D:\deveco\hvigorw.bat`}}}
	if !proj.UseToolchain(tc) {
		t.Fatal("UseToolchain should report usable")
	}
	if proj.HvigorwSource != "工程自带" {
		t.Fatalf("toolchain overrode the project-local builder: %s", proj.HvigorwSource)
	}
}

func TestUseToolchainFallsBackToDevEco(t *testing.T) {
	proj, err := FindProject(makeProject(t, false))
	if err != nil {
		t.Fatal(err)
	}

	tc := &Toolchain{Tools: []Tool{{Name: ToolHvigorw, Path: `D:\deveco\tools\hvigor\bin\hvigorw.bat`}}}
	if !proj.UseToolchain(tc) {
		t.Fatal("should fall back to the DevEco global hvigorw")
	}
	if proj.HvigorwSource != "DevEco 全局" {
		t.Fatalf("source: %q", proj.HvigorwSource)
	}
}

// 两边都没有时必须返回 false：不要假装能构建。
func TestUseToolchainReportsUnavailable(t *testing.T) {
	proj, err := FindProject(makeProject(t, false))
	if err != nil {
		t.Fatal(err)
	}
	if proj.UseToolchain(&Toolchain{}) {
		t.Fatal("must report false when no hvigorw exists anywhere")
	}
	if proj.UseToolchain(nil) {
		t.Fatal("nil toolchain must not panic or claim success")
	}
}

func TestFindProjectOutsideAnyProject(t *testing.T) {
	_, err := FindProject(t.TempDir())
	if err == nil {
		t.Fatal("expected an error outside a project")
	}
	if !strings.Contains(err.Error(), "build-profile.json5") {
		t.Fatalf("error must state the marker used: %v", err)
	}
}

func TestDefaultBuildArgs(t *testing.T) {
	got := DefaultBuildArgs("")
	if len(got) != 2 || got[0] != "assembleHap" || got[1] != "--no-daemon" {
		t.Fatalf("default args: %v", got)
	}
	// --no-daemon 不能丢：常驻 daemon 会持有工程锁，跑完不退只会带来怪问题
	if !strings.Contains(strings.Join(DefaultBuildArgs("clean"), " "), "--no-daemon") {
		t.Fatal("--no-daemon must always be present")
	}
	if DefaultBuildArgs("assembleHsp")[0] != "assembleHsp" {
		t.Fatal("task name must be honored")
	}
}

// 构建产物路径随产品名/模块名变化，猜不如按修改时间取最新的。
func TestFindHapPicksNewestArtifact(t *testing.T) {
	root := makeProject(t, false)

	older := filepath.Join(root, "entry", "build", "default", "outputs", "default", "entry.hap")
	newer := filepath.Join(root, "librarySDK", "build", "default", "outputs", "default", "library.hap")
	for _, p := range []string{older, newer} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("hap"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(older, past, past); err != nil {
		t.Fatal(err)
	}

	proj, err := FindProject(root)
	if err != nil {
		t.Fatal(err)
	}
	got, err := proj.FindHap()
	if err != nil {
		t.Fatal(err)
	}
	if got != newer {
		t.Fatalf("expected the newest artifact %q, got %q", newer, got)
	}
}

func TestFindHapWithoutBuildOutputExplainsItself(t *testing.T) {
	proj, err := FindProject(makeProject(t, false))
	if err != nil {
		t.Fatal(err)
	}
	_, err = proj.FindHap()
	if err == nil {
		t.Fatal("expected an error when nothing was built")
	}
	if !strings.Contains(err.Error(), "构建") {
		t.Fatalf("error should hint that the build may not have run: %v", err)
	}
}

func TestBuildWithoutHvigorwFailsClearly(t *testing.T) {
	proj, err := FindProject(makeProject(t, false))
	if err != nil {
		t.Fatal(err)
	}
	_, err = proj.Build(t.Context(), "assembleHap", nil)
	if err == nil {
		t.Fatal("build without a builder must fail")
	}
	if !strings.Contains(err.Error(), "hvigorw") {
		t.Fatalf("error must name the missing tool: %v", err)
	}
}

// writeEchoBuilder 把工程的构建入口换成一个回显脚本，
// 用来断言我们到底传了哪些环境变量、在哪个目录里跑。
func writeEchoBuilder(t *testing.T, root string) {
	t.Helper()
	var name, body string
	if runtime.GOOS == "windows" {
		name = "hvigorw.bat"
		body = "@echo off\r\necho CWD=%CD%\r\necho SDK=%DEVECO_SDK_HOME%\r\necho ARGS=%*\r\n"
	} else {
		name = "hvigorw"
		body = "#!/bin/sh\necho \"CWD=$PWD\"\necho \"SDK=$DEVECO_SDK_HOME\"\necho \"ARGS=$*\"\n"
	}
	if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// 实测教训：命令行构建不设 DEVECO_SDK_HOME 必然失败，报错是
// "Invalid value of 'DEVECO_SDK_HOME' in the system environment path"。
// DevEco 内置终端会设它，裸 shell 不会——所以必须由我们补上。
func TestBuildRunsInProjectRootAndInjectsSDKHome(t *testing.T) {
	root := makeProject(t, false)
	writeEchoBuilder(t, root)

	// 让宿主机上可能存在的设置失效，才能断言是我们补的
	t.Setenv(EnvDevEcoSDKHome, filepath.Join(t.TempDir(), "not-a-real-sdk"))

	proj, err := FindProject(root)
	if err != nil {
		t.Fatal(err)
	}
	sdk := filepath.Join("D:", "Fake", "sdk")
	proj.UseToolchain(&Toolchain{DevEcoRoot: filepath.Join("D:", "Fake"), SDKDir: sdk})

	res, err := proj.Build(t.Context(), "assembleHap", nil)
	if err != nil {
		t.Fatalf("build: %v\n%s", err, res.Output)
	}
	if !strings.Contains(res.Output, "CWD="+root) {
		t.Fatalf("构建必须在工程根目录里跑（hvigorw 用 cwd 定位工程）：%q", res.Output)
	}
	if !strings.Contains(res.Output, "SDK="+sdk) {
		t.Fatalf("DEVECO_SDK_HOME 必须被补上：%q", res.Output)
	}
	if !strings.Contains(res.Output, "ARGS=assembleHap --no-daemon") {
		t.Fatalf("构建参数不对：%q", res.Output)
	}
}

// 用户可能把 SDK 装在别处，环境里已有有效值时就该听他的。
func TestBuildRespectsValidExistingSDKHome(t *testing.T) {
	root := makeProject(t, false)
	writeEchoBuilder(t, root)

	existing := t.TempDir() // 存在且是目录 = 有效
	t.Setenv(EnvDevEcoSDKHome, existing)

	proj, err := FindProject(root)
	if err != nil {
		t.Fatal(err)
	}
	proj.UseToolchain(&Toolchain{SDKDir: filepath.Join("D:", "Fake", "sdk")})

	res, err := proj.Build(t.Context(), "assembleHap", nil)
	if err != nil {
		t.Fatalf("build: %v\n%s", err, res.Output)
	}
	if !strings.Contains(res.Output, "SDK="+existing) {
		t.Fatalf("环境里已有的有效 SDK 路径不该被覆盖：%q", res.Output)
	}
}

// 没有 SDK 信息时不要瞎塞一个值：塞错了会比不塞更难查。
func TestBuildWithoutSDKInfoDoesNotInventOne(t *testing.T) {
	root := makeProject(t, false)
	writeEchoBuilder(t, root)
	t.Setenv(EnvDevEcoSDKHome, "")

	proj, err := FindProject(root)
	if err != nil {
		t.Fatal(err)
	}
	proj.UseToolchain(&Toolchain{}) // 空工具链，SDKDir 为空

	res, err := proj.Build(t.Context(), "assembleHap", nil)
	if err != nil {
		t.Fatalf("build: %v\n%s", err, res.Output)
	}
	if strings.Contains(res.Output, "SDK=D:") || strings.Contains(res.Output, "SDK=C:") {
		t.Fatalf("不该凭空造一个 SDK 路径：%q", res.Output)
	}
}
