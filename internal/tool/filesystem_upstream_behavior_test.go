package tool

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/llm"
	"golang.org/x/image/bmp"
)

func TestReadDetectsImagesByContentAndReturnsRichBlocks(t *testing.T) {
	suite := newTestSuite(t)
	var encoded bytes.Buffer
	pixel := image.NewNRGBA(image.Rect(0, 0, 1, 1))
	pixel.SetNRGBA(0, 0, color.NRGBA{R: 0x12, G: 0x34, B: 0x56, A: 0xff})
	if err := png.Encode(&encoded, pixel); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(suite.WorkingDir(), "not-an-image.dat")
	if err := os.WriteFile(path, encoded.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := suite.Read(context.Background(), ReadInput{Path: "not-an-image.dat"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Content) != 2 || result.Text != "Read image file [image/png]" {
		t.Fatalf("rich image result = %#v", result)
	}
	textBlock, ok := result.Content[0].(llm.TextBlock)
	if !ok || textBlock.Text() != result.Text {
		t.Fatalf("text block = %#v", result.Content[0])
	}
	imageBlock, ok := result.Content[1].(llm.ImageBlock)
	if !ok || imageBlock.MediaType() != "image/png" || !bytes.Equal(imageBlock.Data(), encoded.Bytes()) {
		t.Fatalf("image block = %#v", result.Content[1])
	}
}

func TestReadImageBranchIsIndependentOfDecodedTextLimit(t *testing.T) {
	root := t.TempDir()
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewNRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, root, "pixel.png", string(encoded.Bytes()))
	suite, err := NewFilesystemSuite(FilesystemOptions{WorkingDir: root, MaxTextUnits: 1})
	if err != nil {
		t.Fatal(err)
	}
	result, err := suite.Read(context.Background(), ReadInput{Path: "pixel.png"})
	if err != nil || len(result.Content) != 2 {
		t.Fatalf("image under one-unit text limit = %#v, %v", result, err)
	}
}

func TestReadRejectsCompressedImageDimensionBombBeforeDecode(t *testing.T) {
	root := t.TempDir()
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewNRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	bomb := append([]byte(nil), encoded.Bytes()...)
	binary.BigEndian.PutUint32(bomb[16:20], 100_000)
	binary.BigEndian.PutUint32(bomb[20:24], 100_000)
	binary.BigEndian.PutUint32(bomb[29:33], crc32.ChecksumIEEE(bomb[12:29]))
	if err := os.WriteFile(filepath.Join(root, "bomb.png"), bomb, 0o600); err != nil {
		t.Fatal(err)
	}
	suite, err := NewFilesystemSuite(FilesystemOptions{WorkingDir: root})
	if err != nil {
		t.Fatal(err)
	}
	result, err := suite.Read(context.Background(), ReadInput{Path: "bomb.png"})
	if err != nil || result.Content != nil || !strings.Contains(result.Text, "safe in-process decode limit") {
		t.Fatalf("dimension bomb result = %#v, %v", result, err)
	}
}

func TestReadImageOutputLimitAlsoAppliesWhenAutoResizeIsDisabled(t *testing.T) {
	root := t.TempDir()
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewNRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, root, "pixel.png", string(encoded.Bytes()))
	autoResize := false
	suite, err := NewFilesystemSuite(FilesystemOptions{WorkingDir: root, AutoResizeImages: &autoResize, MaxImageBytes: 32})
	if err != nil {
		t.Fatal(err)
	}
	result, err := suite.Read(context.Background(), ReadInput{Path: "pixel.png"})
	if err != nil || result.Content != nil || !strings.Contains(result.Text, "encoded image exceeds") {
		t.Fatalf("disabled-resize output bound = %#v, %v", result, err)
	}
}

type cancelImageStageContext struct {
	checks   int
	cancelAt int
}

