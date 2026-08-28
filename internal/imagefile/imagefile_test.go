package imagefile

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writePNG(t *testing.T, dir, name string, width, height int) string {
	t.Helper()
	canvas := image.NewRGBA(image.Rect(0, 0, width, height))
	canvas.Set(0, 0, color.RGBA{R: 1, G: 2, B: 3, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, canvas); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return write(t, dir, name, buf.Bytes())
}

// writeNoisePNG writes a random-noise PNG: deflate can't compress noise, so the file
// size tracks the raw pixel count — a reliable way to exceed MaxUploadBytes.
func writeNoisePNG(t *testing.T, dir, name string, width, height int) string {
	t.Helper()
	canvas := image.NewRGBA(image.Rect(0, 0, width, height))
	rng := rand.New(rand.NewSource(42))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			canvas.SetRGBA(x, y, color.RGBA{
				R: uint8(rng.Intn(256)),
				G: uint8(rng.Intn(256)),
				B: uint8(rng.Intn(256)),
				A: 255,
			})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, canvas); err != nil {
		t.Fatalf("encode noise png: %v", err)
	}
	return write(t, dir, name, buf.Bytes())
}

func write(t *testing.T, dir, name string, content []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func TestProbeReadsRealImageHeader(t *testing.T) {
	dir := t.TempDir()
	path := writePNG(t, dir, "shot.png", 64, 32)

	info, err := Probe(path)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if info.FileName != "shot.png" {
		t.Errorf("FileName = %q, want shot.png", info.FileName)
	}
	if info.ContentType != "image/png" {
		t.Errorf("ContentType = %q, want image/png", info.ContentType)
	}
	if info.Width != 64 || info.Height != 32 {
		t.Errorf("dimensions = %dx%d, want 64x32", info.Width, info.Height)
	}
	if info.Size == 0 {
		t.Error("Size must be reported for upload-limit warnings")
	}
	if info.Path != path {
		t.Errorf("Path = %q, want %q", info.Path, path)
	}
}

// TestProbeTrustsBytesOverExtension: 用户把 png 存成 .jpg 很常见，而 prepare 申报的
// contentType 会被算进预签名，报错的扩展名会让 PUT 直接被对象存储拒掉。
func TestProbeTrustsBytesOverExtension(t *testing.T) {
	dir := t.TempDir()
	canvas := image.NewRGBA(image.Rect(0, 0, 8, 8))
	var buf bytes.Buffer
	if err := png.Encode(&buf, canvas); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	path := write(t, dir, "actually-png.jpg", buf.Bytes())

	info, err := Probe(path)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if info.ContentType != "image/png" {
		t.Errorf("ContentType = %q, want image/png from the magic bytes", info.ContentType)
	}
}

// TestProbeFallsBackToExtension covers formats the stdlib cannot decode: the upload still
// has to work, only Width/Height stay unknown.
func TestProbeFallsBackToExtension(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name     string
		content  []byte
		wantType string
	}{
		// heic 的前 512 字节会被嗅探成 application/octet-stream，只能靠扩展名。
		{name: "photo.heic", content: append([]byte("\x00\x00\x00\x18ftypheic"), bytes.Repeat([]byte{0}, 64)...), wantType: "image/heic"},
		{name: "photo.avif", content: append([]byte("\x00\x00\x00\x1cftypavif"), bytes.Repeat([]byte{0}, 64)...), wantType: "image/avif"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			info, err := Probe(write(t, dir, test.name, test.content))
			if err != nil {
				t.Fatalf("Probe: %v", err)
			}
			if info.ContentType != test.wantType {
				t.Errorf("ContentType = %q, want %q", info.ContentType, test.wantType)
			}
			if info.Width != 0 || info.Height != 0 {
				t.Errorf("dimensions = %dx%d, want 0x0 for an undecodable format", info.Width, info.Height)
			}
		})
	}
}

