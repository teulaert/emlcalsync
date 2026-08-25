package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSkillPrints(t *testing.T) {
	env := newTestEnv(t)
	out := env.MustRun("skill")
	if !strings.HasPrefix(out, "---\nname: emlcal\n") {
		t.Fatalf("skill output does not start with the frontmatter:\n%.200s", out)
	}
	if !strings.Contains(out, "description:") {
		t.Error("frontmatter has no description")
	}
	for _, want := range []string{
		"emlcal mail list", "emlcal mail search", "emlcal mail read", "emlcal mail thread",
		"emlcal cal agenda", "emlcal cal free", "emlcal mail reply", "emlcal status",
		"<account>:<remote>", "<account>:t:<thread>", "<account>:c:<calendar>:<event>",
		"--dry-run", "thread_id", "my_response",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("skill does not mention %q", want)
		}
	}
	if n := strings.Count(out, "\n"); n > 200 {
		t.Errorf("skill is %d lines, want at most 200", n)
	}
}

func TestSkillInstall(t *testing.T) {
	env := newTestEnv(t)
	out, errOut, code := env.Run("skill", "--install")
	if code != 0 {
		t.Fatalf("skill --install exit = %d: %s", code, errOut)
	}
	path := filepath.Join(env.Dir, ".claude", "skills", "emlcal", "SKILL.md")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("skill not installed: %v", err)
	}
	if !strings.Contains(string(b), "name: emlcal") {
		t.Errorf("installed file is not the skill:\n%.200s", b)
	}
	if !strings.Contains(out, path) {
		t.Errorf("stdout did not report the path: %s", out)
	}
	for _, want := range []string{`"permissions"`, `Bash(emlcal mail list*)`, `Bash(emlcal mail send*)`, `"ask"`} {
		if !strings.Contains(errOut, want) {
			t.Errorf("suggested permissions do not contain %q:\n%s", want, errOut)
		}
	}
}