func (*cancelImageStageContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (*cancelImageStageContext) Done() <-chan struct{}       { return nil }
func (c *cancelImageStageContext) Err() error {
	c.checks++
	if c.checks >= c.cancelAt {
		return context.Canceled
	}
	return nil
}
func (*cancelImageStageContext) Value(any) any { return nil }

func TestImageDecodeAndResizeStagesRecheckCancellation(t *testing.T) {
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewNRGBA(image.Rect(0, 0, 3, 3))); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name     string
		cancelAt int
	}{
		{name: "after-decode", cancelAt: 2},
		{name: "after-resize", cancelAt: 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "image.png")
			if err := os.WriteFile(path, encoded.Bytes(), 0o600); err != nil {
				t.Fatal(err)
			}
			file, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			ctx := &cancelImageStageContext{cancelAt: test.cancelAt}
			_, err = processReadImage(ctx, file, int64(encoded.Len()), "image/png", true, DefaultFilesystemMaxImagePixels, 16)
			if !errors.Is(err, ErrOperationCancelled) || !errors.Is(err, context.Canceled) {
				t.Fatalf("stage cancellation = %v", err)
			}
		})
	}
}

func TestReadBMPConversionAndConversionFailureMessage(t *testing.T) {
	root := t.TempDir()
	autoResize := false
	suite, err := NewFilesystemSuite(FilesystemOptions{WorkingDir: root, AutoResizeImages: &autoResize})
	if err != nil {
		t.Fatal(err)
	}
	var encoded bytes.Buffer
	source := image.NewNRGBA(image.Rect(0, 0, 2, 3))
	source.Set(0, 0, color.NRGBA{R: 0xff, A: 0xff})
	if err := bmp.Encode(&encoded, source); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sample.bin"), encoded.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := suite.Read(context.Background(), ReadInput{Path: "sample.bin"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Text, "[Image converted from image/bmp to image/png.]") || len(result.Content) != 2 {
		t.Fatalf("BMP conversion result = %#v", result)
	}
	imageBlock, ok := result.Content[1].(llm.ImageBlock)
	if !ok || imageBlock.MediaType() != "image/png" || !bytes.HasPrefix(imageBlock.Data(), pngSignature) {
		t.Fatalf("converted image block = %#v", result.Content[1])
	}

	malformed := make([]byte, 30)
	copy(malformed, "BM")
	binary.LittleEndian.PutUint32(malformed[2:6], 100)
	binary.LittleEndian.PutUint32(malformed[10:14], 54)
	binary.LittleEndian.PutUint32(malformed[14:18], 40)
	binary.LittleEndian.PutUint16(malformed[26:28], 1)
	binary.LittleEndian.PutUint16(malformed[28:30], 24)
	if detectSupportedImageMIME(malformed) != "image/bmp" {
		t.Fatal("malformed conversion fixture no longer reaches the BMP conversion path")
	}
	if err := os.WriteFile(filepath.Join(root, "broken.bmp"), malformed, 0o644); err != nil {
		t.Fatal(err)
	}
	result, err = suite.Read(context.Background(), ReadInput{Path: "broken.bmp"})
	if err != nil || result.Content != nil || !strings.Contains(result.Text, "could not be converted") {
		t.Fatalf("BMP conversion failure = %#v, %v", result, err)
	}
}

