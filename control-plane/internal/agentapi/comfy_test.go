package agentapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"garrison/control-plane/internal/sandbox"
)

// fakeComfy: a run finishes after two looks at its history and makes one
// PNG, or fails if the prompt says "fail".
type fakeComfy struct {
	mu       sync.Mutex
	queued   []map[string]map[string]any
	looks    map[string]int
	refuse   bool
	uploaded map[string][]byte
}

func (f *fakeComfy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == "POST" && r.URL.Path == "/prompt":
		if f.refuse {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":{"type":"prompt_outputs_failed_validation"},"node_errors":{"3":{"errors":["bad"]}}}`)
			return
		}
		var b struct {
			Prompt map[string]map[string]any `json:"prompt"`
		}
		json.NewDecoder(r.Body).Decode(&b)
		f.queued = append(f.queued, b.Prompt)
		io.WriteString(w, `{"prompt_id":"p`+string(rune('0'+len(f.queued)))+`"}`)
	case strings.HasPrefix(r.URL.Path, "/history/"):
		id := strings.TrimPrefix(r.URL.Path, "/history/")
		if f.looks == nil {
			f.looks = map[string]int{}
		}
		f.looks[id]++
		if f.looks[id] < 2 {
			io.WriteString(w, `{}`)
			return
		}
		n := int(id[1] - '1')
		text, _ := f.queued[n]["6"]["inputs"].(map[string]any)["text"].(string)
		if strings.Contains(text, "fail") {
			io.WriteString(w, `{"`+id+`":{"status":{"status_str":"error","completed":false,"messages":[["execution_error",{"exception_message":"out of memory"}]]},"outputs":{}}}`)
			return
		}
		io.WriteString(w, `{"`+id+`":{"status":{"status_str":"success","completed":true},"outputs":{"9":{"images":[{"filename":"ComfyUI_0001_.png","subfolder":"","type":"output"},{"filename":"preview.png","subfolder":"","type":"temp"}]}}}}`)
	case r.Method == "POST" && r.URL.Path == "/upload/image":
		file, head, err := r.FormFile("image")
		if err != nil || r.FormValue("overwrite") != "true" {
			http.Error(w, "bad upload", http.StatusBadRequest)
			return
		}
		data, _ := io.ReadAll(file)
		if f.uploaded == nil {
			f.uploaded = map[string][]byte{}
		}
		f.uploaded[head.Filename] = data
		io.WriteString(w, `{"name":"`+head.Filename+`","subfolder":"","type":"input"}`)
	case r.URL.Path == "/models":
		io.WriteString(w, `["checkpoints", "configs", "vae", "text_encoders", "diffusion_models"]`)
	case r.URL.Path == "/models/diffusion_models":
		io.WriteString(w, `["wan2.1_t2v_1.3B_fp16.safetensors", "z_image_turbo_bf16.safetensors"]`)
	case r.URL.Path == "/models/text_encoders":
		io.WriteString(w, `["qwen_3_4b.safetensors"]`)
	case r.URL.Path == "/models/vae":
		io.WriteString(w, `["ae.safetensors", "wan_2.1_vae.safetensors"]`)
	case strings.HasPrefix(r.URL.Path, "/models/"):
		io.WriteString(w, `[]`)
	case r.URL.Path == "/view":
		io.WriteString(w, "PNG of "+r.URL.Query().Get("filename"))
	default:
		http.NotFound(w, r)
	}
}

// styles: files of projects, by person: bob may not read media/demo.
type styles map[string]string

func (s styles) Read(_ context.Context, as, org, repo, path string) (string, error) {
	if as == "bob" {
		return "", errors.New("not found")
	}
	v, ok := s[org+"/"+repo+"/"+path]
	if !ok {
		return "", errors.New("not found")
	}
	return v, nil
}

const comicStyle = `workflow: workflows/comic.json
prompt: "{prompt}, comic style, bold lines"
negative: photo, blurry
width: 768
height: 1024
nodes:
  prompt: { node: "6", input: text }
  negative: { node: "7", input: text }
  seed: { node: "3", input: seed }
  width: { node: "5", input: width }
  height: { node: "5", input: height }
`

