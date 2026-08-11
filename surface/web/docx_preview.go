package web

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"path"
	"strconv"
	"strings"
)

const maxDOCXExpandedPartBytes = 20 * 1024 * 1024

type docxRelationship struct {
	target   string
	external bool
}

type docxRun struct {
	content       strings.Builder
	bold          bool
	italic        bool
	underline     bool
	strikethrough bool
}

func renderDOCXPreview(filePath, fileName string) (string, error) {
	archive, err := zip.OpenReader(filePath)
	if err != nil {
		return "", err
	}
	defer archive.Close()
	parts := make(map[string]*zip.File, len(archive.File))
	for _, file := range archive.File {
		parts[path.Clean(strings.TrimPrefix(file.Name, "/"))] = file
	}
	document := parts["word/document.xml"]
	if document == nil {
		return "", errors.New("DOCX document.xml is missing")
	}
	relationships, err := readDOCXRelationships(parts["word/_rels/document.xml.rels"])
	if err != nil {
		return "", err
	}
	body, err := convertDOCXDocument(document, parts, relationships)
	if err != nil {
		return "", err
	}
	return wrapDOCXPreviewHTML(body, fileName), nil
}

func readDOCXRelationships(file *zip.File) (map[string]docxRelationship, error) {
	result := make(map[string]docxRelationship)
	if file == nil {
		return result, nil
	}
	data, err := readDOCXPart(file, maxDOCXExpandedPartBytes)
	if err != nil {
		return nil, err
	}
	decoder := xml.NewDecoder(bytes.NewReader(data))
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return result, nil
		}
		if err != nil {
			return nil, err
		}
		start, ok := token.(xml.StartElement)
		if !ok || start.Name.Local != "Relationship" {
			continue
		}
		id, target, targetMode := xmlAttribute(start.Attr, "Id"), xmlAttribute(start.Attr, "Target"), xmlAttribute(start.Attr, "TargetMode")
		if id != "" && target != "" {
			result[id] = docxRelationship{target: target, external: strings.EqualFold(targetMode, "External")}
		}
	}
}

func convertDOCXDocument(document *zip.File, parts map[string]*zip.File, relationships map[string]docxRelationship) (string, error) {
	data, err := readDOCXPart(document, maxDOCXExpandedPartBytes)
	if err != nil {
		return "", err
	}
	decoder := xml.NewDecoder(bytes.NewReader(data))
	var body strings.Builder
	var paragraph strings.Builder
	var run docxRun
	inParagraph, inRun, inText := false, false, false
	paragraphStyle := ""
	hyperlink := ""
	totalImageBytes := int64(0)

	flushRun := func() {
		if !inRun {
			return
		}
		content := run.content.String()
		if content != "" {
			if run.strikethrough {
				content = "<s>" + content + "</s>"
			}
			if run.underline {
				content = "<u>" + content + "</u>"
			}
			if run.italic {
				content = "<em>" + content + "</em>"
			}
			if run.bold {
				content = "<strong>" + content + "</strong>"
			}
			if hyperlink != "" {
				content = `<a href="` + html.EscapeString(hyperlink) + `">` + content + "</a>"
			}
			paragraph.WriteString(content)
		}
		run = docxRun{}
		inRun = false
	}
	flushParagraph := func() {
		if !inParagraph {
			return
		}
		flushRun()
		tag := docxParagraphTag(paragraphStyle)
		body.WriteByte('<')
		body.WriteString(tag)
		body.WriteByte('>')
		body.WriteString(paragraph.String())
		body.WriteString("</")
		body.WriteString(tag)
		body.WriteByte('>')
		paragraph.Reset()
		paragraphStyle = ""
		inParagraph = false
	}

	for {
		token, tokenErr := decoder.Token()
		if errors.Is(tokenErr, io.EOF) {
			flushParagraph()
			return body.String(), nil
		}
		if tokenErr != nil {
			return "", tokenErr
		}
		switch typed := token.(type) {
		case xml.StartElement:
			switch typed.Name.Local {
			case "tbl":
				flushParagraph()
				body.WriteString("<table>")
			case "tr":
				body.WriteString("<tr>")
			case "tc":
				body.WriteString("<td>")
			case "p":
				flushParagraph()
				inParagraph = true
				paragraph.Reset()
				paragraphStyle = ""
			case "pStyle":
				if inParagraph {
					paragraphStyle = xmlAttribute(typed.Attr, "val")
				}
			case "hyperlink":
				hyperlink = ""
				if relationship := relationships[xmlAttribute(typed.Attr, "id")]; relationship.external {
					hyperlink = relationship.target
				} else if anchor := xmlAttribute(typed.Attr, "anchor"); anchor != "" {
					hyperlink = "#" + anchor
				}
			case "r":
				flushRun()
				inRun = true
				run = docxRun{}
			case "b":
				if inRun {
					run.bold = docxBooleanProperty(typed.Attr)
				}
			case "i":
				if inRun {
					run.italic = docxBooleanProperty(typed.Attr)
				}
			case "u":
				if inRun {
					run.underline = docxBooleanProperty(typed.Attr)
				}
			case "strike", "dstrike":
				if inRun {
					run.strikethrough = docxBooleanProperty(typed.Attr)
				}
			case "t":
				inText = true
			case "tab":
				if inRun {
					run.content.WriteString("&#9;")
				}
			case "br", "cr":
				if inRun {
					run.content.WriteString("<br>")
				}
			case "blip":
				if !inRun {
					continue
				}
				relationship := relationships[xmlAttribute(typed.Attr, "embed")]
				if relationship.target == "" || relationship.external {
					continue
				}
				partName := path.Clean(path.Join("word", relationship.target))
				if !strings.HasPrefix(partName, "word/") {
					continue
				}
				part := parts[partName]
				if part == nil || totalImageBytes+int64(part.UncompressedSize64) > imagePreviewMaxBytes {
					continue
				}
				imageData, readErr := readDOCXPart(part, imagePreviewMaxBytes-totalImageBytes)
				if readErr != nil {
					continue
				}
				totalImageBytes += int64(len(imageData))
				mimeType := imageMIME(partName)
				if mimeType == "" {
					mimeType = "application/octet-stream"
				}
				run.content.WriteString(`<img alt="" src="data:`)
				run.content.WriteString(mimeType)
				run.content.WriteString(";base64,")
				run.content.WriteString(base64.StdEncoding.EncodeToString(imageData))
				run.content.WriteString(`">`)
			}
		case xml.CharData:
			if inText && inRun {
				run.content.WriteString(html.EscapeString(string(typed)))
			}
		case xml.EndElement:
			switch typed.Name.Local {
			case "t":
				inText = false
			case "r":
				flushRun()
			case "hyperlink":
				flushRun()
				hyperlink = ""
			case "p":
				flushParagraph()
			case "tc":
				flushParagraph()
				body.WriteString("</td>")
			case "tr":
				body.WriteString("</tr>")
			case "tbl":
				body.WriteString("</table>")
			}
		}
	}
}

