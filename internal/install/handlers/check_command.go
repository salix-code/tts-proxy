package handlers

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"tts-proxy/internal/install"
)

// CheckCommandRule 表示「检查某个命令是否存在并取版本」的规则。
//
//	{
//	  "type": "check_command",
//	  "name": "确定python版本",
//	  "candidates": ["python", "python3", "py"],
//	  "version_args": ["--version"],
//	  "recommended": ["3.10", "3.11"],   // 命中→提示「最匹配」
//	  "supported":   ["3.12"],           // 命中→提示「支持」
//	  "min_version": "3.8.0"             // 可选：未配置 recommended/supported 时使用
//	}
//
// recommended/supported 中的每一项是一个版本前缀，按段匹配：
//   "3"      匹配 3.x.x
//   "3.10"   匹配 3.10.x
//   "3.10.4" 仅匹配 3.10.4
type CheckCommandRule struct {
	Candidates  []string `json:"candidates"`
	VersionArgs []string `json:"version_args"`
	Recommended []string `json:"recommended,omitempty"`
	Supported   []string `json:"supported,omitempty"`
	MinVersion  string   `json:"min_version,omitempty"`
}

// versionRe 抓取形如 "Python 3.11.4" / "v1.2.3" / "go1.25.2" 的版本号。
var versionRe = regexp.MustCompile(`(\d+)\.(\d+)(?:\.(\d+))?`)

// CheckCommand 是 check_command 规则的处理器。
func CheckCommand(rule install.Rule) (string, error) {
	var spec CheckCommandRule
	if err := json.Unmarshal(rule.Raw, &spec); err != nil {
		return "", fmt.Errorf("解析规则字段失败: %w", err)
	}
	if len(spec.Candidates) == 0 {
		return "", fmt.Errorf("candidates 不能为空")
	}
	if len(spec.VersionArgs) == 0 {
		spec.VersionArgs = []string{"--version"}
	}

	var b strings.Builder
	for _, name := range spec.Candidates {
		path, err := exec.LookPath(name)
		if err != nil {
			fmt.Fprintf(&b, "× %-8s 未在 PATH 中找到\n", name)
			continue
		}

		out, err := exec.Command(name, spec.VersionArgs...).CombinedOutput()
		if err != nil {
			fmt.Fprintf(&b, "× %-8s 找到 (%s) 但执行 %v 失败: %v\n",
				name, path, spec.VersionArgs, err)
			continue
		}

		raw := strings.TrimSpace(string(out))
		ver := versionRe.FindString(raw)
		if ver == "" {
			fmt.Fprintf(&b, "? %-8s 路径=%s 输出=%q（未识别版本号）\n", name, path, raw)
			continue
		}

		fmt.Fprintf(&b, "√ %-8s 版本=%s 路径=%s\n", name, ver, path)

		// 优先用 recommended/supported 模型；都没配置时回退到 min_version。
		if len(spec.Recommended) > 0 || len(spec.Supported) > 0 {
			switch {
			case matchesAny(ver, spec.Recommended):
				fmt.Fprintf(&b, "    最匹配版本（推荐 %s）\n", strings.Join(spec.Recommended, ", "))
			case matchesAny(ver, spec.Supported):
				fmt.Fprintf(&b, "    版本受支持（支持 %s）\n", strings.Join(spec.Supported, ", "))
			default:
				return strings.TrimRight(b.String(), "\n"),
					fmt.Errorf("%s 版本 %s 不在受支持范围；推荐 %s，兼容 %s",
						name, ver,
						strings.Join(spec.Recommended, ", "),
						strings.Join(spec.Supported, ", "))
			}
		} else if spec.MinVersion != "" {
			cmp, err := compareVersions(ver, spec.MinVersion)
			if err != nil {
				fmt.Fprintf(&b, "    最低版本检查跳过: %v\n", err)
			} else if cmp < 0 {
				return strings.TrimRight(b.String(), "\n"),
					fmt.Errorf("%s 版本 %s 低于最低要求 %s", name, ver, spec.MinVersion)
			} else {
				fmt.Fprintf(&b, "    满足最低版本 >= %s\n", spec.MinVersion)
			}
		}

		return strings.TrimRight(b.String(), "\n"), nil
	}

	return strings.TrimRight(b.String(), "\n"),
		fmt.Errorf("候选命令均未找到: %s（请确认已安装并加入 PATH）",
			strings.Join(spec.Candidates, ", "))
}

// matchesAny 检查 version 是否匹配 prefixes 中的任意一个版本前缀。
func matchesAny(version string, prefixes []string) bool {
	for _, p := range prefixes {
		if matchPrefix(version, p) {
			return true
		}
	}
	return false
}

// matchPrefix 判断 version 是否以 prefix 描述的版本段开头。
//   matchPrefix("3.10.4", "3.10") -> true
//   matchPrefix("3.12.0", "3.10") -> false
//   matchPrefix("3",       "3")   -> true
func matchPrefix(version, prefix string) bool {
	v, err := parseVersion(version)
	if err != nil {
		return false
	}
	p, err := parseVersionParts(prefix)
	if err != nil {
		return false
	}
	for i, want := range p {
		if v[i] != want {
			return false
		}
	}
	return true
}

// compareVersions 比较 a 与 b，返回 -1/0/1。缺失的段视为 0。
func compareVersions(a, b string) (int, error) {
	pa, err := parseVersion(a)
	if err != nil {
		return 0, err
	}
	pb, err := parseVersion(b)
	if err != nil {
		return 0, err
	}
	for i := 0; i < 3; i++ {
		switch {
		case pa[i] < pb[i]:
			return -1, nil
		case pa[i] > pb[i]:
			return 1, nil
		}
	}
	return 0, nil
}

// parseVersion 用正则提取一个完整三段版本（缺失补零）。
func parseVersion(v string) ([3]int, error) {
	m := versionRe.FindStringSubmatch(v)
	if m == nil {
		return [3]int{}, fmt.Errorf("无法解析版本字符串 %q", v)
	}
	var out [3]int
	for i := 0; i < 3; i++ {
		if m[i+1] == "" {
			continue
		}
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return [3]int{}, err
		}
		out[i] = n
	}
	return out, nil
}

// parseVersionParts 把 "3.10" 这样的前缀解析为 [3, 10]，最多三段。
func parseVersionParts(prefix string) ([]int, error) {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return nil, fmt.Errorf("空的版本前缀")
	}
	segs := strings.Split(prefix, ".")
	if len(segs) > 3 {
		return nil, fmt.Errorf("版本前缀段数过多: %q", prefix)
	}
	out := make([]int, len(segs))
	for i, s := range segs {
		n, err := strconv.Atoi(s)
		if err != nil {
			return nil, fmt.Errorf("版本前缀段 %q 不是整数", s)
		}
		out[i] = n
	}
	return out, nil
}
