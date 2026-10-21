package selfupdate

import (
	"bytes"
	"debug/elf"
	"debug/macho"
	"debug/pe"
	"errors"
	"io"
)

const guiVersionSymbol = "github.com/cat3399/pi-go/internal/product.Version.str"

// Go's -X linker option emits a NUL-terminated .str symbol. Portable GUI
// releases retain symbols; -trimpath omits -ldflags from debug.BuildInfo.
func guiLinkedVersion(path, goos string) (string, error) {
	switch goos {
	case "darwin":
		file, err := macho.Open(path)
		if err != nil {
			return "", err
		}
		defer file.Close()
		if file.Symtab != nil {
			for _, symbol := range file.Symtab.Syms {
				if symbol.Name == guiVersionSymbol && symbol.Sect > 0 && int(symbol.Sect) <= len(file.Sections) {
					section := file.Sections[symbol.Sect-1]
					if symbol.Value >= section.Addr && symbol.Value-section.Addr < section.Size {
						return readVersionString(section, int64(symbol.Value-section.Addr))
					}
				}
			}
		}
	case "linux":
		file, err := elf.Open(path)
		if err != nil {
			return "", err
		}
		defer file.Close()
		symbols, err := file.Symbols()
		if err != nil {
			return "", err
		}
		for _, symbol := range symbols {
			if symbol.Name == guiVersionSymbol && int(symbol.Section) < len(file.Sections) {
				section := file.Sections[symbol.Section]
				if symbol.Value >= section.Addr && symbol.Value-section.Addr < section.Size {
					return readVersionString(section, int64(symbol.Value-section.Addr))
				}
			}
		}
	case "windows":
		file, err := pe.Open(path)
		if err != nil {
			return "", err
		}
		defer file.Close()
		for _, symbol := range file.Symbols {
			if symbol.Name == guiVersionSymbol && symbol.SectionNumber > 0 && int(symbol.SectionNumber) <= len(file.Sections) {
				return readVersionString(file.Sections[symbol.SectionNumber-1], int64(symbol.Value))
			}
		}
	}
	return "", errors.New("GUI version linker symbol is unavailable")
}

func readVersionString(reader io.ReaderAt, offset int64) (string, error) {
	var buffer [256]byte
	n, err := reader.ReadAt(buffer[:], offset)
	if err != nil && err != io.EOF {
		return "", err
	}
	if end := bytes.IndexByte(buffer[:n], 0); end > 0 {
		return string(buffer[:end]), nil
	}
	return "", errors.New("invalid GUI version linker string")
}
