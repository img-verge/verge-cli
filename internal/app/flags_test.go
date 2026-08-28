package app

import (
	"encoding/base64"
	"flag"
	"io"
	"strings"
	"testing"
)

func TestParseReferenceSpec(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		wantName string
		wantVal  string
		wantErr  bool
	}{
		{name: "a bare path has no reference name", in: "./pics/a.png", wantVal: "./pics/a.png"},
		{name: "NAME=VALUE splits at the first equals", in: "logo=./pics/a.png", wantName: "logo", wantVal: "./pics/a.png"},
		{name: "the value may itself contain equals", in: "logo=https://x.test/a.png?sig=abc", wantName: "logo", wantVal: "https://x.test/a.png?sig=abc"},
		{name: "surrounding whitespace is trimmed", in: "  logo = ./a.png  ", wantName: "logo", wantVal: "./a.png"},
		// 关键歧义：这两个都不是命名形式，否则 Windows 路径和带查询串的 URL 全被切坏。
		{name: "a windows path with equals stays a path", in: `C:\pics\a=b.png`, wantVal: `C:\pics\a=b.png`},
		{name: "a URL query string stays part of the URL", in: "https://x.test/a.png?k=v", wantVal: "https://x.test/a.png?k=v"},
		{name: "a leading equals is not a name", in: "=./a.png", wantVal: "=./a.png"},
		{name: "an empty value after equals is not a name", in: "logo=", wantVal: "logo="},
		// 名字里带方括号会让 [@name] 匹配永远失败，且服务端不会报错，必须本地拦下。
		{name: "brackets in a name are rejected", in: "[图1]=./a.png", wantErr: true},
		{name: "empty input is rejected", in: "   ", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			spec, err := parseReferenceSpec(test.in)
			if test.wantErr {
				if err == nil {
					t.Fatalf("parseReferenceSpec(%q) = %+v, want an error", test.in, spec)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseReferenceSpec(%q): %v", test.in, err)
			}
			if spec.Name != test.wantName || spec.Value != test.wantVal {
				t.Errorf("parseReferenceSpec(%q) = {Name:%q Value:%q}, want {Name:%q Value:%q}",
					test.in, spec.Name, spec.Value, test.wantName, test.wantVal)
			}
		})
	}
}

func TestParseBase64ReferenceSpec(t *testing.T) {
	noPadding := base64.StdEncoding.EncodeToString([]byte{1, 2, 3})
	onePadding := base64.StdEncoding.EncodeToString([]byte{1, 2})
	twoPadding := base64.StdEncoding.EncodeToString([]byte{1})
	dataURI := "data:image/png;base64," + twoPadding

	tests := []struct {
		name     string
		in       string
		wantName string
		wantVal  string
		wantErr  bool
	}{
		{name: "unnamed raw without padding", in: noPadding, wantVal: noPadding},
		{name: "unnamed raw with one padding byte", in: onePadding, wantVal: onePadding},
		{name: "unnamed raw with two padding bytes", in: twoPadding, wantVal: twoPadding},
		{name: "named raw base64", in: "ref=" + twoPadding, wantName: "ref", wantVal: twoPadding},
		{name: "unnamed data URI", in: dataURI, wantVal: dataURI},
		{name: "named data URI", in: "ref=" + dataURI, wantName: "ref", wantVal: dataURI},
		{name: "invalid raw base64", in: "%%%", wantErr: true},
		{name: "invalid name", in: "[ref]=" + twoPadding, wantErr: true},
		{name: "empty", in: "  ", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			spec, err := parseBase64ReferenceSpec(test.in)
			if test.wantErr {
				if err == nil {
					t.Fatalf("parseBase64ReferenceSpec(%q) = %+v, want an error", test.in, spec)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseBase64ReferenceSpec(%q): %v", test.in, err)
			}
			if spec.Name != test.wantName || spec.Value != test.wantVal {
				t.Errorf("parseBase64ReferenceSpec(%q) = {Name:%q Value:%q}, want {Name:%q Value:%q}",
					test.in, spec.Name, spec.Value, test.wantName, test.wantVal)
			}
		})
	}
}

