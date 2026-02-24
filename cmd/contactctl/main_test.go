package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunMainProgramWithInput_HelpAndVersion(t *testing.T) {
	t.Parallel()

	t.Run("help", func(t *testing.T) {
		t.Parallel()
		var stdout, stderr bytes.Buffer
		code := runMainProgramWithInput("contactctl", []string{"--help"}, map[string]string{}, strings.NewReader(""), &stdout, &stderr)
		if code != 0 {
			t.Fatalf("code=%d want 0 stderr=%q", code, stderr.String())
		}
		if got := stdout.String(); !strings.Contains(got, "usage: contactctl user <add|list|delete|passwd>") {
			t.Fatalf("help missing admin usage: %q", got)
		}
		if stderr.Len() != 0 {
			t.Fatalf("stderr=%q want empty", stderr.String())
		}
	})

	t.Run("version", func(t *testing.T) {
		t.Parallel()
		var stdout, stderr bytes.Buffer
		code := runMainProgramWithInput("contactctl", []string{"version", "--format", "text"}, map[string]string{}, strings.NewReader(""), &stdout, &stderr)
		if code != 0 {
			t.Fatalf("code=%d want 0 stderr=%q", code, stderr.String())
		}
		if got := stdout.String(); !strings.Contains(got, "contactctl ") {
			t.Fatalf("version output=%q want contactctl prefix", got)
		}
		if stderr.Len() != 0 {
			t.Fatalf("stderr=%q want empty", stderr.String())
		}
	})
}
