"""ZipVoice 零样本克隆推理脚本。

由 Go 侧 speek-fast 命令调用，参数从命令行传入。
所有路径都是绝对路径，调用方负责解析。

用法：
    python zipvoice_clone.py \
        --text "要合成的文本" \
        --prompt-audio /abs/path/to/template1.wav \
        --prompt-text "模板对应的转写" \
        --model-dir /abs/path/to/sherpa-onnx-zipvoice-distill-int8-zh-en-emilia \
        --vocoder /abs/path/to/vocos_24khz.onnx \
        --output /abs/path/to/out.wav \
        [--num-threads 2] [--num-steps 8] [--speed 1.0] [--debug]
"""

import argparse
import os
import sys

import numpy as np
import soundfile as sf
import sherpa_onnx


def parse_args() -> argparse.Namespace:
    p = argparse.ArgumentParser()
    p.add_argument("--text", required=True, help="要合成的目标文本")
    p.add_argument("--prompt-audio", required=True, help="参考音频 wav 绝对路径")
    p.add_argument("--prompt-text", required=True, help="参考音频对应的转写（必须严格对应）")
    p.add_argument("--model-dir", required=True,
                   help="sherpa-onnx-zipvoice-distill-zh-en-emilia 解压后的目录")
    p.add_argument("--vocoder", default="",
                   help="vocos_24khz.onnx 绝对路径；distill/non-distill 包内已自带，可省略")
    p.add_argument("--output", required=True, help="输出 wav 路径")
    p.add_argument("--num-threads", type=int, default=2)
    p.add_argument("--num-steps", type=int, default=8,
                   help="ZipVoice flow-matching 步数，4-32，越大越细越慢")
    p.add_argument("--guidance-scale", type=float, default=1.5,
                   help="CFG 强度，>1 让生成更贴近参考音；过大会失真，建议 1.0-2.5")
    p.add_argument("--speed", type=float, default=1.0)
    p.add_argument("--debug", action="store_true")
    return p.parse_args()


def must_exist(path: str, label: str) -> str:
    if not os.path.exists(path):
        sys.exit(f"[zipvoice_clone] {label} 不存在: {path}")
    return path


def find_first(dir_path: str, names: list[str], label: str) -> str:
    """在 dir_path 里依次找 names 中的第一个存在的文件，找不到报错。"""
    for n in names:
        p = os.path.join(dir_path, n)
        if os.path.exists(p):
            return p
    sys.exit(f"[zipvoice_clone] {label} 不存在（在 {dir_path} 找不到任一: {', '.join(names)}）")


def load_prompt_audio(path: str) -> tuple[np.ndarray, int]:
    samples, sr = sf.read(path, dtype="float32", always_2d=False)
    if samples.ndim > 1:
        # 多通道取均值；ZipVoice 要单声道
        samples = samples.mean(axis=1).astype(np.float32)
    return samples, int(sr)


# 拼音声母（按长度从长到短匹配，避免 sh 被 s 抢走）
_PINYIN_INITIALS = ("zh", "ch", "sh",
                    "b", "p", "m", "f", "d", "t", "n", "l",
                    "g", "k", "h", "j", "q", "x", "r",
                    "z", "c", "s", "y", "w")


def split_pinyin(syllable: str) -> tuple[str, str]:
    """把 'xie2' 拆成 ('x0', 'ie2')，'er2' 拆成 ('', 'er2')。

    sherpa-onnx ZipVoice 的 tokens.txt 是「声母+0」+「韵母+音调」结构，
    例如 j0/x0/sh0 与 ian1/ing4/uo3 等；零声母（er/an/ou 等）的声母段为空。
    """
    if not syllable:
        return "", ""
    if syllable[-1].isdigit():
        tone = syllable[-1]
        body = syllable[:-1]
    else:
        tone = "5"  # 轻声
        body = syllable
    initial = ""
    final = body
    for ini in _PINYIN_INITIALS:
        if body.startswith(ini):
            initial = ini
            final = body[len(ini):]
            break
    initial_token = initial + "0" if initial else ""
    final_token = (final + tone) if final else ""
    return initial_token, final_token


