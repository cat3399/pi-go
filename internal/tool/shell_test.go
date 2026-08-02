package tool

import "testing"

func TestLegacyWSLBashPath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		path string
		want bool
	}{
		{path: "C:/Windows/System32/bash.exe", want: true},
		{path: "d:/WINDOWS/Sysnative/bash.exe", want: true},
		{path: "C:/Git/bin/bash.exe", want: false},
		{path: "/Windows/System32/bash.exe", want: false},
		{path: "prefix/Windows/System32/bash.exe", want: false},
	}
	for _, test := range tests {
		if got := isLegacyWSLBashPath(test.path); got != test.want {
			t.Errorf("isLegacyWSLBashPath(%q) = %v, want %v", test.path, got, test.want)
		}
	}
}
