package projectcolor

import "testing"

func TestNormalizeAcceptsOpaqueCSSHexColors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value string
		want  string
	}{
		{name: "six digit", value: "#1192e8", want: "#1192e8"},
		{name: "uppercase", value: "#A63F7A", want: "#a63f7a"},
		{name: "three digit", value: "#0aF", want: "#00aaff"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, ok := Normalize(tt.value)
			if !ok {
				t.Fatalf("Normalize(%q) ok = false, want true", tt.value)
			}
			if got != tt.want {
				t.Fatalf("Normalize(%q) = %q, want %q", tt.value, got, tt.want)
			}
		})
	}
}

func TestNormalizeRejectsMalformedColors(t *testing.T) {
	t.Parallel()

	for _, value := range []string{"", "1192e8", "#1192e", "#1192e8ff", "#zzzzzz", "blue"} {
		t.Run(value, func(t *testing.T) {
			t.Parallel()

			if got, ok := Normalize(value); ok {
				t.Fatalf("Normalize(%q) = %q, true; want invalid", value, got)
			}
		})
	}
}

func TestColorForIDIsStableAndOrderIndependent(t *testing.T) {
	t.Parallel()

	leftFirst := []string{ColorForID("detent"), ColorForID("docs-site"), ColorForID("billing-api")}
	rightFirst := []string{ColorForID("billing-api"), ColorForID("detent"), ColorForID("docs-site")}

	if leftFirst[0] != rightFirst[1] {
		t.Fatalf("detent color shifted: %q vs %q", leftFirst[0], rightFirst[1])
	}
	if leftFirst[1] != rightFirst[2] {
		t.Fatalf("docs-site color shifted: %q vs %q", leftFirst[1], rightFirst[2])
	}
	if leftFirst[2] != rightFirst[0] {
		t.Fatalf("billing-api color shifted: %q vs %q", leftFirst[2], rightFirst[0])
	}
	for _, color := range leftFirst {
		if _, ok := Normalize(color); !ok {
			t.Fatalf("ColorForID() = %q, want normalized hex", color)
		}
	}
}

func TestColorForUsesConfiguredColorWhenPresent(t *testing.T) {
	t.Parallel()

	if got := ColorFor("detent", "#0aF"); got != "#00aaff" {
		t.Fatalf("ColorFor() = %q, want configured normalized color", got)
	}
}
