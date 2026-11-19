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
	mu     sync.Mutex
	queued []map[string]map[string]any
	looks  map[string]int
	refuse bool
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
		"media/demo/styles/comic.yaml":    comicStyle,
		"media/demo/workflows/comic.json": comicWorkflow,
		"media/demo/styles/broken.yaml":   "workflow: workflows/comic.json\nnodes:\n  prompt: { node: \"66\", input: text }\n",
		"media/demo/styles/noprompt.yaml": "workflow: workflows/comic.json\n",
		"media/demo/styles/escape.yaml":   "workflow: ../../secret/x.json\nnodes:\n  prompt: { node: \"6\", input: text }\n",
		"media/demo/styles/plain.yaml":    "workflow: workflows/comic.json\nseed: 42\nnodes:\n  prompt: { node: \"6\", input: text }\n",
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
		"no project":         {"alice", map[string]any{"project": "demo", "style": "comic", "prompt": "x"}, "<namespace>/<repo>"},
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
