package harmony

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fan-weibo/ArkPerf/internal/execx"
)

// 模拟器数据在本机的位置。
//
// 关键认识：**它与 DevEco 安装目录无关**。工具链在 DevEco 里（Program Files
// 或别处），而模拟器实例与镜像都在用户目录下：
//
//	%LOCALAPPDATA%\Huawei\Emulator\deployed   实例目录 + lists.json
//	%LOCALAPPDATA%\Huawei\Sdk                 system-image + productConfig.json
//
// 用 DevEco 根去推这两个目录是错的——实测本机 DevEco 在 D 盘，模拟器数据
// 全在 C 盘的用户目录里。
const (
	emulatorDeployedRel = "Huawei/Emulator/deployed"
	emulatorImageRel    = "Huawei/Sdk"
	emulatorListsFile   = "lists.json"
	emulatorCatalogFile = "productConfig.json"
	emulatorImageDir    = "system-image"
)

// 模拟器相关的超时。启动一次模拟器要 30-90s（它要真的启动一个 QEMU 虚拟机），
// 轮询窗口必须给够；给小了只会得到"启动失败"的假象。
const (
	timeoutEmulatorStart = 180 * time.Second
	timeoutEmulatorStop  = 30 * time.Second
	timeoutProcQuery     = 30 * time.Second
	pollEmulatorInterval = 5 * time.Second
)

// Emulator 是一个已部署的模拟器实例。
type Emulator struct {
	Name       string `json:"name"`
	Type       string `json:"type"` // phone / tablet / 2in1 / foldable / wearable
	APIVersion string `json:"apiVersion"`
	ShowVer    string `json:"showVersion"`
	Model      string `json:"model"`
	DevModel   string `json:"devModel"`
	UUID       string `json:"uuid"`
	// 这些字段在 lists.json 里是**字符串**（"3120" 而不是 3120），
	// 必须加 ,string —— 否则 json 解析直接失败，整个清单读不出来。
	Width    int    `json:"resolutionWidth,string"`
	Height   int    `json:"resolutionHeight,string"`
	Density  int    `json:"density,string"`
	Diagonal string `json:"diagonalSize"`
	ImageDir string `json:"imageDir"`
	Path     string `json:"path"`
	ABI      string `json:"abi"`
	// Running 由进程查询结果补上，不是 lists.json 的字段。
	Running bool `json:"-"`
}

// Resolution 返回 "宽x高" 的可读形式。
func (e Emulator) Resolution() string {
	if e.Width == 0 || e.Height == 0 {
		return "（未知）"
	}
	return strconv.Itoa(e.Width) + "x" + strconv.Itoa(e.Height)
}

// EmulatorRoot 返回模拟器实例目录（deployed）。
//
// 找不到 LOCALAPPDATA 时回退到 <USERPROFILE>/AppData/Local —— 这与 DevEco
// 自己的行为一致，免得在某些精简环境里整个能力消失。
func EmulatorRoot() string {
	base := strings.TrimSpace(os.Getenv("LOCALAPPDATA"))
	if base == "" {
		if home, err := os.UserHomeDir(); err == nil {
			base = filepath.Join(home, "AppData", "Local")
		}
	}
	if base == "" {
		return ""
	}
	return filepath.Join(base, filepath.FromSlash(emulatorDeployedRel))
}

// EmulatorImageRoot 返回镜像根目录（含 system-image 与 productConfig.json）。
func EmulatorImageRoot() string {
	base := strings.TrimSpace(os.Getenv("LOCALAPPDATA"))
	if base == "" {
		if home, err := os.UserHomeDir(); err == nil {
			base = filepath.Join(home, "AppData", "Local")
		}
	}
	if base == "" {
		return ""
	}
	return filepath.Join(base, filepath.FromSlash(emulatorImageRel))
}

