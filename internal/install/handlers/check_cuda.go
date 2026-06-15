package handlers

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strings"

	"tts-proxy/internal/install"
)

// CheckCUDARule 表示「检查 CUDA 工具链是否已安装并校验版本」的规则。
//
//	{
//	  "type": "check_cuda",
//	  "name": "检查 CUDA 版本",
//	  "min_version": "12.0",          // 必填，前闭区间
//	  "max_version": "13.0",          // 可选，前开区间
//	  "install_hint": { ... }         // 可选，未安装时的提示
//	}
//
// 检测顺序:
//  1. nvcc --version           （CUDA Toolkit 真实版本）
//  2. nvidia-smi               （驱动支持的最高 CUDA 版本，作为兜底参考）
//
// 若两者都失败则规则报错；任何一个给出版本即视作存在 CUDA。
type CheckCUDARule struct {
	MinVersion  string       `json:"min_version,omitempty"`
	MaxVersion  string       `json:"max_version,omitempty"`
	InstallHint *InstallHint `json:"install_hint,omitempty"`
}

// nvccRe 匹配形如 "release 12.1, V12.1.105"。
var nvccRe = regexp.MustCompile(`release\s+(\d+\.\d+(?:\.\d+)?)`)

// smiRe 匹配 nvidia-smi 输出里的 "CUDA Version: 12.4"。
var smiRe = regexp.MustCompile(`CUDA Version:\s*(\d+\.\d+(?:\.\d+)?)`)

// CheckCUDA 是 check_cuda 规则的处理器。
func CheckCUDA(rule install.Rule, _ *install.HandlerContext) (string, error) {
	var spec CheckCUDARule
	if err := json.Unmarshal(rule.Raw, &spec); err != nil {
		return "", fmt.Errorf("解析规则字段失败: %w", err)
	}

	var b strings.Builder

	// 1) nvcc —— Toolkit 真实版本
	toolkitVer, toolkitPath, toolkitErr := detectByNvcc()
	if toolkitErr == nil {
		fmt.Fprintf(&b, "√ nvcc        版本=%s 路径=%s\n", toolkitVer, toolkitPath)
	} else {
		fmt.Fprintf(&b, "× nvcc        %s\n", toolkitErr)
	}

	// 2) nvidia-smi —— 驱动支持的最高 CUDA 版本（仅作参考）
	driverVer, smiPath, smiErr := detectBySmi()
	if smiErr == nil {
		fmt.Fprintf(&b, "ℹ nvidia-smi  驱动支持 CUDA=%s 路径=%s\n", driverVer, smiPath)
	} else {
		fmt.Fprintf(&b, "× nvidia-smi  %s\n", smiErr)
	}

	// 选择用于版本比较的来源：优先 nvcc。
	var ver, source string
	switch {
	case toolkitErr == nil:
		ver, source = toolkitVer, "CUDA Toolkit (nvcc)"
	case smiErr == nil:
		ver, source = driverVer, "驱动 (nvidia-smi)"
	default:
		return strings.TrimRight(b.String(), "\n"),
			fmt.Errorf("未检测到 CUDA：nvcc 与 nvidia-smi 均不可用%s",
				renderHint(spec.InstallHint, "CUDA"))
	}

	if spec.MinVersion != "" {
		cmp, err := compareVersions(ver, spec.MinVersion)
		if err != nil {
			fmt.Fprintf(&b, "    最低版本检查跳过: %v\n", err)
		} else if cmp < 0 {
			return strings.TrimRight(b.String(), "\n"),
				fmt.Errorf("%s 版本 %s 低于最低要求 %s%s",
					source, ver, spec.MinVersion,
					renderHint(spec.InstallHint, "CUDA"))
		} else {
			fmt.Fprintf(&b, "    满足最低版本 >= %s（来源: %s）\n", spec.MinVersion, source)
		}
	}

	if spec.MaxVersion != "" {
		cmp, err := compareVersions(ver, spec.MaxVersion)
		if err != nil {
			fmt.Fprintf(&b, "    最高版本检查跳过: %v\n", err)
		} else if cmp >= 0 {
			return strings.TrimRight(b.String(), "\n"),
				fmt.Errorf("%s 版本 %s 不低于最高限制 %s%s",
					source, ver, spec.MaxVersion,
					renderHint(spec.InstallHint, "CUDA"))
		} else {
			fmt.Fprintf(&b, "    满足最高版本 < %s（来源: %s）\n", spec.MaxVersion, source)
		}
	}

	return strings.TrimRight(b.String(), "\n"), nil
}

func detectByNvcc() (version, path string, err error) {
	p, err := exec.LookPath("nvcc")
	if err != nil {
		return "", "", fmt.Errorf("未在 PATH 中找到 nvcc")
	}
	out, err := exec.Command("nvcc", "--version").CombinedOutput()
	if err != nil {
		return "", p, fmt.Errorf("调用 nvcc --version 失败: %v", err)
	}
	m := nvccRe.FindStringSubmatch(string(out))
	if m == nil {
		return "", p, fmt.Errorf("无法从 nvcc 输出解析版本: %q", strings.TrimSpace(string(out)))
	}
	return m[1], p, nil
}

func detectBySmi() (version, path string, err error) {
	p, err := exec.LookPath("nvidia-smi")
	if err != nil {
		return "", "", fmt.Errorf("未在 PATH 中找到 nvidia-smi")
	}
	out, err := exec.Command("nvidia-smi").CombinedOutput()
	if err != nil {
		return "", p, fmt.Errorf("调用 nvidia-smi 失败: %v", err)
	}
	m := smiRe.FindStringSubmatch(string(out))
	if m == nil {
		return "", p, fmt.Errorf("nvidia-smi 输出未包含 CUDA Version 行")
	}
	return m[1], p, nil
}
