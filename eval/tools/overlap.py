#!/usr/bin/env python3
"""Overlap between translator training pairs and a test set.

    eval/tools/overlap.py TRAIN.jsonl TEST.jsonl [N]

Slot values (proposal ids, numbers) are masked, text is normalized (case,
accents, punctuation); for every test request the most similar training
request is found (character 3-gram Jaccard and difflib ratio, the higher
of the two). Prints the counts per similarity bucket, exact matches after
masking, how many test units also appear in training, and the N closest
pairs. Used to check that the held-out set is independent of the
generator (docs/eval-suite.md)."""
def norm(s):
    s = unicodedata.normalize("NFKD", s.lower()); s = "".join(c for c in s if not unicodedata.combining(c))
    return re.sub(r"[^a-z0-9]+", " ", s).strip()
def mask(s):  # slot values replaced, to compare phrasing (template) only
    s = re.sub(r"p-[0-9a-f]{6}", "<id>", s); s = re.sub(r"\b\d+\b", "<n>", s); return s
train = [json.loads(l) for l in open(sys.argv[1]) if l.strip() and not l.startswith('{"_meta')]
test = [json.loads(l) for l in open(sys.argv[2]) if l.strip()]
units = set(re.findall(r'"unit": "([^"]+)"', open(sys.argv[1]).read()))
tn = [norm(mask(t["text"])) for t in train]
tset = set(tn)
tg = [set(x[i:i+3] for i in range(len(x)-2)) for x in tn]
templates = set(norm(mask(t["template"].replace("{u}","<u>").replace("{p}","<id>").replace("{a}","<n>").replace("{b}","<n>").replace("{d}","<d>"))) for t in train)
exact = 0; buckets = {">=0.9":0, "0.8-0.9":0, "0.7-0.8":0, "<0.7":0}; rows=[]
for t in test:
    x = norm(mask(t["text"])); g = set(x[i:i+3] for i in range(len(x)-2))
    if x in tset: exact += 1
    best, bi = 0, -1
    for i, y in enumerate(tg):
        if not g or not y: continue
        j = len(g & y) / len(g | y)
        if j > best: best, bi = j, i
    r = difflib.SequenceMatcher(None, x, tn[bi]).ratio() if bi >= 0 else 0
    s = max(best, r)
    k = ">=0.9" if s >= .9 else "0.8-0.9" if s >= .8 else "0.7-0.8" if s >= .7 else "<0.7"
    buckets[k] += 1; rows.append((s, t["id"], t["text"], train[bi]["text"]))
n = len(test)
unit_in = sum(1 for t in test if t["want"].get("unit","").replace(".service","") in {u.replace(".service","") for u in units} and t["want"].get("unit"))
whyn = sum(1 for t in test if t["want"].get("unit"))
print(json.dumps({"test": n, "train": len(train), "exact_after_masking_slots": exact, "max_similarity_buckets": buckets,
  "test_units_seen_in_train": f"{unit_in}/{whyn}", "mean_max_similarity": round(sum(r[0] for r in rows)/n, 3)}))
rows.sort(reverse=True)
for s, i, a, b in rows[:int(sys.argv[3]) if len(sys.argv)>3 else 8]: print(round(s,2), i, "|", a, "|", b)
