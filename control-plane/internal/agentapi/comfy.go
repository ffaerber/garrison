package agentapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"garrison/control-plane/internal/events"
	"garrison/control-plane/internal/sandbox"
)

// GenerateMedia is the tool that makes pictures and videos with ComfyUI, in
// a project's style, into the unit's media library.
const GenerateMedia = "generate_media"

var generateSpec = toolSpec{
	Name: GenerateMedia,
	Description: "Makes pictures or videos with ComfyUI in one of a project's styles, and puts them into the unit's media library, /data/media/<project>/, each with a .json of how it was made. " +
		"A project is a repo of the unit with styles/<style>.yaml and the ComfyUI workflows they name (load the skill media-project for how they look). " +
		"Write the prompt as what the picture shows; the style adds its own words, size and settings. Answers with the files made. Videos can take many minutes.",
	InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"project": map[string]any{"type": "string", "description": "the project's repo, <unit>/<repo>, e.g. media/demo"},
			"style":   map[string]any{"type": "string", "description": "a style of the project: the name of a file in its styles/, without .yaml"},
			"prompt":  map[string]any{"type": "string", "description": "what to show"},
			"count":   map[string]any{"type": "integer", "description": "how many, 1 to 8; default 1"},
			"seed":    map[string]any{"type": "integer", "description": "optional: a seed, to make one again; each further one adds 1"},
			"image":   map[string]any{"type": "string", "description": "for a style that starts from a picture (image to video): a picture of the library to start from, e.g. /data/media/shorts/key-octopus.png, so that every clip shows the same character"},
		},
		"required": []string{"project", "style", "prompt"},
	},
}

// ComfyUI is the installation's ComfyUI, outside Garrison; only the agent
// service calls it. Runs go one at a time.
type ComfyUI struct {
	URL  string
	HTTP *http.Client
	// Wait is how long a run may take; 0: 30 minutes. Every is how often
	// its history is asked for; 0: 2 seconds.
	Wait, Every time.Duration

	mu sync.Mutex
}

// Style is styles/<name>.yaml of a project.
type Style struct {
	Workflow string `yaml:"workflow"` // the workflow, in ComfyUI's API format, e.g. workflows/comic.json
	Prompt   string `yaml:"prompt"`   // the prompt, {prompt} where the agent's goes; empty: the agent's alone
	Negative string `yaml:"negative"`
	Width    int    `yaml:"width"`
	Height   int    `yaml:"height"`
	Frames   int    `yaml:"frames"` // for a video workflow
	Seed     *int64 `yaml:"seed"`   // a fixed seed; empty: random
	// Nodes are where each setting goes: node id and input name. A style
	// whose nodes place image starts from a picture: every run needs one
	// (generate_media's image), e.g. a video that keeps the character of a
	// key picture.
	Nodes map[string]NodeInput `yaml:"nodes"`
}

// NodeInput is an input of a workflow's node.
type NodeInput struct {
	Node  string `yaml:"node"`
	Input string `yaml:"input"`
}

// settings a style may place.
var styleSettings = []string{"prompt", "negative", "seed", "width", "height", "frames", "image"}

var (
	styleName   = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$`)
	projectName = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?)/([A-Za-z0-9._-]{1,100})$`)
)

// StyleReader reads a project's files as a person; gateway.Gitea.
type StyleReader interface {
	Read(ctx context.Context, as, org, repo, path string) (string, error)
}

// Output is one file a run made.
type Output struct {
	ID   string         `json:"id"`   // to fetch it at /media/out/<id>, once
	Name string         `json:"name"` // its name in the library
	Made map[string]any `json:"made"` // how: project, style, prompt, seed, workflow
}

// outputs holds what runs made until the sandbox fetches it: files in a
// directory of their own, by id, for the token of the turn that made them.
type outputs struct {
	mu    sync.Mutex
	dir   string
	files map[string]outputFile
}

type outputFile struct {
	path, token string
	at          time.Time
}

// keepOutputs is how long a made file waits to be fetched.
const keepOutputs = time.Hour

