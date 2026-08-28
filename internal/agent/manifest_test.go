package agent

import (
	"strings"
	"testing"
)

func TestManifestRejectsInvalidContracts(t *testing.T) {
	cases := []struct {
		name    string
		change  func(*Session)
		message string
	}{
		{name: "unsafe name", change: func(s *Session) { s.Name = "Not Safe" }, message: "name"},
		{name: "missing prompt", change: func(s *Session) { s.Prompt = "" }, message: "prompt"},
		{name: "unknown mode", change: func(s *Session) { s.Mode = "daemon" }, message: "unsupported"},
		{name: "negative clone depth", change: func(s *Session) { s.CloneDepth = -1 }, message: "clone depth"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			session := validSession()
			test.change(&session)
			if _, err := Manifest(session); err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("got error %v, want message containing %q", err, test.message)
			}
		})
	}
}

func validSession() Session {
	return Session{
		Name:        "example",
		Namespace:   DefaultNamespace,
		Image:       DefaultImage,
		Mode:        ModeUpdate,
		Ref:         "main",
		Prompt:      "inspect the repository",
		CloneDepth:  1,
		StorageSize: "1Gi",
		Timeout:     600,
	}
}