func TestImageSniffingAndEXIFOrientationMatchesDocumentedUpstreamCases(t *testing.T) {
	if got := detectSupportedImageMIME([]byte{0xff, 0xd8, 0xff}); got != "image/jpeg" {
		t.Fatalf("three-byte JPEG = %q", got)
	}
	if got := detectSupportedImageMIME([]byte{0xff, 0xd8, 0xff, 0xf7}); got != "" {
		t.Fatalf("JPEG XL marker = %q", got)
	}

	var plainPNG bytes.Buffer
	if err := png.Encode(&plainPNG, image.NewNRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	pngBytes := plainPNG.Bytes()
	animated := append([]byte(nil), pngBytes[:33]...)
	animated = append(animated, 0, 0, 0, 8, 'a', 'c', 'T', 'L', 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0)
	animated = append(animated, pngBytes[33:]...)
	if got := detectSupportedImageMIME(animated); got != "" {
		t.Fatalf("APNG = %q", got)
	}

	tiff := exifTIFFForTest(8)
	baseWebP, err := base64.StdEncoding.DecodeString("UklGRiYAAABXRUJQVlA4TBoAAAAvAYAAAA/wzff5/+b7/McDFAIIgIImov/BAA==")
	if err != nil {
		t.Fatal(err)
	}
	webp := append([]byte(nil), baseWebP...)
	webp = appendWebPChunkForTest(webp, "JUNK", bytes.Repeat([]byte{0xa5}, 5001))
	payload := append([]byte{'E', 'x', 'i', 'f', 0, 0}, tiff...)
	webp = appendWebPChunkForTest(webp, "EXIF", payload)
	binary.LittleEndian.PutUint32(webp[4:8], uint32(len(webp)-8))
	if len(webp) <= imageSniffBytes {
		t.Fatal("WebP EXIF fixture is not delayed beyond the MIME prefix")
	}
	if got := webpEXIFOrientation(webp); got != 8 {
		t.Fatalf("WebP EXIF orientation = %d", got)
	}
	webpPath := filepath.Join(t.TempDir(), "delayed.webp")
	if err := os.WriteFile(webpPath, webp, 0o600); err != nil {
		t.Fatal(err)
	}
	webpFile, err := os.Open(webpPath)
	if err != nil {
		t.Fatal(err)
	}
	gotWebPOrientation, scanErr := imageEXIFOrientationFromFile(context.Background(), webpFile, int64(len(webp)), "image/webp")
	if closeErr := webpFile.Close(); scanErr != nil || closeErr != nil || gotWebPOrientation != 8 {
		t.Fatalf("streamed delayed WebP orientation = %d, scan=%v close=%v", gotWebPOrientation, scanErr, closeErr)
	}
	webpRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(webpRoot, "delayed.webp"), webp, 0o600); err != nil {
		t.Fatal(err)
	}
	webpSuite, err := NewFilesystemSuite(FilesystemOptions{WorkingDir: webpRoot, MaxImageBytes: 1000})
	if err != nil {
		t.Fatal(err)
	}
	webpResult, err := webpSuite.Read(context.Background(), ReadInput{Path: "delayed.webp"})
	if err != nil || !strings.Contains(webpResult.Text, "original 3x2, displayed at 3x2") {
		t.Fatalf("delayed WebP resize note = %q, %v", webpResult.Text, err)
	}
	webpImage, ok := webpResult.Content[1].(llm.ImageBlock)
	if !ok {
		t.Fatalf("delayed WebP image block = %#v", webpResult.Content)
	}
	decodedWebP, _, err := image.Decode(bytes.NewReader(webpImage.Data()))
	if err != nil || decodedWebP.Bounds().Dx() != 3 || decodedWebP.Bounds().Dy() != 2 {
		t.Fatalf("delayed WebP oriented bounds = %v, %v", decodedWebP.Bounds(), err)
	}

	root := t.TempDir()
	suite, err := NewFilesystemSuite(FilesystemOptions{WorkingDir: root})
	if err != nil {
		t.Fatal(err)
	}
	wide := image.NewNRGBA(image.Rect(0, 0, 2100, 100))
	for y := 0; y < 100; y++ {
		for x := 0; x < 2100; x++ {
			wide.SetNRGBA(x, y, color.NRGBA{R: uint8(x), G: uint8(y), B: 0x7f, A: 0xff})
		}
	}
	var jpegBytes bytes.Buffer
	if err := jpeg.Encode(&jpegBytes, wide, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatal(err)
	}
	oriented := insertJPEGSegmentForTest(
		insertJPEGEXIFForTest(jpegBytes.Bytes(), 6),
		0xe2,
		bytes.Repeat([]byte{0x5a}, 5000),
	)
	if bytes.Contains(oriented[:imageSniffBytes], []byte{'E', 'x', 'i', 'f', 0, 0}) {
		t.Fatal("JPEG APP1 fixture is not delayed beyond the MIME prefix")
	}
	if err := os.WriteFile(filepath.Join(root, "oriented.jpg"), oriented, 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := suite.Read(context.Background(), ReadInput{Path: "oriented.jpg"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Text, "original 100x2100, displayed at 95x2000") {
		t.Fatalf("orientation resize note = %q", result.Text)
	}
	imageBlock, ok := result.Content[1].(llm.ImageBlock)
	if !ok {
		t.Fatalf("resized image block = %#v", result.Content)
	}
	decoded, _, err := image.Decode(bytes.NewReader(imageBlock.Data()))
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Bounds().Dx() != 95 || decoded.Bounds().Dy() != 2000 {
		t.Fatalf("resized orientation bounds = %v", decoded.Bounds())
	}
}

func exifTIFFForTest(orientation uint16) []byte {
	tiff := make([]byte, 26)
	copy(tiff, "II")
	binary.LittleEndian.PutUint16(tiff[2:4], 42)
	binary.LittleEndian.PutUint32(tiff[4:8], 8)
	binary.LittleEndian.PutUint16(tiff[8:10], 1)
	binary.LittleEndian.PutUint16(tiff[10:12], 0x0112)
	binary.LittleEndian.PutUint16(tiff[12:14], 3)
	binary.LittleEndian.PutUint32(tiff[14:18], 1)
	binary.LittleEndian.PutUint16(tiff[18:20], orientation)
	return tiff
}

func insertJPEGEXIFForTest(jpegBytes []byte, orientation uint16) []byte {
	payload := append([]byte{'E', 'x', 'i', 'f', 0, 0}, exifTIFFForTest(orientation)...)
	return insertJPEGSegmentForTest(jpegBytes, 0xe1, payload)
}

func insertJPEGSegmentForTest(jpegBytes []byte, marker byte, payload []byte) []byte {
	segment := []byte{0xff, marker, 0, 0}
	binary.BigEndian.PutUint16(segment[2:4], uint16(len(payload)+2))
	result := append([]byte(nil), jpegBytes[:2]...)
	result = append(result, segment...)
	result = append(result, payload...)
	return append(result, jpegBytes[2:]...)
}

func appendWebPChunkForTest(webp []byte, name string, payload []byte) []byte {
	chunk := make([]byte, 8)
	copy(chunk, name)
	binary.LittleEndian.PutUint32(chunk[4:], uint32(len(payload)))
	webp = append(webp, chunk...)
	webp = append(webp, payload...)
	if len(payload)%2 != 0 {
		webp = append(webp, 0)
	}
	return webp
}

func TestFindAndGrepRespectToolSpecificIgnoresAndNestedRepositories(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, ".git/HEAD", "ref: refs/heads/main\n")
	writeTestFile(t, root, ".gitignore", "parent.txt\n")
	writeTestFile(t, root, ".fdignore", "find-only.txt\n")
	writeTestFile(t, root, ".rgignore", "grep-only.txt\n")
	writeTestFile(t, root, "parent.txt", "TOKEN root ignored\n")
	writeTestFile(t, root, "find-only.txt", "TOKEN grep sees this\n")
	writeTestFile(t, root, "grep-only.txt", "TOKEN find sees this\n")
	writeTestFile(t, root, "node_modules/dependency.txt", "TOKEN dependency\n")
	writeTestFile(t, root, "nested/.git/HEAD", "ref: refs/heads/nested\n")
	writeTestFile(t, root, "nested/parent.txt", "TOKEN nested boundary\n")
	writeTestFile(t, root, "src/root.go", "package root\n")
	writeTestFile(t, root, "other/src/nested.go", "package nested\n")

	suite, err := NewFilesystemSuite(FilesystemOptions{WorkingDir: root})
	if err != nil {
		t.Fatal(err)
	}
	result, err := suite.Find(context.Background(), FindInput{Pattern: "*.txt"})
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"grep-only.txt", "nested/parent.txt", "node_modules/dependency.txt"} {
		if !containsOutputLine(result.Text, expected) {
			t.Fatalf("find omitted %q: %q", expected, result.Text)
		}
	}
	for _, excluded := range []string{"parent.txt", "find-only.txt"} {
		if containsOutputLine(result.Text, excluded) {
			t.Fatalf("find included %q: %q", excluded, result.Text)
		}
	}

	result, err = suite.Find(context.Background(), FindInput{Pattern: "src/**/*.go"})
	if err != nil || result.Text != "other/src/nested.go\nsrc/root.go" {
		t.Fatalf("path-containing find = %q, %v", result.Text, err)
	}

	result, err = suite.Grep(context.Background(), GrepInput{Pattern: "TOKEN"})
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"find-only.txt:1:", "nested/parent.txt:1:", "node_modules/dependency.txt:1:"} {
		if !strings.Contains(result.Text, expected) {
			t.Fatalf("grep omitted %q: %q", expected, result.Text)
		}
	}
	for _, excluded := range []string{"parent.txt:1:", "grep-only.txt:1:"} {
		if hasOutputLinePrefix(result.Text, excluded) {
			t.Fatalf("grep included %q: %q", excluded, result.Text)
		}
	}
}

