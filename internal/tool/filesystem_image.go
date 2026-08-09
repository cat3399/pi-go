package tool

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"math"
	"os"

	"github.com/cat3399/pi-go/internal/llm"
	"golang.org/x/image/draw"

	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/webp"
	_ "image/gif"
)

const (
	imageSniffBytes       = 4100
	defaultImageMaxWidth  = 2000
	defaultImageMaxHeight = 2000
)

var pngSignature = []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}

var (
	errImageConversion = errors.New("image conversion failed")
	errImageResize     = errors.New("image resize failed")
	errImageSafety     = errors.New("image exceeds safe decode dimensions")
	errImageOutput     = errors.New("image exceeds inline output limit")
)

var lanczos3 = &draw.Kernel{Support: 3, At: func(distance float64) float64 {
	if distance == 0 {
		return 1
	}
	return math.Sin(math.Pi*distance) * math.Sin(math.Pi*distance/3) / (math.Pi * math.Pi * distance * distance / 3)
}}

func detectSupportedImageMIME(data []byte) string {
	probe := data
	if len(probe) > imageSniffBytes {
		probe = probe[:imageSniffBytes]
	}
	switch {
	case len(probe) >= 3 && probe[0] == 0xff && probe[1] == 0xd8 && probe[2] == 0xff && (len(probe) < 4 || probe[3] != 0xf7):
		return "image/jpeg"
	case isPNG(probe) && !isAnimatedPNG(probe):
		return "image/png"
	case hasASCII(probe, 0, "GIF"):
		return "image/gif"
	case hasASCII(probe, 0, "RIFF") && hasASCII(probe, 8, "WEBP"):
		return "image/webp"
	case hasASCII(probe, 0, "BM") && isBMP(probe):
		return "image/bmp"
	default:
		return ""
	}
}

func isPNG(data []byte) bool {
	return len(data) >= 16 && bytes.Equal(data[:8], pngSignature) && readUint32BE(data, 8) == 13 && hasASCII(data, 12, "IHDR")
}

func isAnimatedPNG(data []byte) bool {
	for offset := len(pngSignature); offset+8 <= len(data); {
		length := int64(readUint32BE(data, offset))
		if hasASCII(data, offset+4, "acTL") {
			return true
		}
		if hasASCII(data, offset+4, "IDAT") {
			return false
		}
		next := int64(offset) + 8 + length + 4
		if next <= int64(offset) || next > int64(len(data)) {
			return false
		}
		offset = int(next)
	}
	return false
}

func isBMP(data []byte) bool {
	if len(data) < 26 {
		return false
	}
	declaredSize := readUint32LE(data, 2)
	pixelOffset := readUint32LE(data, 10)
	headerSize := readUint32LE(data, 14)
	if declaredSize != 0 && declaredSize < 26 || pixelOffset < 14+headerSize || declaredSize != 0 && pixelOffset >= declaredSize {
		return false
	}
	var planes, bits uint32
	switch {
	case headerSize == 12:
		planes, bits = uint32(readUint16LE(data, 22)), uint32(readUint16LE(data, 24))
	case headerSize >= 40 && headerSize <= 124 && len(data) >= 30:
		planes, bits = uint32(readUint16LE(data, 26)), uint32(readUint16LE(data, 28))
	default:
		return false
	}
	return planes == 1 && (bits == 1 || bits == 4 || bits == 8 || bits == 16 || bits == 24 || bits == 32)
}

func hasASCII(data []byte, offset int, value string) bool {
	return offset >= 0 && len(data) >= offset+len(value) && string(data[offset:offset+len(value)]) == value
}

func readUint16LE(data []byte, offset int) uint16 {
	if offset < 0 || offset+2 > len(data) {
		return 0
	}
	return uint16(data[offset]) | uint16(data[offset+1])<<8
}

func readUint32LE(data []byte, offset int) uint32 {
	if offset < 0 || offset+4 > len(data) {
		return 0
	}
	return uint32(data[offset]) | uint32(data[offset+1])<<8 | uint32(data[offset+2])<<16 | uint32(data[offset+3])<<24
}

