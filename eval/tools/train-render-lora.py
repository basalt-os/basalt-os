#!/usr/bin/env python3
"""Fine-tune the humanize model: LoRA on a Qwen 3 base with fact -> prose pairs.

    train-render-lora.py --base DIR --data render-train.jsonl --out DIR [--4bit] [--epochs 2]

Runs on one GPU (fp32 compute: Pascal cards such as the GTX 1070 run fp16
arithmetic at a small fraction of fp32 speed and have no bf16) or on the
CPU. The prompt is the humanize layer's compact prompt (internal/explain,
HumanizeCompactPrompt), the user turn is the finding's facts as the
assistant sends them (explain.UserMessage), the target is the prose; only
the target tokens carry loss. The adapter is merged into the base and the
merged model is saved in fp16 (never bf16), ready for convert_hf_to_gguf.py.

Training data: `basalt-eval render-data -split train` only (facts from
fixed pools, prose from the assistant's own templates with random
equivalent phrasings; no third-party text). Recorded in OUT/training.json.
"""
import argparse
import json
import math
import os
import random
import time

import torch
from peft import LoraConfig, get_peft_model
from transformers import AutoModelForCausalLM, AutoTokenizer

# Must equal explain.HumanizeCompactPrompt in the Go code.
COMPACT_PROMPT = "Explain this Basalt OS finding in plain, friendly English from the facts only. No commands."


def encode(tok, user, prose, max_len):
    prompt = tok.apply_chat_template([{"role": "system", "content": COMPACT_PROMPT}, {"role": "user", "content": user}],
                                     tokenize=False, add_generation_prompt=True, enable_thinking=False)
    p_ids = tok(prompt, add_special_tokens=False)["input_ids"]
    t_ids = tok(prose + "<|im_end|>", add_special_tokens=False)["input_ids"]
    ids = (p_ids + t_ids)[:max_len]
    labels = ([-100] * len(p_ids) + t_ids)[:max_len]
    return ids, labels