// TestProbeRejectsNonImages stops a mistyped path from being uploaded and pre-charging
// quota for a task the server will refuse anyway.
func TestProbeRejectsNonImages(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name    string
		path    string
		wantMsg string
	}{
		{name: "text file", path: write(t, dir, "notes.txt", []byte("just some prompt notes")), wantMsg: "does not look like an image"},
		{name: "text disguised as png", path: write(t, dir, "notes.png", []byte("just some prompt notes")), wantMsg: "detected text/plain"},
		{name: "pdf disguised as jpg", path: write(t, dir, "document.jpg", []byte("%PDF-1.7\nnot an image")), wantMsg: "detected application/pdf"},
		{name: "empty file", path: write(t, dir, "empty.png", nil), wantMsg: "is empty"},
		{name: "directory", path: dir, wantMsg: "is a directory"},
		{name: "missing file", path: filepath.Join(dir, "nope.png"), wantMsg: "nope.png"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Probe(test.path)
			if err == nil {
				t.Fatalf("Probe(%s) should fail", test.path)
			}
			if !strings.Contains(err.Error(), test.wantMsg) {
				t.Errorf("error = %q, want it to mention %q", err, test.wantMsg)
			}
		})
	}
}

// TestProbeMissingFileErrorIsNotDoubleWrapped: *fs.PathError 自带 "open <路径>"，
// 再包一层会打印成 "open x: open x: no such file"。
func TestProbeMissingFileErrorIsNotDoubleWrapped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nope.png")
	_, err := Probe(path)
	if err == nil {
		t.Fatal("Probe should fail for a missing file")
	}
	if strings.Count(err.Error(), path) != 1 {
		t.Errorf("error = %q, want the path to appear exactly once", err)
	}
}

func TestCompressIfNeededPassesThroughSmallFiles(t *testing.T) {
	path := writePNG(t, t.TempDir(), "small.png", 64, 64)
	info, err := Probe(path)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	payload, err := CompressIfNeeded(info)
	if err != nil {
		t.Fatalf("compress: %v", err)
	}
	if payload.Reencoded {
		t.Error("a small file must be uploaded untouched, not re-encoded")
	}
	if payload.Data != nil {
		t.Errorf("Data = %d bytes, want nil for passthrough", len(payload.Data))
	}
	if payload.Path != path {
		t.Errorf("Path = %q, want %q", payload.Path, path)
	}
	if payload.ContentType != "image/png" {
		t.Errorf("ContentType = %q, want image/png", payload.ContentType)
	}
	if payload.Width != 64 || payload.Height != 64 {
		t.Errorf("dims = %dx%d, want 64x64", payload.Width, payload.Height)
	}
}

func TestCompressIfNeededReencodesOversizedFiles(t *testing.T) {
	path := writeNoisePNG(t, t.TempDir(), "big.png", 2200, 2200)
	info, err := Probe(path)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if info.Size <= MaxUploadBytes {
		t.Fatalf("test image is %d bytes, not over the %d limit", info.Size, MaxUploadBytes)
	}
	payload, err := CompressIfNeeded(info)
	if err != nil {
		t.Fatalf("compress: %v", err)
	}
	if !payload.Reencoded {
		t.Fatal("an oversized file must be re-encoded")
	}
	if payload.ContentType != "image/jpeg" {
		t.Errorf("ContentType = %q, want image/jpeg", payload.ContentType)
	}
	if len(payload.Data) == 0 {
		t.Error("Data is empty")
	}
	if int64(len(payload.Data)) > MaxUploadBytes {
		t.Errorf("re-encoded payload is %d bytes, still over %d", len(payload.Data), MaxUploadBytes)
	}
	// 质量循环成功时不应降采样：尺寸保持原样。
	if payload.Width != 2200 || payload.Height != 2200 {
		t.Errorf("dims = %dx%d, want 2200x2200 (quality loop should suffice)", payload.Width, payload.Height)
	}
	// 重编码结果必须是合法 JPEG，且能解回原尺寸。
	decoded, format, err := image.Decode(bytes.NewReader(payload.Data))
	if err != nil {
		t.Fatalf("decode re-encoded payload: %v", err)
	}
	if format != "jpeg" {
		t.Errorf("re-encoded format = %q, want jpeg", format)
	}
	if decoded.Bounds().Dx() != 2200 || decoded.Bounds().Dy() != 2200 {
		t.Errorf("decoded dims = %v, want 2200x2200", decoded.Bounds())
	}
}

