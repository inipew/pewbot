package core

import (
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var ridSeq uint64
var ridOnce sync.Once

func newReqID() string {
	ridOnce.Do(func() {
		rand.Seed(time.Now().UnixNano())
	})
	n := atomic.AddUint64(&ridSeq, 1)
	// short-ish: base36 timestamp + seq + 2 random chars
	ts := time.Now().UnixNano()
	return base36(ts) + "-" + base36(int64(n)) + randSuffix(2)
}

func randSuffix(n int) string {
	const alpha = "abcdefghijklmnopqrstuvwxyz0123456789"
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteByte(alpha[rand.Intn(len(alpha))])
	}
	return b.String()
}

func base36(v int64) string {
	const chars = "0123456789abcdefghijklmnopqrstuvwxyz"
	if v < 0 {
		v = -v
	}
	if v == 0 {
		return "0"
	}
	var out [32]byte
	i := len(out)
	for v > 0 {
		i--
		out[i] = chars[v%36]
		v /= 36
	}
	return string(out[i:])
}

// tokenizeCommandLine splits command text into tokens while supporting quotes.
// Examples:
//
//	/cmd a "b c" --k=v
func tokenizeCommandLine(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	var (
		out   []string
		buf   strings.Builder
		inQ   bool
		qChar byte
		esc   bool
	)
	flush := func() {
		if buf.Len() > 0 {
			out = append(out, buf.String())
			buf.Reset()
		}
	}
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if esc {
			buf.WriteByte(ch)
			esc = false
			continue
		}
		if ch == '\\' {
			esc = true
			continue
		}
		if inQ {
			if ch == qChar {
				inQ = false
				continue
			}
			buf.WriteByte(ch)
			continue
		}
		switch ch {
		case '"', '\'':
			inQ = true
			qChar = ch
		case ' ', '\t', '\n', '\r':
			flush()
		default:
			buf.WriteByte(ch)
		}
	}
	flush()
	return out
}

// parseFlags splits raw args into positionals and flags.
//
// Supported:
//
//	--k=v, --k v, --flag (bool)
//	-k=v, -k v, -abc (bool flags a,b,c)
func parseFlags(args []string) (pos []string, flags map[string]string, bools map[string]bool) {
	flags = map[string]string{}
	bools = map[string]bool{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "--") && len(a) > 2 {
			key := strings.TrimPrefix(a, "--")
			if eq := strings.IndexByte(key, '='); eq >= 0 {
				flags[key[:eq]] = key[eq+1:]
				continue
			}
			// value in next token?
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				flags[key] = args[i+1]
				i++
				continue
			}
			bools[key] = true
			continue
		}
		if strings.HasPrefix(a, "-") && len(a) > 1 && a != "-" {
			key := strings.TrimPrefix(a, "-")
			if eq := strings.IndexByte(key, '='); eq >= 0 {
				flags[key[:eq]] = key[eq+1:]
				continue
			}
			// short -k value
			if len(key) == 1 {
				if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
					flags[key] = args[i+1]
					i++
					continue
				}
				bools[key] = true
				continue
			}
			// -abc => bool a,b,c
			for j := 0; j < len(key); j++ {
				bools[string(key[j])] = true
			}
			continue
		}
		pos = append(pos, a)
	}
	return pos, flags, bools
}
