# tts-proxy

Go 写的交互式 TTS 工具，基于 [VoxCPM2](https://github.com/OpenBMB/VoxCPM) 在本地生成语音。配套一个声明式安装器 `install.json`：把环境/依赖/模型/首次跑通都拆成可执行的规则。

## 快速开始

```bash
go build -o tts-proxy.exe .
./tts-proxy.exe
> install        # 首次部署：按 install.json 全自动装环境与模型
> exit
```

REPL 命令：`help` / `install [path]` / `echo` / `upper` / `exit`。

## 目录

```
.
├── main.go                       REPL 入口
├── install.json                  安装规则（声明式）
├── internal/
│   ├── commands/                 REPL 命令注册
│   └── install/
│       ├── install.go            Runner / Rule / Confirm 抽象
│       └── handlers/             一种 type 一个 handler
└── models/                       下载下来的模型（VoxCPM2 / ZipEnhancer）
```

## 安装器设计

### 自动化策略（重要）

**默认全自动，不打扰用户**。仅以下三种情况会询问：

1. **升级或替换** — 已装的工具版本过旧、需要覆盖式安装。例：`ensure_uv` 检测到 uv 缺失或版本 < `min_version` 时询问 `[Y/N]`。
2. **不同源** — 同一个模型/包在多个源都能拉取，用户环境差异决定哪个更稳。例：`download_models` 中的 `sources` 数组（VoxCPM2 在 HuggingFace 与 ModelScope 都有）。
3. **单步失败兜底** — 任意 handler 报错后，Runner 询问"是否已手动处理完？继续下一步"`[Y/N]`，给用户一次手动修复后继续的机会（`fatal: true` 的步骤除外，直接中止）。

> 不再有"是否预下载模型 / 是否生成测试音频"这种询问 —— 这些在原方案里属于"流程性确认"，与自动化原则冲突，已删除。

### Runner 流程

`internal/install/install.go`:

- `Load(path)` 读 JSON。
- `Runner.Register(type, handler)` 注册 type → handler 映射。
- `Runner.Run(cfg, out, confirm)` 顺序执行；遇错时根据 `Fatal` 决定中止 / 询问继续。
- `OS` / `SkipOS` 字段按 `runtime.GOOS` 跳过（别名：`mac/macos/osx → darwin`、`win → windows`）。

每个 handler 拿到 `Rule.Raw` 自行 `json.Unmarshal` 出 type-specific 字段，独立演化。

## 规则类型

> 公共字段：`type`（必填）、`name`、`os`、`skip_os`、`fatal`、`install_hint`。

### `ensure_uv` — 装/升级 uv
| 字段 | 说明 |
|---|---|
| `min_version` | 最低版本，低于则视作缺失 |
| `auto_install` | 默认 true，缺失/过旧时询问后自动装 |
| `install_command` | 可选：覆盖默认安装命令（按 OS 索引） |

行为：检测 → (满足直接放行) → 询问 → 执行安装脚本 → 重新探测（兼容 PATH 未刷新的情况，会扫 `~/.local/bin/uv` / `~/.cargo/bin/uv`）。

### `check_cuda` — 校验 CUDA
| 字段 | 说明 |
|---|---|
| `min_version` / `max_version` | 版本区间（前闭后开） |

依次尝试 `nvcc --version`、`nvidia-smi`；任一给出版本即通过。Mac 用 `skip_os: ["mac"]` 跳过。`fatal: true` —— 没显卡直接停。

### `create_uv_venv` — 创建 venv
| 字段 | 说明 |
|---|---|
| `python` | 传给 `uv venv --python` |
| `dir` | venv 目录名（相对 exe），默认 `.venv` |
| `force` | true 时覆盖已存在的目录 |

存在合法 venv（带 `pyvenv.cfg`）时跳过；存在但不是 venv 报错（除非 force）。

### `install_uv_package` — venv 内装包
| 字段 | 说明 |
|---|---|
| `package` | pip 安装名 |
| `import_as` | 字符串或字符串数组；按顺序逐个 `import` 验证；最后一项的 `__version__` 用于版本判断 |
| `min_version` / `max_version` | 版本区间 |
| `install_args` | 附加给 `uv pip install` 的额外参数（如 `--index-url`） |
| `spec` | pip spec，缺省 = `<package>>=<min_version>` |
| `venv_dir` | 默认 `.venv` |

先 import 校验，全部通过且版本满足则跳过；否则 `uv pip install --python <venv-python> <install_args> <spec>` 后再校验一次。

### `download_models` — 下模型
| 字段 | 说明 |
|---|---|
| `venv_dir` | 默认 `.venv` |
| `env` | 注入子进程的环境变量 |
| `models[]` | 模型列表，见下表 |

每个 model：

| 字段 | 说明 |
|---|---|
| `source` + `repo` | **单源用法**：直接下载 |
| `sources[]` | **多源用法**：每项 `{source, repo, label}`；非空时优先 |
| `source_prompt` | 多源时的询问语，缺省自动生成 |
| `local_dir` | 落到 `<exe 目录>/<local_dir>`；不填用默认缓存 |
| `skip_if_exists` | true 时 `local_dir` 非空目录则跳过 |
| `note` | 显示用说明 |

`source` 可选值：

| 取值 | 走的 CLI | 命令 |
|---|---|---|
| `hf` / `huggingface` | venv 中 `hf` 或 `huggingface-cli` | `hf download <repo> --local-dir <dir>` |
| `modelscope` / `ms` | venv 中 `modelscope` | `modelscope download --model <repo> --local_dir <dir>` |

**多源选择交互（当前实现）**：借用 `Confirm` 的 Y/N 二元提问 —— Y = `sources[0]`，N = `sources[1]`。≥3 个候选时只取前两个并打印提示（足够当下场景；之后真要多于 2 个再扩 `Choose` 回调）。

**部分失败容忍**：每个模型独立 try，某个失败不影响其他；至少一个成功 → 整步成功；全失败 → 整步失败并打印 `install_hint`。

### `run_venv_cli` — 跑 venv 里的 CLI
| 字段 | 说明 |
|---|---|
| `cli` | 必填，Windows 自动加 `.exe` |
| `args` | 命令行参数 |
| `env` | 子进程环境变量 |
| `skip_if_missing` | 默认 true，找不到 cli 直接跳过 |
| `output_file` | 执行后报告该文件是否生成、大小 |

> 之前的 `confirm_prompt` 已删除（流程性确认与自动化原则冲突）。

## VoxCPM2 多源说明

| 模型 | HuggingFace | ModelScope | 推荐源 |
|---|---|---|---|
| **VoxCPM2 主模型** | `openbmb/VoxCPM2` | `OpenBMB/VoxCPM2` | 香港/海外 → HF；国内 → ModelScope |
| **ZipEnhancer 降噪** | — | `iic/speech_zipenhancer_ans_multiloss_16k_base` | 仅 ModelScope（HF 镜像也无法替代） |

在 `download_models` 步骤会询问 VoxCPM2 走哪个源；ZipEnhancer 始终走 ModelScope，无询问。

ModelScope 在香港没有 CDN 节点，下载几 GB 的 ZipEnhancer 可能慢/不稳；如果用不到降噪，可以在调用 `voxcpm` 时加 `--no-denoiser`，安装阶段也不必下 ZipEnhancer。

## 给 AI 看的扩展说明

### 加新规则类型的步骤

1. 在 `internal/install/handlers/` 新建 `<type>.go`，定义 `XxxRule struct` 和 `func Xxx(rule install.Rule, ctx *install.HandlerContext) (string, error)`。
2. 在 `internal/commands/builtins.go` 的 `install` 命令里 `runner.Register("<type>", handlers.Xxx)`。
3. 在 `install.json` 加规则，type 字段就是注册名。

### `HandlerContext` 提供的能力

- `Confirm(prompt) (bool, error)` — Y/N 询问；空行/EOF 视为 N；REPL 共享 stdin。
- `Out` — 实时输出 io.Writer；handler 应把累积日志一次性 return（Runner 会缩进打印），实时进度可往 `ctx.Out` 写。

### 设计准则（来自代码注释）

- 新增字段时优先扩 handler 自己的 `XxxRule`，避免污染顶层 `Rule`。
- 失败应携带 `install_hint`（intro + commands + docs），让用户能手动接管。
- 涉及网络/源选择的步骤要支持「跳过整步」与「部分失败容忍」，不要让一次抖动毁掉整个安装。
- 询问要克制 —— 见上面的"自动化策略"。

## 常见问题

**uv 装完仍然找不到？** Windows 安装脚本写到注册表的 PATH，对当前 Go 进程不生效；`ensure_uv` 会扫 `~/.local/bin/uv.exe` 与 `~/.cargo/bin/uv.exe` 兜底，并把目录前置到当前进程 PATH，无需重开终端。

**模型下到一半中断？** 重跑 `install` 即可。`skip_if_exists: true` 让已下载完成的目录被跳过；中途中断的目录不是空的会被误跳过 —— 此时手动删 `models/VoxCPM2` 再跑一次。
