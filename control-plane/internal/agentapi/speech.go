package agentapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"garrison/control-plane/internal/events"
	"garrison/control-plane/internal/sandbox"
)

// GenerateSpeech is the tool that speaks a text with the installation's
// text-to-speech service (an OpenAI-style /v1/audio/speech, e.g. Kokoro),
// into the unit's media library: the narration of a short.
const GenerateSpeech = "generate_speech"

var speechSpec = toolSpec{
	Name: GenerateSpeech,
	Description: "Speaks a text aloud with the installation's text-to-speech service and puts the audio (MP3) into the unit's media library, /data/media/<project>/, with a .json of how it was made: the narration of a short. " +
		"The voice is the project's, from its narration.yaml (voice, speed), so every narration of a project sounds the same; a project without one takes the voice given, or the service's default. " +
		"Write the text as it is to be spoken: full sentences, numbers as words where they read better.",
	InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"project": map[string]any{"type": "string", "description": "the project's repo, <unit>/<repo>, e.g. animalfacts/shorts"},
			"text":    map[string]any{"type": "string", "description": "what to say"},
			"name":    map[string]any{"type": "string", "description": "a short name for the file, e.g. octopus; lower case, digits and -"},
			"voice":   map[string]any{"type": "string", "description": "only for a project without narration.yaml: a voice of the service, e.g. af_heart"},
		},
		"required": []string{"project", "text", "name"},
	},
}

// DefaultVoice is the voice of a project that names none.
const DefaultVoice = "af_heart"

// MaxSpeech is the longest text spoken at once: a minute or two of audio.
const MaxSpeech = 4000

// Narration is narration.yaml of a project.
type Narration struct {
	Voice string  `yaml:"voice"`
	Speed float64 `yaml:"speed"`
}

// Speech is the installation's text-to-speech service.
type Speech struct {
	URL  string
	HTTP *http.Client
}

var voiceName = regexp.MustCompile(`^[A-Za-z0-9_.+-]{1,64}$`)