const comicWorkflow = `{
 "3": {"class_type": "KSampler", "inputs": {"seed": 1, "steps": 20}},
 "5": {"class_type": "EmptyLatentImage", "inputs": {"width": 512, "height": 512, "batch_size": 1}},
 "6": {"class_type": "CLIPTextEncode", "inputs": {"text": "", "clip": ["4", 1]}},
 "7": {"class_type": "CLIPTextEncode", "inputs": {"text": "", "clip": ["4", 1]}},
 "9": {"class_type": "SaveImage", "inputs": {"images": ["8", 0]}}
}`

func comfyServer(t *testing.T) (*Server, *fakeComfy) {
	f := &fakeComfy{}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	s := &Server{ComfyUI: &ComfyUI{URL: srv.URL, Every: time.Millisecond, Wait: 5 * time.Second}, Styles: styles{
		"media/demo/styles/comic.yaml":     comicStyle,
		"media/demo/workflows/comic.json":  comicWorkflow,
		"media/demo/styles/broken.yaml":    "workflow: workflows/comic.json\nnodes:\n  prompt: { node: \"66\", input: text }\n",
		"media/demo/styles/noprompt.yaml":  "workflow: workflows/comic.json\n",
		"media/demo/styles/escape.yaml":    "workflow: ../../secret/x.json\nnodes:\n  prompt: { node: \"6\", input: text }\n",
		"media/demo/styles/plain.yaml":     "workflow: workflows/comic.json\nseed: 42\nnodes:\n  prompt: { node: \"6\", input: text }\n",
		"media/demo/styles/moving.yaml":    "workflow: workflows/moving.json\nnodes:\n  prompt: { node: \"6\", input: text }\n  image: { node: \"56\", input: image }\n",
		"media/demo/workflows/moving.json": `{"6": {"class_type": "CLIPTextEncode", "inputs": {"text": ""}}, "56": {"class_type": "LoadImage", "inputs": {"image": "example.png"}}, "9": {"class_type": "SaveImage", "inputs": {}}}`,
	}}
	return s, f
}

var alice = sandbox.Caller{Agent: "media/media", Person: "alice", Chat: "c1", Run: sandbox.Run{Key: "k1"}}

func TestGenerateMediaInAStyle(t *testing.T) {
	s, f := comfyServer(t)
	seed := int64(100)
	raw, _ := json.Marshal(map[string]any{"project": "media/demo", "style": "comic", "prompt": "a fox on a bike", "count": 2, "seed": seed})
	out, err := s.generate(context.Background(), alice, raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.queued) != 2 || len(out) != 2 {
		t.Fatalf("queued %d, made %d", len(f.queued), len(out))
	}
	in := func(i int, node, input string) any { return f.queued[i][node]["inputs"].(map[string]any)[input] }
	if in(0, "6", "text") != "a fox on a bike, comic style, bold lines" || in(0, "7", "text") != "photo, blurry" ||
		in(0, "3", "seed") != float64(100) || in(1, "3", "seed") != float64(101) ||
		in(0, "5", "width") != float64(768) || in(0, "5", "height") != float64(1024) || in(0, "3", "steps") != float64(20) {
		t.Errorf("first run: %v", f.queued[0])
	}
	// Only outputs, not previews; named by style; how it was made beside it.
	if !strings.HasPrefix(out[0].Name, "comic-") || !strings.HasSuffix(out[0].Name, "-1.png") || !strings.HasSuffix(out[1].Name, "-2.png") ||
		out[1].Made["seed"] != int64(101) || out[0].Made["prompt"] != "a fox on a bike" || out[0].Made["for"] != "alice" {
		t.Errorf("outputs: %+v", out)
	}

	// The sandbox of the turn fetches each once; no other may.
	s.Sandboxes = nil
	get := func(c sandbox.Caller, id string) (int, string) {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/media/out/"+id, nil)
		r.SetPathValue("id", id)
		s.serveOutput(w, r, c)
		return w.Code, w.Body.String()
	}
	other := alice
	other.Chat = "c2"
	if code, _ := get(other, out[0].ID); code != http.StatusNotFound {
		t.Errorf("another sandbox fetched it: %d", code)
	}
	if code, body := get(alice, out[0].ID); code != 200 || body != "PNG of ComfyUI_0001_.png" {
		t.Errorf("fetch: %d %q", code, body)
	}
	if code, _ := get(alice, out[0].ID); code != http.StatusNotFound {
		t.Errorf("fetched twice: %d", code)
	}
}

