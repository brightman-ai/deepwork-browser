// Package pdftool 是 PDF/office 文档的本地渲染与词级定位工具链（poppler +
// libreoffice）。不依赖浏览器：pdf preview/excerpt 的采集层。
//
// 语义与 web excerpt 同构：红框 = 显式 boxes ∪ 命中词并集，荧光 = 命中词
// 逐词覆盖，裁剪 = 框 + margin。坐标单位 pt（72dpi 基），渲染像素 = pt ×
// dpi / 72。
package pdftool

import (
	"context"
	"fmt"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/text/unicode/norm"
)

// Word 是 pdftotext -bbox 的一个词（pt 坐标，页面左上原点）。
type Word struct {
	XMin, YMin, XMax, YMax float64
	Text                   string
}

// PageInfo 是 pdfinfo 摘要。
type PageInfo struct {
	Pages  int
	Width  float64 // pt，首页尺寸
	Height float64
}

// ErrMissingTool 报依赖缺失（fail-loud：提示安装，不静默降级）。
type ErrMissingTool struct{ Tool, Install string }

func (e *ErrMissingTool) Error() string {
	return fmt.Sprintf("pdftool: %s not found (install %s)", e.Tool, e.Install)
}

func lookPath(tool, install string) (string, error) {
	p, err := exec.LookPath(tool)
	if err != nil {
		return "", &ErrMissingTool{Tool: tool, Install: install}
	}
	return p, nil
}

// IsPDF 按扩展名判断（转换分流用）。
func IsPDF(path string) bool {
	return strings.EqualFold(filepath.Ext(path), ".pdf")
}

// ConvertToPDF 用 soffice 把 office 文档转 PDF。独立 UserInstallation profile
// 避免并发锁死（soffice 经典坑：同 profile 二次启动直接退出）。
func ConvertToPDF(ctx context.Context, src string) (string, error) {
	soffice, err := lookPath("soffice", "libreoffice (e.g. apt install libreoffice)")
	if err != nil {
		return "", err
	}
	outDir, err := os.MkdirTemp("", "dwpdf-conv-*")
	if err != nil {
		return "", fmt.Errorf("pdftool: temp dir: %w", err)
	}
	profile, err := os.MkdirTemp("", "dwpdf-profile-*")
	if err != nil {
		return "", fmt.Errorf("pdftool: temp profile: %w", err)
	}
	c, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(c, soffice, "--headless", "--norestore", "--nolockcheck",
		fmt.Sprintf("-env:UserInstallation=file://%s", profile),
		"--convert-to", "pdf", "--outdir", outDir, src)
	out, err := cmd.CombinedOutput()
	if err != nil {
		os.RemoveAll(outDir)
		os.RemoveAll(profile)
		return "", fmt.Errorf("pdftool: soffice convert failed: %w; output: %s", err, truncate(string(out), 400))
	}
	os.RemoveAll(profile)
	prod := strings.TrimSuffix(filepath.Base(src), filepath.Ext(src)) + ".pdf"
	path := filepath.Join(outDir, prod)
	if _, err := os.Stat(path); err != nil {
		os.RemoveAll(outDir)
		return "", fmt.Errorf("pdftool: soffice produced no %q; output: %s", prod, truncate(string(out), 400))
	}
	return path, nil
}

// CleanupConverted 释放 ConvertToPDF 的临时产物（调用方 defer）。
func CleanupConverted(pdfPath string) {
	if pdfPath != "" && strings.Contains(filepath.Base(filepath.Dir(pdfPath)), "dwpdf-conv-") {
		os.RemoveAll(filepath.Dir(pdfPath))
	}
}

// Info 解析 pdfinfo。
func Info(pdfPath string) (PageInfo, error) {
	if _, err := lookPath("pdfinfo", "poppler-utils"); err != nil {
		return PageInfo{}, err
	}
	out, err := exec.Command("pdfinfo", pdfPath).Output()
	if err != nil {
		return PageInfo{}, fmt.Errorf("pdftool: pdfinfo failed: %w", err)
	}
	var info PageInfo
	for _, line := range strings.Split(string(out), "\n") {
		k, v, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		v = strings.TrimSpace(v)
		switch strings.TrimSpace(k) {
		case "Pages":
			info.Pages, _ = strconv.Atoi(v)
		case "Page size":
			// "612 x 792 pts (letter)"
			m := regexp.MustCompile(`([0-9.]+) x ([0-9.]+)`).FindStringSubmatch(v)
			if m != nil {
				info.Width, _ = strconv.ParseFloat(m[1], 64)
				info.Height, _ = strconv.ParseFloat(m[2], 64)
			}
		}
	}
	if info.Pages <= 0 {
		return PageInfo{}, fmt.Errorf("pdftool: pdfinfo reported no pages for %s", pdfPath)
	}
	return info, nil
}