func readUint32BE(data []byte, offset int) uint32 {
	if offset < 0 || offset+4 > len(data) {
		return 0
	}
	return uint32(data[offset])<<24 | uint32(data[offset+1])<<16 | uint32(data[offset+2])<<8 | uint32(data[offset+3])
}

type processedReadImage struct {
	data                  []byte
	mimeType              string
	originalWidth         int
	originalHeight        int
	width                 int
	height                int
	resized               bool
	convertedFromMIMEType string
}

func processReadImage(
	ctx context.Context,
	file *os.File,
	sourceBytes int64,
	mimeType string,
	autoResize bool,
	maxPixels int64,
	maxOutputBytes int,
) (processedReadImage, error) {
	if err := contextFailure(ctx); err != nil {
		return processedReadImage{}, err
	}
	configuration, err := decodeImageConfig(file, sourceBytes)
	if err != nil {
		kind := errImageResize
		if mimeType == "image/bmp" {
			kind = errImageConversion
		}
		return processedReadImage{}, fmt.Errorf("%w: inspect image: %v", kind, err)
	}
	if err := validateImagePixels(configuration.Width, configuration.Height, maxPixels); err != nil {
		return processedReadImage{}, err
	}

	processed := processedReadImage{mimeType: mimeType}
	var raw []byte
	if fitsInlineImageSource(sourceBytes, maxOutputBytes) {
		raw, err = readImageSource(ctx, file, sourceBytes)
		if err != nil {
			return processedReadImage{}, err
		}
	}
	// MIME sniffing intentionally uses only a short prefix, but EXIF metadata
	// may follow large JPEG APP segments or WebP chunks. Scan the same opened,
	// stat-bounded handle and seek over payloads instead of materializing the
	// source merely to discover its orientation.
	orientation, err := imageEXIFOrientationFromFile(ctx, file, sourceBytes, mimeType)
	if err != nil {
		return processedReadImage{}, err
	}
	configuredWidth, configuredHeight := configuration.Width, configuration.Height
	if orientation >= 5 && orientation <= 8 {
		configuredWidth, configuredHeight = configuredHeight, configuredWidth
	}

	// Upstream's autoResize=false path forwards arbitrarily large supported
	// images. Go keeps the provider payload boundary even in that mode so the
	// in-process result cannot force a later base64 allocation beyond 4.5 MiB.
	if mimeType != "image/bmp" && !autoResize {
		if raw == nil {
			return processedReadImage{}, fmt.Errorf("%w: source does not fit encoded output", errImageOutput)
		}
		processed.data = raw
		processed.originalWidth, processed.originalHeight = configuredWidth, configuredHeight
		processed.width, processed.height = configuredWidth, configuredHeight
		return processed, nil
	}
	source, err := decodeImage(file, sourceBytes)
	if err != nil {
		if failure := contextFailure(ctx); failure != nil {
			return processedReadImage{}, failure
		}
		kind := errImageResize
		if mimeType == "image/bmp" {
			kind = errImageConversion
		}
		return processedReadImage{}, fmt.Errorf("%w: decode image: %v", kind, err)
	}
	if err := contextFailure(ctx); err != nil {
		return processedReadImage{}, err
	}
	width, height := source.Bounds().Dx(), source.Bounds().Dy()
	if err := validateImagePixels(width, height, maxPixels); err != nil {
		return processedReadImage{}, err
	}
	if mimeType == "image/jpeg" || mimeType == "image/webp" {
		source, err = applyImageOrientationWithContext(ctx, source, orientation)
		if err != nil {
			return processedReadImage{}, err
		}
	}
	width, height = source.Bounds().Dx(), source.Bounds().Dy()
	processed.originalWidth, processed.originalHeight = width, height
	processed.width, processed.height = width, height
	if mimeType != "image/bmp" && raw != nil && width <= defaultImageMaxWidth && height <= defaultImageMaxHeight {
		processed.data = raw
		return processed, nil
	}

	if mimeType == "image/bmp" {
		processed.mimeType = "image/png"
		processed.convertedFromMIMEType = mimeType
		if !autoResize || width <= defaultImageMaxWidth && height <= defaultImageMaxHeight {
			var encoded bytes.Buffer
			if err := png.Encode(&encoded, source); err != nil {
				return processedReadImage{}, fmt.Errorf("%w: %v", errImageConversion, err)
			}
			if err := contextFailure(ctx); err != nil {
				return processedReadImage{}, err
			}
			if base64EncodedSize(len(encoded.Bytes())) < maxOutputBytes {
				processed.data = encoded.Bytes()
				return processed, nil
			}
			if !autoResize {
				return processedReadImage{}, fmt.Errorf("%w: converted BMP does not fit encoded output", errImageOutput)
			}
		}
	}

	width, height = boundedImageDimensions(width, height)
	qualitySteps := []int{80, 85, 70, 55, 40}
	for {
		destination := image.NewNRGBA(image.Rect(0, 0, width, height))
		lanczos3.Scale(destination, destination.Bounds(), source, source.Bounds(), draw.Over, nil)
		if err := contextFailure(ctx); err != nil {
			return processedReadImage{}, err
		}
		candidate, ok, err := firstInlineImageCandidate(ctx, destination, qualitySteps, maxOutputBytes)
		if err != nil {
			return processedReadImage{}, err
		}
		if ok {
			processed.data = candidate.data
			processed.mimeType = candidate.mimeType
			processed.width, processed.height = width, height
			processed.resized = true
			return processed, nil
		}
		if width == 1 && height == 1 {
			break
		}
		nextWidth, nextHeight := width, height
		if nextWidth > 1 {
			nextWidth = maxInt(1, int(math.Floor(float64(nextWidth)*0.75)))
		}
		if nextHeight > 1 {
			nextHeight = maxInt(1, int(math.Floor(float64(nextHeight)*0.75)))
		}
		if nextWidth == width && nextHeight == height {
			break
		}
		width, height = nextWidth, nextHeight
	}
	return processedReadImage{}, fmt.Errorf("%w: could not resize image below inline size limit", errImageResize)
}

