package templates

import (
	"strings"
	"testing"
)

func TestAppFilesTodo(t *testing.T) {
	files := AppFiles("todo", "lab", "example")
	for _, p := range []string{"go.mod", "go.sum", "main.go", "store.go", "main_test.go", "page.html", "Dockerfile", ".gitea/workflows/pipeline.yaml", "README.md"} {
		if _, ok := files[p]; !ok {
			t.Errorf("no %s in %v", p, keys(files))
		}
	}
	for p := range files {
		if strings.HasSuffix(p, ".tmpl") {
			t.Errorf("%s keeps its .tmpl", p)
		}
	}
	if !strings.Contains(files["README.md"], "source: example") || !strings.Contains(files["README.md"], `image: "{{registry}}/lab/example:{{commit}}"`) {
		t.Errorf("README.md: %s", files["README.md"])
	}
	if !strings.HasPrefix(files["main.go"], "package main") && !strings.Contains(files["main.go"], "\npackage main") {
		t.Error("main.go is not the app's")
	}
}

func TestAppFilesUnknown(t *testing.T) {
	for _, name := range []string{"", "static-web", "../namespace-config", "todo/.."} {
		if AppFiles(name, "lab", "example") != nil {
			t.Errorf("%q gave files", name)
		}
	}
}

func keys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
