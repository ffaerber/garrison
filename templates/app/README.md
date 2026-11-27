# App templates

What a new app repo can start with: `create_repo` with `template` writes one folder's files into the repo's main branch, as the person asking. `{{unit}}` and `{{repo}}` in them become the repo's; `{{registry}}` and `{{commit}}` stay, for the stack. A file stored as `NAME.tmpl` is written as `NAME`: Go files and `go.mod` are kept that way, so that Go does not take them for the control plane's code.

| Template | What it is |
| --- | --- |
| `storyboards/` | The stories of a channel's shorts, one file each (title, sources, narration, scenes). The chain `storyboards` writes them for a person to review; once merged, `@designer` makes each one's short. Its README has the routines. |
| `channel/` | The public page of a unit's channel: every short `@designer` made in its media library, newest first, to watch, with its title and spoken text. Reads the library read-only and serves only the shorts. Its README has the stack. |
| `todo/` | A todo app in Go on Postgres, a list of their own for everyone signed in, with its tests, Dockerfile and the pipeline that tests and builds its image for every push. Its README has the stack to propose. |