func TestAStylesSeedAndPromptAlone(t *testing.T) {
	s, f := comfyServer(t)
	raw, _ := json.Marshal(map[string]any{"project": "media/demo", "style": "plain", "prompt": "a lake"})
	if _, err := s.generate(context.Background(), alice, raw); err != nil {
		t.Fatal(err)
	}
	if got := f.queued[0]["6"]["inputs"].(map[string]any)["text"]; got != "a lake" {
		t.Errorf("prompt %v", got)
	}
	// No node for the seed: the workflow keeps its own.
	if got := f.queued[0]["3"]["inputs"].(map[string]any)["seed"]; got != float64(1) {
		t.Errorf("seed %v", got)
	}
}

func TestGenerateMediaRefuses(t *testing.T) {
	s, f := comfyServer(t)
	for name, c := range map[string]struct {
		who  string
		args map[string]any
		want string
	}{
		"no project":         {"alice", map[string]any{"project": "demo", "style": "comic", "prompt": "x"}, "<unit>/<repo>"},
		"a style path":       {"alice", map[string]any{"project": "media/demo", "style": "../x", "prompt": "x"}, "style is"},
		"no prompt":          {"alice", map[string]any{"project": "media/demo", "style": "comic", "prompt": " "}, "prompt is empty"},
		"too many":           {"alice", map[string]any{"project": "media/demo", "style": "comic", "prompt": "x", "count": 9}, "1 to 8"},
		"no such style":      {"alice", map[string]any{"project": "media/demo", "style": "nope", "prompt": "x"}, "the style nope"},
		"bob may not read":   {"bob", map[string]any{"project": "media/demo", "style": "comic", "prompt": "x"}, "the style comic"},
		"a missing node":     {"alice", map[string]any{"project": "media/demo", "style": "broken", "prompt": "x"}, `no node "66"`},
		"no prompt node":     {"alice", map[string]any{"project": "media/demo", "style": "noprompt", "prompt": "x"}, "where the prompt goes"},
		"a workflow outside": {"alice", map[string]any{"project": "media/demo", "style": "escape", "prompt": "x"}, "names no workflow"},
		"ComfyUI's error":    {"alice", map[string]any{"project": "media/demo", "style": "comic", "prompt": "please fail"}, "out of memory"},
	} {
		c2 := alice
		c2.Person = c.who
		raw, _ := json.Marshal(c.args)
		if _, err := s.generate(context.Background(), c2, raw); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	f.refuse = true
	raw, _ := json.Marshal(map[string]any{"project": "media/demo", "style": "comic", "prompt": "x"})
	if _, err := s.generate(context.Background(), alice, raw); err == nil || !strings.Contains(err.Error(), "refused the workflow") {
		t.Errorf("refused: %v", err)
	}
}

func TestComfyUITimesOut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/prompt" {
			io.WriteString(w, `{"prompt_id":"p1"}`)
			return
		}
		io.WriteString(w, `{}`) // never done
	}))
	defer srv.Close()
	c := &ComfyUI{URL: srv.URL, Every: time.Millisecond, Wait: 50 * time.Millisecond}
	if _, err := c.Run(context.Background(), map[string]map[string]any{}); err == nil || !strings.Contains(err.Error(), "did not finish") {
		t.Errorf("%v", err)
	}
}

