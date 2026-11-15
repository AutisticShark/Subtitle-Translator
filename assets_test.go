package subtitletranslator

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

type catalog struct {
	Locale   string            `json:"locale"`
	Messages map[string]string `json:"messages"`
}

func TestCatalogCoverageAndPlaceholders(t *testing.T) {
	messages := map[string]bool{}
	literal := regexp.MustCompile(`\bt\(\s*'((?:\\.|[^'\\])*)'`)
	labels := regexp.MustCompile(`\blabel:\s*'((?:\\.|[^'\\])*)'`)
	err := filepath.WalkDir("web/src", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || (!strings.HasSuffix(path, ".svelte") && !strings.HasSuffix(path, ".ts")) {
			return err
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, expression := range []*regexp.Regexp{literal, labels} {
			for _, match := range expression.FindAllStringSubmatch(string(source), -1) {
				messages[strings.ReplaceAll(match[1], `\'`, `'`)] = true
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range []string{
		"Providers", "Translation", "Limits", "Access", "Users", "Administrators", "Panel",
		"Window", "Daily", "Weekly", "Monthly", "Protect login", "Protect registration", "Protect uploads",
		"queued", "processing", "canceling", "completed", "failed", "canceled", "locked", "active", "disabled",
		"Username", "Password", "New password", "Create user", "Save changes", "User created", "User updated",
		"Administrator", "User", "Model", "File encoding", "Default provider",
	} {
		messages[message] = true
	}
	files, err := filepath.Glob("internal/app/*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			index := -1
			switch selector.Sel.Name {
			case "tr", "translate":
				index = 1
			case "fail":
				index = 3
			}
			if index >= 0 && index < len(call.Args) {
				if message, ok := goString(call.Args[index]); ok {
					messages[message] = true
				}
			}
			return true
		})
	}
	for _, properName := range []string{"Anthropic", "DeepL", "Google Cloud Translation"} {
		delete(messages, properName)
	}
	placeholder := regexp.MustCompile(`\{[a-zA-Z_]+\}`)
	entries, err := fs.ReadDir(Locales(), ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		data, err := fs.ReadFile(Locales(), entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		var cat catalog
		if err := json.Unmarshal(data, &cat); err != nil {
			t.Fatal(err)
		}
		if cat.Locale == "en" {
			continue
		}
		for message := range messages {
			if cat.Messages[message] == "" {
				t.Errorf("%s has no translation for %q", cat.Locale, message)
			}
		}
		for message, translated := range cat.Messages {
			if !samePlaceholders(placeholder.FindAllString(message, -1), placeholder.FindAllString(translated, -1)) {
				t.Errorf("%s changed placeholders in %q", cat.Locale, message)
			}
		}
	}
}

func goString(expr ast.Expr) (string, bool) {
	switch value := expr.(type) {
	case *ast.BasicLit:
		if value.Kind == token.STRING {
			text, err := strconv.Unquote(value.Value)
			return text, err == nil
		}
	case *ast.BinaryExpr:
		if value.Op == token.ADD {
			left, okLeft := goString(value.X)
			right, okRight := goString(value.Y)
			return left + right, okLeft && okRight
		}
	}
	return "", false
}

func samePlaceholders(left, right []string) bool {
	counts := make(map[string]int)
	for _, item := range left {
		if !strings.HasSuffix(item, "_plural}") {
			counts[item]++
		}
	}
	for _, item := range right {
		if !strings.HasSuffix(item, "_plural}") {
			counts[item]--
		}
	}
	for _, count := range counts {
		if count != 0 {
			return false
		}
	}
	return true
}

func TestRuntimeManifest(t *testing.T) {
	data, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	manifest := string(data)
	for _, required := range []string{"COPY locales/", "COPY assets.go", "COPY --from=frontend", "CGO_ENABLED=0", "docker-entrypoint.sh", "-healthcheck"} {
		if !strings.Contains(manifest, required) {
			t.Errorf("Dockerfile must include %q", required)
		}
	}
	if strings.Contains(manifest, "FROM python:") || strings.Contains(manifest, "gunicorn") {
		t.Error("production image still depends on Python")
	}
	entrypoint, err := os.ReadFile("docker-entrypoint.sh")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(entrypoint), "\r") {
		t.Error("container entrypoint must use LF line endings")
	}
}

func TestPublishingImageResolution(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is required to execute the publishing step")
	}
	data, err := os.ReadFile(".github/workflows/docker-publish.yml")
	if err != nil {
		t.Fatal(err)
	}
	source := string(data)
	start := strings.Index(source, "          ghcr_image=")
	if start < 0 {
		t.Fatal("image-resolution step was not found")
	}
	end := strings.Index(source[start:], "      - name:")
	if end < 0 {
		t.Fatal("image-resolution step has no ending")
	}
	var script strings.Builder
	for _, line := range strings.Split(source[start:start+end], "\n") {
		script.WriteString(strings.TrimPrefix(line, "          "))
		script.WriteByte('\n')
	}
	for _, tc := range []struct {
		name, image, username, token string
		wantDockerHub, wantError     bool
	}{
		{name: "GHCR only"},
		{name: "complete", image: "Owner/Image", username: "Owner", token: "offline-test-token", wantDockerHub: true},
		{name: "partial", image: "owner/image"},
		{name: "invalid", image: "not a repository", username: "Owner", token: "offline-test-token", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output := filepath.Join(t.TempDir(), "output")
			command := exec.Command(bash, "-eu", "-c", script.String())
			command.Env = append(os.Environ(), "GITHUB_REPOSITORY=Owner/Repo", "GITHUB_OUTPUT="+filepath.ToSlash(output),
				"DOCKERHUB_IMAGE="+tc.image, "DOCKERHUB_USERNAME="+tc.username, "DOCKERHUB_TOKEN="+tc.token)
			log, err := command.CombinedOutput()
			if (err != nil) != tc.wantError {
				t.Fatalf("step error = %v, output: %s", err, log)
			}
			if tc.wantError {
				return
			}
			result, err := os.ReadFile(output)
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantDockerHub {
				if !strings.Contains(string(result), "images<<EOF\nghcr.io/owner/repo\nowner/image\nEOF") {
					t.Errorf("unexpected image list: %s", result)
				}
			} else if !strings.Contains(string(result), "images=ghcr.io/owner/repo\n") {
				t.Errorf("unexpected image list: %s", result)
			}
		})
	}
}
