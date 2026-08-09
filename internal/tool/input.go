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
	if bytes.IndexByte([]byte(i.command), 0) >= 0 {
		return fmt.Errorf("%w: command contains NUL", ErrInvalidBashInput)
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

// DecodeBashInput mirrors JSON.parse plus pi's TypeBox Object contract:
// duplicate fields use their final value and undeclared properties are
// ignored. Timeout conversion retains sub-nanosecond positive durations.
func DecodeBashInput(raw []byte) (BashInput, error) {
	var values map[string]json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return BashInput{}, fmt.Errorf("%w: %v", ErrInvalidBashInput, err)
	}
	if values == nil {
		return BashInput{}, fmt.Errorf("%w: arguments must be an object", ErrInvalidBashInput)
	}
	commandRaw, hasCommand := values["command"]
	if !hasCommand {
		return BashInput{}, fmt.Errorf("%w: command is required", ErrInvalidBashInput)
	}
	command, err := decodeStrictJSONString(commandRaw)
	if err != nil {
		return BashInput{}, fmt.Errorf("%w: command: %v", ErrInvalidBashInput, err)
	}
	var timeout time.Duration
	timeoutRaw, hasTimeout := values["timeout"]
	if hasTimeout {
		timeout, err = decodeTimeoutSeconds(timeoutRaw)
		if err != nil {
			return BashInput{}, fmt.Errorf("%w: timeout: %v", ErrInvalidBashInput, err)
		}
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
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", errors.New("must be a string")
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