func containsOutputLine(output, line string) bool {
	for _, candidate := range strings.Split(output, "\n") {
		if candidate == line {
			return true
		}
	}
	return false
}

func hasOutputLinePrefix(output, prefix string) bool {
	for _, candidate := range strings.Split(output, "\n") {
		if strings.HasPrefix(candidate, prefix) {
			return true
		}
	}
	return false
}

func TestLsDoesNotClaimTruncationAtExactDirectorySize(t *testing.T) {
	suite := newTestSuite(t)
	writeTestFile(t, suite.WorkingDir(), "a", "a")
	writeTestFile(t, suite.WorkingDir(), "b", "b")
	result, err := suite.Ls(context.Background(), LsInput{Limit: intPointer(2)})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "a\nb" || result.Details != nil {
		t.Fatalf("exact-limit ls = %#v", result)
	}
}

func TestPrepareEditArgumentsBeforeSchemaValidation(t *testing.T) {
	original := map[string]any{
		"path":    "sample.txt",
		"edits":   `[{"oldText":"one","newText":"ONE"}]`,
		"oldText": "two",
		"newText": "TWO",
		"extra":   true,
	}
	prepared, ok := PrepareEditArguments(original).(map[string]any)
	if !ok {
		t.Fatalf("prepared arguments type = %T", prepared)
	}
	edits, ok := prepared["edits"].([]any)
	if !ok || len(edits) != 2 || prepared["extra"] != true {
		t.Fatalf("prepared arguments = %#v", prepared)
	}
	if _, exists := prepared["oldText"]; exists {
		t.Fatalf("legacy field retained: %#v", prepared)
	}
	if _, exists := original["oldText"]; !exists || original["edits"].(string) == "" {
		t.Fatalf("input map was mutated: %#v", original)
	}

	suite := newTestSuite(t)
	registry, err := NewFilesystemRegistry(suite)
	if err != nil {
		t.Fatal(err)
	}
	throughRegistry, err := registry.PrepareArguments(EditToolName, original)
	if err != nil {
		t.Fatal(err)
	}
	if got := throughRegistry.(map[string]any)["edits"].([]any); len(got) != 2 {
		t.Fatalf("registry preparation = %#v", throughRegistry)
	}
	untouched := map[string]any{"command": "true"}
	if got, err := registry.PrepareArguments(BashToolName, untouched); err != nil || got.(map[string]any)["command"] != "true" {
		t.Fatalf("non-edit preparation = %#v, %v", got, err)
	}
}

