package bellwether

import (
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestGreet(t *testing.T) {
	t.Parallel()
	if got := Greet(); got != "hello" {
		t.Errorf("Greet() = %q, want %q", got, "hello")
	}
}

func TestToLabel(t *testing.T) {
	t.Parallel()
	// Generic instantiation per case keeps the table simple.
	tests := []struct {
		name string
		got  string
		want string
	}{
		{name: "string", got: ToLabel("x"), want: "label: x"},
		{name: "int", got: ToLabel(42), want: "label: 42"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(tt.want, tt.got); diff != "" {
				t.Errorf("ToLabel() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