def ensure_lexicon(model_dir: str, debug: bool = False) -> str:
    """保证 model_dir 下有可直接被 sherpa-onnx 解析的 lexicon.txt。

    优先用 distill release 自带的 pinyin.raw 转换：
      <词>  <log概率>  <拼音1> <拼音2> ...
    转成 sherpa-onnx 期望的 MatchaTtsLexicon 两列格式：
      <词> <声母0> <韵母音调> <声母0> <韵母音调> ...

    会读取 tokens.txt：拆分后任一 phone 不在 tokens.txt 中（如 'er1' 等模型未训练的
    边缘读音），整条词条直接丢弃，避免运行时报 "Unknown token"。

    缓存：转换结果落到 <model_dir>/lexicon.txt，下次直接复用。
    """
    target = os.path.join(model_dir, "lexicon.txt")
    pinyin_raw = os.path.join(model_dir, "pinyin.raw")
    tokens_path = os.path.join(model_dir, "tokens.txt")

    if os.path.exists(target):
        return target
    if not os.path.exists(pinyin_raw):
        sys.exit(f"[zipvoice_clone] 既无 lexicon.txt 也无 pinyin.raw: {model_dir}")
    if not os.path.exists(tokens_path):
        sys.exit(f"[zipvoice_clone] 缺 tokens.txt: {tokens_path}")

    if debug:
        print(f"[zipvoice_clone] 首次运行：从 {pinyin_raw} 生成 {target}", flush=True)

    # 加载合法 phone 集合，用于过滤不被模型识别的边缘读音
    valid_phones: set[str] = set()
    with open(tokens_path, encoding="utf-8") as f:
        for line in f:
            parts = line.rstrip("\n").split("\t")
            if parts and parts[0]:
                valid_phones.add(parts[0])

    written = 0
    skipped = 0
    dropped_phones: dict[str, int] = {}
    tmp = target + ".part"
    with open(pinyin_raw, encoding="utf-8") as fin, open(tmp, "w", encoding="utf-8") as fout:
        for line in fin:
            parts = line.rstrip("\n").split()
            # 期望: word  weight  pinyin1 pinyin2 ...
            if len(parts) < 3:
                skipped += 1
                continue
            word = parts[0]
            # 第二列若像权重（带小数点或负号），就跳过它；否则按"无权重列"处理
            second = parts[1]
            try:
                float(second)
                pinyins = parts[2:]
            except ValueError:
                pinyins = parts[1:]
            phones: list[str] = []
            ok = True
            for syl in pinyins:
                ini, fin_ = split_pinyin(syl)
                if ini:
                    phones.append(ini)
                if fin_:
                    phones.append(fin_)
                else:
                    ok = False
                    break
            if not ok or not phones:
                skipped += 1
                continue
            # 过滤模型未收录的边缘 phone（如某些 er1 读音）
            unknown = [p for p in phones if p not in valid_phones]
            if unknown:
                for u in unknown:
                    dropped_phones[u] = dropped_phones.get(u, 0) + 1
                skipped += 1
                continue
            fout.write(word + " " + " ".join(phones) + "\n")
            written += 1
    os.replace(tmp, target)
    if debug:
        print(f"[zipvoice_clone] lexicon 写入 {written} 条，跳过 {skipped} 条 → {target}",
              flush=True)
        if dropped_phones:
            top = sorted(dropped_phones.items(), key=lambda x: -x[1])[:5]
            print(f"[zipvoice_clone] 因 phone 不在 tokens 中而丢弃: {top}", flush=True)
    return target


def main() -> int:
    args = parse_args()

    # 校验所有输入路径
    must_exist(args.prompt_audio, "prompt-audio")
    model_dir = must_exist(args.model_dir, "model-dir")

    # 不同 release 包的命名差异：
    #   distill / non-distill: text_encoder.onnx / fm_decoder.onnx / pinyin.raw
    #   distill-int8:          text_encoder_int8.onnx / fm_decoder_int8.onnx / pinyin.raw
    #   旧文档里有人写:        encoder.onnx / decoder.onnx / lexicon.txt
    # 这里按可能的命名依次查找。
    encoder = find_first(model_dir,
                         ["text_encoder.onnx", "encoder.onnx", "text_encoder_int8.onnx", "encoder.int8.onnx"],
                         "encoder")
    decoder = find_first(model_dir,
                         ["fm_decoder.onnx", "decoder.onnx", "fm_decoder_int8.onnx", "decoder.int8.onnx"],
                         "decoder")
    tokens = must_exist(os.path.join(model_dir, "tokens.txt"), "tokens.txt")
    # pinyin.raw 自带 log 概率列且把整音节当 token，sherpa-onnx 的 MatchaTtsLexicon
    # 不识别这两点，必须先转换成两列格式 + 把 'xie2' 拆成 'x0 ie2'。
    lexicon = ensure_lexicon(model_dir, debug=args.debug)
    espeak_data = must_exist(os.path.join(model_dir, "espeak-ng-data"), "espeak-ng-data")

    # vocoder 优先用包内自带（distill release 已捆绑），找不到才用外部传入的。
    bundled_vocoder = os.path.join(model_dir, "vocos_24khz.onnx")
    if os.path.exists(bundled_vocoder):
        vocoder_path = bundled_vocoder
    elif args.vocoder and os.path.exists(args.vocoder):
        vocoder_path = args.vocoder
    else:
        sys.exit(f"[zipvoice_clone] vocoder 不存在: 包内 {bundled_vocoder} 与外部 {args.vocoder} 均缺失")

    zv_cfg = sherpa_onnx.OfflineTtsZipvoiceModelConfig(
        encoder=encoder,
        decoder=decoder,
        vocoder=vocoder_path,
        tokens=tokens,
        lexicon=lexicon,
        data_dir=espeak_data,
        guidance_scale=args.guidance_scale,
    )
    model_cfg = sherpa_onnx.OfflineTtsModelConfig(
        zipvoice=zv_cfg,
        num_threads=args.num_threads,
        debug=args.debug,
        provider="cpu",
    )
    cfg = sherpa_onnx.OfflineTtsConfig(model=model_cfg)
    if not cfg.validate():
        sys.exit("[zipvoice_clone] 配置无效，请检查上面的路径")

    tts = sherpa_onnx.OfflineTts(cfg)
    prompt_samples, prompt_sr = load_prompt_audio(args.prompt_audio)
    if args.debug:
        print(f"[zipvoice_clone] prompt {len(prompt_samples)} samples @ {prompt_sr} Hz",
              flush=True)

    audio = tts.generate(
        text=args.text,
        prompt_text=args.prompt_text,
        prompt_samples=prompt_samples.tolist(),
        sample_rate=prompt_sr,
        speed=args.speed,
        num_steps=args.num_steps,
    )

    samples = np.asarray(audio.samples, dtype=np.float32)
    if samples.size == 0:
        sys.exit("[zipvoice_clone] 生成失败：返回空音频")

    out_dir = os.path.dirname(args.output)
    if out_dir:
        os.makedirs(out_dir, exist_ok=True)
    sf.write(args.output, samples, audio.sample_rate)
    print(f"[zipvoice_clone] √ 已写入 {args.output} ({samples.size} samples @ {audio.sample_rate} Hz)",
          flush=True)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
