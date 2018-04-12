package main

import (
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"sort"

	"golang.org/x/text/encoding/japanese"

	toml "github.com/pelletier/go-toml"
)

type rule struct {
	Dir      string
	File     string
	Text     string
	Encoding string
	Layer    int
	Modifier string

	fileRE *regexp.Regexp
	textRE *regexp.Regexp
}

type setting struct {
	Delta float64
	Rule  []rule
}

func makeWildcard(s string) (*regexp.Regexp, error) {
	buf := make([]byte, 0, 64)
	buf = append(buf, '^')
	pos := 0
	for i, c := range []byte(s) {
		if c != '*' && c != '?' {
			continue
		}
		if i != pos {
			buf = append(buf, regexp.QuoteMeta(s[pos:i])...)
		}
		switch c {
		case '*':
			buf = append(buf, `[^/\\]*?`...)
		case '?':
			buf = append(buf, `[^/\\]`...)
		}
		pos = i + 1
	}
	if pos != len(s) {
		buf = append(buf, regexp.QuoteMeta(s[pos:])...)
	}
	buf = append(buf, '$')
	return regexp.Compile(string(buf))
}

func newSetting(path string) (*setting, error) {
	f, err := openTextFile(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var s setting
	err = toml.NewDecoder(f).Decode(&s)
	if err != nil {
		return nil, err
	}
	for i := range s.Rule {
		s.Rule[i].fileRE, err = makeWildcard(s.Rule[i].File)
		if err != nil {
			return nil, err
		}
		if s.Rule[i].Text != "" {
			s.Rule[i].textRE, err = regexp.Compile(s.Rule[i].Text)
			if err != nil {
				return nil, err
			}
		}
	}
	return &s, nil
}

func (ss *setting) Find(path string) (*rule, string, error) {
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	textRaw, err := readFile(path[:len(path)-4] + ".txt")
	if err != nil {
		return nil, "", err
	}
	var u8, sjis *string

	for i := range ss.Rule {
		r := &ss.Rule[i]
		if dir != r.Dir {
			continue
		}
		if !r.fileRE.MatchString(base) {
			continue
		}
		if r.textRE != nil {
			switch r.Encoding {
			case "utf8":
				if u8 == nil {
					t := string(textRaw)
					u8 = &t
				}
				if !r.textRE.MatchString(*u8) {
					continue
				}
			case "sjis":
				if sjis == nil {
					b, err := japanese.ShiftJIS.NewDecoder().Bytes(textRaw)
					if err != nil {
						return nil, "", err
					}
					t := string(b)
					sjis = &t
				}
				if !r.textRE.MatchString(*sjis) {
					continue
				}
			}
		}
		switch r.Encoding {
		case "utf8":
			if u8 == nil {
				t := string(textRaw)
				u8 = &t
			}
			return r, *u8, nil
		case "sjis":
			if sjis == nil {
				b, err := japanese.ShiftJIS.NewDecoder().Bytes(textRaw)
				if err != nil {
					return nil, "", err
				}
				t := string(b)
				sjis = &t
			}
			return r, *sjis, nil
		default:
			panic("unexcepted encoding value: " + r.Encoding)
		}
	}
	return nil, "", nil
}

func (ss *setting) Dirs() []string {
	dirs := map[string]struct{}{}
	for i := range ss.Rule {
		dirs[ss.Rule[i].Dir] = struct{}{}
	}
	r := make([]string, 0, len(dirs))
	for k := range dirs {
		r = append(r, k)
	}
	sort.Strings(r)
	return r
}

func readFile(path string) ([]byte, error) {
	f, err := openTextFile(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	b, err := ioutil.ReadAll(f)
	if err != nil {
		return nil, err
	}
	return b, nil
}

func openTextFile(path string) (*os.File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}

	// skip BOM
	var bom [3]byte
	_, err = f.ReadAt(bom[:], 0)
	if err != nil {
		f.Close()
		return nil, err
	}
	if bom[0] == 0xef && bom[1] == 0xbb && bom[2] == 0xbf {
		_, err = f.Seek(3, os.SEEK_SET)
		if err != nil {
			f.Close()
			return nil, err
		}
	}
	return f, nil
}
