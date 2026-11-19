package tool

import (
	"context"
	"os"
	"strings"
	"testing"
)

// A smaller version of inserting a model before several similarly shaped models.
// Expected details come from pi's generateUnifiedPatch and generateDiffString
// (diff@8.0.4); applying an unnecessarily noisy patch would also round-trip.
func TestEditInsertBeforeSimilarModels(t *testing.T) {
	const before = `[
  {
    "id": "luna",
    "reasoning": true,
    "cost": 1
  },
  {
    "id": "sol",
    "reasoning": true,
    "cost": 2
  }
]
`
	const anchor = `  {
    "id": "luna",`
	const inserted = `  {
    "id": "astra",
    "reasoning": true,
    "cost": 10
  },
`
	const wantPatch = `--- models.json
+++ models.json
@@ -1,6 +1,11 @@
 [
   {
+    "id": "astra",
+    "reasoning": true,
+    "cost": 10
+  },
+  {
     "id": "luna",
     "reasoning": true,
     "cost": 1
   },
`
	const wantDiff = `  1 [
  2   {
+ 3     "id": "astra",
+ 4     "reasoning": true,
+ 5     "cost": 10
+ 6   },
+ 7   {
  3     "id": "luna",
  4     "reasoning": true,
  5     "cost": 1
  6   },
    ...`
	suite := newTestSuite(t)
	path := writeTestFile(t, suite.WorkingDir(), "models.json", before)
	result, err := suite.Edit(context.Background(), EditInput{Path: "models.json", Edits: []Edit{{OldText: anchor, NewText: inserted + anchor}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := result.Details["patch"]; got != wantPatch {
		t.Fatalf("patch:\n%s\nwant:\n%s", got, wantPatch)
	}
	if got := result.Details["diff"]; got != wantDiff || result.Details["firstChangedLine"] != 3 {
		t.Fatalf("display diff (first change %v):\n%s", result.Details["firstChangedLine"], got)
	}
	want := strings.Replace(before, anchor, inserted+anchor, 1)
	if data, err := os.ReadFile(path); err != nil || string(data) != want {
		t.Fatalf("edited file = %q, %v", data, err)
	}
}

func TestEditDiffInsertionAndReplacementAroundRepeatedLines(t *testing.T) {
	// With different lines at both ends, Myers must align the repeated middle
	// and preserve the missing final newline on both sides of the replacement.
	const before = "luna\nshared\n1\nsol\nshared\n2"
	const after = "astra\nshared\n10\nluna\nshared\n1\nsol\nshared\n3"
	const want = `--- models
+++ models
@@ -1,6 +1,9 @@
+astra
+shared
+10
 luna
 shared
 1
 sol
 shared
-2
\ No newline at end of file
+3
\ No newline at end of file
`
	_, patch, first := makeEditDiff("models", before, after)
	if patch != want || first != 1 {
		t.Fatalf("first change %d, patch:\n%s\nwant:\n%s", first, patch, want)
	}
}

func TestEditDiffDoesNotPinAmbiguousSuffix(t *testing.T) {
	// Pinning the last A as context would display a replacement of X with A.
	// Like jsdiff, Myers matches the earlier A, then inserts the final A.
	display, _, _ := makeEditDiff("sample", "X\nA\n", "A\nA\n")
	if want := "-1 X\n 2 A\n+2 A"; display != want {
		t.Fatalf("display = %q, want %q", display, want)
	}
}

func BenchmarkEditDiffLargeFile(b *testing.B) {
	const old = "luna\nshared\n1\nsol\nshared\n2\n"
	edge := strings.Repeat("unchanged line\n", 50000)
	for _, tc := range []struct{ name, before, after string }{
		{"insert", old, strings.Repeat("added line\n", 4000) + old},
		{"mixed", old, "astra\nshared\n10\nluna\nshared\n1\nsol\nshared\n3\n"},
		{"replace", strings.Repeat("old line\n", 4000), strings.Repeat("new line\n", 4000)},
	} {
		before, after := edge+tc.before+edge, edge+tc.after+edge
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				makeEditDiff("large.txt", before, after)
			}
		})
	}
}
