package tool

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"strconv"
	"time"
	"unicode/utf8"
)

const MaxBashTimeout = 2_147_483_647 * time.Millisecond

var ErrInvalidBashInput = errors.New("invalid bash input")

// BashInput is the validated input for one built-in Bash invocation.
// Timeout is represented as a duration internally; JSON adapters use seconds.
type BashInput struct {
	command    string
	timeout    time.Duration
	hasTimeout bool
}

func NewBashInput(command string, timeout *time.Duration) (BashInput, error) {
	input := BashInput{command: command}
	if timeout != nil {
		input.timeout = *timeout
		input.hasTimeout = true
	}
	if err := input.validate(); err != nil {
		return BashInput{}, err
	}
	return input, nil
}

func (i BashInput) validate() error {
	if !utf8.ValidString(i.command) {
		return fmt.Errorf("%w: command must be valid UTF-8", ErrInvalidBashInput)
	}
	if i.hasTimeout && (i.timeout <= 0 || i.timeout > MaxBashTimeout) {
		return fmt.Errorf(
			"%w: timeout must be greater than zero and no more than %s",
			ErrInvalidBashInput,
			formatSeconds(MaxBashTimeout),
		)
	}
	if !i.hasTimeout && i.timeout != 0 {
		return fmt.Errorf("%w: invalid timeout presence state", ErrInvalidBashInput)
	}
	return nil
}

func (i BashInput) Command() string {
	return i.command
}

func (i BashInput) Timeout() (time.Duration, bool) {
	return i.timeout, i.hasTimeout
}

// DecodeBashInput validates the provider-facing JSON object without losing
// duplicate fields, unknown fields, or sub-nanosecond positive timeouts.
func DecodeBashInput(raw []byte) (BashInput, error) {
	if !utf8.Valid(raw) {
		return BashInput{}, fmt.Errorf("%w: arguments must be valid UTF-8 JSON", ErrInvalidBashInput)
	}

	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil {
		return BashInput{}, fmt.Errorf("%w: %v", ErrInvalidBashInput, err)
	}
	delimiter, ok := token.(json.Delim)
	if !ok || delimiter != '{' {
		return BashInput{}, fmt.Errorf("%w: arguments must be an object", ErrInvalidBashInput)
	}

	seen := make(map[string]struct{}, 2)
	var command string
	var timeout time.Duration
	var hasCommand, hasTimeout bool
	for decoder.More() {
		token, err = decoder.Token()
		if err != nil {
			return BashInput{}, fmt.Errorf("%w: %v", ErrInvalidBashInput, err)
		}
		name, ok := token.(string)
		if !ok {
			return BashInput{}, fmt.Errorf("%w: object key must be a string", ErrInvalidBashInput)
		}
		if _, duplicate := seen[name]; duplicate {
			return BashInput{}, fmt.Errorf("%w: duplicate field %q", ErrInvalidBashInput, name)
		}
		seen[name] = struct{}{}

		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return BashInput{}, fmt.Errorf("%w: field %q: %v", ErrInvalidBashInput, name, err)
		}

		switch name {
		case "command":
			command, err = decodeStrictJSONString(value)
			if err != nil {
				return BashInput{}, fmt.Errorf("%w: command: %v", ErrInvalidBashInput, err)
			}
			hasCommand = true
		case "timeout":
			timeout, err = decodeTimeoutSeconds(value)
			if err != nil {
				return BashInput{}, fmt.Errorf("%w: timeout: %v", ErrInvalidBashInput, err)
			}
			hasTimeout = true
		default:
			return BashInput{}, fmt.Errorf("%w: unknown field %q", ErrInvalidBashInput, name)
		}
	}

	token, err = decoder.Token()
	if err != nil {
		return BashInput{}, fmt.Errorf("%w: %v", ErrInvalidBashInput, err)
	}
	delimiter, ok = token.(json.Delim)
	if !ok || delimiter != '}' {
		return BashInput{}, fmt.Errorf("%w: malformed object", ErrInvalidBashInput)
	}
	if token, err = decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return BashInput{}, fmt.Errorf("%w: unexpected trailing JSON token %v", ErrInvalidBashInput, token)
		}
		return BashInput{}, fmt.Errorf("%w: trailing JSON: %v", ErrInvalidBashInput, err)
	}
	if !hasCommand {
		return BashInput{}, fmt.Errorf("%w: command is required", ErrInvalidBashInput)
	}

	input := BashInput{
		command:    command,
		timeout:    timeout,
		hasTimeout: hasTimeout,
	}
	if err := input.validate(); err != nil {
		return BashInput{}, err
	}
	return input, nil
}

