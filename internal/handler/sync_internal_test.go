package handler

import "testing"

func TestExtractTokensFromResponse(t *testing.T) {
	tests := []struct {
		name               string
		body               []byte
		expectedPrompt     int64
		expectedCompletion int64
	}{
		{
			name:               "prompt and completion present",
			body:               []byte(`{"usage":{"prompt_tokens":120,"completion_tokens":45,"total_tokens":165}}`),
			expectedPrompt:     120,
			expectedCompletion: 45,
		},
		{
			name:               "total_tokens fallback",
			body:               []byte(`{"usage":{"total_tokens":50}}`),
			expectedPrompt:     50,
			expectedCompletion: 0,
		},
		{
			name:               "usage absent",
			body:               []byte(`{"text":"hello"}`),
			expectedPrompt:     0,
			expectedCompletion: 0,
		},
		{
			name:               "empty body",
			body:               nil,
			expectedPrompt:     0,
			expectedCompletion: 0,
		},
		{
			name:               "malformed JSON",
			body:               []byte(`not json`),
			expectedPrompt:     0,
			expectedCompletion: 0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotPrompt, gotCompletion := extractTokensFromResponse(tc.body)
			if gotPrompt != tc.expectedPrompt {
				t.Errorf("prompt: got %d, want %d", gotPrompt, tc.expectedPrompt)
			}
			if gotCompletion != tc.expectedCompletion {
				t.Errorf("completion: got %d, want %d", gotCompletion, tc.expectedCompletion)
			}
		})
	}
}
