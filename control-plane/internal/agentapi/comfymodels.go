package agentapi

import (
	"context"
	"embed"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"garrison/control-plane/internal/sandbox"
)

// MediaModels is the tool that tells which models the installation's ComfyUI
// has, so that a workflow written for a project names only those.
const MediaModels = "media_models"

var modelsSpec = toolSpec{
	Name: MediaModels,
	Description: "Lists the models the installation's ComfyUI has, by folder (checkpoints, diffusion_models, text_encoders, vae, loras, ...), and which known model families they make up, with the loader nodes each needs. " +
		"Call it before writing or changing a workflow: a workflow may name only models listed here, or ComfyUI refuses it.",
	InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
}

// modelFolders are the folders of ComfyUI's models a workflow names files of.
var modelFolders = []string{"checkpoints", "diffusion_models", "unet", "text_encoders", "clip", "vae", "loras", "upscale_models"}

// family is a kind of model and how a workflow loads and runs it, told to
// the agent when ComfyUI has all its files.
type family struct {
	name  string
	files []modelFile
	how   string
	// example, if set, is a workflow in API format that ran on a ComfyUI
	// with these models (workflows/<example>): the agent starts from it
	// rather than writing nodes from memory, which ComfyUI refused.
	example string
	nodes   string // the style's nodes for the example
}

// modelFile is a file a family needs: in folder, with part in its name;
// the example names it as in, replaced by the file ComfyUI has.
type modelFile struct{ folder, part, in string }

//go:embed workflows/*.json
var exampleWorkflows embed.FS

var families = []family{
	{name: "Z-Image Turbo (pictures)",
		files:   []modelFile{{"diffusion_models", "z_image", "z_image_turbo_bf16.safetensors"}, {"text_encoders", "qwen_3_4b", "qwen_3_4b.safetensors"}, {"vae", "ae.", "ae.safetensors"}},
		how:     "UNETLoader (z_image...) → ModelSamplingAuraFlow shift 3; CLIPLoader (qwen_3_4b..., type lumina2); VAELoader (ae...); EmptySD3LatentImage for the size; KSampler steps 8, cfg 1, sampler res_multistep, scheduler simple; VAEDecode → SaveImage",
		example: "z-image.json",
		nodes:   `prompt: { node: "6", input: text }, negative: { node: "7", input: text }, seed: { node: "3", input: seed }, width: { node: "5", input: width }, height: { node: "5", input: height }`},
	{name: "Wan 2.2 TI2V 5B, text to video (prefer it: 720p, 24 fps)",
		files:   []modelFile{{"diffusion_models", "wan2.2_ti2v_5b", "wan2.2_ti2v_5B_fp16.safetensors"}, {"text_encoders", "umt5_xxl", "umt5_xxl_fp16.safetensors"}, {"vae", "wan2.2_vae", "wan2.2_vae.safetensors"}},
		how:     "UNETLoader (wan2.2_ti2v_5B...) → ModelSamplingSD3 shift 8; CLIPLoader (umt5_xxl..., type wan); VAELoader (wan2.2_vae...); Wan22ImageToVideoLatent for size and length (frames, 4n+1: 121 is 5 seconds at 24 fps; 704x1280 portrait for shorts); KSampler steps 20 (a clip takes about 3 minutes on a 4090; 30 is a little cleaner and takes 4), cfg 5, sampler uni_pc, scheduler simple; VAEDecode → CreateVideo fps 24 → SaveVideo",
		example: "wan22-ti2v.json",
		nodes:   `prompt: { node: "6", input: text }, negative: { node: "7", input: text }, seed: { node: "3", input: seed }, width: { node: "55", input: width }, height: { node: "55", input: height }, frames: { node: "55", input: length }`},
	{name: "Wan 2.2 TI2V 5B, image to video (for shorts: every clip starts from one key picture, so the character stays the same)",
		files:   []modelFile{{"diffusion_models", "wan2.2_ti2v_5b", "wan2.2_ti2v_5B_fp16.safetensors"}, {"text_encoders", "umt5_xxl", "umt5_xxl_fp16.safetensors"}, {"vae", "wan2.2_vae", "wan2.2_vae.safetensors"}},
		how:     "as Wan 2.2 text to video, and LoadImage (the key picture; generate_media's image fills it) → ImageScale to 704x1280, crop center → Wan22ImageToVideoLatent's start_image. Make the key picture first with a picture style at 704x1280 (e.g. Z-Image). Size fixed at 704x1280 portrait; each clip's prompt says what the character does",
		example: "wan22-i2v.json",
		nodes:   `prompt: { node: "6", input: text }, negative: { node: "7", input: text }, seed: { node: "3", input: seed }, frames: { node: "55", input: length }, image: { node: "56", input: image }`},
	{name: "Wan 2.1 text to video",
		files:   []modelFile{{"diffusion_models", "wan2.1_t2v", "wan2.1_t2v_1.3B_fp16.safetensors"}, {"text_encoders", "umt5_xxl", "umt5_xxl_fp16.safetensors"}, {"vae", "wan_2.1_vae", "wan_2.1_vae.safetensors"}},
		how:     "Only if Wan 2.2 is not there. UNETLoader (wan2.1_t2v...) → ModelSamplingSD3 shift 8; CLIPLoader (umt5_xxl..., type wan); VAELoader (wan_2.1_vae...); EmptyHunyuanLatentVideo for size and length (frames, 4n+1: 81 is 5 seconds at 16 fps, its longest; 480x832, portrait, for the 1.3B model); KSampler steps 30, cfg 6, sampler uni_pc, scheduler simple; VAEDecode → CreateVideo fps 16 → SaveVideo",
		example: "wan-t2v.json",
		nodes:   `prompt: { node: "6", input: text }, negative: { node: "7", input: text }, seed: { node: "3", input: seed }, frames: { node: "12", input: length }`},
	{name: "Stable Diffusion checkpoint (pictures)",
		files: []modelFile{{"checkpoints", "", ""}},
		how:   "CheckpointLoaderSimple gives model, clip and vae; EmptyLatentImage for the size; KSampler; VAEDecode → SaveImage"},
}