func decodeImageConfig(file *os.File, sourceBytes int64) (image.Config, error) {
	if sourceBytes < 0 {
		return image.Config{}, fmt.Errorf("negative source size")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return image.Config{}, err
	}
	configuration, _, err := image.DecodeConfig(io.LimitReader(file, sourceBytes))
	return configuration, err
}

func decodeImage(file *os.File, sourceBytes int64) (image.Image, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	decoded, _, err := image.Decode(io.LimitReader(file, sourceBytes))
	return decoded, err
}

func validateImagePixels(width, height int, maximum int64) error {
	if width < 1 || height < 1 || int64(width) > maximum/int64(height) {
		return fmt.Errorf("%w: %dx%d exceeds %d pixels", errImageSafety, width, height, maximum)
	}
	return nil
}

func fitsInlineImageSource(sourceBytes int64, maximum int) bool {
	if sourceBytes < 0 || sourceBytes > int64(maximum) {
		return false
	}
	return ((sourceBytes + 2) / 3 * 4) < int64(maximum)
}

func readImageSource(ctx context.Context, file *os.File, sourceBytes int64) ([]byte, error) {
	if sourceBytes < 0 || sourceBytes > int64(int(^uint(0)>>1)) {
		return nil, fmt.Errorf("%w: source size is not addressable", errImageOutput)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	data := make([]byte, int(sourceBytes))
	read := 0
	for read < len(data) {
		if err := contextFailure(ctx); err != nil {
			return nil, err
		}
		count, err := file.Read(data[read:])
		read += count
		if err != nil {
			if errors.Is(err, io.EOF) && read == len(data) {
				break
			}
			return nil, err
		}
		if count == 0 {
			return nil, io.ErrNoProgress
		}
	}
	return data, nil
}

type imageMetadataReader struct {
	ctx    context.Context
	reader *io.SectionReader
}

func newImageMetadataReader(ctx context.Context, source io.ReaderAt, sourceBytes int64) (*imageMetadataReader, error) {
	if sourceBytes < 0 {
		return nil, fmt.Errorf("negative image source size")
	}
	return &imageMetadataReader{ctx: ctx, reader: io.NewSectionReader(source, 0, sourceBytes)}, nil
}

func (r *imageMetadataReader) position() (int64, error) {
	if err := contextFailure(r.ctx); err != nil {
		return 0, err
	}
	return r.reader.Seek(0, io.SeekCurrent)
}

func (r *imageMetadataReader) seek(offset int64) error {
	if err := contextFailure(r.ctx); err != nil {
		return err
	}
	if offset < 0 || offset > r.reader.Size() {
		return io.ErrUnexpectedEOF
	}
	_, err := r.reader.Seek(offset, io.SeekStart)
	return err
}

func (r *imageMetadataReader) skip(count int64) (bool, error) {
	position, err := r.position()
	if err != nil {
		return false, err
	}
	if count < 0 || count > r.reader.Size()-position {
		return false, nil
	}
	if _, err := r.reader.Seek(count, io.SeekCurrent); err != nil {
		return false, err
	}
	return true, nil
}

func (r *imageMetadataReader) readExact(destination []byte) (bool, error) {
	if err := contextFailure(r.ctx); err != nil {
		return false, err
	}
	position, err := r.position()
	if err != nil {
		return false, err
	}
	if int64(len(destination)) > r.reader.Size()-position {
		return false, nil
	}
	_, err = io.ReadFull(r.reader, destination)
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		if failure := contextFailure(r.ctx); failure != nil {
			return false, failure
		}
		return false, nil
	}
	return err == nil, err
}

