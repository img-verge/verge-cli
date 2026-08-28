package app

import (
	"encoding/base64"
	"fmt"
	"strings"
)

// referenceSpec is one reference image argument: an optional name plus a path or URL.
//
// Name 用于在 prompt 里通过 [@名称] 引用这张参考图，必须与提示词里写的完全一致。
type referenceSpec struct {
	Name  string
	Value string
}

// referenceFlag collects repeatable -f/--file and --image-url values in order.
type referenceFlag struct {
	label string
	specs *[]referenceSpec
}

func (f referenceFlag) String() string {
	if f.specs == nil || len(*f.specs) == 0 {
		return ""
	}
	parts := make([]string, 0, len(*f.specs))
	for _, spec := range *f.specs {
		if spec.Name == "" {
			parts = append(parts, spec.Value)
			continue
		}
		parts = append(parts, spec.Name+"="+spec.Value)
	}
	return strings.Join(parts, ",")
}

func (f referenceFlag) Set(raw string) error {
	spec, err := parseReferenceSpec(raw)
	if err != nil {
		return fmt.Errorf("%s: %w", f.label, err)
	}
	*f.specs = append(*f.specs, spec)
	return nil
}

// parseReferenceSpec splits "NAME=VALUE" while leaving bare paths and URLs alone.
//
// 只在第一个 = 处切，并且要求左半段不含路径分隔符 —— 否则 Windows 路径
// `C:\pics\a=b.png` 和带查询串的 URL `https://x/y?k=v` 都会被误判成命名形式。
// 用户真有个叫 `a=b.png` 的文件时，写成 `./a=b.png` 即可绕开。
func parseReferenceSpec(raw string) (referenceSpec, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return referenceSpec{}, fmt.Errorf("empty value")
	}
	if index := strings.Index(trimmed, "="); index > 0 {
		name := strings.TrimSpace(trimmed[:index])
		value := strings.TrimSpace(trimmed[index+1:])
		if value != "" && name != "" && !strings.ContainsAny(name, `/\`) {
			if err := validateReferenceName(name); err != nil {
				return referenceSpec{}, err
			}
			return referenceSpec{Name: name, Value: value}, nil
		}
	}
	return referenceSpec{Value: trimmed}, nil
}

// base64ReferenceFlag handles --base64-data separately from paths and URLs. A raw
// base64 value commonly ends in = or ==; parsing NAME=VALUE before recognizing the
// whole payload would mistake that padding for a reference-name separator.
type base64ReferenceFlag struct {
	label string
	specs *[]referenceSpec
}

func (f base64ReferenceFlag) String() string {
	if f.specs == nil || len(*f.specs) == 0 {
		return ""
	}
	parts := make([]string, 0, len(*f.specs))
	for _, spec := range *f.specs {
		if spec.Name == "" {
			parts = append(parts, spec.Value)
		} else {
			parts = append(parts, spec.Name+"="+spec.Value)
		}
	}
	return strings.Join(parts, ",")
}

func (f base64ReferenceFlag) Set(raw string) error {
	spec, err := parseBase64ReferenceSpec(raw)
	if err != nil {
		return fmt.Errorf("%s: %w", f.label, err)
	}
	*f.specs = append(*f.specs, spec)
	return nil
}

func parseBase64ReferenceSpec(raw string) (referenceSpec, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return referenceSpec{}, fmt.Errorf("empty value")
	}
	if validBase64Argument(trimmed) {
		return referenceSpec{Value: trimmed}, nil
	}
	if index := strings.Index(trimmed, "="); index > 0 {
		name := strings.TrimSpace(trimmed[:index])
		value := strings.TrimSpace(trimmed[index+1:])
		if name != "" && value != "" {
			if err := validateReferenceName(name); err != nil {
				return referenceSpec{}, err
			}
			if validBase64Argument(value) {
				return referenceSpec{Name: name, Value: value}, nil
			}
		}
	}
	return referenceSpec{}, fmt.Errorf("value is neither a data: URI nor valid base64")
}

func validateReferenceName(name string) error {
	if strings.ContainsAny(name, "[]") {
		return fmt.Errorf(
			"reference name %q must not contain [ or ]; the prompt already wraps it as [@%s]", name, name)
	}
	return nil
}

func validBase64Argument(value string) bool {
	encoded := value
	if strings.HasPrefix(value, "data:") {
		comma := strings.Index(value, ",")
		if comma < 0 {
			return false
		}
		metadata := value[len("data:"):comma]
		parts := strings.Split(metadata, ";")
		hasBase64 := false
		for _, part := range parts[1:] {
			if strings.EqualFold(strings.TrimSpace(part), "base64") {
				hasBase64 = true
				break
			}
		}
		if !hasBase64 {
			return false
		}
		encoded = value[comma+1:]
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	return err == nil && len(decoded) > 0
}

// unreferencedNames returns the spec names that never appear as [@name] in the prompt.
//
// 名字对不上是这套 API 最容易踩的坑：服务端只按 [@名称] 精确匹配，写错了不会报错，
// 只是那张参考图静默不参与生成。本地先比一遍，提醒但不拦。
func unreferencedNames(prompt string, specs ...[]referenceSpec) []string {
	var missing []string
	for _, group := range specs {
		for _, spec := range group {
			if spec.Name == "" {
				continue
			}
			if !strings.Contains(prompt, "[@"+spec.Name+"]") {
				missing = append(missing, spec.Name)
			}
		}
	}
	return missing
}

// duplicateNames returns names used by more than one reference image.
func duplicateNames(specs ...[]referenceSpec) []string {
	seen := map[string]int{}
	order := []string{}
	for _, group := range specs {
		for _, spec := range group {
			if spec.Name == "" {
				continue
			}
			if seen[spec.Name] == 0 {
				order = append(order, spec.Name)
			}
			seen[spec.Name]++
		}
	}
	var dupes []string
	for _, name := range order {
		if seen[name] > 1 {
			dupes = append(dupes, name)
		}
	}
	return dupes
}

// joinPrompt turns the remaining positional args into one prompt string.
//
// 允许不加引号写多个词（verge task create a neon city），但加了引号的单参数形式才是
// 推荐写法 —— shell 会吃掉多余空格。
func joinPrompt(args []string) string {
	return strings.Join(args, " ")
}
