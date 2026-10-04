package harmony

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// APISymbol 是 SDK 声明里的一个符号。
type APISymbol struct {
	// Name 是符号名；方法会记成 "Class.method"。
	Name    string
	Kind    string // class / interface / enum / function / method / const / namespace
	File    string // 相对 api 目录的路径
	Line    int
	Snippet string
}

// APIIndex 是 SDK 声明索引。
type APIIndex struct {
	Dir     string
	BuiltAt time.Time
	// SDKMtime 是索引时 api 目录里最新的修改时间，用于判断缓存是否过期。
	SDKMtime time.Time
	Symbols  []APISymbol
}

// 索引的规模上限。SDK 的 d.ts 有上万文件，全量解析一次要好几秒；
// 这些上限不是"限制功能"，而是防止在某台机器上意外扫进一个巨大的目录
// 把工具卡死——超出就停，并如实报告扫了多少。
const (
	apiMaxFiles   = 20000
	apiMaxSymbols = 200000
)

// APIDir 返回 SDK 的 ArkTS 声明目录。
func APIDir(devEcoRoot string) string {
	if devEcoRoot == "" {
		return ""
	}
	return filepath.Join(devEcoRoot, "sdk", "default", "openharmony", "ets", "api")
}

// IndexDir 是索引缓存的存放位置（~/.arkperf）。
func IndexDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), ".arkperf")
	}
	return filepath.Join(home, ".arkperf")
}

func apiCachePath() string { return filepath.Join(IndexDir(), "apikg.json") }

// APIIndexLoad 读取缓存，缓存不存在或已过期时重建。
//
// 缓存判据用 api 目录的最新 mtime：SDK 升级会改文件时间，
// 而"符号总数 > 0"防止写出一个空索引后被当成有效缓存一直用下去。
func APIIndexLoad(devEcoRoot string) (*APIIndex, error) {
	dir := APIDir(devEcoRoot)
	if dir == "" || !isDirNoErr(dir) {
		return nil, &apiDirMissingError{dir: dir}
	}
	current, err := newestMtime(dir)
	if err != nil {
		return nil, err
	}
	if cached, cerr := readAPICache(); cerr == nil && cached != nil {
		if cached.Dir == dir && !current.After(cached.SDKMtime) && len(cached.Symbols) > 0 {
			return cached, nil
		}
	}
	idx, err := buildAPIIndex(dir)
	if err != nil {
		return nil, err
	}
	idx.SDKMtime = current
	// 缓存写失败不影响本次使用：索引已经在内存里了
	_ = writeAPICache(idx)
	return idx, nil
}

func readAPICache() (*APIIndex, error) {
	data, err := os.ReadFile(apiCachePath())
	if err != nil {
		return nil, err
	}
	var idx APIIndex
	if err := json.Unmarshal(data, &idx); err != nil {
		return nil, err
	}
	return &idx, nil
}

func writeAPICache(idx *APIIndex) error {
	if err := os.MkdirAll(IndexDir(), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(idx)
	if err != nil {
		return err
	}
	return os.WriteFile(apiCachePath(), data, 0o644)
}

// newestMtime 取目录树里最新的修改时间。
func newestMtime(dir string) (time.Time, error) {
	var newest time.Time
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // 单个文件读不了不影响整体判断
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
		return nil
	})
	if err != nil {
		return time.Time{}, err
	}
	if newest.IsZero() {
		newest = time.Now()
	}
	return newest, nil
}

// 声明行：export / declare / default 都是可选前缀，写法很规整。
//
// 不用真 TypeScript 解析器：d.ts 是机器生成的声明文件，形态稳定；
// 用正则扫一遍既快（几万文件只要几秒）又不需要引入新依赖。
var declPattern = regexp.MustCompile(
	`^\s*(?:export\s+)?(?:declare\s+)?(?:default\s+)?(class|interface|enum|namespace|function|const|type)\s+([A-Za-z_]\w*)`)

// 成员行：缩进 + 可选 static/readonly + 名字 + 左括号或泛型尖括号。
var memberPattern = regexp.MustCompile(`^\s+(?:static\s+|readonly\s+|async\s+)?([A-Za-z_]\w*)\s*[(:<]`)

// 顶层闭合：缩进为 0 的右花括号表示离开了当前容器。
var closeTopPattern = regexp.MustCompile(`^\}`)

