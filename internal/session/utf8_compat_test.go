package session

import "testing"

func TestReplaceInvalidUTF8LikeNodeUsesMaximalSubparts(t *testing.T) {
	for _, test := range []struct {
		name string
		hex  []byte
		want string
	}{
		{name: "valid prefix then ASCII", hex: []byte{0xe1, 0x80, 'A'}, want: "\ufffdA"},
		{name: "two invalid starters", hex: []byte{0xff, 0xff}, want: "\ufffd\ufffd"},
		{name: "truncated three byte sequence", hex: []byte{0xe1, 0x80}, want: "\ufffd"},
		{name: "truncated four byte sequence", hex: []byte{0xf0, 0x90, 0x80}, want: "\ufffd"},
		{name: "overlong continuations are reprocessed", hex: []byte{0xe0, 0x80, 0x80}, want: "\ufffd\ufffd\ufffd"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, replaced := replaceInvalidUTF8LikeNode(test.hex)
			if !replaced || string(got) != test.want {
				t.Fatalf("replacement = %q, %t; want %q, true", got, replaced, test.want)
			}
		})
	}
}
