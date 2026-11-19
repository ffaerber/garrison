package agentapi

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeConfigs: files of config repos, org/path.
type fakeConfigs struct {
	files map[string]string
	fail  bool
	reads int
}

func (f *fakeConfigs) Files(_ context.Context, org, repo, dir string) ([]string, error) {
	f.reads++
	if f.fail {
		return nil, errors.New("gitea is down")
	}
	var out []string
	for k := range f.files {
		if name, ok := strings.CutPrefix(k, org+"/"+dir+"/"); ok {
			out = append(out, name)
		}
	}
	return out, nil
}

func (f *fakeConfigs) File(_ context.Context, org, repo, path string) (string, error) {
	return f.files[org+"/"+path], nil
}

func TestTheMediaLibrary(t *testing.T) {
	ctx := context.Background()
	f := &fakeConfigs{files: map[string]string{
		"media/stacks/files.yaml": "entrypoint: web\nservices:\n  web:\n    image: filebrowser/filebrowser\n    volumes:\n      library: { share: media, path: /srv }\n",
		"todo/stacks/todo.yaml":   "entrypoint: web\nservices:\n  web:\n    image: x\n    volumes:\n      data: { dataset: d, size: 1Gi }\n",
		"lab/stacks/bad.yaml":     "services: [",
	}}
	s := &Server{Media: f}
	if got := s.media(ctx, "media"); got != "/garrison/ns/media/shares/media" {
		t.Errorf("media: %q", got)
	}
	for _, ns := range []string{"todo", "lab", "garrison", ""} {
		if got := s.media(ctx, ns); got != "" {
			t.Errorf("%s: %q", ns, got)
		}
	}
	// Remembered a minute; Gitea failing then keeps what was known.
	reads := f.reads
	s.media(ctx, "media")
	if f.reads != reads {
		t.Error("asked again within the minute")
	}
	s.mediaCache.at["media"] = s.mediaCache.at["media"].Add(-2 * mediaFor)
	f.fail = true
	if got := s.media(ctx, "media"); got == "" {
		t.Error("lost the library while Gitea was down")
	}
	// Without Media: nobody has one.
	if got := (&Server{}).media(ctx, "media"); got != "" {
		t.Errorf("without Media: %q", got)
	}
}