// ListEmulators 读取已部署的模拟器清单。
//
// 读不到文件不是错误：全新机器上这个文件根本不存在，
// 报成错误会让上层以为"模拟器坏了"，而实际只是"还没建过模拟器"。
func ListEmulators() ([]Emulator, error) {
	root := EmulatorRoot()
	if root == "" {
		return nil, nil
	}
	var list []Emulator
	if err := parseEmulatorLists(filepath.Join(root, emulatorListsFile), &list); err != nil {
		// 文件不存在 = 没有模拟器，不是失败
		if errors.Is(err, os.ErrNotExist) {
			list = nil
		} else {
			return nil, err
		}
	}
	// lists.json 不是完备来源：实测 `Emulator.exe -create` 建出来的实例
	// 只生成 <名字>.ini + <名字>/config.ini，要等**首次启动成功**才写进 lists.json。
	// 只读 lists.json 的话，"刚建好的实例"会查不到，紧接着的 start 就失败。
	// 所以再扫一遍实例目录补齐缺口。
	list = mergeInstanceDirs(root, list)
	sort.SliceStable(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	return list, nil
}

// mergeInstanceDirs 把"目录里有、清单里没有"的实例补进清单。
func mergeInstanceDirs(root string, list []Emulator) []Emulator {
	known := make(map[string]bool, len(list))
	for _, e := range list {
		known[strings.ToLower(e.Name)] = true
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return list
	}
	for _, ent := range entries {
		if !ent.IsDir() || known[strings.ToLower(ent.Name())] {
			continue
		}
		// 指针 ini 是权威存在标志：没有它不代表是实例（有可能是别的缓存目录）
		if !existsNoErr(filepath.Join(root, ent.Name()+".ini")) {
			continue
		}
		emu, err := emulatorFromConfigINI(filepath.Join(root, ent.Name()))
		if err != nil {
			continue
		}
		known[strings.ToLower(emu.Name)] = true
		list = append(list, emu)
	}
	return list
}

// emulatorFromConfigINI 从实例目录的 config.ini 还原实例信息。
//
// 字段名带点号（os.osVersion / hw.lcd.single.width），是 ini 的分层键。
func emulatorFromConfigINI(dir string) (Emulator, error) {
	data, err := os.ReadFile(filepath.Join(dir, "config.ini"))
	if err != nil {
		return Emulator{}, err
	}
	kv := parseINI(string(data))
	name := kv["name"]
	if name == "" {
		return Emulator{}, errors.New("config.ini 里没有 name")
	}
	path := kv["instancePath"]
	if path == "" {
		path = filepath.ToSlash(dir)
	}
	return Emulator{
		Name:       name,
		Type:       kv["deviceType"],
		APIVersion: kv["os.apiVersion"],
		ShowVer:    kv["os.osVersion"],
		Model:      kv["productModel"],
		UUID:       kv["uuid"],
		Path:       path,
		Width:      anyInt(kv["hw.lcd.single.width"]),
		Height:     anyInt(kv["hw.lcd.single.height"]),
		Density:    anyInt(kv["hw.lcd.density"]),
		Diagonal:   kv["hw.lcd.single.diagonalSize"],
		ImageDir:   kv["imageSubPath"],
	}, nil
}

// parseINI 解析 key=value 形式的 ini（忽略空行与 ; 注释）。
func parseINI(content string) map[string]string {
	out := make(map[string]string, 32)
	for line := range strings.SplitSeq(strings.ReplaceAll(content, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, ";") || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return out
}

// parseEmulatorLists 解析 lists.json。
//
// 单独抽出来是因为字段类型很坑（数字写成字符串），必须能脱离环境直接测——
// 等真机上才发现解析失败就晚了。
func parseEmulatorLists(path string, out *[]Emulator) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("读取模拟器清单 %s 失败：%w", path, err)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("解析模拟器清单 %s 失败：%w", path, err)
	}
	return nil
}

// FindEmulator 按名字找实例（大小写不敏感，容错"pura 90"）。
func FindEmulator(name string) (Emulator, error) {
	list, err := ListEmulators()
	if err != nil {
		return Emulator{}, err
	}
	if len(list) == 0 {
		return Emulator{}, errors.New("本机没有已部署的模拟器（先用 harmony_emulator_create 或 DevEco 创建一个）")
	}
	target := strings.ToLower(strings.TrimSpace(name))
	if target != "" {
		for _, e := range list {
			if strings.EqualFold(e.Name, target) {
				return e, nil
			}
		}
		return Emulator{}, fmt.Errorf("没有名为 %q 的模拟器；已部署：%s", name, strings.Join(emulatorNames(list), "、"))
	}
	if len(list) > 1 {
		return Emulator{}, fmt.Errorf("本机有 %d 个模拟器，请指定 name：%s", len(list), strings.Join(emulatorNames(list), "、"))
	}
	return list[0], nil
}

func emulatorNames(list []Emulator) []string {
	names := make([]string, 0, len(list))
	for _, e := range list {
		names = append(names, e.Name)
	}
	return names
}

// ---------------------------------------------------------------- 运行状态

// emulatorArgsPattern 从 Emulator.exe 的命令行里提取 -hvd 后面的设备名。
//
// 设备名可能带空格（本机实测就有 "Mate X7" / "MateBook Pro"），所以两种写法
// 都要认：带引号的 `-hvd "Mate X7"` 和不带引号的 `-hvd MateX7`。
var emulatorArgsPattern = regexp.MustCompile(`-hvd\s+"([^"]+)"|-hvd\s+([^\s]+)`)