func TestDownsampleToFitScalesDownPreservingAspectRatio(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 4000, 2000))
	// 填几个采样点，验证 box 平均保留了内容（非全黑）。
	src.Set(0, 0, color.RGBA{R: 255, A: 255})
	src.Set(3999, 0, color.RGBA{G: 255, A: 255})
	src.Set(1999, 1999, color.RGBA{B: 255, A: 255})

	out := downsampleToFit(src, 1920)
	if out.Rect.Dx() != 1920 || out.Rect.Dy() != 960 {
		t.Errorf("dims = %dx%d, want 1920x960 (2:1 kept)", out.Rect.Dx(), out.Rect.Dy())
	}
	// 缩放后内容仍有非零像素，说明不是全黑退化。
	if !hasNonBlackPixel(out) {
		t.Error("downsample produced an all-black image")
	}

	// 已能装下时原样返回，不重开缓冲区。
	if downsampleToFit(src, 4000) != src {
		t.Error("downsampleToFit must return src unchanged when it already fits")
	}
}

func hasNonBlackPixel(img *image.RGBA) bool {
	for y := img.Rect.Min.Y; y < img.Rect.Max.Y; y++ {
		for x := img.Rect.Min.X; x < img.Rect.Max.X; x++ {
			c := img.RGBAAt(x, y)
			if c.R != 0 || c.G != 0 || c.B != 0 {
				return true
			}
		}
	}
	return false
}

func TestCompressIfNeededReportsUndecodableOversizedFiles(t *testing.T) {
	// 超过 10 MiB、带 PNG 魔数但内容非法的文件：Probe 认得出是 image/png，但 Decode
	// 会失败。此时必须报错而不是把原文件丢给服务器吃 413。
	dir := t.TempDir()
	path := write(t, dir, "fake.png", append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0x00}, MaxUploadBytes+1)...))
	info, err := Probe(path)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	_, err = CompressIfNeeded(info)
	if err == nil {
		t.Fatal("CompressIfNeeded should fail on an undecodable oversized image")
	}
	if !strings.Contains(err.Error(), "cannot be re-encoded") {
		t.Errorf("error = %q, want a 'cannot be re-encoded' hint", err)
	}
}

func TestPayloadFromBase64ValidatesAndProbesImages(t *testing.T) {
	path := writePNG(t, t.TempDir(), "reference.png", 3, 2)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read png: %v", err)
	}
	encoded := base64.StdEncoding.EncodeToString(raw)

	payload, err := PayloadFromBase64(encoded)
	if err != nil {
		t.Fatalf("raw base64: %v", err)
	}
	if payload.ContentType != "image/png" || payload.Width != 3 || payload.Height != 2 {
		t.Errorf("payload metadata = %s %dx%d, want image/png 3x2", payload.ContentType, payload.Width, payload.Height)
	}
	if !bytes.Equal(payload.Data, raw) || payload.Reencoded {
		t.Error("a small decoded image must keep its original bytes")
	}

	// The bytes are authoritative when a data URI declares the wrong image subtype.
	mismatched, err := PayloadFromBase64("data:image/jpeg;base64," + encoded)
	if err != nil {
		t.Fatalf("mismatched declared MIME: %v", err)
	}
	if mismatched.ContentType != "image/png" {
		t.Errorf("ContentType = %q, want detected image/png", mismatched.ContentType)
	}

	for name, input := range map[string]string{
		"non-image":           base64.StdEncoding.EncodeToString([]byte("plain text")),
		"empty decoded data":  "data:image/png;base64,",
		"missing base64 flag": "data:image/png,abcd",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := PayloadFromBase64(input); err == nil {
				t.Fatalf("PayloadFromBase64(%q) should fail", input)
			}
		})
	}
}

func TestPayloadFromBase64ReencodesOversizedImage(t *testing.T) {
	path := writeNoisePNG(t, t.TempDir(), "large.png", 2200, 2200)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read png: %v", err)
	}
	if len(raw) <= MaxUploadBytes {
		t.Fatalf("fixture is %d bytes, want over %d", len(raw), MaxUploadBytes)
	}
	payload, err := PayloadFromBase64(base64.StdEncoding.EncodeToString(raw))
	if err != nil {
		t.Fatalf("PayloadFromBase64: %v", err)
	}
	if !payload.Reencoded || payload.ContentType != "image/jpeg" {
		t.Errorf("payload = reencoded:%v type:%q, want reencoded JPEG", payload.Reencoded, payload.ContentType)
	}
	if len(payload.Data) == 0 || len(payload.Data) > MaxUploadBytes {
		t.Errorf("re-encoded payload size = %d, want 1..%d", len(payload.Data), MaxUploadBytes)
	}
}
