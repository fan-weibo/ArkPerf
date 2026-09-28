package harmony

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeInfo 实现 os.FileInfo，供伪造文件系统使用。
type fakeInfo struct {
	name string
	dir  bool
}

func (i fakeInfo) Name() string       { return i.name }
func (i fakeInfo) Size() int64        { return 0 }
func (i fakeInfo) Mode() os.FileMode  { return 0o644 }
func (i fakeInfo) ModTime() time.Time { return time.Time{} }
func (i fakeInfo) IsDir() bool        { return i.dir }
func (i fakeInfo) Sys() any           { return nil }

// fakeFS 是一个只读的假文件系统：只记录"哪些路径存在"。
type fakeFS struct {
	dirs  map[string]bool
	files map[string]bool
}

func newFakeFS() *fakeFS {
	return &fakeFS{dirs: map[string]bool{}, files: map[string]bool{}}
}

func (f *fakeFS) dir(paths ...string) *fakeFS {
	for _, p := range paths {
		f.dirs[filepath.Clean(p)] = true
	}
	return f
}

func (f *fakeFS) file(paths ...string) *fakeFS {
	for _, p := range paths {
		f.files[filepath.Clean(p)] = true
	}
	return f
}

func (f *fakeFS) stat(p string) (os.FileInfo, error) {
	p = filepath.Clean(p)
	switch {
	case f.files[p]:
		return fakeInfo{name: filepath.Base(p)}, nil
	case f.dirs[p]:
		return fakeInfo{name: filepath.Base(p), dir: true}, nil
	default:
		return nil, os.ErrNotExist
	}
}

// exe 返回本平台上该工具名对应的第一个候选可执行文件名。
func exe(base string) string { return executableNames(base)[0] }

// devEcoLayout 伪造一份 DevEco Studio 的目录布局，路径与真实安装一致。
type devEcoLayout struct {
	root string
	fs   *fakeFS
}

func newDevEcoLayout(root string) *devEcoLayout {
	l := &devEcoLayout{root: root, fs: newFakeFS()}
	l.fs.dir(
		root,
		filepath.Join(root, "sdk"),
		filepath.Join(root, "tools"),
		filepath.Join(root, "sdk", "default", "openharmony", "toolchains"),
		filepath.Join(root, "tools", "hvigor", "bin"),
		filepath.Join(root, "tools", "ohpm", "bin"),
		filepath.Join(root, "tools", "emulator"),
	)
	l.fs.file(
		filepath.Join(root, "sdk", "default", "openharmony", "toolchains", exe("hdc")),
		filepath.Join(root, "tools", "hvigor", "bin", exe("hvigorw")),
		filepath.Join(root, "tools", "ohpm", "bin", exe("ohpm")),
	)
	return l
}

func (l *devEcoLayout) hdcPath() string {
	return filepath.Join(l.root, "sdk", "default", "openharmony", "toolchains", exe("hdc"))
}

func noPath(string) (string, error) { return "", os.ErrNotExist }

// 核心机制：hdc 在 PATH 上时，DevEco 根目录应当被"反推"出来，
// 从而让 hvigorw / ohpm 这些不在 PATH 上的工具也能被找到。
// 这条路径不依赖任何写死的安装目录。
func TestDiscoverDerivesDevEcoRootFromHdcPath(t *testing.T) {
	layout := newDevEcoLayout(filepath.Join("C:", "Some", "Where", "DevEco Studio"))
	hdcPath := layout.hdcPath()

	tc := Discover(t.Context(), DiscoverOptions{
		LookPath: func(name string) (string, error) {
			if name == exe("hdc") {
				return hdcPath, nil
			}
			return "", os.ErrNotExist
		},
		Stat: layout.fs.stat,
	})

	if got := tc.Required(ToolHDC).Path; got != hdcPath {
		t.Fatalf("hdc path: %q", got)
	}
	if tc.DevEcoRoot != layout.root {
		t.Fatalf("DevEco root not derived: %q (want %q)", tc.DevEcoRoot, layout.root)
	}
	if !strings.Contains(tc.DevEcoSource, "反推") {
		t.Fatalf("source should say how the root was found: %q", tc.DevEcoSource)
	}
	for _, name := range []string{ToolHvigorw, ToolOhpm} {
		tool := tc.Required(name)
		if !tool.Found() {
			t.Fatalf("%s should be found under the derived root", name)
		}
		if !strings.Contains(tool.Source, "DevEco") {
			t.Fatalf("%s source: %q", name, tool.Source)
		}
	}
	if tc.EmulatorDir == "" {
		t.Fatal("emulator dir should be detected under tools/emulator")
	}
}

func TestDiscoverWithExplicitRoot(t *testing.T) {
	layout := newDevEcoLayout(`D:\Huawei\DevEco Studio`)

	tc := Discover(t.Context(), DiscoverOptions{
		DevEcoRoot: layout.root,
		LookPath:   noPath,
		Stat:       layout.fs.stat,
	})

	if !strings.Contains(tc.DevEcoSource, "指定") {
		t.Fatalf("source: %q", tc.DevEcoSource)
	}
	if !tc.Required(ToolHDC).Found() || !tc.Required(ToolHvigorw).Found() {
		t.Fatalf("tools under explicit root not found: %s", tc.Summary())
	}
}