func TestReferenceFlagCollectsInOrder(t *testing.T) {
	var specs []referenceSpec
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Var(referenceFlag{label: "--file", specs: &specs}, "f", "")

	if err := fs.Parse([]string{"-f", "a.png", "-f", "logo=b.png", "-f", "c.png"}); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	// 顺序必须保留：prepare 返回的 uploads 是按 images 顺序一一对应的。
	want := []referenceSpec{{Value: "a.png"}, {Name: "logo", Value: "b.png"}, {Value: "c.png"}}
	if len(specs) != len(want) {
		t.Fatalf("specs = %+v, want %+v", specs, want)
	}
	for index := range want {
		if specs[index] != want[index] {
			t.Errorf("specs[%d] = %+v, want %+v", index, specs[index], want[index])
		}
	}
}

func TestReferenceFlagRejectsBadValue(t *testing.T) {
	var specs []referenceSpec
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Var(referenceFlag{label: "--file", specs: &specs}, "f", "")

	err := fs.Parse([]string{"-f", "[图1]=a.png"})
	if err == nil {
		t.Fatal("Parse should reject a reference name containing brackets")
	}
	// 报错要说清是哪个 flag 出的问题。
	if !strings.Contains(err.Error(), "--file") {
		t.Errorf("error = %q, want it to name the flag", err)
	}
}

// TestUnreferencedNames guards the quietest failure mode in this API: the server matches
// [@name] literally, so a typo means that reference is silently ignored.
func TestUnreferencedNames(t *testing.T) {
	files := []referenceSpec{{Name: "logo", Value: "a.png"}, {Value: "b.png"}}
	urls := []referenceSpec{{Name: "bg", Value: "https://x.test/c.png"}}

	if got := unreferencedNames("put [@logo] over [@bg]", files, urls); len(got) != 0 {
		t.Errorf("unreferencedNames = %v, want none", got)
	}
	got := unreferencedNames("put [@logo] over the background", files, urls)
	if len(got) != 1 || got[0] != "bg" {
		t.Errorf("unreferencedNames = %v, want [bg]", got)
	}
	// 只提到名字但没写成 [@名字] 同样不算引用。
	if got := unreferencedNames("use logo and bg", files, urls); len(got) != 2 {
		t.Errorf("unreferencedNames = %v, want both names reported", got)
	}
	// 未命名的参考图按位置生效，不需要出现在提示词里。
	if got := unreferencedNames("nothing referenced", []referenceSpec{{Value: "a.png"}}); len(got) != 0 {
		t.Errorf("unreferencedNames = %v, want none for unnamed references", got)
	}
}

func TestDuplicateNames(t *testing.T) {
	files := []referenceSpec{{Name: "logo", Value: "a.png"}, {Name: "logo", Value: "b.png"}}
	urls := []referenceSpec{{Name: "bg", Value: "https://x.test/c.png"}}

	got := duplicateNames(files, urls)
	if len(got) != 1 || got[0] != "logo" {
		t.Errorf("duplicateNames = %v, want [logo]", got)
	}
	// 跨组重名同样是重名：-f 和 --image-url 的名字在同一个命名空间里。
	crossGroup := duplicateNames([]referenceSpec{{Name: "bg", Value: "a.png"}}, urls)
	if len(crossGroup) != 1 || crossGroup[0] != "bg" {
		t.Errorf("duplicateNames across groups = %v, want [bg]", crossGroup)
	}
	if got := duplicateNames(files[:1], urls); len(got) != 0 {
		t.Errorf("duplicateNames = %v, want none", got)
	}
	if got := duplicateNames([]referenceSpec{{Value: "a.png"}, {Value: "b.png"}}); len(got) != 0 {
		t.Errorf("unnamed references cannot collide, got %v", got)
	}
}

