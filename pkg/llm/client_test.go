package llm

import "testing"

func TestNormalizeAnthropicBaseURL(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "append v1",
			in:   "https://dashscope.aliyuncs.com/apps/anthropic",
			want: "https://dashscope.aliyuncs.com/apps/anthropic/v1",
		},
		{
			name: "keep existing v1",
			in:   "https://api.anthropic.com/v1",
			want: "https://api.anthropic.com/v1",
		},
		{
			name: "trim trailing slash",
			in:   "https://example.com/v1/",
			want: "https://example.com/v1",
		},
		{
			name: "empty input",
			in:   "   ",
			want: "",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeAnthropicBaseURL(tc.in)
			if got != tc.want {
				t.Fatalf("normalizeAnthropicBaseURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseLLMResponse(t *testing.T) {
	testCases := []struct {
		name    string
		input   string
		wantMap map[int]string
		wantOK  bool
	}{
		{
			name:    "single id resolution",
			input:   "<analysis>x</analysis><resolution id=\"0\">\n\tfunc Add(x int) int {\n\t\treturn x + 1\n\t}\n</resolution>",
			wantMap: map[int]string{0: "\tfunc Add(x int) int {\n\t\treturn x + 1\n\t}"},
			wantOK:  true,
		},
		{
			name:    "multiple id resolution",
			input:   "<resolution id=\"0\">a</resolution><resolution id=\"1\">b</resolution>",
			wantMap: map[int]string{0: "a", 1: "b"},
			wantOK:  true,
		},
		{
			name:   "missing id format",
			input:  "<resolution>abc</resolution>",
			wantOK: false,
		},
		{
			name:   "duplicate id",
			input:  "<resolution id=\"0\">a</resolution><resolution id=\"0\">b</resolution>",
			wantOK: false,
		},
		{
			name:   "empty content",
			input:  "<resolution id=\"0\">   </resolution>",
			wantOK: false,
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			result, err := ParseLLMResponse(tc.input)
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}

			if result.Success != tc.wantOK {
				t.Fatalf("ParseLLMResponse success=%v, want %v", result.Success, tc.wantOK)
			}

			if tc.wantOK {
				if len(result.ResolvedMap) != len(tc.wantMap) {
					t.Fatalf("resolved map len=%d, want %d", len(result.ResolvedMap), len(tc.wantMap))
				}
				for id, want := range tc.wantMap {
					got, ok := result.ResolvedMap[id]
					if !ok {
						t.Fatalf("missing id %d in resolved map", id)
					}
					if got != want {
						t.Fatalf("resolvedMap[%d]=%q, want %q", id, got, want)
					}
				}
			} else if len(result.ResolvedMap) != 0 {
				t.Fatalf("expected empty resolved map for failed case, got=%v", result.ResolvedMap)
			}
		})
	}
}
