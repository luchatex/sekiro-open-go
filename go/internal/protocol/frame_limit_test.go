package protocol

import "testing"

func TestSetMaxFrameBytes(t *testing.T) {
	old := MaxFrameBytes()
	defer SetMaxFrameBytes(old)

	SetMaxFrameBytes(8 << 20)
	if got := MaxFrameBytes(); got != 8<<20 {
		t.Fatalf("configured limit = %d, want %d", got, 8<<20)
	}
	// 非正值回退默认，避免误配导致所有请求被拒。
	SetMaxFrameBytes(0)
	if got := MaxFrameBytes(); got != DefaultMaxFrameBytes {
		t.Fatalf("fallback limit = %d, want %d", got, DefaultMaxFrameBytes)
	}
}