// RunningEmulators 返回正在运行的模拟器名（从进程命令行解析）。
//
// 为什么不用 hdc list targets 判断：hdc 里出现的目标也可能是真机或网络设备，
// 而"哪个 Emulator.exe 进程对应哪个实例"只有进程命令行知道。
func RunningEmulators(ctx context.Context) ([]string, error) {
	if runtime.GOOS != "windows" {
		return nil, nil
	}
	ps, err := exec.LookPath("powershell")
	if err != nil {
		return nil, errors.New("找不到 powershell，无法查询模拟器进程")
	}
	script := "Get-CimInstance Win32_Process | Where-Object { $_.Name -eq 'Emulator.exe' } | Select-Object -ExpandProperty CommandLine"
	res, err := execx.Run(ctx, ps, []string{"-NoProfile", "-Command", script}, execx.Options{
		Timeout:   timeoutProcQuery,
		MaxOutput: 1 << 20,
	})
	if err != nil {
		// 没有任何 Emulator.exe 时 CIM 查询仍会成功但输出为空；
		// 真出错了就如实说出来，不要假装"没有在跑的"。
		return nil, fmt.Errorf("查询模拟器进程失败：%w", err)
	}
	var names []string
	for line := range strings.SplitSeq(res.Output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if m := emulatorArgsPattern.FindStringSubmatch(line); m != nil {
			name := m[1]
			if name == "" {
				name = m[2]
			}
			if name != "" && !slicesContains(names, name) {
				names = append(names, name)
			}
		}
	}
	sort.Strings(names)
	return names, nil
}

// anyInt / anyString 把 JSON 里的值转成 int / string。
//
// 同一份配置里数字有时是数字、有时是字符串（实测过两种都出现），
// 所以两个方向都要认，而不是假定一种写法。
func anyInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case string:
		if i, err := strconv.Atoi(strings.TrimSpace(n)); err == nil {
			return i
		}
	}
	return 0
}

func anyString(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case float64:
		return strconv.FormatFloat(s, 'f', -1, 64)
	}
	return ""
}

func slicesContains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- 启停

// EmulatorBinary 定位 Emulator.exe。
func (tc *Toolchain) EmulatorBinary() (string, error) {
	if runtime.GOOS != "windows" {
		return "", errors.New("模拟器管理当前只支持 Windows（需要 DevEco 的 Emulator.exe）")
	}
	if tc != nil && tc.EmulatorDir != "" {
		p := filepath.Join(tc.EmulatorDir, "Emulator.exe")
		if existsNoErr(p) {
			return p, nil
		}
	}
	if tc != nil && tc.DevEcoRoot != "" {
		p := filepath.Join(tc.DevEcoRoot, "tools", "emulator", "Emulator.exe")
		if existsNoErr(p) {
			return p, nil
		}
	}
	return "", errors.New("找不到 Emulator.exe（DevEco 安装不完整，或 tools/emulator 目录缺失）")
}

// emulatorCLI 执行一次 Emulator.exe 的**管理类**子命令（-license / -list /
// -imageList / -stop / -create / -delete）。
//
// 为什么必须设 Dir：Emulator.exe 是 Qt 程序，从自身目录相对加载平台插件与资源。
// 工作目录不对时它不会报错，而是直接静默退出——这类"无错误信息失败"最难查。
func (tc *Toolchain) emulatorCLI(ctx context.Context, timeout time.Duration, args ...string) (execx.Result, error) {
	bin, err := tc.EmulatorBinary()
	if err != nil {
		return execx.Result{}, err
	}
	return execx.Run(ctx, bin, args, execx.Options{
		Timeout: timeout,
		Dir:     filepath.Dir(bin),
	})
}

// 关于许可协议（踩过的坑，记在这里避免后人重复）：
//
// `Emulator.exe -license` 看起来像"查协议是否已接受"，实际不是——它总是打印
// "There are N license agreements that need to be reviewed." 并等你输入 y/N。
// 实测即便刚执行过 `-license accept`（回 "All licenses have been automatically
// accepted."），再跑 `-license` 仍是同一句。所以**它的输出不能当作状态判据**，
// 拿去做启动前的门禁会造成假阴性：明明能起，却被拦下。
//
// 若确实遇到协议阻塞，让用户在 DevEco Studio 里启动一次点同意，或执行
// `Emulator.exe -license accept`。