// parseDeclarations 从一个声明文件里抽取符号。
//
// 容器（class/interface/enum/namespace）之后的成员会记成 "Container.member"，
// 这样查 "UIAbility.onCreate" 能直接命中，而不是只能查到 UIAbility。
func parseDeclarations(path, rel string) []APISymbol {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []APISymbol
	container := ""
	for i, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimRight(raw, "\r")
		if closeTopPattern.MatchString(line) {
			container = ""
			continue
		}
		if m := declPattern.FindStringSubmatch(line); m != nil {
			kind, name := m[1], m[2]
			out = append(out, APISymbol{
				Name:    name,
				Kind:    kind,
				File:    rel,
				Line:    i + 1,
				Snippet: clip(strings.TrimSpace(line), 160),
			})
			switch kind {
			case "class", "interface", "enum", "namespace":
				container = name
			default:
				container = ""
			}
			continue
		}
		if container != "" {
			if m := memberPattern.FindStringSubmatch(line); m != nil {
				out = append(out, APISymbol{
					Name:    container + "." + m[1],
					Kind:    "method",
					File:    rel,
					Line:    i + 1,
					Snippet: clip(strings.TrimSpace(line), 160),
				})
			}
		}
	}
	return out
}

// buildAPIIndex 扫描目录建立索引。
func buildAPIIndex(dir string) (*APIIndex, error) {
	idx := &APIIndex{Dir: dir, BuiltAt: time.Now()}
	files := 0

	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(d.Name()))
		if ext != ".ts" && ext != ".ets" {
			return nil
		}
		// 只收声明文件：实现文件（.ts）里没有 API 契约
		base := strings.ToLower(d.Name())
		if !strings.HasSuffix(base, ".d.ts") && !strings.HasSuffix(base, ".d.ets") {
			return nil
		}
		files++
		if files > apiMaxFiles || len(idx.Symbols) >= apiMaxSymbols {
			return filepath.SkipAll
		}
		rel, rerr := filepath.Rel(dir, path)
		if rerr != nil {
			rel = path
		}
		idx.Symbols = append(idx.Symbols, parseDeclarations(path, filepath.ToSlash(rel))...)
		return nil
	})
	if err != nil && len(idx.Symbols) == 0 {
		return nil, err
	}
	sort.SliceStable(idx.Symbols, func(i, j int) bool {
		if idx.Symbols[i].Name == idx.Symbols[j].Name {
			return idx.Symbols[i].File < idx.Symbols[j].File
		}
		return idx.Symbols[i].Name < idx.Symbols[j].Name
	})
	return idx, nil
}

// LookupSymbol 在索引里查一个符号。
type SymbolHit struct {
	Symbol APISymbol
	Exact  bool
}

// LookupSymbol 查询符号，先精确后模糊。
//
// 支持 "hilog.info" 这类层级写法：先整体查，查不到再取最后一段查。
// 这不是猜测——ArkTS 的 API 文档就是这么写的，模型也习惯这么问。
func (idx *APIIndex) LookupSymbol(symbol string, limit int) ([]SymbolHit, int) {
	symbol = strings.TrimSpace(symbol)
	if symbol == "" || idx == nil {
		return nil, len(idx.Symbols)
	}
	if limit <= 0 {
		limit = 10
	}
	var hits []SymbolHit
	seen := map[string]bool{}

	appendHit := func(s APISymbol, exact bool) {
		key := s.Name + "|" + s.File + "|" + strconv.Itoa(s.Line)
		if seen[key] || len(hits) >= limit*2 {
			return
		}
		seen[key] = true
		hits = append(hits, SymbolHit{Symbol: s, Exact: exact})
	}

	// 1. 完全相等（区分大小写）
	for _, s := range idx.Symbols {
		if s.Name == symbol {
			appendHit(s, true)
		}
	}
	// 2. 完全相等（不区分大小写）
	for _, s := range idx.Symbols {
		if strings.EqualFold(s.Name, symbol) {
			appendHit(s, true)
		}
	}
	// 3. 末段（hilog.info → info）
	if i := strings.LastIndex(symbol, "."); i >= 0 && i < len(symbol)-1 {
		tail := symbol[i+1:]
		for _, s := range idx.Symbols {
			if s.Name == tail || strings.EqualFold(s.Name, tail) {
				appendHit(s, true)
			}
		}
	}
	// 4. 模糊包含
	if len(hits) == 0 {
		for _, s := range idx.Symbols {
			if strings.Contains(strings.ToLower(s.Name), strings.ToLower(symbol)) {
				appendHit(s, false)
			}
		}
	}
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, len(idx.Symbols)
}

type apiDirMissingError struct{ dir string }

func (e *apiDirMissingError) Error() string {
	if e.dir == "" {
		return "没能定位 DevEco 安装目录，因此找不到 SDK 的 ArkTS 声明（先跑 harmony_toolchain_check）"
	}
	return "SDK 声明目录不存在：" + e.dir + "（先在 DevEco 里安装 OpenHarmony SDK）"
}
