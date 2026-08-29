package sessionmanifest

import (
	"strings"
	"testing"
)

func TestRenderRejectsInvalidContracts(t *testing.T) {
	cases := []struct {
		name    string
		change  func(*Spec)
		message string
	}{
		{name: "unsafe name", change: func(s *Spec) { s.Name = "Not Safe" }, message: "name"},
		{name: "missing prompt", change: func(s *Spec) { s.Prompt = "" }, message: "prompt"},
		{name: "unknown mode", change: func(s *Spec) { s.Mode = "daemon" }, message: "unsupported"},
		{name: "negative clone depth", change: func(s *Spec) { s.CloneDepth = -1 }, message: "clone depth"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			spec := validSpec()
			test.change(&spec)
			if _, err := Render(spec); err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("got error %v, want message containing %q", err, test.message)
			}
		})
	}
}

func validSpec() Spec {
	return Spec{
		Name:           "example",
		Namespace:      DefaultNamespace,
		Image:          DefaultImage,
		Mode:           ModeUpdate,
		InitialRef:     "main",
		Prompt:         "inspect the repository",
		CloneDepth:     1,
		StorageSize:    "1Gi",
		TimeoutSeconds: 600,
	}
}
