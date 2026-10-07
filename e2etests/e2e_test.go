//go:build e2e

// Package e2etests deploys a fixture package to a real Chalk environment with
// the chalkfn CLI, calls it through the API server, and deletes it.
//
// Run with Chalk credentials in the environment (see the README):
//
//	go test -tags e2e -v -timeout 40m ./e2etests
package e2etests

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type harness struct {
	t      *testing.T
	bin    string
	root   string
	suffix string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	if os.Getenv("CHALK_CLIENT_ID") == "" || os.Getenv("CHALK_CLIENT_SECRET") == "" {
		t.Skip("CHALK_CLIENT_ID and CHALK_CLIENT_SECRET are not set")
	}
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "chalkfn")
	build := exec.Command("go", "build", "-o", bin, "./cmd/chalkfn")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building chalkfn: %v\n%s", err, out)
	}
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return &harness{t: t, bin: bin, root: root, suffix: hex.EncodeToString(b)}
}

func (h *harness) name(base string) string { return "e2e_" + base + "_" + h.suffix }

// run executes the CLI from the repository root and returns stdout and stderr.
func (h *harness) run(args ...string) (string, string, error) {
	h.t.Helper()
	cmd := exec.Command(h.bin, args...)
	cmd.Dir = h.root
	cmd.Env = append(os.Environ(), "CHALKFN_E2E_SUFFIX="+h.suffix)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return strings.TrimSpace(stdout.String()), stderr.String(), err
}

func (h *harness) mustRun(args ...string) (string, string) {
	h.t.Helper()
	stdout, stderr, err := h.run(args...)
	if err != nil {
		h.t.Fatalf("chalkfn %s: %v\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), err, stdout, stderr)
	}
	return stdout, stderr
}

func (h *harness) deploy(extra ...string) string {
	h.t.Helper()
	args := append([]string{"deploy",
		"--cpu", "250m", "--memory", "512Mi",
		"--env", "CHALKFN_E2E_SUFFIX=" + h.suffix,
	}, extra...)
	args = append(args, "./e2etests/fixture")
	_, stderr := h.mustRun(args...)
	h.t.Logf("deploy %v:\n%s", extra, stderr)
	return stderr
}

func (h *harness) call(fn, input string) (string, error) {
	h.t.Helper()
	stdout, stderr, err := h.run("call", "--function", fn, "--input", input)
	if err != nil {
		return "", &callError{msg: stderr}
	}
	return stdout, nil
}

type callError struct{ msg string }

func (e *callError) Error() string { return e.msg }

func TestDeployCallRedeploy(t *testing.T) {
	h := newHarness(t)
	functions := []string{h.name("add_one"), h.name("divide"), h.name("mixed")}
	t.Cleanup(func() {
		for _, fn := range functions {
			if _, stderr, err := h.run("delete", "--function", fn); err != nil {
				t.Logf("deleting %s: %v\n%s", fn, err, stderr)
			}
		}
	})

	h.deploy()

	cases := []struct {
		fn, input, want string
	}{
		{h.name("add_one"), `{"x":[1,2,null,42]}`, `{"result": [2,3,null,43]}`},
		{h.name("divide"), `{"numerator":[1,3],"denominator":[4,null]}`, `{"result": [0.25,null]}`},
		{h.name("mixed"), `{"n":[7,8],"s":["go",null],"b":[true,false],"f":[1.5,2]}`, `{"result": ["7|go|true|1.50",null]}`},
		{h.name("add_one"), `{"x":[]}`, `{"result": []}`},
	}
	for _, c := range cases {
		got, err := h.call(c.fn, c.input)
		if err != nil {
			t.Errorf("call %s %s: %v", c.fn, c.input, err)
			continue
		}
		if got != c.want {
			t.Errorf("call %s %s = %s, want %s", c.fn, c.input, got, c.want)
		}
	}

	if _, err := h.call(h.name("divide"), `{"numerator":[1],"denominator":[0]}`); err == nil || !strings.Contains(err.Error(), "division by zero") {
		t.Errorf("dividing by zero: got error %v, want one mentioning division by zero", err)
	}

	// A redeploy of unchanged sources reuses the cached image and adds a
	// version to the function's existing scaling group.
	stderr := h.deploy("--function", h.name("add_one"))
	for _, want := range []string{"cached=true", "version=2"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("redeploy output lacks %q", want)
		}
	}
	if got, err := h.call(h.name("add_one"), `{"x":[9]}`); err != nil || got != `{"result": [10]}` {
		t.Errorf("call after redeploy = %s, %v", got, err)
	}
}
