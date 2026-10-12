// Package promtest is a strict parser of the Prometheus text exposition format (0.0.4) for tests: it
// fails on anything a Prometheus server would reject or that indicates a broken exposition (a family
// split in two, HELP/TYPE twice, a sample of an undeclared family, bad label escaping, a histogram
// whose buckets are not cumulative or lack +Inf, _count differing from the +Inf bucket).
package promtest

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Sample is one parsed sample.
type Sample struct {
	Name   string
	Labels map[string]string
	Value  float64
}

// Family is one parsed family.
type Family struct {
	Name, Help, Type string
	Samples          []Sample
}

var nameRe = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)
var labelRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// Parse reads an exposition and validates it.
func Parse(r io.Reader) (map[string]*Family, error) {
	fams := map[string]*Family{}
	var order []string
	closed := map[string]bool{}
	cur := ""
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	ln := 0
	for sc.Scan() {
		ln++
		line := sc.Text()
		if line == "" {
			continue
		}
		fail := func(f string, a ...any) error { return fmt.Errorf("line %d %q: %s", ln, line, fmt.Sprintf(f, a...)) }
		if strings.HasPrefix(line, "#") {
			fs := strings.SplitN(line, " ", 4)
			if len(fs) < 3 || (fs[1] != "HELP" && fs[1] != "TYPE") {
				continue // a comment
			}
			name := fs[2]
			if !nameRe.MatchString(name) {
				return nil, fail("bad metric name")
			}
			if closed[name] {
				return nil, fail("family %s appears twice (families must be contiguous)", name)
			}
			if cur != name && cur != "" {
				closed[cur] = true
			}
			f := fams[name]
			if f == nil {
				f = &Family{Name: name}
				fams[name] = f
				order = append(order, name)
			}
			rest := ""
			if len(fs) == 4 {
				rest = fs[3]
			}
			if fs[1] == "HELP" {
				if f.Help != "" {
					return nil, fail("HELP twice")
				}
				f.Help = rest
				if f.Help == "" {
					f.Help = " " // empty help allowed but counted
				}
			} else {
				if f.Type != "" {
					return nil, fail("TYPE twice")
				}
				switch rest {
				case "counter", "gauge", "histogram", "summary", "untyped":
				default:
					return nil, fail("bad type %q", rest)
				}
				if len(f.Samples) > 0 {
					return nil, fail("TYPE after samples")
				}
				f.Type = rest
			}
			cur = name
			continue
		}
		name, labels, value, err := parseSample(line)
		if err != nil {
			return nil, fail("%v", err)
		}
		fam := name
		if f := fams[cur]; f != nil && f.Type == "histogram" {
			for _, suf := range []string{"_bucket", "_sum", "_count"} {
				if strings.TrimSuffix(name, suf) == cur && strings.HasSuffix(name, suf) {
					fam = cur
				}
			}
		}
		if fam != cur {
			return nil, fail("sample of %s outside its family block (current %s)", name, cur)
		}
		f := fams[fam]
		if f.Type == "" {
			return nil, fail("sample before TYPE")
		}
		f.Samples = append(f.Samples, Sample{Name: name, Labels: labels, Value: value})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	for _, n := range order {
		f := fams[n]
		if f.Type == "histogram" {
			if err := checkHistogram(f); err != nil {
				return nil, fmt.Errorf("%s: %w", n, err)
			}
		}
		// duplicate series
		seen := map[string]bool{}
		for _, s := range f.Samples {
			k := s.Name + labelKey(s.Labels)
			if seen[k] {
				return nil, fmt.Errorf("%s: duplicate series %s", n, k)
			}
			seen[k] = true
		}
	}
	return fams, nil
}

func labelKey(l map[string]string) string {
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString("," + k + "=" + strconv.Quote(l[k]))
	}
	return "{" + b.String() + "}"
}

