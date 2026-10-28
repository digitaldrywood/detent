package projecticon

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"strings"
	"testing"
)

func TestNormalize(t *testing.T) {
	t.Parallel()
	encode := func(width, height int, format string) []byte {
		img := image.NewNRGBA(image.Rect(0, 0, width, height))
		for y := range height {
			for x := range width {
				img.SetNRGBA(x, y, color.NRGBA{R: byte(x), G: byte(y), B: 120, A: 255})
			}
		}
		var buffer bytes.Buffer
		var err error
		if format == "png" {
			err = png.Encode(&buffer, img)
		} else {
			err = jpeg.Encode(&buffer, img, nil)
		}
		if err != nil {
			t.Fatal(err)
		}
		return buffer.Bytes()
	}
	pngImage := encode(64, 64, "png")
	webp, err := os.ReadFile("testdata/icon.webp")
	if err != nil {
		t.Fatal(err)
	}
	animation := append([]byte("RIFF\x16\x00\x00\x00WEBPVP8X\x0a\x00\x00\x00"), []byte{2, 0, 0, 0, 63, 0, 0, 63, 0, 0}...)
	bomb := append([]byte{}, pngImage[:33]...)
	binary.BigEndian.PutUint32(bomb[16:20], 1<<30)
	binary.BigEndian.PutUint32(bomb[20:24], 1<<30)
	binary.BigEndian.PutUint32(bomb[29:33], crc32.ChecksumIEEE(bomb[12:29]))
	metadata := []byte("secret GPS sentinel")
	chunk := binary.BigEndian.AppendUint32(nil, uint32(len(metadata)))
	chunk = append(chunk, []byte("tEXt")...)
	chunk = append(chunk, metadata...)
	chunk = binary.BigEndian.AppendUint32(chunk, crc32.ChecksumIEEE(chunk[4:]))
	withMetadata := append(append(append([]byte{}, pngImage[:33]...), chunk...), pngImage[33:]...)
	jpg := encode(96, 96, "jpeg")
	exif := append([]byte("Exif\x00\x00"), metadata...)
	segment := append([]byte{0xff, 0xe1}, binary.BigEndian.AppendUint16(nil, uint16(len(exif)+2))...)
	segment = append(segment, exif...)
	jpg = append(append(append([]byte{}, jpg[:2]...), segment...), jpg[2:]...)
	for _, test := range []struct {
		name    string
		content []byte
		failure string
	}{
		{"PNG", pngImage, ""}, {"JPEG EXIF", jpg, ""}, {"WebP", webp, ""}, {"extended WebP", webPWithCanvas(webp[12:]), ""}, {"PNG named jpg", pngImage, ""}, {"PNG metadata", withMetadata, ""}, {"crop", encode(70, 64, "png"), ""},
		{"SVG", []byte(`<svg width="64" height="64"/>`), "SVG and GIF"}, {"GIF", []byte("GIF89a"), "SVG and GIF"}, {"animated WebP", animation, "animated"},
		{"size", make([]byte, MaxBytes+1), "256 KB"}, {"63 px", encode(63, 64, "png"), "64 to 1024"}, {"1025 px", encode(1025, 1024, "png"), "64 to 1024"},
		{"rectangle", encode(128, 64, "png"), "square"}, {"bomb before decode", bomb, "64 to 1024"}, {"truncated", pngImage[:40], "decoded"},
	} {
		t.Run(test.name, func(t *testing.T) {
			output, err := Normalize(test.content)
			if test.failure != "" {
				if err == nil || !strings.Contains(err.Error(), test.failure) {
					t.Fatalf("error = %v, want %q", err, test.failure)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			config, format, err := image.DecodeConfig(bytes.NewReader(output))
			if err != nil || format != "png" || config.Width != 128 || config.Height != 128 {
				t.Fatalf("output = %+v, %s, %v", config, format, err)
			}
			for offset := 8; offset < len(output); {
				size := int(binary.BigEndian.Uint32(output[offset : offset+4]))
				kind := string(output[offset+4 : offset+8])
				if kind != "IHDR" && kind != "IDAT" && kind != "IEND" {
					t.Fatalf("metadata chunk %s", kind)
				}
				offset += 12 + size
			}
			if bytes.Contains(output, metadata) {
				t.Fatal("metadata survived")
			}
		})
	}
}

func TestWebPRejectsConflictingFrameDimensions(t *testing.T) {
	t.Parallel()
	for _, codec := range []struct {
		name    string
		maxSide int
	}{{"VP8 ", 16383}, {"VP8L", 16384}} {
		for _, dimensions := range []struct {
			name          string
			width, height int
		}{
			{"oversized width", 1025, 64},
			{"oversized height", 64, 1025},
			{"maximum frame", codec.maxSide, codec.maxSide},
			{"bounded mismatch", 128, 64},
		} {
			t.Run(codec.name+"/"+dimensions.name, func(t *testing.T) {
				var header []byte
				if codec.name == "VP8 " {
					header = []byte{0x10, 0, 0, 0x9d, 0x01, 0x2a}
					header = binary.LittleEndian.AppendUint16(header, uint16(dimensions.width))
					header = binary.LittleEndian.AppendUint16(header, uint16(dimensions.height))
				} else {
					header = binary.LittleEndian.AppendUint32([]byte{0x2f}, uint32(dimensions.width-1)|uint32(dimensions.height-1)<<14)
				}
				chunk := binary.LittleEndian.AppendUint32([]byte(codec.name), uint32(len(header)))
				chunk = append(chunk, header...)
				if len(header)%2 != 0 {
					chunk = append(chunk, 0)
				}
				content := webPWithCanvas(chunk)
				config, format, err := image.DecodeConfig(bytes.NewReader(content))
				if err != nil || format != "webp" || config.Width != 64 || config.Height != 64 {
					t.Fatalf("canvas = %+v, %s, %v", config, format, err)
				}
				decoded, _, err := image.Decode(bytes.NewReader(content))
				if decoded != nil || err == nil || err.Error() != "webp: invalid format" {
					t.Fatalf("frame was not rejected before reading pixel data: image = %v, error = %v", decoded, err)
				}
				output, err := Normalize(content)
				if output != nil || err == nil || !strings.Contains(err.Error(), "could not be decoded") {
					t.Fatalf("Normalize() = %d bytes, %v; want conflicting frame rejected", len(output), err)
				}
			})
		}
	}
}

func webPWithCanvas(chunks []byte) []byte {
	content := append([]byte("RIFF\x00\x00\x00\x00WEBPVP8X\x0a\x00\x00\x00"), []byte{0, 0, 0, 0, 63, 0, 0, 63, 0, 0}...)
	content = append(content, chunks...)
	binary.LittleEndian.PutUint32(content[4:8], uint32(len(content)-8))
	return content
}

func TestIconChoices(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		icon  Icon
		valid bool
	}{
		{"automatic", Icon{Kind: "automatic"}, true}, {"lucide", Icon{Kind: "lucide", Name: "rocket", Color: "emerald"}, true}, {"unknown name", Icon{Kind: "lucide", Name: "made-up", Color: "blue"}, false},
		{"multiple names", Icon{Kind: "lucide", Name: "a-arrow-down\na-arrow-up", Color: "blue"}, false}, {"unknown color", Icon{Kind: "lucide", Name: "rocket", Color: "made-up"}, false},
		{"emoji", Icon{Kind: "emoji", Emoji: "🚀"}, true}, {"family grapheme", Icon{Kind: "emoji", Emoji: "👨‍👩‍👧‍👦"}, true}, {"flag", Icon{Kind: "emoji", Emoji: "🇺🇸"}, true}, {"two emoji", Icon{Kind: "emoji", Emoji: "🚀🌍"}, false},
		{"control", Icon{Kind: "emoji", Emoji: "\n"}, false}, {"empty", Icon{Kind: "emoji"}, false}, {"mixed", Icon{Kind: "automatic", Name: "rocket"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.icon.Validate(); (err == nil) != test.valid {
				t.Fatalf("Validate() = %v, want valid %v", err, test.valid)
			}
		})
	}
}
