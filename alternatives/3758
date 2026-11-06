package projecticon

import (
	"bytes"
	_ "embed"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	"image/png"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/rivo/uniseg"
	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

const MaxBytes = 256 * 1024

//go:embed lucide.txt
var lucideNames string

type Icon struct {
	Kind        string `json:"kind"`
	Name        string `json:"name,omitempty"`
	Color       string `json:"color,omitempty"`
	Emoji       string `json:"emoji,omitempty"`
	ImageKey    string `json:"image_key,omitempty"`
	ImageSHA256 string `json:"image_sha256,omitempty"`
	URL         string `json:"url,omitempty"`
}

func (i Icon) Validate() error {
	switch i.Kind {
	case "automatic":
		if i.Name == "" && i.Color == "" && i.Emoji == "" && i.ImageKey == "" {
			return nil
		}
	case "lucide":
		if slices.Contains(strings.Split(strings.TrimSpace(lucideNames), "\n"), i.Name) && i.Name != "" && slices.Contains(strings.Split("gray red orange amber yellow lime green emerald teal cyan sky blue indigo violet purple fuchsia pink rose", " "), i.Color) && i.Color != "" && i.Emoji == "" && i.ImageKey == "" {
			return nil
		}
	case "emoji":
		if utf8.ValidString(i.Emoji) && strings.IndexFunc(i.Emoji, unicode.IsControl) == -1 && len(i.Emoji) <= 128 && strings.TrimSpace(i.Emoji) != "" && uniseg.GraphemeClusterCount(i.Emoji) == 1 && i.Name == "" && i.Color == "" && i.ImageKey == "" {
			return nil
		}
	case "image":
		if i.Name == "" && i.Color == "" && i.Emoji == "" {
			return nil
		}
	}
	return errors.New("choose a Lucide icon and color, a single emoji, an image, or automatic")
}

func Normalize(content []byte) ([]byte, error) {
	if len(content) > MaxBytes {
		return nil, errors.New("the limit is 256 KB")
	}
	var format string
	switch {
	case bytes.HasPrefix(content, []byte("\x89PNG\r\n\x1a\n")):
		format = "png"
	case bytes.HasPrefix(content, []byte{0xff, 0xd8, 0xff}):
		format = "jpeg"
	case len(content) >= 12 && string(content[:4]) == "RIFF" && string(content[8:12]) == "WEBP":
		format = "webp"
		for offset := 12; offset < len(content); {
			if len(content)-offset < 8 {
				return nil, errors.New("the WebP header is invalid")
			}
			kind := string(content[offset : offset+4])
			declared := binary.LittleEndian.Uint32(content[offset+4 : offset+8])
			if uint64(declared) > uint64(len(content)-offset-8) {
				return nil, errors.New("the WebP header is invalid")
			}
			if kind == "ANIM" || kind == "ANMF" || kind == "VP8X" && declared > 0 && content[offset+8]&2 != 0 {
				return nil, errors.New("animated WebP is not supported")
			}
			size := int(declared)
			offset += 8 + size + (size & 1)
		}
	default:
		return nil, errors.New("use a PNG, JPEG or WebP image; SVG and GIF are not supported")
	}
	if format == "png" && len(content) >= 24 {
		width, height := int(binary.BigEndian.Uint32(content[16:20])), int(binary.BigEndian.Uint32(content[20:24]))
		if width < 64 || height < 64 || width > 1024 || height > 1024 {
			return nil, fmt.Errorf("the image is %d × %d; each side must be 64 to 1024 px", width, height)
		}
	}
	config, detected, err := image.DecodeConfig(bytes.NewReader(content))
	if err != nil || detected != format {
		return nil, errors.New("the image header is invalid")
	}
	if config.Width < 64 || config.Height < 64 || config.Width > 1024 || config.Height > 1024 {
		return nil, fmt.Errorf("the image is %d × %d; each side must be 64 to 1024 px", config.Width, config.Height)
	}
	if max(config.Width, config.Height)*10 > min(config.Width, config.Height)*11 {
		return nil, errors.New("the image must be square within 10%")
	}
	source, _, err := image.Decode(bytes.NewReader(content))
	if err != nil {
		return nil, errors.New("the image could not be decoded")
	}
	bounds := source.Bounds()
	side := min(bounds.Dx(), bounds.Dy())
	crop := image.Rect(bounds.Min.X+(bounds.Dx()-side)/2, bounds.Min.Y+(bounds.Dy()-side)/2, bounds.Min.X+(bounds.Dx()+side)/2, bounds.Min.Y+(bounds.Dy()+side)/2)
	output := image.NewNRGBA(image.Rect(0, 0, 128, 128))
	draw.CatmullRom.Scale(output, output.Bounds(), source, crop, draw.Src, nil)
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, output); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}
