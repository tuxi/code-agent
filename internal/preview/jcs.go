package preview

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// RevisionSHA256 implements the frozen preview digest:
// sha256(UTF-8(RFC8785-JCS(artifact_without_revision_sha256))).
func RevisionSHA256(artifact Artifact) (string, error) {
	artifact = artifact.Clone()
	artifact.RevisionSHA256 = ""
	data, err := json.Marshal(artifact)
	if err != nil {
		return "", fmt.Errorf("marshal preview artifact: %w", err)
	}
	canonical, err := CanonicalizeJSON(data)
	if err != nil {
		return "", err
	}
	return DigestBytes(canonical), nil
}

// CanonicalizeJSON converts valid JSON into RFC 8785 JSON Canonicalization
// Scheme bytes. It rejects invalid UTF-8, non-finite numbers, and duplicate
// object keys (the latter is already collapsed by encoding/json).
func CanonicalizeJSON(data []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil, fmt.Errorf("decode JSON for JCS: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("decode JSON for JCS: multiple values")
		}
		return nil, fmt.Errorf("decode JSON for JCS: %w", err)
	}
	return canonicalValue(value)
}

func Canonicalize(value any) ([]byte, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("marshal JSON for JCS: %w", err)
	}
	return CanonicalizeJSON(data)
}

func canonicalValue(value any) ([]byte, error) {
	switch v := value.(type) {
	case nil:
		return []byte("null"), nil
	case bool:
		if v {
			return []byte("true"), nil
		}
		return []byte("false"), nil
	case string:
		return appendJSONString(nil, v)
	case json.Number:
		number, err := strconv.ParseFloat(string(v), 64)
		if err != nil {
			return nil, fmt.Errorf("JCS number %q: %w", v, err)
		}
		return []byte(formatJCSNumber(number)), nil
	case float64:
		return []byte(formatJCSNumber(v)), nil
	case float32:
		return []byte(formatJCSNumber(float64(v))), nil
	case int:
		return []byte(formatJCSNumber(float64(v))), nil
	case int64:
		return []byte(formatJCSNumber(float64(v))), nil
	case int32:
		return []byte(formatJCSNumber(float64(v))), nil
	case uint:
		return []byte(formatJCSNumber(float64(v))), nil
	case uint64:
		return []byte(formatJCSNumber(float64(v))), nil
	case []any:
		var out bytes.Buffer
		out.WriteByte('[')
		for i, item := range v {
			if i > 0 {
				out.WriteByte(',')
			}
			encoded, err := canonicalValue(item)
			if err != nil {
				return nil, err
			}
			out.Write(encoded)
		}
		out.WriteByte(']')
		return out.Bytes(), nil
	case map[string]any:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Slice(keys, func(i, j int) bool {
			return lessUTF16(keys[i], keys[j])
		})
		var out bytes.Buffer
		out.WriteByte('{')
		for i, key := range keys {
			if i > 0 {
				out.WriteByte(',')
			}
			encodedKey, err := appendJSONString(nil, key)
			if err != nil {
				return nil, err
			}
			out.Write(encodedKey)
			out.WriteByte(':')
			encodedValue, err := canonicalValue(v[key])
			if err != nil {
				return nil, err
			}
			out.Write(encodedValue)
		}
		out.WriteByte('}')
		return out.Bytes(), nil
	default:
		return nil, fmt.Errorf("unsupported JCS value type %T", value)
	}
}

func appendJSONString(dst []byte, value string) ([]byte, error) {
	if !utf8.ValidString(value) {
		return nil, fmt.Errorf("JCS string is not valid UTF-8")
	}
	dst = append(dst, '"')
	for _, r := range value {
		switch r {
		case '"', '\\':
			dst = append(dst, '\\', byte(r))
		case '\b':
			dst = append(dst, '\\', 'b')
		case '\f':
			dst = append(dst, '\\', 'f')
		case '\n':
			dst = append(dst, '\\', 'n')
		case '\r':
			dst = append(dst, '\\', 'r')
		case '\t':
			dst = append(dst, '\\', 't')
		default:
			if r < 0x20 {
				const hexDigits = "0123456789abcdef"
				dst = append(dst, '\\', 'u', '0', '0', hexDigits[byte(r)>>4], hexDigits[byte(r)&0x0f])
				continue
			}
			dst = utf8.AppendRune(dst, r)
		}
	}
	dst = append(dst, '"')
	return dst, nil
}

func lessUTF16(a, b string) bool {
	ua := utf16.Encode([]rune(a))
	ub := utf16.Encode([]rune(b))
	for i := 0; i < len(ua) && i < len(ub); i++ {
		if ua[i] != ub[i] {
			return ua[i] < ub[i]
		}
	}
	return len(ua) < len(ub)
}

func formatJCSNumber(value float64) string {
	if value == 0 {
		return "0"
	}
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return "null"
	}
	s := strconv.FormatFloat(value, 'g', -1, 64)
	if !strings.ContainsAny(s, "eE") {
		return s
	}
	parts := strings.FieldsFunc(s, func(r rune) bool { return r == 'e' || r == 'E' })
	if len(parts) != 2 {
		return s
	}
	mantissa, exponentText := parts[0], parts[1]
	exponent, err := strconv.Atoi(exponentText)
	if err != nil {
		return s
	}
	if exponent >= -6 && exponent < 21 {
		return exponentToDecimal(mantissa, exponent)
	}
	sign := ""
	if exponent >= 0 {
		sign = "+"
	}
	return mantissa + "e" + sign + strconv.Itoa(exponent)
}

func exponentToDecimal(mantissa string, exponent int) string {
	sign := ""
	if strings.HasPrefix(mantissa, "-") {
		sign = "-"
		mantissa = strings.TrimPrefix(mantissa, "-")
	}
	integerPart := mantissa
	fractionPart := ""
	if before, after, ok := strings.Cut(mantissa, "."); ok {
		integerPart, fractionPart = before, after
	}
	digits := integerPart + fractionPart
	if digits == "" {
		return "0"
	}
	decimalPos := len(integerPart) + exponent
	var out string
	switch {
	case decimalPos <= 0:
		out = "0." + strings.Repeat("0", -decimalPos) + digits
	case decimalPos >= len(digits):
		out = digits + strings.Repeat("0", decimalPos-len(digits))
	default:
		out = digits[:decimalPos] + "." + digits[decimalPos:]
	}
	if strings.Contains(out, ".") {
		out = strings.TrimRight(out, "0")
		out = strings.TrimRight(out, ".")
	}
	return sign + out
}
