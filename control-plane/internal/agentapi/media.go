package agentapi

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"garrison/control-plane/internal/agents"
	"garrison/control-plane/internal/render"
)

// MediaShare is the share of a unit that is its media library: its
// agents get it at /data/media in its channel and its routines.
const MediaShare = "media"

// MediaLibrary is added to the prompt of agents that have the library.
const MediaLibrary = `

## The media library

/data/media is this unit's media library: pictures, videos and other files its people and agents share, which its people browse and upload in the browser. Put finished media there, one folder per project (/data/media/<project>/), with names that say what each file is; keep drafts in /data/work. Never delete or overwrite a file there that you did not make in this conversation unless the person asks for it by name.`

// MediaFiles reads a unit's config; gateway.Gitea.
type MediaFiles interface {
	Files(ctx context.Context, org, repo, dir string) ([]string, error)
	File(ctx context.Context, org, repo, path string) (string, error)
}

// mediaCache remembers which units have a media library.
type mediaCache struct {
	mu   sync.Mutex
	have map[string]bool
	at   map[string]time.Time
}

// mediaFor is how long whether a unit has a library is remembered.
const mediaFor = time.Minute

// media is the host folder of ns's media library, bound at /data/media in
// its sandboxes, or "" if it has none: one of the stacks of its config, on
// main, mounts the share media, which the deployer then keeps as a dataset.
// garrison, the platform's unit, has none.
func (s *Server) media(ctx context.Context, ns string) string {
	if s.Media == nil || ns == "" || ns == agents.PlatformUnit {
		return ""
	}
	root := s.DataRoot
	if root == "" {
		root = render.DefaultDataRoot
	}
	s.mediaCache.mu.Lock()
	if s.mediaCache.have == nil {
		s.mediaCache.have, s.mediaCache.at = map[string]bool{}, map[string]time.Time{}
	}
	have, fresh := s.mediaCache.have[ns], time.Since(s.mediaCache.at[ns]) < mediaFor
	s.mediaCache.mu.Unlock()
	if !fresh {
		// On an error, what was known stays: a sandbox does not lose its
		// library because Gitea did not answer once.
		if now, err := hasMediaShare(ctx, s.Media, ns); err != nil {
			log.Printf("media: %s: %v", ns, err)
		} else {
			have = now
			s.mediaCache.mu.Lock()
			s.mediaCache.have[ns], s.mediaCache.at[ns] = have, time.Now()
			s.mediaCache.mu.Unlock()
		}
	}
	if !have {
		return ""
	}
	return render.SharePath(root, ns, MediaShare)
}

// hasMediaShare tells whether a stack of ns's config mounts the share media.
func hasMediaShare(ctx context.Context, g MediaFiles, ns string) (bool, error) {
	files, err := g.Files(ctx, ns, "config", "stacks")
	if err != nil {
		return false, err
	}
	for _, f := range files {
		if !strings.HasSuffix(f, ".yaml") {
			continue
		}
		raw, err := g.File(ctx, ns, "config", "stacks/"+f)
		if err != nil {
			return false, err
		}
		var st struct {
			Services map[string]struct {
				Volumes map[string]struct {
					Share string `yaml:"share"`
				} `yaml:"volumes"`
			} `yaml:"services"`
		}
		if yaml.Unmarshal([]byte(raw), &st) != nil {
			continue // the deployer refuses it, so it makes no share either
		}
		for _, svc := range st.Services {
			for _, v := range svc.Volumes {
				if v.Share == MediaShare {
					return true, nil
				}
			}
		}
	}
	return false, nil
}
