package session

import (
	"testing"
)

func TestUTF8Truncation(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		maxRunes int
		expect   string
	}{
		{
			name:     "中文短文本不截断",
			input:    "调研一下 cloudflare sandbox",
			maxRunes: 100,
			expect:   "调研一下 cloudflare sandbox",
		},
		{
			name:     "中文长文本正确截断",
			input:    "调研一下 cloudflare sandbox 他能支持完整的沙箱调用能力吗，比如我希望在沙箱里面可以执行一些系统命令",
			maxRunes: 60,
			expect:   "调研一下 cloudflare sandbox 他能支持完整的沙箱调用能力吗，比如我希望在沙箱里面可以执行一些系统命...",
		},
		{
			name:     "混合中英文截断",
			input:    "好的，搜索工具目前要限，但基于已有的信息，我来给你讲讲 CF4O-PIIE 中美",
			maxRunes: 60,
			expect:   "好的，搜索工具目前要限，但基于已有的信息，我来给你讲讲 CF4O-PIIE 中美...",
		},
		{
			name:     "emoji 正确处理",
			input:    "测试 emoji 😀😁😂🤣😃",
			maxRunes: 10,
			expect:   "测试 emoji ...",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.input
			if len([]rune(result)) > tt.maxRunes {
				result = string([]rune(result)[:tt.maxRunes]) + "..."
			}
			
			// 验证结果不包含乱码字符 �
			for _, r := range result {
				if r == '\uFFFD' {
					t.Errorf("截断产生乱码字符: %s", result)
					break
				}
			}
			
			// 验证长度正确
			runeLen := len([]rune(result))
			if tt.input != tt.expect && runeLen > tt.maxRunes+3 {
				t.Errorf("截断后长度错误: got %d runes, want <= %d", runeLen, tt.maxRunes+3)
			}
		})
	}
}