// 什么都没有时要如实报缺失，并且给出可操作的提示与"我找过哪里"。
func TestDiscoverReportsMissingHonestly(t *testing.T) {
	empty := newFakeFS()
	tc := Discover(t.Context(), DiscoverOptions{LookPath: noPath, Stat: empty.stat})

	if tc.DevEcoRoot != "" {
		t.Fatalf("no root should be derived, got %q", tc.DevEcoRoot)
	}
	for _, tool := range tc.Tools {
		if tool.Found() {
			t.Fatalf("%s unexpectedly found", tool.Name)
		}
		if tool.Hint == "" {
			t.Fatalf("%s missing without a hint — 用户不知道下一步该做什么", tool.Name)
		}
		if len(tool.Searched) == 0 {
			t.Fatalf("%s missing without listing where we looked", tool.Name)
		}
		if !strings.Contains(tool.Describe(), "未找到") {
			t.Fatalf("describe: %q", tool.Describe())
		}
	}
	if !strings.Contains(tc.Summary(), "缺：") {
		t.Fatalf("summary must name what is missing: %q", tc.Summary())
	}
}

// hdc 在 PATH 上、但周边没有 DevEco 时：不能凭空造出根目录。
func TestDiscoverDoesNotInventRoot(t *testing.T) {
	fs := newFakeFS().dir(filepath.Join("C:", "Tools", "bin")).
		file(filepath.Join("C:", "Tools", "bin", exe("hdc")))
	hdcPath := filepath.Join("C:", "Tools", "bin", exe("hdc"))

	tc := Discover(t.Context(), DiscoverOptions{
		LookPath: func(name string) (string, error) {
			if name == exe("hdc") {
				return hdcPath, nil
			}
			return "", os.ErrNotExist
		},
		Stat: fs.stat,
	})

	if tc.DevEcoRoot != "" {
		t.Fatalf("must not invent a DevEco root, got %q", tc.DevEcoRoot)
	}
	if !tc.Required(ToolHDC).Found() {
		t.Fatal("hdc from PATH should be found")
	}
	if tc.Required(ToolHvigorw).Found() {
		t.Fatal("hvigorw cannot exist without a DevEco root here")
	}
}

func TestDeriveDevEcoRootBoundaries(t *testing.T) {
	layout := newDevEcoLayout(filepath.Join("C:", "a", "b", "DevEco"))

	// 从 hdc 所在目录出发应当找到根
	if got := deriveDevEcoRoot(filepath.Dir(layout.hdcPath()), layout.fs.stat); got != layout.root {
		t.Fatalf("derive from toolchain dir: %q", got)
	}
	// 完全不相关的路径不该命中
	empty := newFakeFS().dir(`C:\orphan`)
	if got := deriveDevEcoRoot(`C:\orphan\deep`, empty.stat); got != "" {
		t.Fatalf("unrelated path must not match: %q", got)
	}
}

func TestExecutableNamesPerPlatform(t *testing.T) {
	names := executableNames("hdc")
	if len(names) == 0 {
		t.Fatal("no candidate names")
	}
	if names[0] != exe("hdc") {
		t.Fatalf("unstable first candidate: %v", names)
	}
}

// hdc -v 的真实输出是 "Ver: 3.2.0d"，展示时只留版本号本身。
func TestFirstLineAndVersionExtraction(t *testing.T) {
	if got := firstLine("\n\nVer: 3.2.0d\n"); got != "Ver: 3.2.0d" {
		t.Fatalf("firstLine: %q", got)
	}
	if got := firstLine("   \n\t\n"); got != "" {
		t.Fatalf("firstLine of blank: %q", got)
	}

	tool := Tool{Name: ToolHDC, Path: "hdc"}
	_ = tool
	if args := versionArgs(ToolHDC); len(args) != 1 || args[0] != "-v" {
		t.Fatalf("hdc version args: %v", args)
	}
	if args := versionArgs(ToolHvigorw); args[0] != "--version" {
		t.Fatalf("hvigorw version args: %v", args)
	}
}

func TestToolDescribe(t *testing.T) {
	found := Tool{Name: "hdc", Path: `C:\x\hdc.exe`, Source: "PATH", Version: "3.2.0d"}
	got := found.Describe()
	for _, want := range []string{"hdc", `C:\x\hdc.exe`, "PATH", "3.2.0d"} {
		if !strings.Contains(got, want) {
			t.Fatalf("describe missing %q: %q", want, got)
		}
	}
}

// Discover 的版本探测是可选的：默认不启动子进程。
func TestDiscoverSkipsVersionProbeByDefault(t *testing.T) {
	layout := newDevEcoLayout(`D:\DevEco Studio`)
	tc := Discover(context.Background(), DiscoverOptions{
		DevEcoRoot: layout.root,
		LookPath:   noPath,
		Stat:       layout.fs.stat,
	})
	for _, tool := range tc.Tools {
		if tool.Version != "" {
			t.Fatalf("%s version should not be probed by default, got %q", tool.Name, tool.Version)
		}
	}
}
