package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"sekiro-go/internal/version"
)

func TestOpPageShowsVersion(t *testing.T) {
	old := version.Version
	t.Cleanup(func() { version.Version = old })
	version.Version = "1.2.3-test"

	rec := httptest.NewRecorder()
	handleOpPage(rec)
	if rec.Code != http.StatusOK {
		t.Fatalf("op page status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "__SEKIRO_VERSION__") {
		t.Fatal("监控页仍保留版本占位符")
	}
	if !strings.Contains(body, "1.2.3-test") {
		t.Fatal("监控页未显示构建时注入的版本号")
	}
}
