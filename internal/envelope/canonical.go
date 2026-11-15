package envelope

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
)

// Canonicalize 输出签名的规范字节：对象键按字典序、无多余空白、
// 数字只允许整数。两端必须用同一套实现，否则会出现查不到原因的验签失败。
func Canonicalize(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := writeCanonical(&buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeCanonical(b *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if x {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case string:
		enc, err := json.Marshal(x)
		if err != nil {
			return err
		}
		b.Write(enc)
	case json.Number:
		if !isInteger(x.String()) {
			return fmt.Errorf("规范化 JSON 只允许整数，得到 %q", x.String())
		}
		b.WriteString(x.String())
	case int:
		b.WriteString(strconv.Itoa(x))
	case int64:
		b.WriteString(strconv.FormatInt(x, 10))
	case uint64:
		b.WriteString(strconv.FormatUint(x, 10))
	case []any:
		b.WriteByte('[')
		for i, item := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := writeCanonical(b, item); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			kb, err := json.Marshal(k)
			if err != nil {
				return err
			}
			b.Write(kb)
			b.WriteByte(':')
			if err := writeCanonical(b, x[k]); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	case json.RawMessage:
		var decoded any
		if err := decode([]byte(x), &decoded); err != nil {
			return err
		}
		return writeCanonical(b, decoded)
	default:
		// 其他类型（结构体、切片等）先过一遍 JSON，再按通用规则处理。
		raw, err := json.Marshal(x)
		if err != nil {
			return err
		}
		var decoded any
		if err := decode(raw, &decoded); err != nil {
			return err
		}
		return writeCanonical(b, decoded)
	}
	return nil
}

// decode 用 UseNumber 解析，避免浮点把整数变成 1e+06 这类形式。
func decode(raw []byte, out any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("解析 JSON: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return fmt.Errorf("JSON 之后还有多余内容")
	}
	return nil
}

func isInteger(s string) bool {
	if s == "" {
		return false
	}
	for i, c := range s {
		if i == 0 && (c == '-' || c == '+') {
			continue
		}
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
