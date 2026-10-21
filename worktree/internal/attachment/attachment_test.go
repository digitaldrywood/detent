package attachment

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestPrepare(t *testing.T) {
	t.Parallel()
	var pngData, jpegData, gifData bytes.Buffer
	picture := image.NewRGBA(image.Rect(0, 0, 2, 3))
	if err := png.Encode(&pngData, picture); err != nil {
		t.Fatal(err)
	}
	jpegPicture := image.NewRGBA(image.Rect(0, 0, 24, 19))
	for y := range 19 {
		for x := range 24 {
			jpegPicture.SetRGBA(x, y, color.RGBA{R: uint8(x * 43), G: uint8(y * 37), B: uint8(x*y + 17), A: 255})
		}
	}
	if err := jpeg.Encode(&jpegData, jpegPicture, &jpeg.Options{Quality: 100}); err != nil {
		t.Fatal(err)
	}
	if err := gif.Encode(&gifData, picture, nil); err != nil {
		t.Fatal(err)
	}
	exif := []byte("Exif\x00\x00GPS-sentinel")
	withEXIF := append([]byte{}, jpegData.Bytes()[:2]...)
	withEXIF = append(withEXIF, jpegTestSegment(0xe1, exif)...)
	withEXIF = append(withEXIF, jpegData.Bytes()[2:]...)
	icc := jpegTestSegment(0xe2, []byte("ICC_PROFILE\x00\x01\x02profile-part-one"))
	icc = append(icc, jpegTestSegment(0xe2, []byte("ICC_PROFILE\x00\x02\x02profile-part-two"))...)
	withICC := append([]byte{}, jpegData.Bytes()[:2]...)
	withICC = append(withICC, icc...)
	withICC = append(withICC, jpegData.Bytes()[2:]...)
	withApplication := func(segments ...[]byte) []byte {
		content := append([]byte{}, jpegData.Bytes()[:2]...)
		for _, segment := range segments {
			content = append(content, segment...)
		}
		return append(content, jpegData.Bytes()[2:]...)
	}
	jfif := []byte("JFIF\x00\x01\x01\x00\x00\x01\x00\x01\x00\x00")
	jfifThumbnail := append([]byte{}, jfif...)
	jfifThumbnail[12], jfifThumbnail[13] = 1, 1
	jfifThumbnail = append(jfifThumbnail, 0x12, 0x34, 0x56)
	withJFIF := withApplication(jpegTestSegment(0xe0, jfif))
	withMetadata := append([]byte{}, withICC[:2]...)
	withMetadata = append(withMetadata, jpegTestSegment(0xe1, exif)...)
	withMetadata = append(withMetadata, withICC[2:len(withICC)-2]...)
	withMetadata = append(withMetadata, jpegTestSegment(0xe1, []byte("http://ns.adobe.com/xap/1.0/\x00XMP-sentinel"))...)
	withMetadata = append(withMetadata, jpegTestSegment(0xed, []byte("IPTC-sentinel"))...)
	withMetadata = append(withMetadata, jpegTestSegment(0xfe, []byte("comment-sentinel\xff\xd9"))...)
	withMetadata = append(withMetadata, 0xff, 0xd9)
	progressive := jpegTestFixture(t, "progressive")
	restart := jpegTestFixture(t, "restart")
	cmyk := jpegTestFixture(t, "cmyk")
	adobe := bytes.Index(cmyk, []byte{0xff, 0xee})
	adobeEnd := adobe + 2 + int(binary.BigEndian.Uint16(cmyk[adobe+2:]))
	adobeMetadata := append([]byte{}, cmyk[:adobe]...)
	adobePayload := append([]byte{}, cmyk[adobe+4:adobeEnd]...)
	adobePayload = append(adobePayload, []byte("private-thumbnail-sentinel")...)
	adobeMetadata = append(adobeMetadata, jpegTestSegment(0xee, adobePayload)...)
	adobeMetadata = append(adobeMetadata, cmyk[adobeEnd:]...)
	secondScan := bytes.Index(progressive, []byte{0xff, 0xda}) + 2
	secondScan += bytes.Index(progressive[secondScan:], []byte{0xff, 0xda})
	progressiveMetadata := append([]byte{}, progressive[:secondScan]...)
	progressiveMetadata = append(progressiveMetadata, jpegTestSegment(0xe1, exif)...)
	progressiveMetadata = append(progressiveMetadata, progressive[secondScan:]...)
	oversizeJPEG := append([]byte{}, jpegData.Bytes()...)
	frame := bytes.Index(oversizeJPEG, []byte{0xff, 0xc0})
	binary.BigEndian.PutUint16(oversizeJPEG[frame+5:frame+7], 4001)
	binary.BigEndian.PutUint16(oversizeJPEG[frame+7:frame+9], 4000)
	oversizeImage := append([]byte{}, pngData.Bytes()...)
	binary.BigEndian.PutUint32(oversizeImage[16:20], 4001)
	binary.BigEndian.PutUint32(oversizeImage[20:24], 4000)
	binary.BigEndian.PutUint32(oversizeImage[29:33], crc32.ChecksumIEEE(oversizeImage[12:29]))
	webp, err := base64.StdEncoding.DecodeString("UklGRiIAAABXRUJQVlA4IBYAAAAwAQCdASoBAAEADsD+JaQAA3AAAAAA")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, file, declared, media string
		content                     []byte
		wantJPEG                    []byte
		failure                     error
		image                       bool
	}{
		{name: "png inferred", content: pngData.Bytes(), media: "image/png", image: true},
		{name: "png polyglot", declared: "image/png", content: append(append([]byte{}, pngData.Bytes()...), []byte("<script>sentinel</script>")...), media: "image/png", image: true},
		{name: "jpeg plain", declared: "image/jpeg", content: jpegData.Bytes(), wantJPEG: jpegData.Bytes(), media: "image/jpeg", image: true},
		{name: "jpeg EXIF", declared: "image/jpeg", content: withEXIF, wantJPEG: jpegData.Bytes(), media: "image/jpeg", image: true},
		{name: "jpeg ICC", declared: "image/jpeg", content: withICC, wantJPEG: withICC, media: "image/jpeg", image: true},
		{name: "jpeg JFIF thumbnail", declared: "image/jpeg", content: withApplication(jpegTestSegment(0xe0, jfifThumbnail)), wantJPEG: withJFIF, media: "image/jpeg", image: true},
		{name: "jpeg JFXX thumbnail", declared: "image/jpeg", content: withApplication(jpegTestSegment(0xe0, append([]byte("JFXX\x00\x10"), jpegData.Bytes()...))), wantJPEG: jpegData.Bytes(), media: "image/jpeg", image: true},
		{name: "jpeg unrecognized application payloads", declared: "image/jpeg", content: withApplication(jpegTestSegment(0xe0, []byte("private-app0")), jpegTestSegment(0xe2, []byte("private-app2")), jpegTestSegment(0xee, []byte("private-app14"))), wantJPEG: jpegData.Bytes(), media: "image/jpeg", image: true},
		{name: "jpeg invalid ICC chunk headers", declared: "image/jpeg", content: withApplication(jpegTestSegment(0xe2, []byte("ICC_PROFILE\x00")), jpegTestSegment(0xe2, []byte("ICC_PROFILE\x00\x00\x01private")), jpegTestSegment(0xe2, []byte("ICC_PROFILE\x00\x02\x01private"))), wantJPEG: jpegData.Bytes(), media: "image/jpeg", image: true},
		{name: "jpeg mixed metadata", declared: "image/jpeg", content: withMetadata, wantJPEG: withICC, media: "image/jpeg", image: true},
		{name: "jpeg polyglot", declared: "image/jpeg", content: append(append([]byte{}, jpegData.Bytes()...), []byte("<script>sentinel</script>")...), wantJPEG: jpegData.Bytes(), media: "image/jpeg", image: true},
		{name: "jpeg progressive metadata between scans", declared: "image/jpeg", content: progressiveMetadata, wantJPEG: progressive, media: "image/jpeg", image: true},
		{name: "jpeg restart markers", declared: "image/jpeg", content: restart, wantJPEG: restart, media: "image/jpeg", image: true},
		{name: "jpeg Adobe CMYK", declared: "image/jpeg", content: cmyk, wantJPEG: cmyk, media: "image/jpeg", image: true},
		{name: "jpeg Adobe trailing metadata", declared: "image/jpeg", content: adobeMetadata, wantJPEG: cmyk, media: "image/jpeg", image: true},
		{name: "jpeg truncated scan", declared: "image/jpeg", content: jpegData.Bytes()[:jpegData.Len()-10], failure: ErrInvalid},
		{name: "jpeg truncated metadata", declared: "image/jpeg", content: withEXIF[:10], failure: ErrInvalid},
		{name: "jpeg pixel cap", declared: "image/jpeg", content: oversizeJPEG, failure: ErrInvalid},
		{name: "gif", declared: "image/gif", content: gifData.Bytes(), media: "image/png", image: true},
		{name: "webp", declared: "image/webp", content: webp, media: "image/png", image: true},
		{name: "pdf", declared: "application/pdf", content: []byte("%PDF-1.7\nfile"), media: "application/pdf"},
		{name: "text", declared: "text/plain", content: []byte("hello"), media: "text/plain"},
		{name: "markdown", declared: "text/markdown", content: []byte("# hello"), media: "text/markdown"},
		{name: "csv", declared: "text/csv", content: []byte("a,b\n1,2"), media: "text/csv"},
		{name: "json", declared: "application/json", content: []byte(`{"a":1}`), media: "application/json"},
		{name: "json containing markup", declared: "application/json", content: []byte(`{"html":"<svg/>"}`), media: "application/json"},
		{name: "har declared", file: "session.har", declared: "application/har+json", content: []byte(`{"log":{"version":"1.2","entries":[]}}`), media: HARMediaType},
		{name: "har uploaded as json", file: "session.har", declared: "application/json", content: []byte(`{"log":{"entries":[{"response":{"content":{"text":"<html></html>"}}}]}}`), media: HARMediaType},
		{name: "har without declared type", file: "session.HAR", declared: "application/octet-stream", content: []byte(`{"log":{}}`), media: HARMediaType},
		{name: "har sniffed as text", file: "session.har", declared: "text/plain; charset=utf-8", content: []byte(`{"log":{}}`), media: HARMediaType},
		{name: "har not json", file: "session.har", declared: "application/har+json", content: []byte("not json"), failure: ErrInvalid},
		{name: "har html", file: "session.har", content: []byte("<!doctype html><html>hi</html>"), failure: ErrInvalid},
		{name: "log declared text", file: "console.log", declared: "text/plain", content: []byte("[error] Uncaught TypeError\n"), media: "text/plain"},
		{name: "log without declared type", file: "console.log", content: []byte("12:00:01 GET /api 500\n"), media: "text/plain"},
		{name: "log as octet stream", file: "console.log", declared: "application/octet-stream", content: []byte("warn: slow\n"), media: "text/plain"},
		{name: "log binary", file: "console.log", declared: "application/octet-stream", content: []byte{0, 1, 2}, failure: ErrInvalid},
		{name: "log html", file: "console.log", content: []byte("<!doctype html><html>hi</html>"), failure: ErrInvalid},
		{name: "html named log", file: "console.log", declared: "text/html", content: []byte("<html>hi</html>"), failure: ErrInvalid},
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
			file, name := "../../file", "file"
			if test.file != "" {
				file, name = test.file, test.file
			}
			upload, err := Prepare(bytes.NewReader(test.content), file, test.declared, t.TempDir())
			if test.failure != nil {
				if !errors.Is(err, test.failure) {
					t.Fatalf("error=%v want %v", err, test.failure)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if upload.File != nil {
					if err := upload.Close(); err != nil {
						t.Error(err)
					}
				}
			})
			path := upload.File.Name()
			stored, err := io.ReadAll(upload.File)
			if err != nil {
				t.Fatal(err)
			}
			if upload.ContentType != test.media || upload.Name != name || upload.Size != int64(len(stored)) || upload.Validate() != nil {
				t.Fatalf("metadata=%+v", upload.Metadata)
			}
			digest := sha256.Sum256(stored)
			if upload.SHA256 != hex.EncodeToString(digest[:]) {
				t.Error("digest does not match stored bytes")
			}
			if test.wantJPEG != nil {
				if !bytes.Equal(stored, test.wantJPEG) {
					t.Error("JPEG scan data or retained markers changed")
				}
				original, err := jpeg.Decode(bytes.NewReader(test.content))
				if err != nil {
					t.Fatal(err)
				}
				decoded, err := jpeg.Decode(bytes.NewReader(stored))
				if err != nil {
					t.Fatal(err)
				}
				if decoded.Bounds() != original.Bounds() || upload.Width != original.Bounds().Dx() || upload.Height != original.Bounds().Dy() {
					t.Fatal("JPEG dimensions changed")
				}
				for y := original.Bounds().Min.Y; y < original.Bounds().Max.Y; y++ {
					for x := original.Bounds().Min.X; x < original.Bounds().Max.X; x++ {
						if color.RGBA64Model.Convert(decoded.At(x, y)) != color.RGBA64Model.Convert(original.At(x, y)) {
							t.Fatalf("JPEG pixel changed at (%d, %d)", x, y)
						}
					}
				}
			}
			if test.image && (bytes.Contains(stored, exif) || bytes.Contains(stored, []byte("<script>"))) {
				t.Fatal("image retained untrusted metadata or trailing content")
			}
			if err := upload.Close(); err != nil {
				t.Fatal(err)
			}
			upload.File = nil
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("scratch file remains")
			}
		})
	}
}

func jpegTestSegment(marker byte, payload []byte) []byte {
	segment := []byte{0xff, marker, 0, 0}
	binary.BigEndian.PutUint16(segment[2:], uint16(len(payload)+2))
	return append(segment, payload...)
}

func jpegTestFixture(t *testing.T, name string) []byte {
	t.Helper()
	content, err := os.ReadFile(filepath.Join("testdata", name+".jpg"))
	if err != nil {
		t.Fatal(err)
	}
	return content
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
