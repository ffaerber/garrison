# Models

The LLMs Garrison's agents use. Root admins change these files by pull request.

- `providers.yaml`: the providers, OpenAI-compatible APIs, and the default model. Written by the setup in the browser after the install, where each provider was tested with its key and the default with a question (or from the install's answers, if they named an LLM).

  **Adding a provider:** on the dashboard's **Models** page (`https://<domain>/models`, root admins only), give its name, the API's base URL (the one `/chat/completions` and `/models` are under) and its API key. The key is saved encrypted in this repo, `secrets/llm-<provider>.yaml`, and the deployer puts it where the agent service reads it; the provider is proposed here as a pull request, to merge like any change. The page also replaces and deletes keys. In the chat, a root admin can ask the operator for it (`add_provider`): it proposes the provider and links the Models page for the key, which never goes into the chat. Or by pull request, by hand:

  ```yaml
  default: ollama-cloud/gpt-oss:120b
  providers:
    ollama-cloud:
      url: https://ollama.com/v1
    openrouter:
      url: https://openrouter.ai/api/v1
      include: ["anthropic/*", "openai/gpt-6*"]   # optional: only these of its models
    claude:                        # Anthropic's OpenAI-compatible API
      url: https://api.anthropic.com/v1
    local:
      url: http://192.168.1.10:11434/v1
  ```

  A key can also go, on the machine, into `/etc/garrison/llm-keys/<provider>` (root only), e.g. `sudo install -m 600 /dev/stdin /etc/garrison/llm-keys/openrouter`, then paste it and press Ctrl-D; a key saved on the Models page replaces such a file of the same name. A provider without a key gets none, as a local Ollama needs.

- `models.yaml`: every model the providers offer, written by the model sync. Once a day, soon after `providers.yaml` changes, and a few minutes later again if a provider could not be asked (its key may not be there yet), the agent service asks each provider for its models:
  - new models are written to `main` at once, turned off: listing one changes nothing an agent uses, and turning one on is a reviewed change;
  - other changes are proposed as a pull request (on a branch `garrison/models-<time>`):
    - a model a provider no longer offers is marked `gone` with the date; agents that name it fall back to the default model, and the pull request names them;
  - a provider that cannot be reached leaves its models as they are.

**Choosing the models:** on the same page, under *Models the agents use*, choose the default model, for every agent, and another for the agents that need one, e.g. a model for code for the coder. The page proposes it as one pull request: the default in `providers.yaml`, each agent's `model` in its `agent.yaml`, and the models chosen turned on in `models.yaml`. Right after the install, the page shows these steps until an agent has a model of its own.

While a single root admin person exists, as right after the setup (the platform's account does not count), nobody else could approve their changes here: theirs pass the approval check by themselves, as in a unit of one person, and merging is their review.

**One model per agent, everywhere:** agents are garrison's alone, so an agent uses the same model in every unit; units cannot choose another.

An agent names its model in its `agent.yaml` as `<provider>/<model>`; a name without a known provider means the default provider. The approval check of a unit's config refuses a model that is not enabled. Agents list the models with the tool `list_models`.
