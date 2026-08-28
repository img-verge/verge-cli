package imagefile

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"math"
	"net/http"
	"os"
	"strings"
)

// 对象存储的预签名 PUT 对单文件有硬上限：实测超限会得到
// HTTP 413 {"error":"file too large","max_bytes":10485760}。文档只写了公网 URL
// 参考图的 10MB 限制，但本地直传同样被这道门卡住 —— 压缩算法就是为了把超限的
// 本地文件塞进这 10 MiB。
const MaxUploadBytes = 10 << 20 // 10 MiB，与服务器 max_bytes 一致

// Payload describes what to upload for one reference image.
//
// Reencoded 为 true 时 Data 是重编码后的 JPEG 字节，调用方必须 PUT 这些字节并把
// ContentType/Width/Height 声明进 prepare；Data 非 nil 时（包括 base64 解码）也走
// PutBytes；否则原样保留，调用方照旧流式上传 Path。
type Payload struct {
	Path         string
	Data         []byte
	ContentType  string
	Width        int
	Height       int
	Reencoded    bool
	OriginalSize int64
}

// CompressIfNeeded re-encodes a local image to JPEG when it exceeds MaxUploadBytes:
// progressive quality reduction (95 down to 10 in steps of 5), then downsampling.
//
// 只用标准库：image/jpeg 的 Options.Quality 原生支持质量参数，降采样用手写的 box
// 平均（标准库 image/draw 只有 Draw 没有缩放）。重编码会丢弃透明通道和动图帧，
// 参考图场景可接受；无法解码的格式（webp/heic 等标准库不支持）直接报错，让用户
// 先转码，而不是把原文件丢给服务器吃 413。
func CompressIfNeeded(info Info) (Payload, error) {
	if info.Size <= MaxUploadBytes {
		return Payload{
			Path:        info.Path,
			ContentType: info.ContentType,
			Width:       info.Width,
			Height:      info.Height,
		}, nil
	}

	raw, err := os.ReadFile(info.Path)
	if err != nil {
		return Payload{}, err
	}
	return reencodeBytesToFit(info.Path, raw)
}

// reencodeBytesToFit is shared by oversized local files and decoded base64 images.
// Callers have already established that raw exceeds MaxUploadBytes.
func reencodeBytesToFit(label string, raw []byte) (Payload, error) {
	img, _, decodeErr := image.Decode(bytes.NewReader(raw))
	if decodeErr != nil {
		return Payload{}, fmt.Errorf(
			"%s is %.1f MiB, over the %d MiB per-file storage limit, and cannot be re-encoded to fit: %v",
			label, float64(len(raw))/(1<<20), MaxUploadBytes>>20, decodeErr,
		)
	}

	bounds := img.Bounds()
	// 丢弃 alpha：JPEG 没有透明通道，透明像素保留原 RGB 值，Go 的 JPEG 编码器只取
	// RGB 分量。
	rgb := image.NewRGBA(bounds)
	draw.Draw(rgb, bounds, img, bounds.Min, draw.Src)

	// 主路径：原图渐进降质（95→10 每步 5），取第一个压进 10 MiB 的质量档。
	if data, err := jpegUnderLimit(rgb); err != nil {
		return Payload{}, fmt.Errorf("re-encode %s: %w", label, err)
	} else if data != nil {
		return Payload{
			Data:         data,
			ContentType:  "image/jpeg",
			Width:        bounds.Dx(),
			Height:       bounds.Dy(),
			Reencoded:    true,
			OriginalSize: int64(len(raw)),
		}, nil
	}

	// 兜底：主循环没压进限内（罕见的大噪点图），逐档降采样再压。box 平均降采样保持
	// 宽高比、只缩不拉。逐档缩小直到必然进得了 10 MiB，保证返回值不会再次触发对象
	// 存储的 413。
	maxDim := 1920
	for maxDim >= 128 {
		rgb = downsampleToFit(rgb, maxDim)
		if data, err := jpegUnderLimit(rgb); err != nil {
			return Payload{}, fmt.Errorf("re-encode %s: %w", label, err)
		} else if data != nil {
			return Payload{
				Data:         data,
				ContentType:  "image/jpeg",
				Width:        rgb.Rect.Dx(),
				Height:       rgb.Rect.Dy(),
				Reencoded:    true,
				OriginalSize: int64(len(raw)),
			}, nil
		}
		maxDim /= 2
	}
	return Payload{}, fmt.Errorf("re-encode %s: could not fit under %d MiB", label, MaxUploadBytes>>20)
}

