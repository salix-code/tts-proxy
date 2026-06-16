import os
from voxcpm import VoxCPM
import soundfile as sf

# 用绝对路径，避免依赖运行时的 cwd（双击运行 / IDE 启动 / 改目录跑都不会出错）
HERE = os.path.dirname(os.path.abspath(__file__))
VOXCPM_DIR = os.path.join(HERE, "models", "VoxCPM2")
ZIPENHANCER_DIR = os.path.join(HERE, "models", "ZipEnhancer")

# ---- 加载 VoxCPM2（完全离线）------------------------------------------------
# from_pretrained 第一个参数若是已存在的本地目录，就直接用，根本不联网。
# local_files_only=True 是双保险。
model = VoxCPM.from_pretrained(
    VOXCPM_DIR,
    load_denoiser=True,                        # VoxCPM2 配合 denoise=True 给参考/prompt 音频降噪
    zipenhancer_model_id=ZIPENHANCER_DIR,
    local_files_only=True,
)


# ---- 描述说话人的三种方式（VoxCPM2 才有的能力）---------------------------------

# 方式 A：文字描述音色（最方便，对应 CLI 的 --control）
#   原理很简单——把控制语包成 "(...)正文" 一起送进模型。
#   语种不限，英文中文都行；常用关键词：性别 / 年龄 / 情绪 / 节奏 / 口音 等。
def design_by_text(target_text: str, control: str) -> "np.ndarray":
    """用一句文字描述声音特征来合成。"""
    final_text = f"({control}){target_text}" if control.strip() else target_text
    return model.generate(
        text=final_text,
        cfg_value=2.0,
        inference_timesteps=10,
    )


# 方式 B：参考音频克隆（给 5–15 秒干净人声，模型学音色）
#   - reference_wav_path 是 VoxCPM2 独有的"隔离参考"模式，最干净。
#   - denoise=True 会先用 ZipEnhancer 给参考音频降噪，再做克隆。
def clone_by_reference(target_text: str, reference_wav: str) -> "np.ndarray":
    return model.generate(
        text=target_text,
        reference_wav_path=reference_wav,
        denoise=True,
        cfg_value=2.0,
        inference_timesteps=10,
    )


# 方式 C：continuation 继续模式（旧式 prompt，必须同时给音频和它的逐字转写）
def clone_by_prompt(target_text: str, prompt_wav: str, prompt_text: str) -> "np.ndarray":
    return model.generate(
        text=target_text,
        prompt_wav_path=prompt_wav,
        prompt_text=prompt_text,
        denoise=True,
        cfg_value=2.0,
        inference_timesteps=10,
    )


# ---- 当前演示：用文字描述音色合成 -------------------------------------------
# 想换音色就改 control 字符串，无需任何参考音频。
control = "完人、舞者，英雄：雌雄同体的始祖神、完成旅程的“宇宙舞者，一个循环的圆满结束，英雄与世界共舞，在天堂中实现了圆满。对立面完全融合，自我与宇宙合一，达到了终极的自由与完整。"
wav = design_by_text("你好，我使用VoxCPM", control)

sf.write("demo.wav", wav, model.tts_model.sample_rate)
print(f"saved: demo.wav  (control = {control!r})")