// Speak asks the service for text in voice at speed (0: its own), as MP3.
func (t *Speech) Speak(ctx context.Context, text, voice string, speed float64) ([]byte, error) {
	body := map[string]any{"model": "kokoro", "input": text, "voice": voice, "response_format": "mp3"}
	if speed > 0 {
		body["speed"] = speed
	}
	raw, _ := json.Marshal(body)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimSuffix(t.URL, "/")+"/v1/audio/speech", strings.NewReader(string(raw)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	client := t.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("the text-to-speech service: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		why := strings.TrimSpace(string(data))
		if len(why) > 500 {
			why = why[:500] + "…"
		}
		// A voice it lacks is the usual mistake: say which it has.
		if v, err := t.Voices(ctx); err == nil && len(v) > 0 && !slices.Contains(v, voice) {
			why += "; its voices: " + strings.Join(v, ", ")
		}
		return nil, fmt.Errorf("the text-to-speech service answered %s: %s", resp.Status, why)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("the text-to-speech service answered with no audio")
	}
	return data, nil
}

// Voices lists the service's voices: Kokoro's /v1/voices, else the
// OpenAI-style /v1/audio/voices.
func (t *Speech) Voices(ctx context.Context) ([]string, error) {
	client := t.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	var last error
	for _, p := range []string{"/v1/voices", "/v1/audio/voices"} {
		req, err := http.NewRequestWithContext(ctx, "GET", strings.TrimSuffix(t.URL, "/")+p, nil)
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		if err != nil {
			last = err
			continue
		}
		var out struct {
			Voices []string `json:"voices"`
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out)
		resp.Body.Close()
		if resp.StatusCode == 200 && err == nil {
			return out.Voices, nil
		}
		last = fmt.Errorf("%s: %s", p, resp.Status)
	}
	return nil, last
}

// maySpeak tells whether the sandbox of c gets generate_speech: its agent
// lists it, there is a text-to-speech service, and the unit a media library.
func (s *Server) maySpeak(ctx context.Context, c sandbox.Caller) bool {
	if s.Speech == nil || s.Styles == nil {
		return false
	}
	a, ok, err := s.Agents.Get(ctx, c.Agent)
	if err != nil || !ok || !slices.Contains(a.Tools, GenerateSpeech) {
		return false
	}
	ns, _, _ := strings.Cut(c.Agent, "/")
	return s.media(ctx, ns) != ""
}

// speak runs generate_speech for the sandbox c: in is its arguments. It
// answers with the file made, kept for the sandbox to fetch.
func (s *Server) speak(ctx context.Context, c sandbox.Caller, raw json.RawMessage) ([]Output, error) {
	var in struct{ Project, Text, Name, Voice string }
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("arguments: %v", err)
	}
	m := projectName.FindStringSubmatch(in.Project)
	switch {
	case m == nil:
		return nil, fmt.Errorf("project is <unit>/<repo>, e.g. animalfacts/shorts")
	case strings.TrimSpace(in.Text) == "":
		return nil, fmt.Errorf("text is empty: say what to speak")
	case len(in.Text) > MaxSpeech:
		return nil, fmt.Errorf("text is %d characters; at most %d at once: split it", len(in.Text), MaxSpeech)
	case !styleName.MatchString(in.Name):
		return nil, fmt.Errorf("name is lower case letters, digits and -, e.g. octopus")
	}
	ns, repo := m[1], m[3]
	n := Narration{Voice: in.Voice}
	from := "the voice given"
	if rawN, err := s.Styles.Read(ctx, c.Person, ns, repo, "narration.yaml"); err == nil {
		var pn Narration
		dec := yaml.NewDecoder(strings.NewReader(rawN))
		dec.KnownFields(true)
		if err := dec.Decode(&pn); err != nil {
			return nil, fmt.Errorf("narration.yaml of %s: %v (voice: af_heart, speed: 1.0)", in.Project, err)
		}
		if pn.Voice != "" {
			n.Voice, from = pn.Voice, "narration.yaml"
		}
		n.Speed = pn.Speed
	}
	if n.Voice == "" {
		n.Voice, from = DefaultVoice, "the default"
	}
	if !voiceName.MatchString(n.Voice) {
		return nil, fmt.Errorf("voice %q is not a voice name, e.g. af_heart", n.Voice)
	}
	if n.Speed < 0 || n.Speed > 3 {
		return nil, fmt.Errorf("narration.yaml of %s: speed is 0.5 to 2, e.g. 1.0", in.Project)
	}
	audio, err := s.Speech.Speak(ctx, in.Text, n.Voice, n.Speed)
	if err != nil {
		return nil, err
	}
	id, err := s.keep(c, audio)
	if err != nil {
		return nil, err
	}
	stamp := time.Now().UTC().Format("20060102-150405")
	made := map[string]any{
		"project": in.Project, "narration": in.Text, "voice": n.Voice, "voice_from": from,
		"made": time.Now().UTC().Format(time.RFC3339), "by": c.Agent, "for": c.Person,
	}
	if n.Speed > 0 {
		made["speed"] = n.Speed
	}
	return []Output{{ID: id, Name: fmt.Sprintf("narration-%s-%s.mp3", in.Name, stamp), Made: made}}, nil
}

// sandboxSpeak is generate_speech for a sandbox: it answers like
// sandboxGenerate, with the file for the sandbox to fetch.
func (s *Server) sandboxSpeak(w http.ResponseWriter, r *http.Request) {
	c, ok := s.caller(w, r)
	if !ok {
		return
	}
	if !s.maySpeak(r.Context(), c) {
		apiError(w, http.StatusForbidden, "this agent has no generate_speech here: its agent.yaml must list it, and the unit needs a media library and the installation a text-to-speech service (TTS_URL)")
		return
	}
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	files, err := s.speak(r.Context(), c, raw)
	if err != nil {
		writeJSON(w, map[string]any{"files": files, "error": err.Error()})
		return
	}
	ns, _, _ := strings.Cut(c.Agent, "/")
	if s.Events != nil {
		project, _ := files[0].Made["project"].(string)
		s.Events.Record(r.Context(), events.Event{Source: events.SourceAgents, Kind: events.KindMedia, Status: events.StatusOK,
			Unit: ns, Audience: events.AudienceMembers, Actor: c.Person, Agent: c.Agent, StartedBy: c.Run.StartedBy,
			Title: fmt.Sprintf("%s spoke a narration of %s for %s", c.Agent, project, c.Person)})
	}
	writeJSON(w, map[string]any{"files": files})
}