func TestMediaModels(t *testing.T) {
	s, _ := comfyServer(t)
	models, err := s.ComfyUI.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models["diffusion_models"]) != 2 || len(models["vae"]) != 2 || models["checkpoints"] != nil {
		t.Fatalf("models: %v", models)
	}
	text := describeModels(models)
	// Z-Image has all its files; Wan lacks its text encoder; no checkpoints.
	for _, want := range []string{"diffusion_models: wan2.1_t2v_1.3B_fp16.safetensors, z_image_turbo_bf16.safetensors", "Z-Image Turbo", "type lumina2",
		// The example that ran, with the files this ComfyUI has.
		"A workflow that ran", `"unet_name": "z_image_turbo_bf16.safetensors"`, `"weight_dtype": "default"`, `"class_type": "SaveImage"`, `prompt: { node: "6", input: text }`} {
		if !strings.Contains(text, want) {
			t.Errorf("no %q in:\n%s", want, text)
		}
	}
	for _, not := range []string{"Wan 2.1", "CheckpointLoaderSimple", "checkpoints:"} {
		if strings.Contains(text, not) {
			t.Errorf("%q in:\n%s", not, text)
		}
	}
	// Another file of the same model: the example names it.
	other := describeModels(map[string][]string{"diffusion_models": {"z_image_turbo_fp8.safetensors"}, "text_encoders": {"qwen_3_4b.safetensors"}, "vae": {"ae.safetensors"}})
	if !strings.Contains(other, `"unet_name": "z_image_turbo_fp8.safetensors"`) || strings.Contains(other, "z_image_turbo_bf16") {
		t.Errorf("the example keeps the demo's file:\n%s", other)
	}
	// Wan 2.2 beside Wan 2.1: both offered, Wan 2.2 first, with its example.
	both := describeModels(map[string][]string{
		"diffusion_models": {"wan2.1_t2v_1.3B_fp16.safetensors", "wan2.2_ti2v_5B_fp16.safetensors"},
		"text_encoders":    {"umt5_xxl_fp16.safetensors"}, "vae": {"wan_2.1_vae.safetensors", "wan2.2_vae.safetensors"}})
	i22, i21 := strings.Index(both, "### Wan 2.2"), strings.Index(both, "### Wan 2.1")
	if i22 < 0 || i21 < 0 || i22 > i21 || !strings.Contains(both, `"class_type": "Wan22ImageToVideoLatent"`) || !strings.Contains(both, `frames: { node: "55", input: length }`) {
		t.Errorf("Wan 2.2 not offered first with its example:\n%s", both)
	}
	if !strings.Contains(both, "image to video") || !strings.Contains(both, `image: { node: "56", input: image }`) || !strings.Contains(both, `"class_type": "LoadImage"`) {
		t.Errorf("no image-to-video example:\n%s", both)
	}
	if !strings.Contains(describeModels(nil), "no models") {
		t.Error("an empty ComfyUI is not said so")
	}
}

func TestAStyleThatStartsFromAPicture(t *testing.T) {
	s, f := comfyServer(t)
	png := append([]byte("\x89PNG\r\n\x1a\n"), []byte("key picture")...)
	gen := func(args map[string]any) ([]Output, error) {
		raw, _ := json.Marshal(args)
		return s.generate(context.Background(), alice, raw)
	}
	out, err := gen(map[string]any{"project": "media/demo", "style": "moving", "prompt": "the octopus swims", "count": 2,
		"image": "/data/media/demo/key-octopus.png", "image_name": "key-octopus.png", "image_data": png})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.uploaded) != 1 || len(f.queued) != 2 {
		t.Fatalf("uploaded %d, queued %d", len(f.uploaded), len(f.queued))
	}
	var name string
	for n, data := range f.uploaded {
		name = n
		if string(data) != string(png) || !strings.HasPrefix(n, "garrison-") || !strings.HasSuffix(n, ".png") {
			t.Errorf("uploaded %q: %q", n, data)
		}
	}
	// Uploaded once, and every clip starts from it.
	for i, q := range f.queued {
		if q["56"]["inputs"].(map[string]any)["image"] != name {
			t.Errorf("run %d starts from %v", i, q["56"]["inputs"])
		}
	}
	if out[0].Made["image"] != "/data/media/demo/key-octopus.png" {
		t.Errorf("made: %+v", out[0].Made)
	}
	for _, bad := range []map[string]any{
		{"project": "media/demo", "style": "moving", "prompt": "x"},                                                      // it needs a picture
		{"project": "media/demo", "style": "comic", "prompt": "x", "image_name": "a.png", "image_data": png},             // it takes none
		{"project": "media/demo", "style": "moving", "prompt": "x", "image_name": "a.txt", "image_data": []byte("text")}, // not a picture
	} {
		if _, err := gen(bad); err == nil {
			t.Errorf("%v: no error", bad["style"])
		}
	}
}