// Models lists ComfyUI's model files by folder; folders it does not have, or
// that are empty, are left out.
func (c *ComfyUI) Models(ctx context.Context) (map[string][]string, error) {
	var have []string
	if _, err := c.do(ctx, "GET", "/models", nil, &have); err != nil {
		return nil, fmt.Errorf("ComfyUI's models: %w", err)
	}
	out := map[string][]string{}
	for _, f := range modelFolders {
		if !slices.Contains(have, f) {
			continue
		}
		var files []string
		if _, err := c.do(ctx, "GET", "/models/"+url.PathEscape(f), nil, &files); err != nil {
			return nil, fmt.Errorf("ComfyUI's %s: %w", f, err)
		}
		if len(files) > 0 {
			out[f] = files
		}
	}
	return out, nil
}

// mediaModels runs media_models for the sandbox c.
func (s *Server) mediaModels(ctx context.Context, c sandbox.Caller) (string, bool) {
	if !s.mayGenerate(ctx, c) {
		return "not allowed: media_models comes with generate_media, which needs the tool in your agent.yaml, a media library in the unit and a ComfyUI in the installation", true
	}
	models, err := s.ComfyUI.Models(ctx)
	if err != nil {
		return "error: " + err.Error(), true
	}
	return describeModels(models), false
}

// describeModels is media_models's answer for ComfyUI's models by folder.
func describeModels(models map[string][]string) string {
	if len(models) == 0 {
		return "ComfyUI has no models yet: someone with access to its machine has to add them before anything can be made."
	}
	var b strings.Builder
	b.WriteString("ComfyUI's models, by folder (a workflow names them exactly so):\n")
	for _, f := range modelFolders {
		if len(models[f]) > 0 {
			fmt.Fprintf(&b, "- %s: %s\n", f, strings.Join(models[f], ", "))
		}
	}
	var known, examples []string
	for _, fam := range families {
		complete := true
		names := map[string]string{} // the example's file: ComfyUI's
		for _, f := range fam.files {
			found := ""
			for _, m := range models[f.folder] {
				if found == "" && strings.Contains(strings.ToLower(m), f.part) {
					found = m
				}
			}
			complete = complete && found != ""
			if f.in != "" {
				names[f.in] = found
			}
		}
		if !complete {
			continue
		}
		known = append(known, fmt.Sprintf("- %s: %s", fam.name, fam.how))
		if fam.example == "" {
			continue
		}
		raw, err := exampleWorkflows.ReadFile("workflows/" + fam.example)
		if err != nil {
			continue
		}
		wf := string(raw)
		for in, have := range names {
			wf = strings.ReplaceAll(wf, `"`+in+`"`, `"`+have+`"`)
		}
		examples = append(examples, fmt.Sprintf("### %s\n\nA workflow that ran on this kind of ComfyUI. Copy it as it is into the project; change only the prompt texts' defaults, the size, the length or the sampler's steps if the look needs it, never a node's other inputs. Its style's nodes: { %s }\n\n```json\n%s\n```", fam.name, fam.nodes, strings.TrimSpace(wf)))
	}
	if len(known) > 0 {
		b.WriteString("\nWhat these make up, and how a workflow in API format runs each:\n")
		b.WriteString(strings.Join(known, "\n"))
		b.WriteString("\n")
	}
	if len(examples) > 0 {
		b.WriteString("\n" + strings.Join(examples, "\n\n") + "\n")
	}
	b.WriteString("\nIn API format, a node's links to others are inputs too: \"model\": [\"16\", 0] takes output 0 of node 16. If ComfyUI refuses a workflow, its error names the node and the input: fix exactly that.")
	return b.String()
}
