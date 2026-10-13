package agent

import (
	"strings"
	"testing"
)

// A Tail keeps the last bytes written, however they're split, and shows
// them as text.
func TestTail(t *testing.T) {
	for name, tt := range map[string]struct {
		writes []string
		want   string
	}{
		"short":         {writes: []string{"ab", "cd"}, want: "abcd"},
		"one long":      {writes: []string{"abcdefgh"}, want: "efgh"},
		"spills over":   {writes: []string{"abc", "def", "g"}, want: "defg"},
		"exactly full":  {writes: []string{"ab", "cd"}, want: "abcd"},
		"control chars": {writes: []string{"a\x1b\n\tb"}, want: "�\n\tb"},
		"cut rune":      {writes: []string{"€€"}, want: "�€"},
	} {
		t.Run(name, func(t *testing.T) {
			tail := NewTail(4)
			total := 0
			for _, w := range tt.writes {
				if n, err := tail.Write([]byte(w)); n != len(w) || err != nil {
					t.Fatalf("Write(%q) = %d, %v", w, n, err)
				}
				total += len(w)
			}
			if tail.Text() != tt.want || tail.Total() != int64(total) || tail.Len() > 4 {
				t.Errorf("Text, Total, Len = %q, %d, %d; want %q, %d and at most 4", tail.Text(), tail.Total(), tail.Len(), tt.want, total)
			}
		})
	}
	tail := NewTail(4096)
	for range 100 {
		if _, err := tail.Write([]byte(strings.Repeat("x", 1000))); err != nil {
			t.Fatal(err)
		}
	}
	if tail.Len() != 4096 || cap(tail.buf) > 2*4096 {
		t.Errorf("after 100 KB: holds %d bytes in %d; want 4096, bounded", tail.Len(), cap(tail.buf))
	}
}
