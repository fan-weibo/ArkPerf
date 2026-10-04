package harmony

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fan-weibo/ArkPerf/internal/execx"
)

// 签名材料的相对位置（相对 <DevEcoRoot>/sdk/default/openharmony/toolchains/lib）。
//
// 这些都是 SDK 自带的调试身份，不需要开发者的正式证书——
// 对"把 demo 装到模拟器上跑一测"这个场景是标准做法。
const (
	signJarRel      = "sdk/default/openharmony/toolchains/lib/hap-sign-tool.jar"
	signP12Rel      = "sdk/default/openharmony/toolchains/lib/OpenHarmony.p12"
	signPemRel      = "sdk/default/openharmony/toolchains/lib/OpenHarmonyProfileDebug.pem"
	signTemplateRel = "sdk/default/openharmony/toolchains/lib/UnsgnedDebugProfileTemplate.json"
)

// 调试身份的固定口令与别名（SDK 自带材料的公开常量，不是秘密）。
const (
	signKeyAlias = "openharmony application profile debug"
	signKeyPwd   = "123456"
	signStorePwd = "123456"
)

// 签名两步的超时。签名本身是本地计算，慢在 JVM 启动。
const (
	timeoutSignProfile = 120 * time.Second
	timeoutSignApp     = 180 * time.Second
)

// SignMaterial 是签名所需的全部材料。
type SignMaterial struct {
	Java     string
	Jar      string
	P12      string
	Pem      string
	Template string
}

// SigningMaterial 定位签名材料与 java。
//
// java 优先用 DevEco 自带的 jbr：系统里装的 JDK 版本五花八门，
// 而 hap-sign-tool.jar 是跟着 DevEco 的 JDK 编译的，用它自己的运行时最不容易出
// "Unsupported class file major version" 这类错。
func SigningMaterial(tc *Toolchain) (SignMaterial, error) {
	if tc == nil || tc.DevEcoRoot == "" {
		return SignMaterial{}, errors.New("没能定位 DevEco 安装目录，无法签名（先跑 harmony_toolchain_check）")
	}
	m := SignMaterial{
		Jar:      filepath.Join(tc.DevEcoRoot, filepath.FromSlash(signJarRel)),
		P12:      filepath.Join(tc.DevEcoRoot, filepath.FromSlash(signP12Rel)),
		Pem:      filepath.Join(tc.DevEcoRoot, filepath.FromSlash(signPemRel)),
		Template: filepath.Join(tc.DevEcoRoot, filepath.FromSlash(signTemplateRel)),
	}
	var missing []string
	for name, p := range map[string]string{
		"hap-sign-tool.jar": m.Jar,
		"OpenHarmony.p12":   m.P12,
		"调试证书":              m.Pem,
		"profile 模板":        m.Template,
	} {
		if !existsNoErr(p) {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return SignMaterial{}, fmt.Errorf("SDK 签名材料缺失：%s（在 %s 下没找到；请在 DevEco 里确认 OpenHarmony SDK 已完整安装）",
			strings.Join(missing, "、"), tc.DevEcoRoot)
	}

	if p := filepath.Join(tc.DevEcoRoot, "jbr", "bin", javaExe()); existsNoErr(p) {
		m.Java = p
	} else if java := tc.Required(ToolJava); java.Found() {
		m.Java = java.Path
	} else {
		return SignMaterial{}, errors.New("找不到 java：DevEco 自带的 jbr 不存在，PATH 上也没有 java")
	}
	return m, nil
}

func javaExe() string {
	if os.PathSeparator == '\\' {
		return "java.exe"
	}
	return "java"
}

// SignHap 给一个未签名的 hap 签名，返回签名后的文件路径。
//
// 分两步是 hap-sign-tool 的硬性要求：先由模板签出 profile（.p7b），
// 再用这个 profile 签应用。想一步到位会报 profile 缺失。
func SignHap(ctx context.Context, tc *Toolchain, hap string) (signed string, out string, err error) {
	hap, err = filepath.Abs(hap)
	if err != nil {
		return "", "", err
	}
	if !existsNoErr(hap) {
		return "", "", fmt.Errorf("hap 不存在：%s", hap)
	}
	m, err := SigningMaterial(tc)
	if err != nil {
		return "", "", err
	}

	// 1. 生成 profile：SDK 自带模板的 validity 是写死的（实测到 2024 年就过期了），
	//    必须克隆一份改写有效期与 uuid，否则 sign-app 会以"profile 已过期"失败。
	tpl, err := freshProfileTemplate(m.Template)
	if err != nil {
		return "", "", err
	}
	p7b := filepath.Join(os.TempDir(), "arkperf-ohos-debug.p7b")
	res, err := execx.Run(ctx, m.Java, []string{
		"-jar", m.Jar, "sign-profile",
		"-mode", "localSign",
		"-keyAlias", signKeyAlias,
		"-keyPwd", signKeyPwd,
		"-keystoreFile", m.P12,
		"-keystorePwd", signStorePwd,
		"-profileCertFile", m.Pem,
		"-inFile", tpl,
		"-signAlg", "SHA256withECDSA",
		"-outFile", p7b,
	}, execx.Options{Timeout: timeoutSignProfile})
	profileOut := res.Output
	if err != nil {
		return "", profileOut, fmt.Errorf("生成签名 profile 失败：%w\n%s", err, clip(res.Output, 2000))
	}

	// 2. 签应用
	ext := filepath.Ext(hap)
	signed = strings.TrimSuffix(hap, ext) + "-signed" + ext
	res, err = execx.Run(ctx, m.Java, []string{
		"-jar", m.Jar, "sign-app",
		"-mode", "localSign",
		"-keyAlias", signKeyAlias,
		"-keyPwd", signKeyPwd,
		"-keystoreFile", m.P12,
		"-keystorePwd", signStorePwd,
		"-appCertFile", m.Pem,
		"-profileFile", p7b,
		"-inFile", hap,
		"-signAlg", "SHA256withECDSA",
		"-outFile", signed,
	}, execx.Options{Timeout: timeoutSignApp})
	out = profileOut + "\n" + res.Output
	if err != nil {
		return "", out, fmt.Errorf("签名应用失败：%w\n%s", err, clip(res.Output, 2000))
	}
	if !existsNoErr(signed) {
		return "", out, fmt.Errorf("签名命令返回成功但产物不存在：%s", signed)
	}
	return signed, out, nil
}

// freshProfileTemplate 克隆 SDK 的 profile 模板并改写有效期与 uuid。
//
// 只改这两项是有依据的：bundle-name 等字段在调试签名下不参与校验，
// 改了反而可能引入新的不一致。
func freshProfileTemplate(template string) (string, error) {
	data, err := os.ReadFile(template)
	if err != nil {
		return "", err
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return "", fmt.Errorf("解析签名模板失败：%w", err)
	}
	now := time.Now().Unix()
	// 往前挪一天：设备时间略慢于本机时也不会立刻判定"尚未生效"
	doc["validity"] = map[string]any{
		"not-before": now - 86400,
		"not-after":  now + 30*365*86400,
	}
	doc["uuid"] = newUUID()

	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", err
	}
	dst := filepath.Join(os.TempDir(), "arkperf-ohos-profile-template.json")
	if err := os.WriteFile(dst, out, 0o644); err != nil {
		return "", err
	}
	return dst, nil
}