// mayGenerate tells whether the sandbox of c gets generate_media: its agent
// lists it, there is a ComfyUI, and the unit a media library.
func (s *Server) mayGenerate(ctx context.Context, c sandbox.Caller) bool {
	if s.ComfyUI == nil || s.Styles == nil {
		return false
	}
	a, ok, err := s.Agents.Get(ctx, c.Agent)
	if err != nil || !ok || !slices.Contains(a.Tools, GenerateMedia) {
		return false
	}
	ns, _, _ := strings.Cut(c.Agent, "/")
	return s.media(ctx, ns) != ""
}

// generate runs a style for the sandbox c: in is generate_media's
// arguments. It answers with the files made, kept for the sandbox to fetch.
func (s *Server) generate(ctx context.Context, c sandbox.Caller, raw json.RawMessage) ([]Output, error) {
	var in struct {
		Project, Style, Prompt string
		Count                  int
		Seed                   *int64
		Image                  string // the picture's path in the library, as the agent named it
		ImageName              string `json:"image_name"`
		ImageData              []byte `json:"image_data"` // the picture, which the sandbox read
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("arguments: %v", err)
	}
	m := projectName.FindStringSubmatch(in.Project)
	switch {
	case m == nil:
		return nil, fmt.Errorf("project is <unit>/<repo>, e.g. media/demo")
	case !styleName.MatchString(in.Style):
		return nil, fmt.Errorf("style is the name of a file in the project's styles/, without .yaml")
	case strings.TrimSpace(in.Prompt) == "":
		return nil, fmt.Errorf("prompt is empty: say what to show")
	}
	if in.Count == 0 {
		in.Count = 1
	}
	if in.Count < 1 || in.Count > 8 {
		return nil, fmt.Errorf("count is 1 to 8")
	}
	ns, repo := m[1], m[3]
	raw2, err := s.Styles.Read(ctx, c.Person, ns, repo, "styles/"+in.Style+".yaml")
	if err != nil {
		return nil, fmt.Errorf("the style %s of %s: %w", in.Style, in.Project, err)
	}
	var st Style
	dec := yaml.NewDecoder(strings.NewReader(raw2))
	dec.KnownFields(true)
	if err := dec.Decode(&st); err != nil {
		return nil, fmt.Errorf("styles/%s.yaml of %s: %v", in.Style, in.Project, err)
	}
	if st.Workflow == "" || strings.Contains(st.Workflow, "..") || path.IsAbs(st.Workflow) {
		return nil, fmt.Errorf("styles/%s.yaml of %s names no workflow in the project, e.g. workflow: workflows/comic.json", in.Style, in.Project)
	}
	if st.Nodes["prompt"].Node == "" {
		return nil, fmt.Errorf("styles/%s.yaml of %s does not say where the prompt goes: nodes: { prompt: { node: \"6\", input: text } }", in.Style, in.Project)
	}
	for k := range st.Nodes {
		if !slices.Contains(styleSettings, k) {
			return nil, fmt.Errorf("styles/%s.yaml of %s: nodes has %q; it may place %s", in.Style, in.Project, k, strings.Join(styleSettings, ", "))
		}
	}
	takesImage := st.Nodes["image"].Node != ""
	switch {
	case takesImage && len(in.ImageData) == 0:
		return nil, fmt.Errorf("the style %s starts from a picture: give image, a picture of the library, e.g. a key picture made with the project's picture style", in.Style)
	case !takesImage && len(in.ImageData) > 0:
		return nil, fmt.Errorf("the style %s does not start from a picture: leave image out, or use a style whose nodes place image", in.Style)
	}
	rawWF, err := s.Styles.Read(ctx, c.Person, ns, repo, st.Workflow)
	if err != nil {
		return nil, fmt.Errorf("the workflow %s of %s: %w", st.Workflow, in.Project, err)
	}
	var wf map[string]map[string]any
	if err := json.Unmarshal([]byte(rawWF), &wf); err != nil {
		return nil, fmt.Errorf("%s of %s is not a workflow in ComfyUI's API format (Export (API) in ComfyUI): %v", st.Workflow, in.Project, err)
	}
	prompt := in.Prompt
	if st.Prompt != "" {
		prompt = strings.ReplaceAll(st.Prompt, "{prompt}", in.Prompt)
		if !strings.Contains(st.Prompt, "{prompt}") {
			prompt = in.Prompt + ", " + st.Prompt
		}
	}
	seed := in.Seed
	if seed == nil {
		seed = st.Seed
	}
	if seed == nil {
		n, _ := rand.Int(rand.Reader, big.NewInt(1<<48))
		v := n.Int64()
		seed = &v
	}
	uploaded := ""
	if takesImage {
		if uploaded, err = s.ComfyUI.Upload(ctx, in.ImageName, in.ImageData); err != nil {
			return nil, err
		}
	}
	var out []Output
	for i := 0; i < in.Count; i++ {
		values := map[string]any{"prompt": prompt, "seed": *seed + int64(i)}
		if uploaded != "" {
			values["image"] = uploaded
		}
		if st.Negative != "" {
			values["negative"] = st.Negative
		}
		for k, v := range map[string]int{"width": st.Width, "height": st.Height, "frames": st.Frames} {
			if v > 0 {
				values[k] = v
			}
		}
		run, err := fill(wf, st.Nodes, values)
		if err != nil {
			return out, fmt.Errorf("styles/%s.yaml of %s: %w", in.Style, in.Project, err)
		}
		files, err := s.ComfyUI.Run(ctx, run)
		if err != nil {
			return out, err
		}
		stamp := time.Now().UTC().Format("20060102-150405")
		for j, f := range files {
			ext := strings.ToLower(path.Ext(f.name))
			name := fmt.Sprintf("%s-%s-%d%s", in.Style, stamp, i*len(files)+j+1, ext)
			id, err := s.keep(c, f.data)
			if err != nil {
				return out, err
			}
			made := map[string]any{
				"project": in.Project, "style": in.Style, "prompt": in.Prompt, "full_prompt": prompt,
				"seed": *seed + int64(i), "workflow": st.Workflow, "made": time.Now().UTC().Format(time.RFC3339),
				"by": c.Agent, "for": c.Person,
			}
			if uploaded != "" {
				made["image"] = in.Image
			}
			out = append(out, Output{ID: id, Name: name, Made: made})
		}
	}
	return out, nil
}

