# Problems we cannot fix

Gaps we know of that the platform cannot close itself, usually because Gitea, Open WebUI or Docker offer no way to. For each: why, and what we do instead. Gaps we can and plan to close are in [status.md](status.md), "Known limits".

## Root admins can switch off "changes only by pull request"

Branch protection keeps everyone, root admins included, from pushing to `main` of a config repo (tested: a Gitea site admin's push to `system/config` is refused, by the API and by `git push`). But a site admin can edit or delete the protection rule itself in Gitea's settings, push, and put it back.

- **Why:** Gitea has no setting that binds its site admins; they can change any repo's settings.
- **Instead:** the gateway writes the rule back whenever it differs, at its next sync, so the window is short. Keep the group `root-admins` small. To come: an alert when a protection rule changes, and the change records of the deployer.

## Repos can be deleted in Gitea's UI

- Root admins (Gitea site admins) can delete any repo, in the UI or the API.
- Namespace admins can delete their namespace's repos too, as long as their team has admin access (today). Lowering it to write access, with deletion through an approved pull request done by the gateway, is planned and closes this part.

- **Why:** Gitea has no setting to switch off repo deletion; it follows the access level, and site admins have every level.
- **Instead:** keep deletion rights with root admins only (planned, see above). A deleted repo's history is gone unless there is a backup: snapshots of Gitea's data are not built yet, so today nothing brings a deleted repo back.

## Namespace channels in Open WebUI can be deleted by its admins

People cannot delete or rename a namespace's channel (only its creator, the gateway's account, and Open WebUI admins can). But root admins are Open WebUI admins.

- **Why:** Open WebUI lets every admin delete any channel and has no setting against it.
- **Instead:** the gateway makes a deleted channel again at its next sync, and puts back a renamed one, but the messages of a deleted channel are lost.
- **Workaround (planned):** Traefik refuses Open WebUI's API calls for deleting, renaming and changing the access of channels, for everyone. This is a hack:
  - It depends on Open WebUI's internal API paths (`/api/v1/channels/<id>/delete` and so on), which are not a public interface and may change with any update. A test must try them after every update of Open WebUI.
  - It only guards the way in through Traefik. Whoever reaches Open WebUI's container directly (root on the machine, the gateway) is not stopped.
  - Open WebUI's UI still shows the buttons; they fail with an error.

## People can delete their own messages in channels

- **Why:** Open WebUI 0.12.4 has no setting for it; `USER_PERMISSIONS_CHAT_DELETE_MESSAGE` only covers private chats.
- **Instead:** the same Traefik hack can refuse it, with the same caveats. Without it, a channel is not a complete record.

## Open WebUI does not say which thread an agent is answering in

In a channel, Open WebUI sends the agent service the channel's id, not the thread's, so a channel has one sandbox per agent for all its threads, sharing one workspace and one conversation.

- **Why:** Open WebUI builds the request itself (`chat_id: channel:<id>`); no setting adds the thread.
- **Instead:** each message still carries its thread's history. Private work belongs in a private chat, which has a sandbox of its own.

## Gitea logs agents' Git fetches as the admin

Sandboxes clone and fetch through the agent service, which asks Gitea with the admin account after checking, as the person, that they may read the repo.

- **Why:** Gitea's Git protocol has no Sudo, and the agent service holds no token of the person's for Gitea.
- **Instead:** the agent service logs each fetch with the person and the agent. Changes never go this way: they are patches applied by Gitea's API as the person, so commits, pull requests and branch protection show and bind the person.

## Root on the machine can do everything

Whoever is root on the machine, or can use its Docker daemon, can read every secret, change any container and bypass every rule above.

- **Why:** that is what root is; the platform runs on one machine.
- **Instead:** only the deployer, the gateway, the guards and the system runner reach Docker; people and agents never do. Keep root access to the people who are root admins anyway.

## Agents can misjudge

The LLM sometimes refuses a harmless request or writes a sloppy change (seen in the tests: an agent took a request for a test note as a prompt injection, and another listed bob under both roles in `members.yaml`).

- **Why:** a language model's judgement; better prompts and models reduce it but never remove it.
- **Instead:** nothing an agent does takes effect without a person: changes are pull requests, checked by `homecloud/approval` and merged by someone allowed to. The tests retry where a model may need a second ask.
