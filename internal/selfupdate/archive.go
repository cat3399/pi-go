package selfupdate

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"strings"
)

const maxArchiveBytes int64 = 512 << 20
const maxExecutableBytes int64 = 512 << 20

func (u *Updater) download(ctx context.Context, plan Plan) (string, error) {
	response, err := u.get(ctx, plan.archiveURL)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.ContentLength > maxArchiveBytes {
		return "", fmt.Errorf("update archive exceeds size limit")
	}
	file, err := os.CreateTemp("", "pi-go-update-*")
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(file, hash), io.LimitReader(response.Body, maxArchiveBytes+1))
	if err != nil {
		return "", fmt.Errorf("download update (partial archive at %s): %w", file.Name(), err)
	}
	if n > maxArchiveBytes {
		return "", fmt.Errorf("update archive exceeds size limit (saved at %s)", file.Name())
	}
	if string(hash.Sum(nil)) != string(plan.checksum[:]) {
		return "", fmt.Errorf("SHA-256 mismatch for %s (archive at %s); installation was not changed", plan.Asset, file.Name())
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	return file.Name(), nil
}

func extractExecutable(ctx context.Context, archive, destination string, plan Plan) error {
	name := binaryName(plan.Surface, plan.OS)
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
	if err != nil {
		return err
	}
	defer output.Close()
	if strings.HasSuffix(plan.Asset, ".zip") {
		err = extractZIP(ctx, archive, name, output)
	} else {
		err = extractTar(ctx, archive, name, output)
	}
	if err != nil {
		return fmt.Errorf("extract executable: %w", err)
	}
	if err := output.Sync(); err != nil {
		return err
	}
	return output.Close()
}

func extractZIP(ctx context.Context, archive, name string, output io.Writer) error {
	reader, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer reader.Close()
	// Portable releases contain precisely one root-level regular executable.
	// Do not interpret archive paths, follow links, or unpack arbitrary files.
	if len(reader.File) != 1 || reader.File[0].Name != name {
		return fmt.Errorf("archive must contain exactly %s", name)
	}
	entry := reader.File[0]
	if !entry.Mode().IsRegular() || entry.UncompressedSize64 == 0 || entry.UncompressedSize64 > uint64(maxExecutableBytes) {
		return fmt.Errorf("invalid executable entry %s", name)
	}
	input, err := entry.Open()
	if err != nil {
		return err
	}
	defer input.Close()
	n, err := copyContext(ctx, output, io.LimitReader(input, maxExecutableBytes+1))
	if err != nil {
		return err
	}
	if n != int64(entry.UncompressedSize64) {
		return fmt.Errorf("executable size mismatch")
	}
	return nil
}

func extractTar(ctx context.Context, archive, name string, output io.Writer) error {
	file, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer gz.Close()
	limited := &io.LimitedReader{R: gz, N: maxExecutableBytes + 1<<20}
	reader := tar.NewReader(limited)
	header, err := reader.Next()
	if err != nil {
		return err
	}
	if header.Name != name || header.Typeflag != tar.TypeReg || header.Size <= 0 || header.Size > maxExecutableBytes {
		return fmt.Errorf("archive must contain a regular %s executable", name)
	}
	if _, err := copyContext(ctx, output, reader); err != nil {
		return err
	}
	if _, err := reader.Next(); err != io.EOF {
		return fmt.Errorf("archive must contain exactly %s (extra entry or invalid trailer: %v)", name, err)
	}
	// Drain the gzip trailer too, so a damaged compressed stream is rejected.
	if _, err := copyContext(ctx, io.Discard, limited); err != nil {
		return err
	}
	if limited.N == 0 {
		return fmt.Errorf("uncompressed archive exceeds size limit")
	}
	return nil
}

type contextReader struct {
	ctx context.Context
	io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(p)
}

func copyContext(ctx context.Context, output io.Writer, input io.Reader) (int64, error) {
	return io.Copy(output, contextReader{ctx: ctx, Reader: input})
}
