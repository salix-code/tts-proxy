package handlers

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"tts-proxy/internal/install"
)

// CheckPythonPackageRule 表示「检查某个 Python 包是否安装并校验版本」的规则。
//
//	{
//	  "type": "check_python_package",
//	  "name": "检查PyTorch版本",
//	  "package": "torch",                       // 必填，import 用的包名
//	  "import_as": "torch",                     // 可选，import 时实际使用的名字（缺省 = package）
//	  "python_candidates": ["python", "python3", "py"],  // 可选，查找 python 解释器
//	  "min_version": "2.5.0",                   // 可选，最低版本（前闭区间）
//	  "max_version": "3.0.0",                   // 可选，最高版本（前开区间）
//	  "install_hint": {                         // 可选，未安装时的提示
//	    "intro": "...",                         //   首行说明
//	    "commands": [                           //   可执行命令列表
//	      {"label": "CPU", "cmd": "pip install torch"}
//	    ],
//	    "docs": "https://..."                   //   官网/文档链接
//	  }
//	}
type CheckPythonPackageRule struct {
	Package          string       `json:"package"`
	ImportAs         string       `json:"import_as,omitempty"`
	PythonCandidates []string     `json:"python_candidates,omitempty"`
	MinVersion       string       `json:"min_version,omitempty"`
	MaxVersion       string       `json:"max_version,omitempty"`
	InstallHint      *InstallHint `json:"install_hint,omitempty"`
}

// String 把提示渲染成可读文本，用于追加到错误信息后。
func renderHint(h *InstallHint, pkg string) string {
	fallback := ""
	if pkg != "" {
		fallback = "请先安装 " + pkg + "："
	}
	return h.String(fallback)
}

// CheckPythonPackage 是 check_python_package 规则的处理器。
func CheckPythonPackage(rule install.Rule) (string, error) {
	var spec CheckPythonPackageRule
	if err := json.Unmarshal(rule.Raw, &spec); err != nil {
		return "", fmt.Errorf("解析规则字段失败: %w", err)
	}
	if spec.Package == "" {
		return "", fmt.Errorf("package 不能为空")
	}
	if spec.ImportAs == "" {
		spec.ImportAs = spec.Package
	}
	if len(spec.PythonCandidates) == 0 {
		spec.PythonCandidates = []string{"python", "python3", "py"}
	}

	pyName, pyPath, err := findPython(spec.PythonCandidates)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "使用解释器 %s (%s)\n", pyName, pyPath)

	// 用一行 Python 拿到版本号；版本字段未知时也尝试常见兜底。
	script := fmt.Sprintf(
		`import importlib, sys
try:
    m = importlib.import_module(%q)
except Exception as e:
    sys.stderr.write("IMPORT_ERROR: " + repr(e))
    sys.exit(2)
v = getattr(m, "__version__", None) or getattr(m, "VERSION", None)
if v is None:
    sys.stderr.write("NO_VERSION_ATTR")
    sys.exit(3)
print(v)
`, spec.ImportAs)

	cmd := exec.Command(pyName, "-c", script)
	out, err := cmd.Output()
	if err != nil {
		stderr := ""
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = strings.TrimSpace(string(ee.Stderr))
		}
		switch {
		case strings.HasPrefix(stderr, "IMPORT_ERROR"):
			return strings.TrimRight(b.String(), "\n"),
				fmt.Errorf("未在 %s 中找到包 %q（%s）%s",
					pyName, spec.Package,
					strings.TrimPrefix(stderr, "IMPORT_ERROR: "),
					renderHint(spec.InstallHint, spec.Package))
		case strings.HasPrefix(stderr, "NO_VERSION_ATTR"):
			return strings.TrimRight(b.String(), "\n"),
				fmt.Errorf("包 %q 未暴露 __version__ 属性，无法判断版本", spec.Package)
		default:
			return strings.TrimRight(b.String(), "\n"),
				fmt.Errorf("调用 python 失败: %v；stderr=%s", err, stderr)
		}
	}

	ver := strings.TrimSpace(string(out))
	// PyTorch 版本里可能带 "+cu121" 这样的本地标记；只取数字段做比较，原串照常展示。
	clean := versionRe.FindString(ver)
	if clean == "" {
		return strings.TrimRight(b.String(), "\n"),
			fmt.Errorf("包 %q 报告的版本 %q 无法解析", spec.Package, ver)
	}
	fmt.Fprintf(&b, "√ %s 版本=%s\n", spec.Package, ver)

	if spec.MinVersion != "" {
		cmp, err := compareVersions(clean, spec.MinVersion)
		if err != nil {
			fmt.Fprintf(&b, "    最低版本检查跳过: %v\n", err)
		} else if cmp < 0 {
			return strings.TrimRight(b.String(), "\n"),
				fmt.Errorf("%s 版本 %s 低于最低要求 %s%s",
					spec.Package, ver, spec.MinVersion,
					renderHint(spec.InstallHint, spec.Package))
		} else {
			fmt.Fprintf(&b, "    满足最低版本 >= %s\n", spec.MinVersion)
		}
	}
	if spec.MaxVersion != "" {
		cmp, err := compareVersions(clean, spec.MaxVersion)
		if err != nil {
			fmt.Fprintf(&b, "    最高版本检查跳过: %v\n", err)
		} else if cmp >= 0 {
			return strings.TrimRight(b.String(), "\n"),
				fmt.Errorf("%s 版本 %s 不低于最高限制 %s", spec.Package, ver, spec.MaxVersion)
		} else {
			fmt.Fprintf(&b, "    满足最高版本 < %s\n", spec.MaxVersion)
		}
	}

	return strings.TrimRight(b.String(), "\n"), nil
}

// findPython 在 candidates 中找第一个可用的 python 解释器。
func findPython(candidates []string) (name, path string, err error) {
	for _, c := range candidates {
		if p, e := exec.LookPath(c); e == nil {
			return c, p, nil
		}
	}
	return "", "", fmt.Errorf("候选 python 解释器均未找到: %s", strings.Join(candidates, ", "))
}