// RenderPage 渲染第 page 页为 PNG（1 起），返回字节。
func RenderPage(pdfPath string, page, dpi int) ([]byte, error) {
	if _, err := lookPath("pdftoppm", "poppler-utils"); err != nil {
		return nil, err
	}
	if page < 1 {
		return nil, fmt.Errorf("pdftool: page must be >= 1, got %d", page)
	}
	tmp, err := os.MkdirTemp("", "dwpdf-render-*")
	if err != nil {
		return nil, fmt.Errorf("pdftool: temp dir: %w", err)
	}
	defer os.RemoveAll(tmp)
	prefix := filepath.Join(tmp, "pg")
	cmd := exec.Command("pdftoppm", "-png", "-r", strconv.Itoa(dpi), "-f", strconv.Itoa(page), "-l", strconv.Itoa(page), pdfPath, prefix)
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("pdftool: pdftoppm failed: %w; output: %s", err, truncate(string(out), 400))
	}
	matches, _ := filepath.Glob(prefix + "-*.png")
	if len(matches) == 0 {
		return nil, fmt.Errorf("pdftool: pdftoppm produced no page image for page %d", page)
	}
	return os.ReadFile(matches[0])
}

var wordRe = regexp.MustCompile(`<word xMin="([\d.]+)" yMin="([\d.]+)" xMax="([\d.]+)" yMax="([\d.]+)">(.*?)</word>`)

// Words 解析 pdftotext -bbox 的第 page 页词表。
func Words(pdfPath string, page int) ([]Word, error) {
	if _, err := lookPath("pdftotext", "poppler-utils"); err != nil {
		return nil, err
	}
	out, err := exec.Command("pdftotext", "-f", strconv.Itoa(page), "-l", strconv.Itoa(page), "-bbox", pdfPath, "-").Output()
	if err != nil {
		return nil, fmt.Errorf("pdftool: pdftotext failed: %w", err)
	}
	var words []Word
	for _, m := range wordRe.FindAllStringSubmatch(string(out), -1) {
		x0, _ := strconv.ParseFloat(m[1], 64)
		y0, _ := strconv.ParseFloat(m[2], 64)
		x1, _ := strconv.ParseFloat(m[3], 64)
		y1, _ := strconv.ParseFloat(m[4], 64)
		words = append(words, Word{XMin: x0, YMin: y0, XMax: x1, YMax: y1, Text: NormalizeWord(html.UnescapeString(m[5]))})
	}
	return words, nil
}

// NormalizeWord 归一词面：HTML 实体、NFKC、异体破折号/引号 → ASCII。
func NormalizeWord(s string) string {
	s = norm.NFKC.String(s)
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '–', '—', '−':
			r = '-'
		case '‘', '’', '‚':
			r = '\''
		case '“', '”', '„':
			r = '"'
		}
		b.WriteRune(r)
	}
	return b.String()
}

// NormalizePhrase 归一匹配短语并切词（空白分隔）。
func NormalizePhrase(s string) []string {
	s = NormalizeWord(s)
	return strings.Fields(s)
}

// matchKey 是词比较键：归一词面再剥首尾标点（pdftotext 词与原文短语的
// 标点粘连容忍：'parameters,' ≡ 'parameters'）。
func matchKey(s string) string {
	return strings.Trim(NormalizeWord(s), ",.:;!?\"'()[]{}<>")
}

// FindPhrase 在词表里找短语的连续词序匹配，返回命中词（含 bbox）。跨行/
// 跨列天然支持（词序与视觉顺序一致）。未命中返回 nil。
func FindPhrase(words []Word, phrase string) []Word {
	toks := NormalizePhrase(phrase)
	if len(toks) == 0 {
		return nil
	}
	keys := make([]string, len(words))
	for i, w := range words {
		keys[i] = matchKey(w.Text)
	}
	want := make([]string, len(toks))
	for i, t := range toks {
		want[i] = matchKey(t)
		if want[i] == "" {
			return nil // 短语切成纯标点 = 无法定位
		}
	}
	for i := 0; i+len(toks) <= len(words); i++ {
		ok := true
		for k := range want {
			if keys[i+k] != want[k] {
				ok = false
				break
			}
		}
		if ok {
			return words[i : i+len(toks)]
		}
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
