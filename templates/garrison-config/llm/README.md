# Models

The LLMs Garrison's agents use. Root admins change these files by pull request.

- `providers.yaml`: the providers, OpenAI-compatible APIs, and the default model. Written at install from its answers; add one by pull request:

  ```yaml
  default: ollama-cloud/gpt-oss:120b
  providers:
    ollama-cloud:
      url: https://ollama.com/v1
    openrouter:
      url: https://openrouter.ai/api/v1
      include: ["anthropic/*", "openai/gpt-6*"]   # optional: only these of its models
    local:
      url: http://192.168.1.10:11434/v1
  ```

  A provider's key goes, on the machine, into `/etc/garrison/llm-keys/<provider>` (root only, never into Git), e.g. `sudo install -m 600 /dev/stdin /etc/garrison/llm-keys/openrouter`, then paste it and press Ctrl-D. A provider without a key file gets none, as a local Ollama needs.

- `models.yaml`: every model the providers offer, written by the model sync. Once a day, and soon after `providers.yaml` changes, the agent service asks each provider for its models and, if they changed, proposes a new `models.yaml` as a pull request (on a branch `garrison/models-<time>`):
  - a new model comes in turned off: set `enabled: true` to let agents use it;
  - a model a provider no longer offers is marked `gone` with the date; agents that name it fall back to the default model, and the pull request names them;
  - a provider that cannot be reached leaves its models as they are.

An agent names its model in its `agent.yaml` as `<provider>/<model>`; a name without a known provider means the default provider. The approval check of a namespace's config refuses a model that is not enabled. Agents list the models with the tool `list_models`.