func TestBashRegistryPreservesCompleteTruncationDetails(t *testing.T) {
	artifactDirectory := filepath.Join(t.TempDir(), "private-artifacts")
	bash, err := NewBash(BashOptions{
		WorkingDir: t.TempDir(), Environment: []string{}, ArtifactDirectory: artifactDirectory, MaxOutputLines: 1,
		Runner: runnerFunc(func(_ context.Context, _ RunRequest, sink OutputSink) (ExitStatus, error) {
			if err := sink([]byte("one\ntwo\n")); err != nil {
				return ExitStatus{}, err
			}
			return testExitStatus(t, 0), nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := (bashJSONTool{bash: bash}).ExecuteJSON(context.Background(), []byte(`{"command":"chatty"}`))
	if err != nil {
		t.Fatal(err)
	}
	truncation, ok := result.Details["truncation"].(map[string]any)
	if !ok || truncation["truncated"] != true || truncation["truncatedBy"] != "lines" || truncation["lastLinePartial"] != false || truncation["firstLineExceedsLimit"] != false {
		t.Fatalf("truncation details = %#v", result.Details)
	}
	for _, key := range []string{"content", "totalLines", "totalBytes", "outputLines", "outputBytes", "maxLines", "maxBytes"} {
		if _, exists := truncation[key]; !exists {
			t.Fatalf("truncation details omit %q: %#v", key, truncation)
		}
	}
	path, ok := result.Details["fullOutputPath"].(string)
	if !ok || path == "" {
		t.Fatalf("full output path = %#v", result.Details)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "one\ntwo\n" {
		t.Fatalf("full output = %q, %v", data, err)
	}
}