// fill is the workflow with values put into the nodes that say where.
func fill(wf map[string]map[string]any, nodes map[string]NodeInput, values map[string]any) (map[string]map[string]any, error) {
	b, _ := json.Marshal(wf)
	var out map[string]map[string]any
	json.Unmarshal(b, &out)
	for k, v := range values {
		at, ok := nodes[k]
		if !ok {
			if k == "prompt" {
				return nil, errors.New("no node for the prompt")
			}
			continue // the workflow keeps its own
		}
		node, ok := out[at.Node]
		if !ok {
			return nil, fmt.Errorf("nodes: %s: the workflow has no node %q", k, at.Node)
		}
		inputs, _ := node["inputs"].(map[string]any)
		if inputs == nil || at.Input == "" {
			return nil, fmt.Errorf("nodes: %s: node %q has no input %q", k, at.Node, at.Input)
		}
		if _, ok := inputs[at.Input]; !ok {
			return nil, fmt.Errorf("nodes: %s: node %q has no input %q", k, at.Node, at.Input)
		}
		inputs[at.Input] = v
	}
	return out, nil
}

type madeFile struct {
	name string
	data []byte
}

// Run queues a workflow, waits for it and fetches what it made.
func (c *ComfyUI) Run(ctx context.Context, wf map[string]map[string]any) ([]madeFile, error) {
	wait, every := c.Wait, c.Every
	if wait == 0 {
		wait = 30 * time.Minute
	}
	if every == 0 {
		every = 2 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	// One at a time: ComfyUI has one GPU, and a run waiting here is one the
	// agent knows is waiting.
	for !c.mu.TryLock() {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("ComfyUI was busy with other runs for %s", wait)
		case <-time.After(every):
		}
	}
	defer c.mu.Unlock()
	body, _ := json.Marshal(map[string]any{"prompt": wf, "client_id": "garrison"})
	var queued struct {
		PromptID   string         `json:"prompt_id"`
		Error      any            `json:"error"`
		NodeErrors map[string]any `json:"node_errors"`
	}
	status, err := c.do(ctx, http.MethodPost, "/prompt", body, &queued)
	if err != nil || status != http.StatusOK || queued.PromptID == "" {
		if queued.Error != nil || len(queued.NodeErrors) > 0 {
			detail, _ := json.Marshal(map[string]any{"error": queued.Error, "node_errors": queued.NodeErrors})
			return nil, fmt.Errorf("ComfyUI refused the workflow: %s", detail)
		}
		return nil, fmt.Errorf("ComfyUI: %d %v", status, err)
	}
	for {
		var hist map[string]struct {
			Status struct {
				StatusStr string `json:"status_str"`
				Completed bool   `json:"completed"`
				Messages  []any  `json:"messages"`
			} `json:"status"`
			Outputs map[string]map[string][]struct {
				Filename  string `json:"filename"`
				Subfolder string `json:"subfolder"`
				Type      string `json:"type"`
			} `json:"outputs"`
		}
		if _, err := c.do(ctx, http.MethodGet, "/history/"+url.PathEscape(queued.PromptID), nil, &hist); err != nil && ctx.Err() == nil {
			log.Printf("comfyui: history of %s: %v", queued.PromptID, err)
		}
		if h, ok := hist[queued.PromptID]; ok {
			if h.Status.StatusStr == "error" {
				detail, _ := json.Marshal(h.Status.Messages)
				return nil, fmt.Errorf("ComfyUI's run failed: %.2000s", detail)
			}
			if h.Status.Completed || h.Status.StatusStr == "success" {
				var out []madeFile
				for _, id := range sortedOutputKeys(h.Outputs) {
					for _, kind := range []string{"images", "gifs", "videos"} {
						for _, f := range h.Outputs[id][kind] {
							if f.Type != "output" {
								continue // previews and temporary files
							}
							q := url.Values{"filename": {f.Filename}, "subfolder": {f.Subfolder}, "type": {f.Type}}
							var data []byte
							if _, err := c.do(ctx, http.MethodGet, "/view?"+q.Encode(), nil, &data); err != nil {
								return nil, fmt.Errorf("ComfyUI: %s: %w", f.Filename, err)
							}
							out = append(out, madeFile{name: f.Filename, data: data})
						}
					}
				}
				if len(out) == 0 {
					return nil, errors.New("ComfyUI's run made nothing to keep: the workflow needs a Save Image or Save Video node")
				}
				return out, nil
			}
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("ComfyUI's run did not finish within %s", wait)
		case <-time.After(every):
		}
	}
}

func sortedOutputKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// MaxImage is the largest picture a run may start from.
const MaxImage = 20 << 20

// Upload gives ComfyUI a picture to start from, under a name of its own,
// and answers with the name a workflow's LoadImage takes.
func (c *ComfyUI) Upload(ctx context.Context, name string, data []byte) (string, error) {
	if len(data) > MaxImage {
		return "", fmt.Errorf("the picture is %d MB; at most %d MB", len(data)>>20, MaxImage>>20)
	}
	ext := ""
	switch {
	case bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")):
		ext = ".png"
	case bytes.HasPrefix(data, []byte{0xFF, 0xD8, 0xFF}):
		ext = ".jpg"
	case len(data) > 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		ext = ".webp"
	default:
		return "", fmt.Errorf("image %s is not a PNG, JPEG or WebP picture", path.Base(name))
	}
	b := make([]byte, 8)
	rand.Read(b)
	unique := "garrison-" + hex.EncodeToString(b) + ext
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	part, _ := w.CreateFormFile("image", unique)
	part.Write(data)
	w.WriteField("overwrite", "true")
	w.Close()
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimSuffix(c.URL, "/")+"/upload/image", &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("ComfyUI: upload: %w", err)
	}
	defer resp.Body.Close()
	var up struct {
		Name      string `json:"name"`
		Subfolder string `json:"subfolder"`
	}
	if resp.StatusCode >= 300 || json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&up) != nil || up.Name == "" {
		return "", fmt.Errorf("ComfyUI did not take the picture: %s", resp.Status)
	}
	if up.Subfolder != "" {
		return up.Subfolder + "/" + up.Name, nil
	}
	return up.Name, nil
}