// StartEmulator 启动一个已部署的模拟器，并等待它出现在 hdc 设备列表里。
//
// 启动方式刻意用 `cmd /c start /B`：模拟器是长期运行的 GUI 程序，
// 直接 exec 会让它变成当前进程的子进程，父进程一退出它也跟着被带走
// （在 Agent 里就是"工具一返回，模拟器就消失"）。
//
// 返回值里的 waited 说明是否真的等到设备上线：等到了才敢说成功。
func (tc *Toolchain) StartEmulator(ctx context.Context, name string) (waited bool, out string, err error) {
	bin, err := tc.EmulatorBinary()
	if err != nil {
		return false, "", err
	}
	emu, err := FindEmulator(name)
	if err != nil {
		return false, "", err
	}
	running, _ := RunningEmulators(ctx)
	if slicesContains(running, emu.Name) {
		return true, emu.Name + " 已经在运行", nil
	}

	deployed := EmulatorRoot()
	imageRoot := EmulatorImageRoot()
	args := []string{"-hvd", emu.Name, "-path", deployed, "-imageRoot", imageRoot}

	// 分离启动（参数按数组传递，不做任何命令行拼接，避免空格/引号问题）。
	// Dir 必须设为 Emulator.exe 所在目录：它是 Qt 程序，从自身目录加载插件。
	pid, err := execx.StartDetached(bin, args, execx.Options{Dir: filepath.Dir(bin)})
	if err != nil {
		return false, "", fmt.Errorf("启动模拟器失败：%w", err)
	}
	out = fmt.Sprintf("已拉起 Emulator.exe（pid=%d）：%s", pid, execx.Display(bin, args))

	// 轮询 hdc list targets：模拟器启动要 30-90s，
	// 而且"进程起来了"不等于"设备已连上"，必须看 hdc。
	deadline := time.Now().Add(timeoutEmulatorStart)
	saw := map[string]bool{}
	if before, lerr := tc.ListDevices(ctx); lerr == nil {
		for _, d := range before {
			saw[d.Serial] = true
		}
	}
	// 快速失败检测：先给进程一个"露面窗口"——刚 Start 完进程表可能还没更新，
	// 立刻查会误判成没起来。窗口内始终没出现 = 启动命令被直接拒绝。
	if !waitEmulatorAlive(ctx, emu.Name, 12*time.Second) {
		return false, out, &EmulatorStartTimeout{
			Name: emu.Name, Running: false, Log: EmulatorLogPath(emu.Name),
		}
	}
	for {
		select {
		case <-ctx.Done():
			return false, out, ctx.Err()
		default:
		}
		// 已经露过面又没了 = 启动过程中失败（参数被拒 / 镜像缺失 / 虚拟化未开），
		// 继续等满 180s 纯属浪费时间。
		if !emulatorStillRunning(ctx, emu.Name) {
			return false, out, &EmulatorStartTimeout{
				Name: emu.Name, Running: false, Log: EmulatorLogPath(emu.Name),
			}
		}
		if devs, lerr := tc.ListDevices(ctx); lerr == nil {
			for _, d := range devs {
				if !saw[d.Serial] {
					return true, fmt.Sprintf("%s 已启动并上线：%s", emu.Name, d.Serial), nil
				}
			}
			// 单设备场景下新设备可能与之前同名（都是 127.0.0.1:5555），
			// 此时"数量增加"看不出来，改为"有设备即可"
			if len(saw) == 0 && len(devs) > 0 {
				return true, fmt.Sprintf("%s 已启动并上线：%s", emu.Name, devs[0].Serial), nil
			}
		}
		if time.Now().After(deadline) {
			// 超时必须给出**可继续排查的信息**。只说"没出现"等于把人丢在半路：
			// 到底是还在开机，还是进程已经死了？这两种情况的处置完全不同。
			return false, out, &EmulatorStartTimeout{Name: emu.Name, Running: emulatorStillRunning(ctx, emu.Name), Log: EmulatorLogPath(emu.Name)}
		}
		select {
		case <-ctx.Done():
			return false, out, ctx.Err()
		case <-time.After(pollEmulatorInterval):
		}
	}
}

// waitEmulatorAlive 在预算时间内等待某个实例的进程出现。
func waitEmulatorAlive(ctx context.Context, name string, budget time.Duration) bool {
	deadline := time.Now().Add(budget)
	for {
		if emulatorStillRunning(ctx, name) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(2 * time.Second):
		}
	}
}

