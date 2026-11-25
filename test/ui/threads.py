"""A thread in a unit's channel, as a person goes on in it with the agents.
Open WebUI calls an agent only for a message that mentions it, or that is a
Reply to one of its messages (its Reply button): a plain message in the thread
goes to no agent. A thread is not one agent's: another agent mentioned
answers there too. See docs/scenarios.md, "Going on in a thread". Run by
test/ui/run.sh threads.
"""

from lib import Person, browser, check, done, section

NS = "todo"
OPERATOR, COMMANDER = "garrison/operator", "garrison/commander"
b = browser()
bob = Person(b, "bob")

section("1. bob asks the operator in todo's channel")
check("bob signs in to Open WebUI", bob.sign_in_chat)
bob.channel(NS)
asked = bob.say("operator", "Which stacks does the unit todo have? Only their names, in one line.")
check("the operator answers in the thread", bob.next_answer, asked, OPERATOR, 0)

section("2. a plain message in the thread goes to no agent")
bob.reply(asked, "And which repos does todo have? Only their names.")


def nobody_answered():
    bob.page.wait_for_timeout(90_000)
    if len(bob.answers(asked, OPERATOR)) != 1 or bob.answers(asked, COMMANDER):
        raise AssertionError("an agent answered a message that neither mentions nor replies to it")
    return True


check("after a minute and a half, no agent has answered it", nobody_answered)

section("3. Reply on the operator's message goes on with it, without @")
its = bob.answer_ids(asked, OPERATOR)[-1]
sent = check("bob answers it with its Reply button", bob.reply_to, asked, its,
             "Which repos does todo have? Only their names, in one line.") or {}
check("his message is a Reply to the operator's, and mentions nobody",
      lambda: sent.get("reply_to_id") == its and "<@" not in (sent.get("content") or ""))
check("the operator answers it", bob.next_answer, asked, OPERATOR, 1)

section("4. the thread holds several agents: @commander answers there too")
bob.reply(asked, "Which agent makes screenshots? Only its name.", "commander")
check("the commander answers in the same thread", bob.next_answer, asked, COMMANDER, 0)
check("and the operator does not answer that", lambda: len(bob.answers(asked, OPERATOR)) == 2)

done()
