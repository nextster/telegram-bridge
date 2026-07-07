package bot

import "testing"

func TestNormalizePhone(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "empty", in: "", want: ""},
		{name: "spaces", in: "   ", want: ""},
		{name: "already international", in: "+995 555-12-34-56", want: "+995555123456"},
		{name: "digits", in: "995 555 12 34 56", want: "+995555123456"},
		{name: "parentheses", in: "(995) 555-12-34-56", want: "+995555123456"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizePhone(tt.in); got != tt.want {
				t.Fatalf("normalizePhone(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
