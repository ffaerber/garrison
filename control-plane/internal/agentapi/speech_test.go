package agentapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeTTS answers /v1/audio/speech with "MP3 <voice>: <text>", and refuses
// the voice "nobody", as Kokoro does, without saying which it has; those
// are at /v1/voices.
func fakeTTS(t *testing.T) (*Speech, *[]map[string]any) {
	var asked []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/voices" {
			w.Write([]byte(`{"voices":["af_heart","am_adam"]}`))
			return
		}
		if r.Method != "POST" || r.URL.Path != "/v1/audio/speech" {
			http.NotFound(w, r)
			return
		}
		var b map[string]any
		json.NewDecoder(r.Body).Decode(&b)
		asked = append(asked, b)
		if b["voice"] == "nobody" {
			http.Error(w, `{"detail":"Voice 'nobody' not found"}`, http.StatusBadRequest)
			return
		}
		w.Write([]byte("MP3 " + b["voice"].(string) + ": " + b["input"].(string)))
	}))
	t.Cleanup(srv.Close)
	return &Speech{URL: srv.URL}, &asked
}

func TestGenerateSpeechInTheProjectsVoice(t *testing.T) {
	sp, asked := fakeTTS(t)
	s := &Server{Speech: sp, Styles: styles{
		"animalfacts/shorts/narration.yaml": "voice: am_adam\nspeed: 1.1\n",
		"animalfacts/bad/narration.yaml":    "voice: am_adam\npitch: 3\n",
	}}
	say := func(args map[string]any) ([]Output, error) {
		raw, _ := json.Marshal(args)
		return s.speak(context.Background(), alice, raw)
	}

	// The project's voice wins over the one given, so all of it sounds alike.
	out, err := say(map[string]any{"project": "animalfacts/shorts", "text": "Octopuses have three hearts.", "name": "octopus", "voice": "af_heart"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || (*asked)[0]["voice"] != "am_adam" || (*asked)[0]["speed"] != 1.1 || (*asked)[0]["response_format"] != "mp3" {
		t.Fatalf("asked %v, made %+v", *asked, out)
	}
	if !strings.HasPrefix(out[0].Name, "narration-octopus-") || !strings.HasSuffix(out[0].Name, ".mp3") ||
		out[0].Made["voice"] != "am_adam" || out[0].Made["voice_from"] != "narration.yaml" || out[0].Made["narration"] != "Octopuses have three hearts." {
		t.Errorf("output: %+v", out[0])
	}
	if data, _ := s.outputs.files[out[0].ID]; data.path == "" {
		t.Error("the audio is not kept for the sandbox")
	}

	// Without narration.yaml: the voice given, else the default.
	if out, err := say(map[string]any{"project": "media/demo", "text": "Hi.", "name": "hi", "voice": "bf_emma"}); err != nil || out[0].Made["voice"] != "bf_emma" {
		t.Errorf("given voice: %v %+v", err, out)
	}
	if out, err := say(map[string]any{"project": "media/demo", "text": "Hi.", "name": "hi"}); err != nil || out[0].Made["voice"] != DefaultVoice {
		t.Errorf("default voice: %v %+v", err, out)
	}

	// The service's refusal reaches the agent, with the voices it has.
	if _, err := say(map[string]any{"project": "media/demo", "text": "Hi.", "name": "hi", "voice": "nobody"}); err == nil || !strings.Contains(err.Error(), "its voices: af_heart, am_adam") {
		t.Errorf("refused voice: %v", err)
	}
	for _, bad := range []map[string]any{
		{"project": "shorts", "text": "Hi.", "name": "hi"},
		{"project": "media/demo", "text": " ", "name": "hi"},
		{"project": "media/demo", "text": "Hi.", "name": "../x"},
		{"project": "media/demo", "text": strings.Repeat("a", MaxSpeech+1), "name": "hi"},
		{"project": "media/demo", "text": "Hi.", "name": "hi", "voice": "a voice; rm"},
		{"project": "animalfacts/bad", "text": "Hi.", "name": "hi"},
	} {
		if _, err := say(bad); err == nil {
			t.Errorf("%v: no error", bad)
		}
	}
}