func parseSample(line string) (string, map[string]string, float64, error) {
	i := strings.IndexAny(line, "{ ")
	if i < 0 {
		return "", nil, 0, fmt.Errorf("no value")
	}
	name := line[:i]
	if !nameRe.MatchString(name) {
		return "", nil, 0, fmt.Errorf("bad name %q", name)
	}
	labels := map[string]string{}
	rest := line[i:]
	if strings.HasPrefix(rest, "{") {
		j := 1
		for {
			if j >= len(rest) {
				return "", nil, 0, fmt.Errorf("unterminated labels")
			}
			if rest[j] == '}' {
				j++
				break
			}
			eq := strings.IndexByte(rest[j:], '=')
			if eq < 0 {
				return "", nil, 0, fmt.Errorf("label without =")
			}
			ln := rest[j : j+eq]
			if !labelRe.MatchString(ln) {
				return "", nil, 0, fmt.Errorf("bad label name %q", ln)
			}
			j += eq + 1
			if j >= len(rest) || rest[j] != '"' {
				return "", nil, 0, fmt.Errorf("label value not quoted")
			}
			j++
			var v strings.Builder
			for {
				if j >= len(rest) {
					return "", nil, 0, fmt.Errorf("unterminated label value")
				}
				c := rest[j]
				if c == '\\' {
					if j+1 >= len(rest) {
						return "", nil, 0, fmt.Errorf("dangling escape")
					}
					switch rest[j+1] {
					case '\\':
						v.WriteByte('\\')
					case '"':
						v.WriteByte('"')
					case 'n':
						v.WriteByte('\n')
					default:
						return "", nil, 0, fmt.Errorf("bad escape \\%c", rest[j+1])
					}
					j += 2
					continue
				}
				if c == '"' {
					j++
					break
				}
				if c == '\n' {
					return "", nil, 0, fmt.Errorf("raw newline in label value")
				}
				v.WriteByte(c)
				j++
			}
			if _, dup := labels[ln]; dup {
				return "", nil, 0, fmt.Errorf("label %s twice", ln)
			}
			labels[ln] = v.String()
			if j < len(rest) && rest[j] == ',' {
				j++
			}
		}
		rest = rest[j:]
	}
	if !strings.HasPrefix(rest, " ") {
		return "", nil, 0, fmt.Errorf("no space before the value")
	}
	fs := strings.Fields(rest)
	if len(fs) < 1 || len(fs) > 2 {
		return "", nil, 0, fmt.Errorf("bad value part %q", rest)
	}
	var v float64
	switch fs[0] {
	case "+Inf":
		v = math.Inf(1)
	case "-Inf":
		v = math.Inf(-1)
	case "NaN":
		v = math.NaN()
	default:
		var err error
		if v, err = strconv.ParseFloat(fs[0], 64); err != nil {
			return "", nil, 0, fmt.Errorf("bad value %q", fs[0])
		}
	}
	return name, labels, v, nil
}

func checkHistogram(f *Family) error {
	type series struct {
		buckets    []struct{ le, v float64 }
		sum, count *float64
		hasInf     bool
		infV       float64
	}
	ss := map[string]*series{}
	get := func(l map[string]string) *series {
		c := map[string]string{}
		for k, v := range l {
			if k != "le" {
				c[k] = v
			}
		}
		k := labelKey(c)
		if ss[k] == nil {
			ss[k] = &series{}
		}
		return ss[k]
	}
	for _, s := range f.Samples {
		x := get(s.Labels)
		v := s.Value
		switch {
		case strings.HasSuffix(s.Name, "_bucket"):
			le, ok := s.Labels["le"]
			if !ok {
				return fmt.Errorf("bucket without le")
			}
			if le == "+Inf" {
				x.hasInf, x.infV = true, v
				continue
			}
			b, err := strconv.ParseFloat(le, 64)
			if err != nil {
				return fmt.Errorf("bad le %q", le)
			}
			x.buckets = append(x.buckets, struct{ le, v float64 }{b, v})
		case strings.HasSuffix(s.Name, "_sum"):
			x.sum = &v
		case strings.HasSuffix(s.Name, "_count"):
			x.count = &v
		}
	}
	for k, x := range ss {
		if !x.hasInf || x.sum == nil || x.count == nil {
			return fmt.Errorf("series %s lacks +Inf, _sum or _count", k)
		}
		prevLe, prevV := math.Inf(-1), 0.0
		for _, b := range x.buckets {
			if b.le <= prevLe || b.v < prevV {
				return fmt.Errorf("series %s: buckets not increasing/cumulative", k)
			}
			prevLe, prevV = b.le, b.v
		}
		if x.infV < prevV || *x.count != x.infV {
			return fmt.Errorf("series %s: +Inf %v, count %v, last bucket %v", k, x.infV, *x.count, prevV)
		}
	}
	return nil
}
