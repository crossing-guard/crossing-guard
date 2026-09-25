package taskinput

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/png"
	"os"

	_ "golang.org/x/image/webp"
)

func normalizeImage(sourcePath, mediaType string, limits Limits, makeTemp func() (*os.File, error)) (string, int64, error) {
	raw, err := os.ReadFile(sourcePath)
	if err != nil {
		return "", 0, err
	}
	if hasICCProfile(raw, mediaType) {
		return "", 0, inputError("unsupported_color_profile", "embedded ICC color profiles are not supported safely")
	}
	if animatedImage(raw, mediaType) {
		return "", 0, inputError("animated_image", "animated images are not supported")
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return "", 0, inputError("invalid_image", "image header is invalid: %v", err)
	}
	if config.Width <= 0 || config.Height <= 0 || config.Width > limits.MaxImageDimension || config.Height > limits.MaxImageDimension || int64(config.Width)*int64(config.Height) > limits.MaxImagePixels {
		return "", 0, inputError("image_dimensions", "image dimensions exceed configured limits")
	}
	decoded, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return "", 0, inputError("invalid_image", "image could not be fully decoded: %v", err)
	}
	orientation, err := imageOrientation(raw, mediaType)
	if err != nil {
		return "", 0, inputError("invalid_orientation", "%v", err)
	}
	normalized := orientImage(decoded, orientation)
	output, err := makeTemp()
	if err != nil {
		return "", 0, err
	}
	path := output.Name()
	if err := png.Encode(output, normalized); err != nil {
		_ = output.Close()
		_ = os.Remove(path)
		return "", 0, err
	}
	if err := output.Sync(); err != nil {
		_ = output.Close()
		_ = os.Remove(path)
		return "", 0, err
	}
	if err := output.Close(); err != nil {
		_ = os.Remove(path)
		return "", 0, err
	}
	info, err := os.Stat(path)
	if err != nil {
		_ = os.Remove(path)
		return "", 0, err
	}
	if info.Size() > limits.MaxNormalizedBytesPerItem {
		_ = os.Remove(path)
		return "", 0, inputError("normalized_too_large", "normalized image exceeds configured limit")
	}
	check, err := os.Open(path)
	if err != nil {
		_ = os.Remove(path)
		return "", 0, err
	}
	verified, err := png.Decode(check)
	_ = check.Close()
	if err != nil || !samePixels(normalized, verified) {
		_ = os.Remove(path)
		return "", 0, inputError("normalization_failed", "normalized image verification failed")
	}
	return path, info.Size(), nil
}

func animatedImage(raw []byte, mediaType string) bool {
	switch mediaType {
	case "image/gif":
		decoded, err := gif.DecodeAll(bytes.NewReader(raw))
		return err == nil && len(decoded.Image) != 1
	case "image/png":
		return bytes.Contains(raw, []byte("acTL"))
	case "image/webp":
		return bytes.Contains(raw, []byte("ANIM")) || bytes.Contains(raw, []byte("ANMF"))
	default:
		return false
	}
}

func hasICCProfile(raw []byte, mediaType string) bool {
	switch mediaType {
	case "image/png":
		return bytes.Contains(raw, []byte("iCCP"))
	case "image/jpeg":
		return bytes.Contains(raw, []byte("ICC_PROFILE\x00"))
	case "image/webp":
		return bytes.Contains(raw, []byte("ICCP"))
	default:
		return false
	}
}

func imageOrientation(raw []byte, mediaType string) (int, error) {
	if mediaType != "image/jpeg" {
		return 1, nil
	}
	for offset := 2; offset+4 <= len(raw) && raw[offset] == 0xff; {
		marker := raw[offset+1]
		offset += 2
		if marker == 0xd9 || marker == 0xda {
			break
		}
		if offset+2 > len(raw) {
			break
		}
		length := int(binary.BigEndian.Uint16(raw[offset:]))
		if length < 2 || offset+length > len(raw) {
			return 0, fmt.Errorf("malformed JPEG metadata")
		}
		segment := raw[offset+2 : offset+length]
		if marker == 0xe1 && len(segment) >= 6 && bytes.Equal(segment[:6], []byte("Exif\x00\x00")) {
			return exifOrientation(segment[6:])
		}
		offset += length
	}
	return 1, nil
}

func exifOrientation(tiff []byte) (int, error) {
	if len(tiff) < 8 {
		return 0, fmt.Errorf("truncated EXIF metadata")
	}
	var order binary.ByteOrder
	switch string(tiff[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return 0, fmt.Errorf("invalid EXIF byte order")
	}
	if order.Uint16(tiff[2:4]) != 42 {
		return 0, fmt.Errorf("invalid EXIF header")
	}
	offset := int(order.Uint32(tiff[4:8]))
	if offset < 0 || offset+2 > len(tiff) {
		return 0, fmt.Errorf("invalid EXIF directory")
	}
	count := int(order.Uint16(tiff[offset : offset+2]))
	for index := 0; index < count; index++ {
		entry := offset + 2 + index*12
		if entry+12 > len(tiff) {
			return 0, fmt.Errorf("truncated EXIF directory")
		}
		if order.Uint16(tiff[entry:entry+2]) != 0x0112 {
			continue
		}
		if order.Uint16(tiff[entry+2:entry+4]) != 3 || order.Uint32(tiff[entry+4:entry+8]) != 1 {
			return 0, fmt.Errorf("invalid EXIF orientation")
		}
		value := int(order.Uint16(tiff[entry+8 : entry+10]))
		if value < 1 || value > 8 {
			return 0, fmt.Errorf("invalid EXIF orientation %d", value)
		}
		return value, nil
	}
	return 1, nil
}

func orientImage(source image.Image, orientation int) *image.NRGBA {
	bounds := source.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	outWidth, outHeight := width, height
	if orientation >= 5 {
		outWidth, outHeight = height, width
	}
	result := image.NewNRGBA(image.Rect(0, 0, outWidth, outHeight))
	for y := 0; y < outHeight; y++ {
		for x := 0; x < outWidth; x++ {
			sx, sy := x, y
			switch orientation {
			case 2:
				sx = width - 1 - x
			case 3:
				sx, sy = width-1-x, height-1-y
			case 4:
				sy = height - 1 - y
			case 5:
				sx, sy = y, x
			case 6:
				sx, sy = y, height-1-x
			case 7:
				sx, sy = width-1-y, height-1-x
			case 8:
				sx, sy = width-1-y, x
			}
			result.SetNRGBA(x, y, color.NRGBAModel.Convert(source.At(bounds.Min.X+sx, bounds.Min.Y+sy)).(color.NRGBA))
		}
	}
	return result
}

func samePixels(left, right image.Image) bool {
	if left.Bounds().Dx() != right.Bounds().Dx() || left.Bounds().Dy() != right.Bounds().Dy() {
		return false
	}
	for y := 0; y < left.Bounds().Dy(); y++ {
		for x := 0; x < left.Bounds().Dx(); x++ {
			l := color.NRGBAModel.Convert(left.At(left.Bounds().Min.X+x, left.Bounds().Min.Y+y)).(color.NRGBA)
			r := color.NRGBAModel.Convert(right.At(right.Bounds().Min.X+x, right.Bounds().Min.Y+y)).(color.NRGBA)
			if l != r {
				return false
			}
		}
	}
	return true
}
