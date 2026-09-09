package llm

import "testing"

func TestTruncate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input  string
		maxLen int
		want   string
	}{
		{"hello", 10, "hello"},
		{"hello world", 5, "hello..."},
		{"", 5, ""},
		{"hi", 0, "..."},
		{"你好世界测试", 3, "你好世..."},
		{"🎉🚀💡🔧🛠", 2, "🎉🚀..."},
		{"abc", 3, "abc"},
	}
	for _, tt := range tests {
		if got := Truncate(tt.input, tt.maxLen); got != tt.want {
			t.Errorf("Truncate(%q, %d) = %q, want %q", tt.input, tt.maxLen, got, tt.want)
		}
	}
}
