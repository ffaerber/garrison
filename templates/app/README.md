# App templates

What a new app repo can start with: `create_repo` with `template` writes one folder's files into the repo's main branch, as the person asking. `{{namespace}}` and `{{repo}}` in them become the repo's; `{{registry}}` and `{{commit}}` stay, for the stack.

| Template | What it is |
| --- | --- |
| `static-web/` | One static page (`index.html`) served by nginx on port 8080, with the pipeline that builds its image for every push. |