// EmulatorLogPath 返回某个实例的日志文件路径（存在与否都返回，交由调用方判断）。
//
// 实测本机布局：deployed/<实例名>/Log/Emulator.log。DevEco 自己也是看这个文件
// 排错，所以它是"模拟器为什么起不来"的第一手证据。
func EmulatorLogPath(name string) string {
	return filepath.Join(EmulatorRoot(), name, "Log", "Emulator.log")
}

// emulatorStillRunning 判断某个实例的进程是否还活着。
//
// 出错时返回 false 而不是报错：它只用于生成提示文案，不该喧宾夺主把真正的
// 错误（启动超时）盖掉。
func emulatorStillRunning(ctx context.Context, name string) bool {
	running, err := RunningEmulators(ctx)
	if err != nil {
		return false
	}
	return slicesContains(running, name)
}

// EmulatorStartTimeout 是启动超时的错误。
//
// 之所以专门定义类型而不是拼一个 error 字符串：调用方（工具层）要用
// errors.As 把它取出来，在错误后面追加日志尾部。错误本身不该去读文件——
// 那会让域层的行为依赖磁盘 IO，单测也没法写。
type EmulatorStartTimeout struct {
	Name    string
	Running bool
	Log     string
}

func (e *EmulatorStartTimeout) Error() string {
	if e.Running {
		return fmt.Sprintf("%s 已发出启动命令、进程还在运行，但 %s 内没有出现在 hdc list targets 里（可能在开机自检，也可能卡在启动画面；可稍后用 harmony_devices 复查）",
			e.Name, timeoutEmulatorStart)
	}
	return fmt.Sprintf("%s 已发出启动命令，但进程随后退出了（%s 内既没上线、进程也没存活）——启动失败，不是启动慢",
		e.Name, timeoutEmulatorStart)
}

// LogPath 让工具层能拿到日志位置去补一段证据。
func (e *EmulatorStartTimeout) LogPath() string { return e.Log }

// Hint 给出"下一步该干什么"。
//
// 两种情况处置完全不同，必须分开说：进程还活着只是慢 → 等着；进程已死 →
// 查日志找原因。混成一句"启动失败"会误导人反复重试。
func (e *EmulatorStartTimeout) Hint() string {
	if e.Running {
		return "进程仍在运行，多半只是开机慢，不要反复重试——等一会儿用 harmony_devices 复查即可。"
	}
	return "进程已经退出，属于启动失败而非启动慢。常见原因：镜像缺失或与实例类型不匹配、" +
		"Hyper-V/虚拟化未开启、上一次异常退出留下的锁文件。先看日志尾部定位。"
}

// StopEmulator 停止模拟器。name 为空表示停止全部。
//
// 分两步：先按命令行匹配的 PID 发 taskkill，再复查进程是否真的消失。
// 实测 taskkill 会"成功返回但进程还活几秒"，只信返回值会误报已停止。
func (tc *Toolchain) StopEmulator(ctx context.Context, name string) (stopped []string, err error) {
	if runtime.GOOS != "windows" {
		return nil, errors.New("模拟器管理当前只支持 Windows")
	}
	target := strings.TrimSpace(name)
	if target == "" {
		// 停止全部：不再依赖 FindEmulator（那要求至少有一个实例）
		running, err := RunningEmulators(ctx)
		if err != nil {
			return nil, err
		}
		if len(running) == 0 {
			return nil, errors.New("当前没有正在运行的模拟器")
		}
		for _, n := range running {
			if err := tc.stopOne(ctx, n); err != nil {
				return stopped, err
			}
			stopped = append(stopped, n)
		}
		return stopped, nil
	}
	emu, err := FindEmulator(target)
	if err != nil {
		return nil, err
	}
	if err := tc.stopOne(ctx, emu.Name); err != nil {
		return nil, err
	}
	return []string{emu.Name}, nil
}

// stopOne 停止单个实例：先走官方 -stop，不生效才退回强杀。
//
// 优先用官方命令的理由：它让模拟器走正常关机流程（保存快照、释放镜像锁）。
// 直接 taskkill 会跳过这些，实测会在实例目录里留下锁文件，下次启动失败。
func (tc *Toolchain) stopOne(ctx context.Context, name string) error {
	if _, err := tc.emulatorCLI(ctx, timeoutEmulatorStop, "-stop", name); err == nil {
		if waitEmulatorGone(ctx, name, 15*time.Second) {
			return nil
		}
		// 官方命令"成功"但进程还在——继续往下走强杀，不要假装成功了
	}
	return killEmulator(ctx, name)
}