// jpegUnderLimit encodes rgb as JPEG with progressively lower quality (95 to 10, step 5)
// and returns the first result at most MaxUploadBytes. It returns (nil, nil) when no
// quality fits, and (nil, err) only when encoding itself fails.
func jpegUnderLimit(rgb *image.RGBA) ([]byte, error) {
	for q := 95; q >= 10; q -= 5 {
		buf := &bytes.Buffer{}
		if err := jpeg.Encode(buf, rgb, &jpeg.Options{Quality: q}); err != nil {
			return nil, err
		}
		if buf.Len() <= MaxUploadBytes {
			return buf.Bytes(), nil
		}
	}
	return nil, nil
}

// downsampleToFit returns a box-averaged copy of src scaled down so its longer side
// is at most maxDim, preserving aspect ratio; src is returned unchanged when it already
// fits. Box averaging sums each destination pixel over its source rectangle, which is
// cheap when scaling far down (the only case this runs in).
func downsampleToFit(src *image.RGBA, maxDim int) *image.RGBA {
	srcW := src.Rect.Dx()
	srcH := src.Rect.Dy()
	if srcW <= maxDim && srcH <= maxDim {
		return src
	}
	scale := math.Min(float64(maxDim)/float64(srcW), float64(maxDim)/float64(srcH))
	dstW := int(math.Floor(float64(srcW) * scale))
	dstH := int(math.Floor(float64(srcH) * scale))
	if dstW < 1 {
		dstW = 1
	}
	if dstH < 1 {
		dstH = 1
	}

	dst := image.NewRGBA(image.Rect(0, 0, dstW, dstH))
	min := src.Rect.Min
	for y := 0; y < dstH; y++ {
		sy0 := min.Y + y*srcH/dstH
		sy1 := min.Y + (y+1)*srcH/dstH
		for x := 0; x < dstW; x++ {
			sx0 := min.X + x*srcW/dstW
			sx1 := min.X + (x+1)*srcW/dstW
			var r, g, b, n uint32
			for sy := sy0; sy < sy1; sy++ {
				for sx := sx0; sx < sx1; sx++ {
					c := src.RGBAAt(sx, sy)
					r += uint32(c.R)
					g += uint32(c.G)
					b += uint32(c.B)
					n++
				}
			}
			if n == 0 {
				continue
			}
			dst.SetRGBA(x, y, color.RGBA{
				R: uint8(r / n),
				G: uint8(g / n),
				B: uint8(b / n),
				A: 255,
			})
		}
	}
	return dst
}

// PayloadFromBase64 decodes a data: URI or raw base64 string and returns a Payload
// ready for PUT via PutBytesWithOptions. Recognizable image bytes determine the content type;
// an explicit image/* data URI is only used for formats the standard library cannot sniff.
func PayloadFromBase64(input string) (Payload, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return Payload{}, fmt.Errorf("empty base64 input")
	}

	declaredType := ""
	encoded := input
	if strings.HasPrefix(input, "data:") {
		comma := strings.Index(input, ",")
		if comma < 0 {
			return Payload{}, fmt.Errorf("invalid data: URI: missing comma")
		}
		metadata := input[len("data:"):comma]
		parts := strings.Split(metadata, ";")
		declaredType = strings.ToLower(strings.TrimSpace(parts[0]))
		if !strings.HasPrefix(declaredType, "image/") {
			return Payload{}, fmt.Errorf("data: URI media type must start with image/")
		}
		hasBase64 := false
		for _, part := range parts[1:] {
			if strings.EqualFold(strings.TrimSpace(part), "base64") {
				hasBase64 = true
				break
			}
		}
		if !hasBase64 {
			return Payload{}, fmt.Errorf("data: URI must use ;base64 encoding")
		}
		encoded = input[comma+1:]
	}

	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return Payload{}, fmt.Errorf("base64 decode: %w", err)
	}
	if len(decoded) == 0 {
		return Payload{}, fmt.Errorf("decoded base64 image is empty")
	}

	detectedType := http.DetectContentType(decoded)
	contentType := ""
	switch {
	case strings.HasPrefix(detectedType, "image/"):
		contentType = detectedType
	case declaredType != "" && detectedType == "application/octet-stream":
		// HEIC/AVIF and other valid formats may not be recognized by the standard
		// library sniffer. An explicit image/* data URI keeps them usable.
		contentType = declaredType
	default:
		return Payload{}, fmt.Errorf("decoded base64 data does not look like an image (detected %s)", detectedType)
	}

	width, height := 0, 0
	if config, _, err := image.DecodeConfig(bytes.NewReader(decoded)); err == nil {
		width, height = config.Width, config.Height
	}
	if len(decoded) <= MaxUploadBytes {
		return Payload{
			Data:        decoded,
			ContentType: contentType,
			Width:       width,
			Height:      height,
		}, nil
	}
	return reencodeBytesToFit("decoded base64 image", decoded)
}
