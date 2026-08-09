package tool

import (
	"errors"
	"testing"
	"time"
)

func TestDecodeBashInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		raw         string
		command     string
		timeout     time.Duration
		hasTimeout  bool
		wantFailure bool
	}{
		{name: "empty command", raw: "{\"command\":\"\"}"},
		{name: "escaped slash", raw: "{\"command\":\"cat \\/tmp\\/x\"}", command: "cat /tmp/x"},
		{name: "surrogate pair", raw: "{\"command\":\"echo \\ud83d\\ude00\"}", command: "echo 😀"},
		{
			name:       "maximum timeout",
			raw:        "{\"command\":\"sleep\",\"timeout\":2147483.647}",
			command:    "sleep",
			timeout:    MaxBashTimeout,
			hasTimeout: true,
		},
		{
			name:       "fractional timeout rounds up to one nanosecond",
			raw:        "{\"command\":\"sleep\",\"timeout\":0.0000000001}",
			command:    "sleep",
			timeout:    time.Nanosecond,
			hasTimeout: true,
		},
		{name: "missing command", raw: "{\"timeout\":1}", wantFailure: true},
		{name: "null command", raw: "{\"command\":null}", wantFailure: true},
		{name: "unknown field is allowed", raw: "{\"command\":\"x\",\"cwd\":\"/\"}", command: "x"},
		{name: "duplicate field uses last value", raw: "{\"command\":\"x\",\"command\":\"y\"}", command: "y"},
		{name: "zero timeout", raw: "{\"command\":\"x\",\"timeout\":0}", wantFailure: true},
		{name: "negative timeout", raw: "{\"command\":\"x\",\"timeout\":-1}", wantFailure: true},
		{name: "timeout string", raw: "{\"command\":\"x\",\"timeout\":\"1\"}", wantFailure: true},
		{name: "timeout too large", raw: "{\"command\":\"x\",\"timeout\":2147483.6470000001}", wantFailure: true},
		{name: "non-finite exponent", raw: "{\"command\":\"x\",\"timeout\":1e999999}", wantFailure: true},
		{name: "lone surrogate is replacement decoded", raw: "{\"command\":\"\\ud800\"}", command: "�"},
		{name: "NUL command", raw: "{\"command\":\"\\u0000\"}", wantFailure: true},
		{name: "array", raw: "[]", wantFailure: true},
		{name: "trailing JSON", raw: "{\"command\":\"x\"} {}", wantFailure: true},
		{name: "invalid UTF-8", raw: string([]byte{'{', '"', 'c', '"', ':', '"', 0xff, '"', '}'}), wantFailure: true},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			input, err := DecodeBashInput([]byte(test.raw))
			if test.wantFailure {
				if !errors.Is(err, ErrInvalidBashInput) {
					t.Fatalf("DecodeBashInput() error = %v, want ErrInvalidBashInput", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("DecodeBashInput() error = %v", err)
			}
			if input.Command() != test.command {
				t.Fatalf("Command() = %q, want %q", input.Command(), test.command)
			}
			timeout, ok := input.Timeout()
			if ok != test.hasTimeout || timeout != test.timeout {
				t.Fatalf("Timeout() = (%s, %v), want (%s, %v)", timeout, ok, test.timeout, test.hasTimeout)
			}
		})
	}
}

func TestNewBashInputCopiesTimeout(t *testing.T) {
	t.Parallel()
	timeout := time.Second
	input, err := NewBashInput("echo ok", &timeout)
	if err != nil {
		t.Fatal(err)
	}
	timeout = -time.Second
	got, ok := input.Timeout()
	if !ok || got != time.Second {
		t.Fatalf("Timeout() = (%s, %v), want (1s, true)", got, ok)
	}
}

func FuzzDecodeBashInput(f *testing.F) {
	for _, seed := range [][]byte{
		[]byte("{\"command\":\"echo ok\"}"),
		[]byte("{\"command\":\"\",\"timeout\":0.5}"),
		[]byte("{\"command\":\"\\ud83d\\ude00\"}"),
		{0xff},
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		input, err := DecodeBashInput(raw)
		if err == nil {
			if err := input.validate(); err != nil {
				t.Fatalf("successful decode produced invalid input: %v", err)
			}
		}
	})
}