// waitEmulatorGone 等待某个实例的进程消失，返回是否等到。
func waitEmulatorGone(ctx context.Context, name string, budget time.Duration) bool {
	deadline := time.Now().Add(budget)
	for {
		running, rerr := RunningEmulators(ctx)
		if rerr != nil || !slicesContains(running, name) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(2 * time.Second):
		}
	}
}

// killEmulator 杀掉指定实例的 Emulator.exe 并复查。
//
// 只在官方 -stop 无效时使用——见 stopOne 的说明。
func killEmulator(ctx context.Context, name string) error {
	ps, err := exec.LookPath("powershell")
	if err != nil {
		return errors.New("找不到 powershell，无法停止模拟器")
	}
	// 按命令行里的 -hvd <name> 精确匹配，避免误杀别的实例
	script := fmt.Sprintf(
		"Get-CimInstance Win32_Process | Where-Object { $_.Name -eq 'Emulator.exe' -and $_.CommandLine -like '*-hvd \"%s\"*' } | ForEach-Object { Stop-Process -Id $_.ProcessId -Force }",
		name)
	if _, err := execx.Run(ctx, ps, []string{"-NoProfile", "-Command", script}, execx.Options{
		Timeout: timeoutEmulatorStop,
	}); err != nil {
		return fmt.Errorf("停止 %s 失败：%w", name, err)
	}

	// 复查：最多等 15s
	if waitEmulatorGone(ctx, name, 15*time.Second) {
		return nil
	}
	return fmt.Errorf("%s 已发出停止命令但进程仍在运行（可稍后用 harmony_emulator_list 复查）", name)
}

// ---------------------------------------------------------------- 镜像与设备目录

// SystemImage 是一个已安装的模拟器系统镜像。
type SystemImage struct {
	OSVersion   string // 官方口径，如 "HarmonyOS 6.1.1(24)"；创建实例时 -osVersion 就用它
	DeviceType  string // phone / tablet / 2in1 / foldable / widefold / triplefold
	SoftVersion string // 如 6.1.0.126
	ReleaseType string
	Dir         string // 仅目录扫描兜底时有值
	FromCLI     bool   // true = 官方 -imageList 的结果（权威）
}

// InstalledImages 列出本机已下载的系统镜像。
//
// 首选官方 `Emulator.exe -imageList -downloaded true`：它返回权威 JSON。
// 实测过两种来源的差异——目录扫描只能看到 system-image 下的子目录名
// （phone_all_x86 / tablet_x86…），既分不出 foldable 这类形态，也会漏项；
// 本机扫描得 3 项、官方得 6 项。所以目录扫描只作兜底。
func (tc *Toolchain) InstalledImages(ctx context.Context) ([]SystemImage, error) {
	if images, err := tc.imageListFromCLI(ctx); err == nil && len(images) > 0 {
		return images, nil
	}
	return installedImagesFromDir(), nil
}

func (tc *Toolchain) imageListFromCLI(ctx context.Context) ([]SystemImage, error) {
	res, err := tc.emulatorCLI(ctx, timeoutProcQuery, "-imageList", "-downloaded", "true")
	if err != nil {
		return nil, err
	}
	return parseImageListOutput(res.Output)
}

// parseImageListOutput 解析 -imageList 的输出。
//
// 输出不保证是纯 JSON：前面可能夹着进度行或提示语，所以从第一个 '[' 开始取。
func parseImageListOutput(raw string) ([]SystemImage, error) {
	if i := strings.Index(raw, "["); i >= 0 {
		raw = raw[i:]
	}
	var items []struct {
		SoftWareVersion string `json:"SoftWareVersion"`
		DeviceType      string `json:"deviceType"`
		Downloaded      string `json:"downloaded"`
		OSVersion       string `json:"osVersion"`
		ReleaseType     string `json:"releaseType"`
	}
	if err := json.Unmarshal([]byte(raw), &items); err != nil {
		return nil, fmt.Errorf("解析 -imageList 输出失败：%w", err)
	}
	var out []SystemImage
	for _, it := range items {
		out = append(out, SystemImage{
			OSVersion:   it.OSVersion,
			DeviceType:  it.DeviceType,
			SoftVersion: it.SoftWareVersion,
			ReleaseType: it.ReleaseType,
			FromCLI:     true,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].OSVersion == out[j].OSVersion {
			return out[i].DeviceType < out[j].DeviceType
		}
		return out[i].OSVersion < out[j].OSVersion
	})
	return out, nil
}

