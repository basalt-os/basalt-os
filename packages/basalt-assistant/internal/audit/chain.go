package audit

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Files returns the log's files in chain order: the sealed files
// (<stem>-<UTC time>.jsonl, oldest first), then the current file at path
// when it exists.
func Files(path string) ([]string, error) {
	base := filepath.Base(path)
	stem := strings.TrimSuffix(base, filepath.Ext(base))
	sealed, err := filepath.Glob(filepath.Join(filepath.Dir(path), globEscape(stem)+"-*.jsonl"))
	if err != nil {
		return nil, err
	}
	// Order by seal time, then by the counter of files sealed in the same
	// second (<stem>-TS.jsonl, <stem>-TS-2.jsonl, ...). A plain string
	// sort puts "-2" before ".jsonl" and breaks the chain order.
	key := func(p string) string {
		rest := strings.TrimPrefix(strings.TrimSuffix(filepath.Base(p), ".jsonl"), stem+"-")
		ts, n, _ := strings.Cut(rest, "-")
		if n == "" {
			n = "1"
		}
		return fmt.Sprintf("%s-%06s", ts, n)
	}
	sort.SliceStable(sealed, func(i, j int) bool { return key(sealed[i]) < key(sealed[j]) })
	out := make([]string, 0, len(sealed)+1)
	for _, p := range sealed {
		if p != path {
			out = append(out, p)
		}
	}
	if _, err := os.Stat(path); err == nil {
		out = append(out, path)
	} else if !os.IsNotExist(err) {
		return nil, err
	} else if len(out) == 0 {
		return nil, err
	}
	return out, nil
}