def batches(rows, bs, pad_id, shuffle, rng):
    idx = list(range(len(rows)))
    if shuffle:
        rng.shuffle(idx)
    for i in range(0, len(idx), bs):
        chunk = [rows[j] for j in idx[i:i + bs]]
        n = max(len(r[0]) for r in chunk)
        ids = torch.full((len(chunk), n), pad_id)
        lab = torch.full((len(chunk), n), -100)
        att = torch.zeros((len(chunk), n), dtype=torch.long)
        for k, (a, b) in enumerate(chunk):
            ids[k, :len(a)] = torch.tensor(a)
            lab[k, :len(b)] = torch.tensor(b)
            att[k, :len(a)] = 1
        yield ids, lab, att


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--base", required=True)
    ap.add_argument("--data", required=True)
    ap.add_argument("--out", required=True)
    ap.add_argument("--epochs", type=float, default=2)
    ap.add_argument("--lr", type=float, default=2e-4)
    ap.add_argument("--bs", type=int, default=16)
    ap.add_argument("--micro", type=int, default=8)
    ap.add_argument("--rank", type=int, default=16)
    ap.add_argument("--max-len", type=int, default=448)
    ap.add_argument("--4bit", dest="four", action="store_true")
    ap.add_argument("--seed", type=int, default=1)
    a = ap.parse_args()
    torch.manual_seed(a.seed)
    rng = random.Random(a.seed)
    dev = "cuda" if torch.cuda.is_available() else "cpu"
    tok = AutoTokenizer.from_pretrained(a.base)
    kw = {"torch_dtype": torch.float32}
    if a.four:
        from transformers import BitsAndBytesConfig
        kw = {"quantization_config": BitsAndBytesConfig(load_in_4bit=True, bnb_4bit_quant_type="nf4",
                                                        bnb_4bit_compute_dtype=torch.float32), "device_map": {"": 0}}
    model = AutoModelForCausalLM.from_pretrained(a.base, **kw)
    if not a.four:
        model.to(dev)
    model.gradient_checkpointing_enable()
    model.enable_input_require_grads()
    model = get_peft_model(model, LoraConfig(r=a.rank, lora_alpha=2 * a.rank, lora_dropout=0.05, task_type="CAUSAL_LM",
                                             target_modules=["q_proj", "k_proj", "v_proj", "o_proj", "gate_proj", "up_proj", "down_proj"]))
    model.print_trainable_parameters()

    rows = [json.loads(l) for l in open(a.data) if l.strip()]
    rng.shuffle(rows)
    nval = max(50, len(rows) // 20)
    enc = [encode(tok, r["user"], r["prose"], a.max_len) for r in rows]
    val, train = enc[:nval], enc[nval:]
    steps_per_epoch = math.ceil(len(train) / a.bs)
    total = int(steps_per_epoch * a.epochs)
    opt = torch.optim.AdamW([p for p in model.parameters() if p.requires_grad], lr=a.lr, weight_decay=0.0)
    sched = torch.optim.lr_scheduler.LambdaLR(opt, lambda s: min(1, (s + 1) / 30) * 0.5 * (1 + math.cos(math.pi * min(s, total) / total)))
    accum = a.bs // a.micro
    pad = tok.pad_token_id if tok.pad_token_id is not None else tok.eos_token_id

    def evaluate():
        model.eval()
        tot, n = 0.0, 0
        with torch.no_grad():
            for ids, lab, att in batches(val, a.micro, pad, False, rng):
                out = model(input_ids=ids.to(dev), attention_mask=att.to(dev), labels=lab.to(dev))
                tot += out.loss.item() * ids.shape[0]
                n += ids.shape[0]
        model.train()
        return tot / n

    log = {"base": a.base, "data": os.path.basename(a.data), "examples": len(train), "validation": len(val),
           "epochs": a.epochs, "lr": a.lr, "batch": a.bs, "rank": a.rank, "four_bit": a.four, "device": dev,
           "gpu": torch.cuda.get_device_name(0) if dev == "cuda" else None, "prompt": COMPACT_PROMPT,
           "compute": "fp32" if not a.four else "4-bit NF4 weights, fp32 compute", "saved": "fp16", "history": []}
    t0 = time.time()
    step, done = 0, False
    model.train()
    log["history"].append({"step": 0, "val_loss": evaluate()})
    print(log["history"][-1], flush=True)
    while not done:
        micro_i = 0
        for ids, lab, att in batches(train, a.micro, pad, True, rng):
            out = model(input_ids=ids.to(dev), attention_mask=att.to(dev), labels=lab.to(dev))
            (out.loss / accum).backward()
            micro_i += 1
            if micro_i % accum:
                continue
            torch.nn.utils.clip_grad_norm_(model.parameters(), 1.0)
            opt.step()
            sched.step()
            opt.zero_grad()
            step += 1
            if step % 50 == 0 or step == total:
                h = {"step": step, "loss": round(out.loss.item(), 4), "val_loss": round(evaluate(), 4),
                     "elapsed_s": round(time.time() - t0)}
                log["history"].append(h)
                print(h, flush=True)
            if step >= total:
                done = True
                break
    log["train_seconds"] = round(time.time() - t0)
    os.makedirs(a.out, exist_ok=True)
    model.save_pretrained(os.path.join(a.out, "adapter"))
    json.dump(log, open(os.path.join(a.out, "training.json"), "w"), indent=1)
    # Merge into a full-precision copy of the base (on the CPU).
    del model
    torch.cuda.empty_cache() if dev == "cuda" else None
    from peft import PeftModel
    base = AutoModelForCausalLM.from_pretrained(a.base, torch_dtype=torch.float32)
    merged = PeftModel.from_pretrained(base, os.path.join(a.out, "adapter")).merge_and_unload()
    merged = merged.to(torch.float16)
    merged.save_pretrained(os.path.join(a.out, "merged"), safe_serialization=True)
    tok.save_pretrained(os.path.join(a.out, "merged"))
    print("saved", a.out, "train seconds", log["train_seconds"], flush=True)


if __name__ == "__main__":
    main()
