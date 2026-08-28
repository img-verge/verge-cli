// Package imagefile inspects local image files before they are uploaded.
//
// 只用标准库：宽高靠 image.DecodeConfig 只读文件头，内容类型先按真实字节嗅探再退回
// 扩展名。宽高在 prepare 阶段是可选字段，探测不出来就留空，不该因此拒绝上传。
package imagefile

import (
	"bufio"
	"errors"
	"fmt"
	"image"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	// 注册标准库支持的解码器，供 image.DecodeConfig 读取宽高。
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
)

// Info describes one local image file.
type Info struct {
	Path        string
	FileName    string
	Size        int64
	ContentType string
	// Width/Height 为 0 表示未能探测（webp、heic 等标准库不支持解码的格式）。
	Width  int
	Height int
}

// extensionTypes covers image formats Go's builtin mime table or content sniffer miss.
//
// 不依赖系统 /etc/mime.types：容器和 Windows 上未必存在，同一份代码的行为必须一致。
var extensionTypes = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".jpe":  "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
	".bmp":  "image/bmp",
	".heic": "image/heic",
	".heif": "image/heif",
	".avif": "image/avif",
	".tif":  "image/tiff",
	".tiff": "image/tiff",
}

// Probe reads just enough of path to report its content type and dimensions.
func Probe(path string) (Info, error) {
	file, err := os.Open(path)
	if err != nil {
		// *fs.PathError 自己就带 "open <路径>"，再包一层只会把路径打印两遍。
		return Info{}, err
	}
	defer file.Close()

	stat, err := file.Stat()
	if err != nil {
		return Info{}, err
	}
	if stat.IsDir() {
		return Info{}, fmt.Errorf("%s is a directory, not an image file", path)
	}
	if stat.Size() == 0 {
		return Info{}, fmt.Errorf("%s is empty", path)
	}

	info := Info{
		Path:     path,
		FileName: filepath.Base(path),
		Size:     stat.Size(),
	}

	// 一个 bufio.Reader 同时喂给嗅探和 DecodeConfig：Peek 不消耗数据，
	// 所以不需要 Seek 回头，也就不要求输入必须可 Seek。
	reader := bufio.NewReaderSize(file, 1024)
	head, err := reader.Peek(512)
	if err != nil && !errors.Is(err, io.EOF) {
		return Info{}, fmt.Errorf("read %s: %w", path, err)
	}
	info.ContentType = detectContentType(head, path)
	if !strings.HasPrefix(info.ContentType, "image/") {
		return Info{}, fmt.Errorf(
			"%s does not look like an image (detected %s); the API only accepts image/* reference files",
			path, info.ContentType,
		)
	}

	if config, _, err := image.DecodeConfig(reader); err == nil {
		info.Width = config.Width
		info.Height = config.Height
	}
	return info, nil
}

// detectContentType prefers what the bytes actually say, then falls back to extension.
//
// http.DetectContentType 认得 png/jpeg/gif/webp/bmp，但 heic/heif/avif 会被判成
// application/octet-stream，这些只能靠扩展名兜。
func detectContentType(head []byte, path string) string {
	if len(head) == 0 {
		return "application/octet-stream"
	}

	sniffed := http.DetectContentType(head)
	if strings.HasPrefix(sniffed, "image/") {
		return sniffed
	}
	// Extension compatibility is only safe when the sniffer has no stronger
	// opinion. It keeps unsupported-but-valid formats such as HEIC/AVIF usable
	// without allowing text or PDF files to masquerade as images.
	if sniffed != "application/octet-stream" {
		return sniffed
	}
	if mapped, ok := extensionTypes[strings.ToLower(filepath.Ext(path))]; ok {
		return mapped
	}
	return sniffed
}