func globEscape(s string) string {
	r := strings.NewReplacer(`*`, `\*`, `?`, `\?`, `[`, `\[`, `\`, `\\`)
	return r.Replace(s)
}

// Summary describes a verified chain.
type Summary struct {
	Files     []string `json:"files"`
	FirstSeq  int64    `json:"first_seq"`
	LastSeq   int64    `json:"last_seq"`
	Records   int64    `json:"records"`
	Seals     int      `json:"seals"`
	Truncated bool     `json:"truncated"`  // the oldest file left starts with a continue record: earlier files were removed
	Unsealed  bool     `json:"unfinished"` // the current file ends with a seal: a rotation did not finish
}

// VerifyChain checks every file of the log at path: in each file, the
// sequence numbers, the prev links and every record's hash; across files,
// that each sealed file ends with a seal whose SHA-256 matches the file's
// content and whose name matches the file, and that the next file starts
// with a continue record chained to that seal. The returned summary covers
// the records verified up to the first problem.
func VerifyChain(path string) (Summary, error) {
	var s Summary
	files, err := Files(path)
	if err != nil {
		return s, err
	}
	s.Files = files
	var prev *Record // the last record of the previous file (its seal)
	for i, file := range files {
		isLast := i == len(files)-1
		recs, offsets, data, err := readFile(file)
		if err != nil {
			return s, err
		}
		name := filepath.Base(file)
		if len(recs) == 0 {
			if prev != nil || !isLast {
				return s, fmt.Errorf("%s: empty", name)
			}
			continue
		}
		first := recs[0]
		switch {
		case prev != nil:
			if first.Type != TypeContinue {
				return s, fmt.Errorf("%s: does not start with a continue record after the seal of the previous file", name)
			}
			var cd ContinueData
			_ = json.Unmarshal(first.Data, &cd)
			if first.Prev != prev.Hash || first.Seq != prev.Seq+1 || cd.SealHash != prev.Hash || cd.SealSeq != prev.Seq {
				return s, fmt.Errorf("%s: continue record %d is not chained to the seal record %d", name, first.Seq, prev.Seq)
			}
			if cd.From != filepath.Base(files[i-1]) {
				return s, fmt.Errorf("%s: continues from %q, but the previous file is %s", name, cd.From, filepath.Base(files[i-1]))
			}
		case first.Type == TypeContinue:
			s.Truncated = true // earlier files are gone; the chain is checked from here
		default:
			if first.Seq != 1 || first.Prev != zeroHash {
				return s, fmt.Errorf("%s: the chain does not start at record 1", name)
			}
		}
		if s.FirstSeq == 0 {
			s.FirstSeq = first.Seq
		}
		for j, r := range recs {
			if j > 0 {
				p := recs[j-1]
				if r.Seq != p.Seq+1 {
					return s, fmt.Errorf("%s: seq %d follows seq %d (records missing or reordered)", name, r.Seq, p.Seq)
				}
				if r.Prev != p.Hash {
					return s, fmt.Errorf("%s: seq %d: prev hash does not match the previous record", name, r.Seq)
				}
			}
			if h := hashOf(r); h != r.Hash {
				return s, fmt.Errorf("%s: seq %d: content does not match its hash (edited)", name, r.Seq)
			}
			if r.Type == TypeSeal {
				if j != len(recs)-1 {
					return s, fmt.Errorf("%s: seal record %d is not the last record of its file", name, r.Seq)
				}
				var sd SealData
				if err := json.Unmarshal(r.Data, &sd); err != nil {
					return s, fmt.Errorf("%s: seal record %d: %v", name, r.Seq, err)
				}
				sum := sha256.Sum256(data[:offsets[j]])
				if hex.EncodeToString(sum[:]) != sd.SHA256 {
					return s, fmt.Errorf("%s: content before the seal record %d does not match its SHA-256", name, r.Seq)
				}
				if sd.FirstSeq != first.Seq || sd.Records != r.Seq-first.Seq {
					return s, fmt.Errorf("%s: seal record %d counts records %d+%d, the file holds %d+%d", name, r.Seq, sd.FirstSeq, sd.Records, first.Seq, r.Seq-first.Seq)
				}
				if !isLast && sd.File != name {
					return s, fmt.Errorf("%s: sealed as %q", name, sd.File)
				}
				s.Seals++
			}
			s.Records++
			s.LastSeq = r.Seq
		}
		last := recs[len(recs)-1]
		if !isLast && last.Type != TypeSeal {
			return s, fmt.Errorf("%s: a rotated file that does not end with a seal record", name)
		}
		if isLast && last.Type == TypeSeal {
			s.Unsealed = true
		}
		prev = &last
	}
	return s, nil
}

// readFile parses one file and returns its records, the byte offset where
// each record starts, and the raw content.
func readFile(path string) ([]Record, []int, []byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, nil, err
	}
	var recs []Record
	var offs []int
	off := 0
	for off < len(data) {
		end := bytes.IndexByte(data[off:], '\n')
		line := data[off:]
		next := len(data)
		if end >= 0 {
			line, next = data[off:off+end], off+end+1
		}
		if len(bytes.TrimSpace(line)) > 0 {
			var r Record
			if err := json.Unmarshal(line, &r); err != nil {
				after := int64(0)
				if len(recs) > 0 {
					after = recs[len(recs)-1].Seq
				}
				return nil, nil, nil, fmt.Errorf("%s: record after seq %d: not JSON: %w", filepath.Base(path), after, err)
			}
			recs = append(recs, r)
			offs = append(offs, off)
		}
		off = next
	}
	return recs, offs, data, nil
}

// Verify checks the whole chain (every file) and returns the sequence
// number of the last record verified.
func Verify(path string) (int64, error) {
	s, err := VerifyChain(path)
	return s.LastSeq, err
}

// Tail returns the last n records (n <= 0: all) across the log's files.
func Tail(path string, n int) ([]Record, error) {
	files, err := Files(path)
	if err != nil {
		return nil, err
	}
	var out []Record
	for i := len(files) - 1; i >= 0; i-- {
		recs, err := readRecords(files[i])
		if err != nil {
			return nil, err
		}
		out = append(recs, out...)
		if n > 0 && len(out) >= n {
			break
		}
	}
	if n > 0 && len(out) > n {
		out = out[len(out)-n:]
	}
	return out, nil
}

// readRecords reads a file's records, skipping lines that do not parse.
func readRecords(path string) ([]Record, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		var r Record
		if json.Unmarshal(sc.Bytes(), &r) == nil {
			out = append(out, r)
		}
	}
	return out, sc.Err()
}
