package attachment

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"os"
	"testing"
)

func TestPrepare(t *testing.T) {
	t.Parallel()
	var pngData, jpegData, gifData bytes.Buffer
	picture := image.NewRGBA(image.Rect(0, 0, 2, 3))
	if err := png.Encode(&pngData, picture); err != nil {
		t.Fatal(err)
	}
	if err := jpeg.Encode(&jpegData, picture, nil); err != nil {
		t.Fatal(err)
	}
	if err := gif.Encode(&gifData, picture, nil); err != nil {
		t.Fatal(err)
	}
	exif := []byte("Exif\x00\x00GPS-sentinel")
	withEXIF := append([]byte{}, jpegData.Bytes()[:2]...)
	withEXIF = append(withEXIF, 0xff, 0xe1, 0, byte(len(exif)+2))
	withEXIF = append(withEXIF, exif...)
	withEXIF = append(withEXIF, jpegData.Bytes()[2:]...)
	oversizeImage := append([]byte{}, pngData.Bytes()...)
	binary.BigEndian.PutUint32(oversizeImage[16:20], 4001)
	binary.BigEndian.PutUint32(oversizeImage[20:24], 4000)
	binary.BigEndian.PutUint32(oversizeImage[29:33], crc32.ChecksumIEEE(oversizeImage[12:29]))
	webp, err := base64.StdEncoding.DecodeString("UklGRiIAAABXRUJQVlA4IBYAAAAwAQCdASoBAAEADsD+JaQAA3AAAAAA")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, declared, media string
		content               []byte
		failure               error
		image                 bool
	}{
		{name: "png inferred", content: pngData.Bytes(), media: "image/png", image: true},
		{name: "png polyglot", declared: "image/png", content: append(append([]byte{}, pngData.Bytes()...), []byte("<script>sentinel</script>")...), media: "image/png", image: true},
		{name: "jpeg EXIF", declared: "image/jpeg", content: withEXIF, media: "image/jpeg", image: true},
		{name: "gif", declared: "image/gif", content: gifData.Bytes(), media: "image/png", image: true},
		{name: "webp", declared: "image/webp", content: webp, media: "image/png", image: true},
		{name: "pdf", declared: "application/pdf", content: []byte("%PDF-1.7\nfile"), media: "application/pdf"},
		{name: "text", declared: "text/plain", content: []byte("hello"), media: "text/plain"},
		{name: "markdown", declared: "text/markdown", content: []byte("# hello"), media: "text/markdown"},
		{name: "csv", declared: "text/csv", content: []byte("a,b\n1,2"), media: "text/csv"},
		{name: "json", declared: "application/json", content: []byte(`{"a":1}`), media: "application/json"},
		{name: "json containing markup", declared: "application/json", content: []byte(`{"html":"<svg/>"}`), media: "application/json"},
		{name: "empty", failure: ErrInvalid},
		{name: "mismatch", declared: "image/jpeg", content: pngData.Bytes(), failure: ErrInvalid},
		{name: "svg", declared: "image/svg+xml", content: []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`), failure: ErrInvalid},
		{name: "svg as text", declared: "text/plain", content: []byte("<?xml version=\"1.0\"?><svg/>"), failure: ErrInvalid},
		{name: "html as text", declared: "text/plain", content: []byte("<!doctype html><html>hi</html>"), failure: ErrInvalid},
		{name: "html beyond sniff window", declared: "text/plain", content: append(bytes.Repeat([]byte(" "), 600), []byte("<html>hi</html>")...), failure: ErrInvalid},
		{name: "html", declared: "text/html", content: []byte("<html>hi</html>"), failure: ErrInvalid},
		{name: "binary as text", declared: "text/plain", content: []byte{0, 1, 2}, failure: ErrInvalid},
		{name: "invalid json", declared: "application/json", content: []byte("{nope}"), failure: ErrInvalid},
		{name: "pixel cap", declared: "image/png", content: oversizeImage, failure: ErrInvalid},
		{name: "size cap", declared: "text/plain", content: bytes.Repeat([]byte("a"), MaxBytes+1), failure: ErrTooLarge},
	} {
		t.Run(test.name, func(t *testing.T) {
			upload, err := Prepare(bytes.NewReader(test.content), "../../file", test.declared, t.TempDir())
			if test.failure != nil {
				if !errors.Is(err, test.failure) {
					t.Fatalf("error=%v want %v", err, test.failure)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			path := upload.File.Name()
			stored, err := io.ReadAll(upload.File)
			if err != nil {
				t.Fatal(err)
			}
			if upload.ContentType != test.media || upload.Name != "file" || upload.Size != int64(len(stored)) || upload.Validate() != nil {
				t.Fatalf("metadata=%+v", upload.Metadata)
			}
			if test.image && (bytes.Contains(stored, exif) || bytes.Contains(stored, []byte("<script>"))) {
				t.Fatal("image retained untrusted metadata or trailing content")
			}
			if err := upload.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("scratch file remains")
			}
		})
	}
}

func TestKey(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		org, id, want string
		invalid       bool
	}{
		{org: "org_alpha", id: "att_0123456789abcdef0123456789abcdef", want: "orgs/org_alpha/attachments/att_0123456789abcdef0123456789abcdef"},
		{org: "org_beta", id: "att_0123456789abcdef0123456789abcdef", want: "orgs/org_beta/attachments/att_0123456789abcdef0123456789abcdef"},
		{org: "../org_beta", id: "att_0123456789abcdef0123456789abcdef", invalid: true},
		{org: "org_alpha", id: "../org_beta", invalid: true},
	} {
		t.Run(test.org+test.id, func(t *testing.T) {
			key, err := Key(test.org, test.id)
			if test.invalid {
				if !errors.Is(err, ErrInvalid) {
					t.Fatalf("key=%q error=%v", key, err)
				}
			} else if err != nil || key != test.want {
				t.Fatalf("key=%q error=%v", key, err)
			}
		})
	}
}
