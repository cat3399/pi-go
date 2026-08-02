package session

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"reflect"
	"strings"
)

type semanticJSONNumber string

func semanticJSONEqual(left, right []byte) (bool, error) {
	leftValue, err := decodeSemanticJSON(left)
	if err != nil {
		return false, err
	}
	rightValue, err := decodeSemanticJSON(right)
	if err != nil {
		return false, err
	}
	return reflect.DeepEqual(leftValue, rightValue), nil
}

func decodeSemanticJSON(raw []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("multiple JSON values")
		}
		return nil, err
	}
	return normalizeSemanticJSON(value)
}

func normalizeSemanticJSON(value any) (any, error) {
	switch value := value.(type) {
	case nil, bool, string:
		return value, nil
	case json.Number:
		return canonicalJSONNumber(value.String())
	case []any:
		result := make([]any, len(value))
		for index, item := range value {
			normalized, err := normalizeSemanticJSON(item)
			if err != nil {
				return nil, err
			}
			result[index] = normalized
		}
		return result, nil
	case map[string]any:
		result := make(map[string]any, len(value))
		for key, item := range value {
			normalized, err := normalizeSemanticJSON(item)
			if err != nil {
				return nil, err
			}
			result[key] = normalized
		}
		return result, nil
	default:
		return nil, fmt.Errorf("unsupported decoded JSON value %T", value)
	}
}

// canonicalJSONNumber compares JSON numbers as exact decimal values without
// converting through float64 or expanding large exponents. The result is a
// normalized coefficient and base-10 exponent.
func canonicalJSONNumber(value string) (semanticJSONNumber, error) {
	negative := strings.HasPrefix(value, "-")
	if negative {
		value = value[1:]
	}

	exponentText := ""
	if index := strings.IndexAny(value, "eE"); index >= 0 {
		exponentText = value[index+1:]
		value = value[:index]
	}
	var exponent big.Int
	if exponentText != "" {
		exponentNegative := strings.HasPrefix(exponentText, "-")
		if exponentNegative || strings.HasPrefix(exponentText, "+") {
			exponentText = exponentText[1:]
		}
		if _, ok := exponent.SetString(exponentText, 10); !ok {
			return "", fmt.Errorf("invalid JSON number exponent")
		}
		if exponentNegative {
			exponent.Neg(&exponent)
		}
	}

	fractionDigits := 0
	if index := strings.IndexByte(value, '.'); index >= 0 {
		fractionDigits = len(value) - index - 1
		value = value[:index] + value[index+1:]
	}
	value = strings.TrimLeft(value, "0")
	if value == "" {
		return semanticJSONNumber("0e0"), nil
	}
	trimmed := strings.TrimRight(value, "0")
	trailingZeros := len(value) - len(trimmed)
	value = trimmed
	exponent.Sub(&exponent, big.NewInt(int64(fractionDigits)))
	exponent.Add(&exponent, big.NewInt(int64(trailingZeros)))
	if negative {
		value = "-" + value
	}
	return semanticJSONNumber(value + "e" + exponent.String()), nil
}
