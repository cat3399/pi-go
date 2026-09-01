package main

import "testing"

func TestDecodeUploadFiles(t *testing.T) {
	t.Parallel()
	files, err := decodeUploadFiles(`[{"name":"notes.txt","data":"aGVsbG8="}]`)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Name != "notes.txt" || string(files[0].Data) != "hello" {
		t.Fatalf("files = %#v", files)
	}
	if _, err := decodeUploadFiles(`[{"name":"bad.bin","data":"***"}]`); err == nil {
		t.Fatal("invalid base64 upload succeeded")
	}
}