func imageEXIFOrientationFromFile(
	ctx context.Context,
	file *os.File,
	sourceBytes int64,
	mimeType string,
) (int, error) {
	reader, err := newImageMetadataReader(ctx, file, sourceBytes)
	if err != nil {
		return 1, err
	}
	switch mimeType {
	case "image/jpeg":
		return jpegEXIFOrientationFromReader(reader)
	case "image/webp":
		return webpEXIFOrientationFromReader(reader)
	default:
		return 1, nil
	}
}

func jpegEXIFOrientationFromReader(reader *imageMetadataReader) (int, error) {
	var signature [2]byte
	if ok, err := reader.readExact(signature[:]); err != nil || !ok {
		return 1, err
	}
	if signature != [2]byte{0xff, 0xd8} {
		return 1, nil
	}
	for {
		if err := contextFailure(reader.ctx); err != nil {
			return 1, err
		}
		var prefix [1]byte
		if ok, err := reader.readExact(prefix[:]); err != nil || !ok {
			return 1, err
		}
		if prefix[0] != 0xff {
			return 1, nil
		}
		var marker [1]byte
		if ok, err := reader.readExact(marker[:]); err != nil || !ok {
			return 1, err
		}
		for marker[0] == 0xff {
			if ok, err := reader.readExact(marker[:]); err != nil || !ok {
				return 1, err
			}
		}
		if marker[0] == 0xd8 || marker[0] == 0x01 || marker[0] >= 0xd0 && marker[0] <= 0xd7 {
			continue
		}
		if marker[0] == 0xd9 || marker[0] == 0xda {
			return 1, nil
		}
		var encodedLength [2]byte
		if ok, err := reader.readExact(encodedLength[:]); err != nil || !ok {
			return 1, err
		}
		length := int64(encodedLength[0])<<8 | int64(encodedLength[1])
		if length < 2 {
			return 1, nil
		}
		payloadBytes := length - 2
		if marker[0] != 0xe1 {
			if ok, err := reader.skip(payloadBytes); err != nil || !ok {
				return 1, err
			}
			continue
		}
		if payloadBytes < 6 {
			return 1, nil
		}
		var exifHeader [6]byte
		if ok, err := reader.readExact(exifHeader[:]); err != nil || !ok {
			return 1, err
		}
		if exifHeader != [6]byte{'E', 'x', 'i', 'f', 0, 0} {
			// Match the pinned upstream parser: the first APP1 segment is
			// authoritative even when it carries XMP instead of EXIF.
			return 1, nil
		}
		tiffStart, err := reader.position()
		if err != nil {
			return 1, err
		}
		return tiffOrientationFromReader(reader, tiffStart, payloadBytes-6)
	}
}

