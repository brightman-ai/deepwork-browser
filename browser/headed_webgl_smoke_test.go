package browser

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHeadedWebGLSmoke(t *testing.T) {
	if os.Getenv("DW_BROWSER_HEADED_WEBGL_SMOKE") != "1" {
		t.Skip("set DW_BROWSER_HEADED_WEBGL_SMOKE=1 to run headed WebGL smoke")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	profileID := "webgl-smoke-" + time.Now().Format("20060102150405")
	bc, err := NewBrowserCore(ctx, profileID, WithMode(ModeHeaded), WithViewport(1280, 720))
	if err != nil {
		t.Fatalf("NewBrowserCore headed: %v", err)
	}
	defer func() {
		_ = bc.Close(context.Background())
		if home, err := os.UserHomeDir(); err == nil {
			_ = os.RemoveAll(filepath.Join(home, ".deepwork", "browser-cli", profileID))
		}
	}()

	if _, err := bc.Navigate(ctx, "about:blank"); err != nil {
		t.Fatalf("Navigate: %v", err)
	}

	var webgl string
	err = bc.EvalJS(ctx, `(() => {
		const canvas = document.createElement('canvas');
		const gl = canvas.getContext('webgl') || canvas.getContext('experimental-webgl');
		if (!gl) return 'NO_WEBGL';
		const ext = gl.getExtension('WEBGL_debug_renderer_info');
		const vendor = ext ? gl.getParameter(ext.UNMASKED_VENDOR_WEBGL) : gl.getParameter(gl.VENDOR);
		const renderer = ext ? gl.getParameter(ext.UNMASKED_RENDERER_WEBGL) : gl.getParameter(gl.RENDERER);
		return 'WEBGL_OK|' + vendor + '|' + renderer;
	})()`, &webgl)
	if err != nil {
		t.Fatalf("EvalJS webgl: %v", err)
	}
	if !strings.HasPrefix(webgl, "WEBGL_OK|") {
		t.Fatalf("headed WebGL unavailable: %s", webgl)
	}
	t.Logf("headed WebGL: %s", webgl)
}