// installedImagesFromDir 扫描 system-image 目录（官方命令不可用时的兜底）。
func installedImagesFromDir() []SystemImage {
	root := EmulatorImageRoot()
	if root == "" {
		return nil
	}
	base := filepath.Join(root, emulatorImageDir)
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil // 没装镜像不是错误
	}
	var images []SystemImage
	for _, ver := range entries {
		if !ver.IsDir() {
			continue
		}
		verDir := filepath.Join(base, ver.Name())
		types, err := os.ReadDir(verDir)
		if err != nil {
			continue
		}
		for _, t := range types {
			if !t.IsDir() {
				continue
			}
			images = append(images, SystemImage{
				OSVersion:  ver.Name(),
				DeviceType: t.Name(),
				Dir:        filepath.Join(verDir, t.Name()),
			})
		}
	}
	sort.SliceStable(images, func(i, j int) bool {
		if images[i].OSVersion == images[j].OSVersion {
			return images[i].DeviceType < images[j].DeviceType
		}
		return images[i].OSVersion < images[j].OSVersion
	})
	return images
}

// DeviceCatalog 是可创建的官方设备型号条目（来自 productConfig.json）。
type DeviceCatalog struct {
	Name       string
	Width      int
	Height     int
	Density    int
	Diagonal   string
	DeviceType string // phone / tablet / 2in1 / wearable / foldable
}

// Catalog 读取本机 SDK 里的设备目录。
//
// productConfig.json 的结构是 {设备类型: [ {name, screenWidth, ...}, ... ] }，
// 类型本身不在条目里，要在遍历时补上——否则结果里只剩一堆名字，看不出是手机还是平板。
func Catalog() ([]DeviceCatalog, error) {
	root := EmulatorImageRoot()
	if root == "" {
		return nil, nil
	}
	data, err := os.ReadFile(filepath.Join(root, emulatorCatalogFile))
	if err != nil {
		return nil, nil
	}
	// 字段类型不能写死：实测 productConfig.json 里 screenWidth 是**字符串**
	// （"1320" 而不是 1320），写 int 会直接解析失败，整个目录读不出来。
	// 用 any 接住再逐个转换，比赌对方的类型更稳。
	var raw map[string][]map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("解析 %s 失败：%w", emulatorCatalogFile, err)
	}
	var out []DeviceCatalog
	for typ, items := range raw {
		for _, it := range items {
			out = append(out, DeviceCatalog{
				Name:       anyString(it["name"]),
				Width:      anyInt(it["screenWidth"]),
				Height:     anyInt(it["screenHeight"]),
				Density:    anyInt(it["screenDensity"]),
				Diagonal:   anyString(it["screenDiagonal"]),
				DeviceType: typ,
			})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].DeviceType == out[j].DeviceType {
			return out[i].Name < out[j].Name
		}
		return out[i].DeviceType < out[j].DeviceType
	})
	return out, nil
}

// ---------------------------------------------------------------- 创建 / 删除