func webpEXIFOrientationFromReader(reader *imageMetadataReader) (int, error) {
	var header [12]byte
	if ok, err := reader.readExact(header[:]); err != nil || !ok {
		return 1, err
	}
	if string(header[:4]) != "RIFF" || string(header[8:]) != "WEBP" {
		return 1, nil
	}
	for {
		if err := contextFailure(reader.ctx); err != nil {
			return 1, err
		}
		position, err := reader.position()
		if err != nil {
			return 1, err
		}
		if reader.reader.Size()-position < 8 {
			return 1, nil
		}
		var chunkHeader [8]byte
		if ok, err := reader.readExact(chunkHeader[:]); err != nil || !ok {
			return 1, err
		}
		chunkBytes := int64(uint32(chunkHeader[4]) | uint32(chunkHeader[5])<<8 | uint32(chunkHeader[6])<<16 | uint32(chunkHeader[7])<<24)
		dataStart, err := reader.position()
		if err != nil {
			return 1, err
		}
		if chunkBytes > reader.reader.Size()-dataStart {
			return 1, nil
		}
		if string(chunkHeader[:4]) == "EXIF" {
			tiffStart, tiffBytes := dataStart, chunkBytes
			if chunkBytes >= 6 {
				var exifHeader [6]byte
				if ok, err := reader.readExact(exifHeader[:]); err != nil || !ok {
					return 1, err
				}
				if exifHeader == [6]byte{'E', 'x', 'i', 'f', 0, 0} {
					tiffStart += 6
					tiffBytes -= 6
				}
			}
			return tiffOrientationFromReader(reader, tiffStart, tiffBytes)
		}
		if ok, err := reader.skip(chunkBytes + chunkBytes%2); err != nil || !ok {
			return 1, err
		}
	}
}

func tiffOrientationFromReader(reader *imageMetadataReader, start, size int64) (int, error) {
	if size < 8 {
		return 1, nil
	}
	if err := reader.seek(start); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return 1, nil
		}
		return 1, err
	}
	var header [8]byte
	if ok, err := reader.readExact(header[:]); err != nil || !ok {
		return 1, err
	}
	littleEndian := header[0] == 'I' && header[1] == 'I'
	read16 := func(data []byte) uint16 {
		if littleEndian {
			return uint16(data[0]) | uint16(data[1])<<8
		}
		return uint16(data[0])<<8 | uint16(data[1])
	}
	read32 := func(data []byte) uint32 {
		if littleEndian {
			return uint32(data[0]) | uint32(data[1])<<8 | uint32(data[2])<<16 | uint32(data[3])<<24
		}
		return uint32(data[0])<<24 | uint32(data[1])<<16 | uint32(data[2])<<8 | uint32(data[3])
	}
	ifdOffset := int64(read32(header[4:8]))
	if ifdOffset < 0 || ifdOffset > size-2 {
		return 1, nil
	}
	if err := reader.seek(start + ifdOffset); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return 1, nil
		}
		return 1, err
	}
	var countBytes [2]byte
	if ok, err := reader.readExact(countBytes[:]); err != nil || !ok {
		return 1, err
	}
	entryCount := int(read16(countBytes[:]))
	for index := 0; index < entryCount; index++ {
		if err := contextFailure(reader.ctx); err != nil {
			return 1, err
		}
		entryOffset := ifdOffset + 2 + int64(index)*12
		if entryOffset < 0 || entryOffset > size-12 {
			return 1, nil
		}
		if err := reader.seek(start + entryOffset); err != nil {
			return 1, err
		}
		var entry [12]byte
		if ok, err := reader.readExact(entry[:]); err != nil || !ok {
			return 1, err
		}
		if read16(entry[:2]) != 0x0112 {
			continue
		}
		orientation := int(read16(entry[8:10]))
		if orientation >= 1 && orientation <= 8 {
			return orientation, nil
		}
		return 1, nil
	}
	return 1, nil
}

func imageEXIFOrientation(data []byte, mimeType string) int {
	reader, err := newImageMetadataReader(context.Background(), bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return 1
	}
	var orientation int
	switch mimeType {
	case "image/jpeg":
		orientation, err = jpegEXIFOrientationFromReader(reader)
	case "image/webp":
		orientation, err = webpEXIFOrientationFromReader(reader)
	default:
		return 1
	}
	if err != nil {
		return 1
	}
	return orientation
}

func jpegEXIFOrientation(data []byte) int {
	return imageEXIFOrientation(data, "image/jpeg")
}

func webpEXIFOrientation(data []byte) int {
	return imageEXIFOrientation(data, "image/webp")
}