// TestPermuteArgs covers the whole reason permutation exists: without it Go's flag package
// stops at the first positional, so `verge task create "prompt" -o ./out` would fold "-o" and
// "./out" into the prompt and create an image whose prompt contains flag text.
func TestPermuteArgs(t *testing.T) {
	newFS := func() *flag.FlagSet {
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		fs.String("o", "", "")
		fs.String("model", "", "")
		fs.Bool("wait", false, "")
		return fs
	}
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "a trailing value flag moves ahead of the prompt",
			in:   []string{"a neon city", "-o", "./out"},
			want: []string{"-o", "./out", "--", "a neon city"},
		},
		{
			name: "a trailing bool flag does not swallow the next argument",
			in:   []string{"prompt", "-wait", "extra"},
			want: []string{"-wait", "--", "prompt", "extra"},
		},
		{
			name: "flag=value form is left intact",
			in:   []string{"prompt", "--model=gpt-image-2"},
			want: []string{"--model=gpt-image-2", "--", "prompt"},
		},
		{
			name: "already-ordered arguments are unchanged in effect",
			in:   []string{"-o", "./out", "prompt"},
			want: []string{"-o", "./out", "--", "prompt"},
		},
		{
			name: "everything after -- stays positional",
			in:   []string{"-o", "./out", "--", "-not-a-flag"},
			want: []string{"-o", "./out", "--", "-not-a-flag"},
		},
		{
			// 单个 "-" 是"从 stdin 读"的惯例，不能被当成 flag。
			name: "a lone dash stays positional",
			in:   []string{"-", "-wait"},
			want: []string{"-wait", "--", "-"},
		},
		{
			// 未知 flag 当作不吃值，交给 fs.Parse 去报"未定义"，而不是在这里悄悄吞掉一个参数。
			name: "an unknown flag does not consume its neighbour",
			in:   []string{"prompt", "-nope", "value"},
			want: []string{"-nope", "--", "prompt", "value"},
		},
		{name: "no positionals means no separator", in: []string{"-wait"}, want: []string{"-wait"}},
		{name: "empty input stays empty", in: []string{}, want: []string{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := permuteArgs(newFS(), test.in)
			if strings.Join(got, "\x00") != strings.Join(test.want, "\x00") {
				t.Errorf("permuteArgs(%q) = %q, want %q", test.in, got, test.want)
			}
		})
	}
}

// TestPermutedParseKeepsPromptIntact is the end-to-end version of the bug above.
func TestPermutedParseKeepsPromptIntact(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	output := fs.String("o", "", "")
	if err := fs.Parse(permuteArgs(fs, []string{"a neon city", "-o", "./out"})); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if *output != "./out" {
		t.Errorf("-o = %q, want ./out", *output)
	}
	if prompt := joinPrompt(fs.Args()); prompt != "a neon city" {
		t.Errorf("prompt = %q, want %q", prompt, "a neon city")
	}
}

func TestFlagNeedsValue(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.String("o", "", "")
	fs.Bool("wait", false, "")
	fs.Int("n", 0, "")
	fs.Duration("timeout", 0, "")

	for _, name := range []string{"o", "n", "timeout"} {
		if !flagNeedsValue(fs, name) {
			t.Errorf("flagNeedsValue(%q) = false, want true", name)
		}
	}
	if flagNeedsValue(fs, "wait") {
		t.Error("flagNeedsValue(\"wait\") = true; a bool flag takes no separate value")
	}
	if flagNeedsValue(fs, "unknown") {
		t.Error("an unknown flag must be treated as taking no value")
	}
}

func TestJoinPrompt(t *testing.T) {
	if got := joinPrompt([]string{"a", "neon", "city"}); got != "a neon city" {
		t.Errorf("joinPrompt = %q, want %q", got, "a neon city")
	}
	if got := joinPrompt([]string{"a neon city"}); got != "a neon city" {
		t.Errorf("joinPrompt = %q, want the quoted form unchanged", got)
	}
	if got := joinPrompt(nil); got != "" {
		t.Errorf("joinPrompt(nil) = %q, want empty", got)
	}
}

func TestFirstNonEmpty(t *testing.T) {
	if got := firstNonEmpty("", "  ", "2k", "4k"); got != "2k" {
		t.Errorf("firstNonEmpty = %q, want 2k: whitespace-only values must not win", got)
	}
	if got := firstNonEmpty("", " "); got != "" {
		t.Errorf("firstNonEmpty = %q, want empty", got)
	}
}