// emulatorNamePattern 是实例名的允许格式：字母数字开头，可含空格与短横线。
//
// 限制它不只是洁癖：名字会进目录名、ini 键值和进程命令行参数，
// 特殊字符会在其中至少一处出问题。
var emulatorNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 \-]{0,30}$`)

// CreateEmulator 新建一个模拟器实例。
//
// 走官方 `Emulator.exe -create`。之前的实现是"复制 config.ini + 手写 lists.json"，
// 那条路有个致命问题：lists.json 与实例目录都是 DevEco 自己的状态，我们按猜测的
// 格式写回去，一旦字段对不上就会让 DevEco 认不出实例（甚至整个清单坏掉）。
// 官方命令不存在这个问题——它自己维护状态。
//
// deviceType 为空时取 phone；osVersion 为空时从已下载镜像里挑一个匹配
// deviceType 的；screenProfile 为空时直接用实例名（本机实例名本身就是型号名）。
func (tc *Toolchain) CreateEmulator(ctx context.Context, name, deviceType, osVersion, screenProfile string) (Emulator, error) {
	name = strings.TrimSpace(name)
	if !emulatorNamePattern.MatchString(name) {
		return Emulator{}, fmt.Errorf("模拟器名 %q 不合法：只能包含字母、数字、空格与短横线，且以字母或数字开头（长度 1-31）", name)
	}
	dt := strings.TrimSpace(deviceType)
	if dt == "" {
		dt = "phone"
	}
	if list, _ := ListEmulators(); len(list) > 0 {
		for _, e := range list {
			if strings.EqualFold(e.Name, name) {
				return Emulator{}, fmt.Errorf("已存在同名模拟器 %q", name)
			}
		}
	}

	// 版本号必须落在"已下载的镜像"上，否则 -create 会失败。
	// 与其让它失败后再回头猜，不如先查一次并给出可选项。
	ov := strings.TrimSpace(osVersion)
	if ov == "" {
		images, ierr := tc.InstalledImages(ctx)
		if ierr != nil || len(images) == 0 {
			return Emulator{}, errors.New("拿不到已下载镜像列表，必须显式指定 os_version（形如 \"HarmonyOS 6.1.1(24)\"）")
		}
		picked, ok := pickImage(images, dt)
		if !ok {
			return Emulator{}, fmt.Errorf("%s 形态没有已下载的镜像；先用 harmony_image_check 看有哪些，或在 DevEco 里下载该形态镜像", dt)
		}
		ov = picked.OSVersion
	}
	sp := strings.TrimSpace(screenProfile)
	if sp == "" {
		sp = name
	}

	args := []string{"-create", name, "-deviceType", dt, "-osVersion", ov, "-screenProfile", sp,
		"-instancePath", EmulatorRoot(), "-imageRoot", EmulatorImageRoot()}
	// 创建要展开镜像、生成 qcow2，给足时间
	res, err := tc.emulatorCLI(ctx, 180*time.Second, args...)
	if err != nil {
		return Emulator{}, fmt.Errorf("创建模拟器 %s 失败：%w%s", name, err, cliDetail(res))
	}
	// 命令成功不等于实例真的落盘：以"实例目录 + 指针 ini"为准做验收。
	// 注意别拿 lists.json 当判据——实测 -create 之后它要等首次启动才更新。
	created, ferr := FindEmulator(name)
	if ferr != nil {
		return Emulator{}, fmt.Errorf("-create 已返回成功，但找不到 %s 的实例目录（%s）；命令输出：%s",
			name, filepath.Join(EmulatorRoot(), name), res.Output)
	}
	return created, nil
}

// pickImage 从镜像列表里挑一个匹配形态的（优先精确匹配，其次包含匹配）。
func pickImage(images []SystemImage, deviceType string) (SystemImage, bool) {
	want := strings.ToLower(strings.TrimSpace(deviceType))
	for _, img := range images {
		if strings.EqualFold(img.DeviceType, want) {
			return img, true
		}
	}
	for _, img := range images {
		if strings.Contains(strings.ToLower(img.DeviceType), want) {
			return img, true
		}
	}
	return SystemImage{}, false
}

// DeleteEmulator 删除一个模拟器实例。
//
// 走官方 `Emulator.exe -delete -force`（-force 跳过交互确认，否则非交互下会卡住）。
// 删完以 lists.json 为准复查：官方命令有时只删了目录没更新清单。
func (tc *Toolchain) DeleteEmulator(ctx context.Context, name string) error {
	if EmulatorRoot() == "" {
		return errors.New("拿不到 %LOCALAPPDATA%，无法定位模拟器目录")
	}
	emu, err := FindEmulator(name)
	if err != nil {
		return err
	}
	if running, rerr := RunningEmulators(ctx); rerr == nil && slicesContains(running, emu.Name) {
		return fmt.Errorf("%s 正在运行，拒绝删除（先 harmony_emulator_stop）", emu.Name)
	}
	res, err := tc.emulatorCLI(ctx, 60*time.Second, "-delete", emu.Name, "-force",
		"-instancePath", EmulatorRoot())
	if err != nil {
		return fmt.Errorf("删除模拟器 %s 失败：%w%s", emu.Name, err, cliDetail(res))
	}
	if _, ferr := FindEmulator(emu.Name); ferr == nil {
		return fmt.Errorf("-delete 已返回成功，但 %s 的实例目录仍然存在（命令输出：%s）", emu.Name, res.Output)
	}
	return nil
}

// cliDetail 把命令输出附到错误后面。
//
// 官方 CLI 失败时 stdout 常常是空的、原因写在 stderr 里，
// 不带出来就只能看到一句"退出码 1"。
func cliDetail(res execx.Result) string {
	s := strings.TrimSpace(res.Output)
	if s == "" {
		return ""
	}
	if len(s) > 400 {
		s = "…" + s[len(s)-400:]
	}
	return "；命令输出：" + strings.ReplaceAll(s, "\n", " | ")
}

// newUUID 生成一个随机的 UUID 字符串。
//
// 不引入第三方 UUID 库：这里只需要"一个没见过的标识"，
// crypto/rand 直接拼就够，而且避免了为了一个字段加依赖。
func newUUID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		// 拿不到随机数时退回时间戳：uuid 只用于区分实例，
		// 不用于安全目的，降级不会影响正确性
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", buf[0:4], buf[4:6], buf[6:8], buf[8:10], buf[10:16])
}