func decodeStrictJSONString(raw []byte) (string, error) {
	if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return "", errors.New("must be a string")
	}
	if err := validateJSONSurrogates(raw); err != nil {
		return "", errors.New("must be a valid Unicode JSON string")
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", errors.New("must be a valid Unicode JSON string")
	}
	if !utf8.ValidString(value) {
		return "", errors.New("must be valid UTF-8")
	}
	return value, nil
}

func decodeTimeoutSeconds(raw []byte) (time.Duration, error) {
	if len(raw) == 0 || raw[0] != '-' && (raw[0] < '0' || raw[0] > '9') {
		return 0, errors.New("must be a finite number of seconds")
	}
	var number json.Number
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&number); err != nil {
		return 0, errors.New("must be a finite number of seconds")
	}
	if token, err := decoder.Token(); !errors.Is(err, io.EOF) {
		_ = token
		return 0, errors.New("must be one finite JSON number")
	}

	seconds, ok := new(big.Rat).SetString(number.String())
	if !ok || seconds.Sign() <= 0 {
		return 0, errors.New("must be a finite number greater than zero")
	}
	maxSeconds := new(big.Rat).SetFrac(
		big.NewInt(int64(MaxBashTimeout/time.Millisecond)),
		big.NewInt(1000),
	)
	if seconds.Cmp(maxSeconds) > 0 {
		return 0, fmt.Errorf("maximum is %s seconds", formatSeconds(MaxBashTimeout))
	}

	nanoseconds := new(big.Rat).Mul(seconds, big.NewRat(int64(time.Second), 1))
	quotient := new(big.Int)
	remainder := new(big.Int)
	quotient.QuoRem(nanoseconds.Num(), nanoseconds.Denom(), remainder)
	if remainder.Sign() != 0 {
		quotient.Add(quotient, big.NewInt(1))
	}
	if !quotient.IsInt64() {
		return 0, errors.New("cannot be represented as a duration")
	}
	duration := time.Duration(quotient.Int64())
	if duration <= 0 {
		duration = time.Nanosecond
	}
	return duration, nil
}

func validateJSONSurrogates(raw []byte) error {
	for index := 1; index < len(raw)-1; index++ {
		if raw[index] != '\\' {
			continue
		}
		index++
		if index >= len(raw)-1 || raw[index] != 'u' {
			continue
		}
		if index+4 >= len(raw) {
			return errors.New("short Unicode escape")
		}
		code, err := strconv.ParseUint(string(raw[index+1:index+5]), 16, 16)
		if err != nil {
			return err
		}
		index += 4
		switch {
		case code >= 0xd800 && code <= 0xdbff:
			if index+6 >= len(raw) ||
				raw[index+1] != '\\' ||
				raw[index+2] != 'u' {
				return errors.New("unpaired high surrogate")
			}
			low, err := strconv.ParseUint(string(raw[index+3:index+7]), 16, 16)
			if err != nil || low < 0xdc00 || low > 0xdfff {
				return errors.New("unpaired high surrogate")
			}
			index += 6
		case code >= 0xdc00 && code <= 0xdfff:
			return errors.New("unpaired low surrogate")
		}
	}
	return nil
}

func formatSeconds(duration time.Duration) string {
	return strconv.FormatFloat(duration.Seconds(), 'f', -1, 64)
}