// maxOutput is the largest file a run may make.
const maxOutput = 2 << 30

func (c *ComfyUI) do(ctx context.Context, method, p string, body []byte, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(c.URL, "/")+p, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxOutput))
	if err != nil {
		return resp.StatusCode, err
	}
	if resp.StatusCode >= 300 {
		json.Unmarshal(data, out)
		return resp.StatusCode, fmt.Errorf("%s %s: %s", method, p, resp.Status)
	}
	if b, ok := out.(*[]byte); ok {
		*b = data
		return resp.StatusCode, nil
	}
	return resp.StatusCode, json.Unmarshal(data, out)
}

// keep holds a made file for the sandbox of c to fetch.
func (s *Server) keep(c sandbox.Caller, data []byte) (string, error) {
	o := &s.outputs
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.dir == "" {
		dir, err := os.MkdirTemp("", "garrison-media-")
		if err != nil {
			return "", err
		}
		o.dir, o.files = dir, map[string]outputFile{}
	}
	for id, f := range o.files {
		if time.Since(f.at) > keepOutputs {
			os.Remove(f.path)
			delete(o.files, id)
		}
	}
	b := make([]byte, 16)
	rand.Read(b)
	id := hex.EncodeToString(b)
	p := filepath.Join(o.dir, id)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		return "", err
	}
	o.files[id] = outputFile{path: p, token: c.Run.Key + "\x00" + c.Chat, at: time.Now()}
	return id, nil
}

// sandboxGenerate is generate_media for a sandbox: it answers with the files
// made, as JSON, for the sandbox to fetch and write into the library.
func (s *Server) sandboxGenerate(w http.ResponseWriter, r *http.Request) {
	c, ok := s.caller(w, r)
	if !ok {
		return
	}
	if !s.mayGenerate(r.Context(), c) {
		apiError(w, http.StatusForbidden, "this agent has no generate_media here: its agent.yaml must list it, and the unit needs a media library (a stack of its config mounting the share media) and the installation a ComfyUI (COMFYUI_URL)")
		return
	}
	// A picture to start from comes along, base64: a third more than it.
	raw, _ := io.ReadAll(io.LimitReader(r.Body, MaxImage*4/3+1<<20))
	files, err := s.generate(r.Context(), c, raw)
	if err != nil {
		writeJSON(w, map[string]any{"files": files, "error": err.Error()})
		return
	}
	ns, _, _ := strings.Cut(c.Agent, "/")
	var project, style string
	if len(files) > 0 {
		project, _ = files[0].Made["project"].(string)
		style, _ = files[0].Made["style"].(string)
	}
	made := fmt.Sprintf("%d files", len(files))
	if len(files) == 1 {
		made = "1 file"
	}
	if s.Events != nil {
		s.Events.Record(r.Context(), events.Event{Source: events.SourceAgents, Kind: events.KindMedia, Status: events.StatusOK,
			Unit: ns, Audience: events.AudienceMembers, Actor: c.Person, Agent: c.Agent, StartedBy: c.Run.StartedBy,
			Title: fmt.Sprintf("%s made %s in the style %s of %s for %s", c.Agent, made, style, project, c.Person)})
	}
	writeJSON(w, map[string]any{"files": files})
}

// sandboxOutput hands a made file to the sandbox whose turn made it, once.
func (s *Server) sandboxOutput(w http.ResponseWriter, r *http.Request) {
	c, ok := s.caller(w, r)
	if !ok {
		return
	}
	s.serveOutput(w, r, c)
}

func (s *Server) serveOutput(w http.ResponseWriter, r *http.Request, c sandbox.Caller) {
	o := &s.outputs
	o.mu.Lock()
	f, found := o.files[r.PathValue("id")]
	mine := found && f.token == c.Run.Key+"\x00"+c.Chat
	if mine {
		delete(o.files, r.PathValue("id"))
	}
	o.mu.Unlock()
	if !mine {
		apiError(w, http.StatusNotFound, "no such file of this sandbox")
		return
	}
	defer os.Remove(f.path)
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeFile(w, r, f.path)
}