func applyImageOrientationWithContext(ctx context.Context, source image.Image, orientation int) (image.Image, error) {
	if orientation < 2 || orientation > 8 {
		return source, nil
	}
	bounds := source.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	destinationWidth, destinationHeight := width, height
	if orientation >= 5 {
		destinationWidth, destinationHeight = height, width
	}
	destination := image.NewNRGBA(image.Rect(0, 0, destinationWidth, destinationHeight))
	for y := 0; y < height; y++ {
		if err := contextFailure(ctx); err != nil {
			return nil, err
		}
		for x := 0; x < width; x++ {
			destinationX, destinationY := x, y
			switch orientation {
			case 2:
				destinationX = width - 1 - x
			case 3:
				destinationX, destinationY = width-1-x, height-1-y
			case 4:
				destinationY = height - 1 - y
			case 5:
				destinationX, destinationY = y, x
			case 6:
				destinationX, destinationY = height-1-y, x
			case 7:
				destinationX, destinationY = height-1-y, width-1-x
			case 8:
				destinationX, destinationY = y, width-1-x
			}
			destination.Set(destinationX, destinationY, source.At(bounds.Min.X+x, bounds.Min.Y+y))
		}
	}
	return destination, nil
}

func boundedImageDimensions(width, height int) (int, int) {
	targetWidth, targetHeight := width, height
	if targetWidth > defaultImageMaxWidth {
		targetHeight = int(math.Round(float64(targetHeight) * float64(defaultImageMaxWidth) / float64(targetWidth)))
		targetWidth = defaultImageMaxWidth
	}
	if targetHeight > defaultImageMaxHeight {
		targetWidth = int(math.Round(float64(targetWidth) * float64(defaultImageMaxHeight) / float64(targetHeight)))
		targetHeight = defaultImageMaxHeight
	}
	return maxInt(1, targetWidth), maxInt(1, targetHeight)
}

type encodedImageCandidate struct {
	data     []byte
	mimeType string
}

func firstInlineImageCandidate(ctx context.Context, source image.Image, qualities []int, maximum int) (encodedImageCandidate, bool, error) {
	var pngOutput bytes.Buffer
	if err := png.Encode(&pngOutput, source); err == nil {
		if err := contextFailure(ctx); err != nil {
			return encodedImageCandidate{}, false, err
		}
		if base64EncodedSize(pngOutput.Len()) < maximum {
			return encodedImageCandidate{data: pngOutput.Bytes(), mimeType: "image/png"}, true, nil
		}
	}
	for _, quality := range qualities {
		var jpegOutput bytes.Buffer
		if err := jpeg.Encode(&jpegOutput, source, &jpeg.Options{Quality: quality}); err == nil {
			if err := contextFailure(ctx); err != nil {
				return encodedImageCandidate{}, false, err
			}
			if base64EncodedSize(jpegOutput.Len()) < maximum {
				return encodedImageCandidate{data: jpegOutput.Bytes(), mimeType: "image/jpeg"}, true, nil
			}
		}
	}
	return encodedImageCandidate{}, false, nil
}

func base64EncodedSize(rawBytes int) int { return ((rawBytes + 2) / 3) * 4 }

func imageToolResult(processed processedReadImage) (ToolResult, error) {
	note := fmt.Sprintf("Read image file [%s]", processed.mimeType)
	if processed.convertedFromMIMEType != "" && processed.convertedFromMIMEType != processed.mimeType {
		note += fmt.Sprintf("\n[Image converted from %s to %s.]", processed.convertedFromMIMEType, processed.mimeType)
	}
	if processed.resized {
		scale := float64(processed.originalWidth) / float64(processed.width)
		note += fmt.Sprintf(
			"\n[Image: original %dx%d, displayed at %dx%d. Multiply coordinates by %.2f to map to original image.]",
			processed.originalWidth, processed.originalHeight, processed.width, processed.height, scale,
		)
	}
	textBlock, err := llm.NewTextBlock(note)
	if err != nil {
		return ToolResult{}, err
	}
	imageBlock, err := llm.NewImageDataBlock(processed.mimeType, processed.data)
	if err != nil {
		return ToolResult{}, err
	}
	return ToolResult{Text: note, Content: []llm.ToolResultContentBlock{textBlock, imageBlock}}, nil
}