func readDOCXPart(file *zip.File, limit int64) ([]byte, error) {
	if file == nil {
		return nil, osErrNotExist("DOCX part")
	}
	if int64(file.UncompressedSize64) > limit {
		return nil, fmt.Errorf("DOCX expanded part exceeds %d bytes", limit)
	}
	reader, err := file.Open()
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("DOCX expanded part exceeds %d bytes", limit)
	}
	return data, nil
}

type osErrNotExist string

func (err osErrNotExist) Error() string { return string(err) + " not found" }

func xmlAttribute(attributes []xml.Attr, localName string) string {
	for _, attribute := range attributes {
		if attribute.Name.Local == localName {
			return attribute.Value
		}
	}
	return ""
}

func docxBooleanProperty(attributes []xml.Attr) bool {
	value := strings.ToLower(strings.TrimSpace(xmlAttribute(attributes, "val")))
	return value != "0" && value != "false" && value != "off" && value != "none"
}

func docxParagraphTag(style string) string {
	lower := strings.ToLower(strings.TrimSpace(style))
	for _, prefix := range []string{"heading", "title"} {
		if strings.HasPrefix(lower, prefix) {
			digits := strings.TrimLeft(lower[len(prefix):], " -_")
			if level, err := strconv.Atoi(digits); err == nil && level >= 1 && level <= 6 {
				return "h" + strconv.Itoa(level)
			}
		}
	}
	return "p"
}

func wrapDOCXPreviewHTML(bodyHTML, fileName string) string {
	return `<!doctype html>
<html>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<style>
  :root { color-scheme: light; }
  html, body { margin: 0; min-height: 100%; background: #eef1f5; color: #171717; }
  body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; padding: 28px; }
  main {
    box-sizing: border-box;
    max-width: 840px;
    min-height: calc(100vh - 56px);
    margin: 0 auto;
    padding: 56px 64px;
    background: #fff;
    box-shadow: 0 8px 28px rgba(15, 23, 42, 0.14);
  }
  .file-title {
    margin: 0 0 28px;
    padding-bottom: 10px;
    border-bottom: 1px solid #e5e7eb;
    color: #6b7280;
    font: 12px ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
    word-break: break-word;
  }
  h1, h2, h3, h4, h5, h6 { line-height: 1.3; margin: 1.1em 0 0.45em; color: #111827; }
  p { margin: 0.65em 0; line-height: 1.7; }
  table { border-collapse: collapse; max-width: 100%; margin: 1em 0; }
  th, td { border: 1px solid #d1d5db; padding: 6px 9px; vertical-align: top; }
  img { max-width: 100%; height: auto; }
  pre { white-space: pre-wrap; overflow-wrap: anywhere; }
  a { color: #2563eb; }
  @media (max-width: 720px) {
    body { padding: 0; background: #fff; }
    main { min-height: 100vh; padding: 28px 22px; box-shadow: none; }
  }
</style>
</head>
<body>
<main>
<div class="file-title">` + html.EscapeString(fileName) + `</div>
` + bodyHTML + `
</main>
</body>
</html>`
}
